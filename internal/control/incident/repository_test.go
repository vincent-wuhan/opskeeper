package incident

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSQLRepository_AppendAndReplayTimeline(t *testing.T) {
	repository, _ := setupRepository(t)
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{
			ID: "017f2b01-1001-4000-8000-000000000001", TenantID: "opskeeper-demo", IncidentID: "INC-SQL-001",
			OccurredAt: base, Phase: "detection", EventType: EventAlertReceived, ActorType: "system",
			Actor: "prometheus", Status: "firing", EvidenceRef: "evidence/alert.json", TraceID: "trace-sql",
		},
		{
			ID: "017f2b01-1002-4000-8000-000000000002", TenantID: "opskeeper-demo", IncidentID: "INC-SQL-001",
			OccurredAt: base.Add(time.Minute), Phase: "verification", EventType: EventRecovery, ActorType: "system",
			Actor: "probe", Status: "passed", EvidenceRef: "evidence/recovery.json", TraceID: "trace-sql", RecoverySignal: true,
		},
		{
			ID: "017f2b01-1003-4000-8000-000000000003", TenantID: "opskeeper-demo", IncidentID: "INC-SQL-001",
			OccurredAt: base.Add(2 * time.Minute), Phase: "closure", EventType: EventClosed, ActorType: "agent",
			Actor: "closer", Status: "closed", EvidenceRef: "evidence/closure.json", TraceID: "trace-sql",
		},
	}
	for _, event := range events {
		require.NoError(t, repository.Append(context.Background(), event))
	}

	stored, err := repository.ListIncident(context.Background(), "opskeeper-demo", "INC-SQL-001")
	require.NoError(t, err)
	require.Equal(t, events, stored)

	report, err := ComputeReport(stored)
	require.NoError(t, err)
	require.Equal(t, 1, report.IncidentCount)
	require.Equal(t, 0, report.WrongClosureCount)

	tenantEvents, err := repository.ListTenant(context.Background(), "opskeeper-demo")
	require.NoError(t, err)
	require.Len(t, tenantEvents, 3)
}

func TestSQLRepository_DuplicateEvent_IsRejected(t *testing.T) {
	repository, _ := setupRepository(t)
	event := Event{
		ID: "017f2b01-2001-4000-8000-000000000001", TenantID: "opskeeper-demo", IncidentID: "INC-SQL-002",
		OccurredAt: time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC), Phase: "detection",
		EventType: EventAlertReceived, ActorType: "system", Actor: "prometheus", Status: "firing",
		EvidenceRef: "evidence/alert.json", TraceID: "trace-sql",
	}
	require.NoError(t, repository.Append(context.Background(), event))
	err := repository.Append(context.Background(), event)
	require.True(t, errors.Is(err, ErrDuplicateEvent), err)
}

func TestSQLRepository_DuplicateTraceEventWithDifferentIDAndEvidence_IsRejected(t *testing.T) {
	repository, _ := setupRepository(t)
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	event := Event{
		ID: "017f2b01-2101-4000-8000-000000000001", TenantID: "opskeeper-demo", IncidentID: "INC-SQL-003",
		OccurredAt: base, Phase: "detection", EventType: EventAlertReceived, ActorType: "system",
		Actor: "prometheus", Status: "firing", EvidenceRef: "evidence/alert-1.json", TraceID: "trace-sql",
	}
	require.NoError(t, repository.Append(context.Background(), event))

	event.ID = "017f2b01-2102-4000-8000-000000000002"
	event.EvidenceRef = "evidence/alert-2.json"
	event.OccurredAt = base.Add(time.Second)
	err := repository.Append(context.Background(), event)
	require.True(t, errors.Is(err, ErrDuplicateEvent), err)
}

func TestEventValidate_RecoverySignalMustMatchEventType(t *testing.T) {
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	event := Event{
		ID: "017f2b01-2201-4000-8000-000000000001", TenantID: "opskeeper-demo", IncidentID: "INC-SQL-004",
		OccurredAt: base, Phase: "verification", EventType: EventRecovery, ActorType: "system",
		Actor: "probe", Status: "passed", EvidenceRef: "evidence/recovery.json", TraceID: "trace-sql",
	}
	require.ErrorContains(t, event.Validate(), "recovery event requires recovery_signal=true")

	event.RecoverySignal = true
	require.NoError(t, event.Validate())

	event.EventType = EventRootCause
	require.ErrorContains(t, event.Validate(), "only recovery events may set recovery_signal=true")
}

func TestMigrate_CreatesTraceEventIndexAndRejectsDuplicates(t *testing.T) {
	_, db := setupRepository(t)

	insert := func(id, traceID string) {
		require.NoError(t, db.Exec(`INSERT INTO incident_timeline
			(id, tenant_id, incident_id, occurred_at, phase, event_type, actor_type, actor, status, evidence, trace_id)
			VALUES (?, 'opskeeper-demo', 'INC-SQL-005', '2026-09-01 09:00:00', 'detection', ?, 'system', 'prometheus', 'firing', '{}', ?)`,
			id, EventAlertReceived, traceID).Error)
	}
	insert("017f2b01-2301-4000-8000-000000000001", "trace-a")
	insert("017f2b01-2304-4000-8000-000000000004", "trace-a")
	require.ErrorContains(t, Migrate(db), "duplicate trace events")

	require.NoError(t, db.Exec("DELETE FROM incident_timeline WHERE trace_id = 'trace-a'").Error)
	require.NoError(t, Migrate(db))
	insert("017f2b01-2302-4000-8000-000000000002", "trace-a")
	err := db.Exec(`INSERT INTO incident_timeline
		(id, tenant_id, incident_id, occurred_at, phase, event_type, actor_type, actor, status, evidence, trace_id)
		VALUES ('017f2b01-2303-4000-8000-000000000003', 'opskeeper-demo', 'INC-SQL-005', '2026-09-01 09:00:01', 'detection', ?, 'system', 'prometheus', 'firing', '{}', 'trace-a')`,
		EventAlertReceived).Error
	require.ErrorContains(t, err, "UNIQUE constraint failed")
}

func TestSQLRepository_PostgresSchema_WhenDSNProvided(t *testing.T) {
	dsn := os.Getenv("INCIDENT_PG_DSN")
	if dsn == "" {
		t.Skip("set INCIDENT_PG_DSN to run the PostgreSQL integration test")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS incident_timeline (
		id UUID PRIMARY KEY, tenant_id TEXT NOT NULL, incident_id TEXT NOT NULL, occurred_at TIMESTAMPTZ NOT NULL,
		phase TEXT NOT NULL, event_type TEXT NOT NULL, actor_type TEXT NOT NULL, actor TEXT NOT NULL,
		status TEXT NOT NULL, action_fingerprint TEXT, evidence JSONB NOT NULL DEFAULT '{}'::jsonb, evidence_ref TEXT,
		trace_id TEXT, recovery_signal BOOLEAN NOT NULL DEFAULT FALSE, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`).Error)
	require.NoError(t, db.Exec(`CREATE INDEX IF NOT EXISTS idx_incident_timeline_lookup
		ON incident_timeline (tenant_id, incident_id, occurred_at)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS runbook_memory (
		id UUID PRIMARY KEY, tenant_id TEXT NOT NULL, incident_id TEXT NOT NULL, database_type TEXT NOT NULL,
		database_version TEXT NOT NULL, topology_fingerprint TEXT NOT NULL, fault_fingerprint TEXT NOT NULL,
		symptom JSONB NOT NULL, diagnosis JSONB NOT NULL, effective_actions JSONB NOT NULL DEFAULT '[]'::jsonb,
		ineffective_actions JSONB NOT NULL DEFAULT '[]'::jsonb, recovery_signals JSONB NOT NULL DEFAULT '[]'::jsonb,
		applicable_boundary JSONB NOT NULL DEFAULT '{}'::jsonb, source_evidence JSONB NOT NULL DEFAULT '[]'::jsonb,
		confirmed_by TEXT NOT NULL, confirmed_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), UNIQUE(tenant_id, incident_id)
	)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS rrf_recall_log (
		id UUID PRIMARY KEY, tenant_id TEXT NOT NULL, incident_id TEXT NOT NULL, query_text TEXT NOT NULL,
		candidate_ref TEXT NOT NULL, vector_rank INTEGER, keyword_rank INTEGER, rrf_score DOUBLE PRECISION NOT NULL,
		selected BOOLEAN NOT NULL DEFAULT FALSE, rejected_reason TEXT, recalled_at TIMESTAMPTZ NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`).Error)
	require.NoError(t, db.Exec("DELETE FROM incident_timeline WHERE tenant_id = 'goai-integration'").Error)
	require.NoError(t, db.Exec("DELETE FROM rrf_recall_log WHERE tenant_id = 'goai-integration'").Error)
	require.NoError(t, db.Exec("DELETE FROM runbook_memory WHERE tenant_id = 'goai-integration'").Error)

	repository := NewSQLRepository(db)
	event := Event{
		ID: "017f2b01-3001-4000-8000-000000000001", TenantID: "goai-integration", IncidentID: "INC-PG-001",
		OccurredAt: time.Date(2026, 8, 29, 16, 0, 0, 0, time.UTC), Phase: "detection",
		EventType: EventAlertReceived, ActorType: "system", Actor: "pg-exporter", Status: "firing",
		EvidenceRef: "evidence/alert.json", TraceID: "trace-pg",
	}
	require.NoError(t, repository.Append(context.Background(), event))
	require.ErrorIs(t, repository.Append(context.Background(), event), ErrDuplicateEvent)
	stored, err := repository.ListTenant(context.Background(), "goai-integration")
	require.NoError(t, err)
	require.Equal(t, []Event{event}, stored)

	postmortem := testPostmortem("INC-PG-MEM-001", "pool_capacity_exhausted")
	postmortem.TenantID = "goai-integration"
	require.NoError(t, repository.SaveRunbook(context.Background(), postmortem))
	runbooks, err := repository.ListRunbooks(context.Background(), "goai-integration", "PostgreSQL", "pool_capacity_exhausted")
	require.NoError(t, err)
	require.Len(t, runbooks, 1)
	require.Equal(t, postmortem.EffectiveActions, runbooks[0].EffectiveActions)

	ranked, err := SelectTopRRF([]RankedCandidate{
		{CandidateRef: "runbook:INC-PG-MEM-001", VectorRank: 1, KeywordRank: 2},
		{CandidateRef: "runbook:other", VectorRank: 3, KeywordRank: 1},
	})
	require.NoError(t, err)
	recalledAt := time.Date(2026, 8, 29, 18, 0, 0, 0, time.UTC)
	logs := make([]RecallLog, 0, len(ranked))
	for _, candidate := range ranked {
		logs = append(logs, RecallLog{
			TenantID: "goai-integration", IncidentID: "INC-PG-RECALL-001",
			QueryText: "pool saturation", CandidateRef: candidate.CandidateRef,
			VectorRank: candidate.VectorRank, KeywordRank: candidate.KeywordRank,
			RRFScore: candidate.RRFScore, Selected: candidate.Selected,
			RejectedReason: candidate.RejectedReason, RecalledAt: recalledAt,
		})
	}
	require.NoError(t, repository.AppendRecallLogs(context.Background(), logs))
	storedLogs, err := repository.ListRecallLogs(context.Background(), "goai-integration", "INC-PG-RECALL-001")
	require.NoError(t, err)
	require.Len(t, storedLogs, 2)
	require.NoError(t, db.Exec("DELETE FROM incident_timeline WHERE tenant_id = 'goai-integration'").Error)
	require.NoError(t, db.Exec("DELETE FROM rrf_recall_log WHERE tenant_id = 'goai-integration'").Error)
	require.NoError(t, db.Exec("DELETE FROM runbook_memory WHERE tenant_id = 'goai-integration'").Error)
}

func setupRepository(t *testing.T) (Repository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`CREATE TABLE incident_timeline (
		id text PRIMARY KEY, tenant_id text NOT NULL, incident_id text NOT NULL, occurred_at datetime NOT NULL,
		phase text NOT NULL, event_type text NOT NULL, actor_type text NOT NULL, actor text NOT NULL,
		status text NOT NULL, action_fingerprint text, evidence text NOT NULL DEFAULT '{}', evidence_ref text,
		trace_id text, recovery_signal numeric NOT NULL DEFAULT 0
		)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE runbook_memory (
		id text PRIMARY KEY, tenant_id text NOT NULL, incident_id text NOT NULL, database_type text NOT NULL,
		database_version text NOT NULL, topology_fingerprint text NOT NULL, fault_fingerprint text NOT NULL,
		symptom text NOT NULL, diagnosis text NOT NULL, effective_actions text NOT NULL DEFAULT '[]',
		ineffective_actions text NOT NULL DEFAULT '[]', recovery_signals text NOT NULL DEFAULT '[]',
		applicable_boundary text NOT NULL DEFAULT '{}', source_evidence text NOT NULL DEFAULT '[]',
		confirmed_by text NOT NULL, confirmed_at datetime NOT NULL, created_at datetime NOT NULL,
		updated_at datetime NOT NULL, UNIQUE(tenant_id, incident_id)
	)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE rrf_recall_log (
		id text PRIMARY KEY, tenant_id text NOT NULL, incident_id text NOT NULL, query_text text NOT NULL,
		candidate_ref text NOT NULL, vector_rank integer, keyword_rank integer, rrf_score real NOT NULL,
		selected numeric NOT NULL DEFAULT 0, rejected_reason text, recalled_at datetime NOT NULL,
		created_at datetime NOT NULL
	)`).Error)
	return NewSQLRepository(db), db
}
