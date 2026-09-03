\connect opskeeper

CREATE TABLE IF NOT EXISTS incident_timeline (
    id                UUID PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    incident_id       TEXT NOT NULL,
    occurred_at       TIMESTAMPTZ NOT NULL,
    phase             TEXT NOT NULL,
    event_type        TEXT NOT NULL,
    actor_type        TEXT NOT NULL CHECK (actor_type IN ('agent','human','system')),
    actor             TEXT NOT NULL,
    status            TEXT NOT NULL,
    action_fingerprint TEXT,
    evidence          JSONB NOT NULL DEFAULT '{}'::jsonb,
    evidence_ref      TEXT,
    trace_id          TEXT,
    recovery_signal   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, incident_id, id)
);
CREATE INDEX IF NOT EXISTS idx_incident_timeline_lookup
    ON incident_timeline (tenant_id, incident_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_incident_timeline_event
    ON incident_timeline (tenant_id, event_type, occurred_at);
-- Resolve duplicate groups before applying this index. The application
-- startup migrator performs the same check and fails closed.
CREATE UNIQUE INDEX IF NOT EXISTS uq_incident_timeline_trace_event
    ON incident_timeline (tenant_id, incident_id, trace_id, event_type)
    WHERE trace_id IS NOT NULL AND trace_id <> '';

CREATE TABLE IF NOT EXISTS runbook_memory (
    id                   UUID PRIMARY KEY,
    tenant_id            TEXT NOT NULL,
    incident_id          TEXT NOT NULL,
    database_type        TEXT NOT NULL,
    database_version     TEXT NOT NULL,
    topology_fingerprint TEXT NOT NULL,
    fault_fingerprint    TEXT NOT NULL,
    symptom              JSONB NOT NULL,
    diagnosis            JSONB NOT NULL,
    effective_actions    JSONB NOT NULL DEFAULT '[]'::jsonb,
    ineffective_actions  JSONB NOT NULL DEFAULT '[]'::jsonb,
    recovery_signals     JSONB NOT NULL DEFAULT '[]'::jsonb,
    applicable_boundary  JSONB NOT NULL DEFAULT '{}'::jsonb,
    source_evidence      JSONB NOT NULL DEFAULT '[]'::jsonb,
    confirmed_by         TEXT NOT NULL,
    confirmed_at         TIMESTAMPTZ NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, incident_id)
);
CREATE INDEX IF NOT EXISTS idx_runbook_memory_recall
    ON runbook_memory (tenant_id, database_type, fault_fingerprint);

CREATE TABLE IF NOT EXISTS rrf_recall_log (
    id              UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    incident_id     TEXT NOT NULL,
    query_text      TEXT NOT NULL,
    candidate_ref   TEXT NOT NULL,
    vector_rank     INTEGER,
    keyword_rank    INTEGER,
    rrf_score       DOUBLE PRECISION NOT NULL,
    selected        BOOLEAN NOT NULL DEFAULT FALSE,
    rejected_reason TEXT,
    recalled_at     TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_rrf_recall_log_incident
    ON rrf_recall_log (tenant_id, incident_id, recalled_at);
