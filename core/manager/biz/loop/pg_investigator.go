package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type PGStatInvestigatorToolset struct {
	db *sql.DB
}

func NewPGStatInvestigatorToolset(dsn string) (*PGStatInvestigatorToolset, error) {
	if dsn == "" {
		return nil, errors.New("loop: PostgreSQL investigator DSN is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("loop: open PostgreSQL investigator: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(time.Minute)
	return &PGStatInvestigatorToolset{db: db}, nil
}

func (t *PGStatInvestigatorToolset) Investigate(ctx context.Context, resourceType, alertID string, timeWindow TimeWindow) ([]EvidenceItem, error) {
	if t == nil || t.db == nil {
		return nil, errors.New("loop: PostgreSQL investigator is not configured")
	}
	if err := t.db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("loop: PostgreSQL investigator ping: %w", err)
	}
	const query = `
SELECT coalesce(jsonb_agg(row_to_json(activity) order by activity.xact_start), '[]'::jsonb)::text
FROM (
  SELECT pid, usename, application_name, state, xact_start, query
  FROM pg_stat_activity
  WHERE xact_start IS NOT NULL
    AND state = 'idle in transaction'
    AND now() - xact_start > interval '60 seconds'
) AS activity`
	var raw string
	if err := t.db.QueryRowContext(ctx, query).Scan(&raw); err != nil {
		return nil, fmt.Errorf("loop: PostgreSQL investigator query: %w", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, fmt.Errorf("loop: decode PostgreSQL activity: %w", err)
	}
	return []EvidenceItem{{
		Tool:      "pg_stat_activity",
		Query:     "state=idle in transaction AND age(xact_start)>60s",
		Value:     rows,
		Count:     len(rows),
		Timestamp: time.Now().UTC(),
	}}, nil
}

func (t *PGStatInvestigatorToolset) ListRemediations(_ context.Context, resourceType string) ([]RemediationOption, error) {
	if resourceType != "pg" && resourceType != "postgres" && resourceType != "postgresql" {
		return nil, fmt.Errorf("loop: PostgreSQL investigator unsupported resource_type=%s", resourceType)
	}
	return []RemediationOption{{
		Action:      "pg_terminate_backend",
		Target:      "pg:primary",
		Risk:        "mutating",
		AutoApprove: false,
	}}, nil
}

func (t *PGStatInvestigatorToolset) Close() error {
	if t == nil || t.db == nil {
		return nil
	}
	return t.db.Close()
}

var _ InvestigatorToolset = (*PGStatInvestigatorToolset)(nil)
