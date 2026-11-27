//go:build e2e

// Package explore is the S1 foundation of the mcp-ai-test-entry change:
// the objective half of "AI explores, the verdict decides". Everything
// here is test-only code; the server side is untouched by design.
//
// This file is the audit read port (design.md 裁决三): a direct MySQL
// read of audit_logs. It opens no HTTP endpoint, adds none to the
// server, and exists because the question the verdict function answers
// ("what actually happened") is a question about the table, not about
// what a query surface is willing to show.
package explore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// AuditRow is the subset of audit_logs the verdict reads. Field names
// mirror the wire log of GET /v1/admin/audit-logs so the cross-check
// (CrossCheckWithAdminAPI) compares like with like.
type AuditRow struct {
	ID           uint64
	OccurredAt   time.Time
	Role         string
	Action       string
	ResourceType string
	ResourceID   string
	Status       string
	ErrorCode    string
	ErrorMessage string
	RequestID    string
	PayloadJSON  string
}

// AuditFilter selects rows. Empty fields are not filtered on. The reader
// is read-only: no method on Reader writes, updates, or deletes.
type AuditFilter struct {
	Worker       string // matches payload->actor.worker (AgentTeams rows)
	Action       string
	ResourceType string
	SinceID      uint64 // rows with ID > SinceID
}

// PayloadPhase is the audit sink's two-phase marker: "start" rows are
// written before a tool runs, "end" rows after it. OnToolEnd fires
// regardless of the tool's own error, so a *failed* tool call still has
// both phases — the shape that distinguishes "stopped at the approval
// gate" (error present, phases complete) from "never finished" is the
// error code, not a missing row.
type PayloadPhase struct {
	Phase     string `json:"phase"`
	ToolName  string `json:"tool"`
	Duration  int64  `json:"duration_ms"`
	ErrorCode string `json:"code"`
}

// Tenant returns the caller identity the server recorded for this row.
// The MCP audit sink writes the AgentTeams caller at payload.actor.tenant_id
// and falls back to payload.tenant for other callers; both are read so the
// tenant invariant judges every row by the same rule. An empty result means
// the row names no tenant, which the caller treats as a finding.
func (r AuditRow) Tenant() string {
	if r.PayloadJSON == "" {
		return ""
	}
	var p struct {
		Tenant string `json:"tenant"`
		Actor  struct {
			TenantID string `json:"tenant_id"`
		} `json:"actor"`
	}
	if err := json.Unmarshal([]byte(r.PayloadJSON), &p); err != nil {
		return ""
	}
	if p.Actor.TenantID != "" {
		return p.Actor.TenantID
	}
	return p.Tenant
}

// ParsePayload decodes the row's payload JSON. Malformed payloads decode
// to a zero PayloadPhase rather than failing the read: the verdict's job
// is to report what the table says, and a row whose payload is not JSON
// is itself a finding.
func (r AuditRow) ParsePayload() PayloadPhase {
	var p PayloadPhase
	_ = json.Unmarshal([]byte(r.PayloadJSON), &p)
	return p
}

// Reader reads audit_logs directly over the testenv DSN.
type Reader struct {
	db *sql.DB
}

// NewReader opens one connection pool against the DSN. The caller owns
// closing it (Close); testenv's shared MySQL container lives for the
// whole `go test` process, so a reader opened per test is fine.
func NewReader(dsn string) (*Reader, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("explore: open audit reader: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("explore: ping audit reader: %w", err)
	}
	return &Reader{db: db}, nil
}

func (r *Reader) Close() error { return r.db.Close() }

// Rows returns rows matching f, ordered by ID.
//
// Worker filtering is payload-side (JSON_EXTRACT over the actor object)
// because AgentTeams start rows carry the worker in payload.actor.worker,
// not in a column; end rows carry no actor at all, so the filter only
// constrains start rows in SQL and the verdict pairs phases in Go via
// rowsByPhase. The request is built with placeholders only.
func (r *Reader) Rows(ctx context.Context, f AuditFilter) ([]AuditRow, error) {
	q, args, err := r.selectRows(f)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("explore: query audit_logs: %w", err)
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var row AuditRow
		if err := rows.Scan(&row.ID, &row.OccurredAt, &row.Role, &row.Action, &row.ResourceType, &row.ResourceID, &row.Status, &row.ErrorCode, &row.ErrorMessage, &row.RequestID, &row.PayloadJSON); err != nil {
			return nil, fmt.Errorf("explore: scan audit row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (r *Reader) selectRows(f AuditFilter) (string, []any, error) {
	sb := strings.Builder{}
	args := []any{}
	sb.WriteString("SELECT id, occurred_at, role, action, resource_type, resource_id, status, error_code, error_message, request_id, payload_json FROM audit_logs")
	conds := []string{"1=1"}
	if f.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, f.Action)
	}
	if f.ResourceType != "" {
		conds = append(conds, "resource_type = ?")
		args = append(args, f.ResourceType)
	}
	if f.SinceID > 0 {
		conds = append(conds, "id > ?")
		args = append(args, f.SinceID)
	}
	if f.Worker != "" {
		sb.Reset()
		sb.WriteString("SELECT id, occurred_at, role, action, resource_type, resource_id, status, error_code, error_message, request_id, payload_json FROM audit_logs WHERE ")
		sb.WriteString(strings.Join(conds, " AND "))
		sb.WriteString(" AND (JSON_UNQUOTE(JSON_EXTRACT(payload_json, '$.actor.worker')) = ? OR JSON_UNQUOTE(JSON_EXTRACT(payload_json, '$.phase')) = 'end')")
		args = append(args, f.Worker)
		sb.WriteString(" ORDER BY id")
		return sb.String(), args, nil
	}
	sb.WriteString(" WHERE ")
	sb.WriteString(strings.Join(conds, " AND "))
	sb.WriteString(" ORDER BY id")
	return sb.String(), args, nil
}

// Count returns the number of rows matching f (worker filter included).
// It exists for the boundary test's snapshot: "zero new rows" is a count
// comparison, not a re-scan of every row.
func (r *Reader) Count(ctx context.Context, f AuditFilter) (int64, error) {
	conds := []string{"1=1"}
	args := []any{}
	if f.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, f.Action)
	}
	if f.ResourceType != "" {
		conds = append(conds, "resource_type = ?")
		args = append(args, f.ResourceType)
	}
	if f.SinceID > 0 {
		conds = append(conds, "id > ?")
		args = append(args, f.SinceID)
	}
	if f.Worker != "" {
		conds = append(conds, `JSON_UNQUOTE(JSON_EXTRACT(payload_json, '$.actor.worker')) = ?`)
		args = append(args, f.Worker)
	}
	q := "SELECT COUNT(*) FROM audit_logs WHERE " + strings.Join(conds, " AND ")
	var n int64
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("explore: count audit_logs: %w", err)
	}
	return n, nil
}

// MaxID returns the highest audit row ID (0 when the table is empty).
// The boundary test snapshots this before a rejected call and asserts it
// is unchanged after: a stronger claim than a count, because a row
// deleted *and* a row written would keep the count while moving the max.
func (r *Reader) MaxID(ctx context.Context) (uint64, error) {
	var id sql.NullInt64
	if err := r.db.QueryRowContext(ctx, "SELECT MAX(id) FROM audit_logs").Scan(&id); err != nil {
		return 0, fmt.Errorf("explore: max audit id: %w", err)
	}
	if !id.Valid {
		return 0, nil
	}
	if id.Int64 < 0 {
		return 0, nil
	}
	return uint64(id.Int64), nil
}

// AdminAPIResponse mirrors the fields of GET /v1/admin/audit-logs that
// the cross-check compares. It is decoded from the JSON body the server
// actually returns, not constructed from the table.
type AdminAPIResponse struct {
	Items []struct {
		ID           uint64 `json:"id"`
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
		Status       string `json:"status"`
	} `json:"items"`
	Total int64 `json:"total"`
}

// CrossCheckWithAdminAPI compares the direct read against the server's
// query surface for one action filter. GET /v1/admin/audit-logs pages at
// 50 by default and its Total is the filtered count, so the comparison
// the cross-check can honestly make is: direct-read count == API Total
// for the same action. The query surface is NOT the verdict's input;
// this exists to prove the reader is reading the table the server
// writes (design.md 裁决三).
func CrossCheckWithAdminAPI(directCount, apiTotal int64, action string) error {
	if directCount != apiTotal {
		return fmt.Errorf(
			"audit cross-check failed for action=%q: direct read found %d row(s), admin API reports total=%d — the reader is not reading the table the server writes",
			action, directCount, apiTotal)
	}
	return nil
}
