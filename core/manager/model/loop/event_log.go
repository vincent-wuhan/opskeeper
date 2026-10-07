// Package loop holds persistence entities for the closed-loop
// orchestrator (zero-manual-ops-loop · D1/D8).
//
// Three tables back the seven-phase state machine:
//
//   - loop_event_log: append-only event source of truth. Every
//     phase transition, contract write, failure, rollback, pause/resume
//     is recorded WITH a unique idempotency_key so writes are
//     exactly-once across instances.
//   - loop_state:     derived snapshot used by the Web UI for O(1)
//     "current phase" lookup. NEVER edited outside the append-event
//     transaction; the truth lives in loop_event_log.
//   - loop_contract:  per-phase contract payload storage (RootCauseJSON /
//     CritiqueScore / VerifiedDelta / PostmortemDoc / ApprovalDecision).
//     Payloads > 64 KB are spilled to OSS (storage_backend="oss") and
//     only the key + metadata are stored inline; this keeps the table
//     skinny and the index useful.
//
// Append-only constraint:
//
//	The loop_event_log table is enforced APPEND-ONLY at the DB layer
//	(see migration `0001-loop-event-log.sql` for the trigger that
//	raises SIGNAL on UPDATE/DELETE). Correcting an event is only
//	possible by APPENDING a new event with event_type="correction".
//	Do NOT add application-level Update/Delete methods to LoopEventLogRepo.
//
// Multi-tenant:
//
//	Every row carries tenant_id; queries MUST filter by tenant. The
//	(tenant_id, created_at) composite index supports tenant-scoped
//	purge ops.
package loop

import "time"

// Event is one row of loop_event_log. Append-only; do not UPDATE or DELETE.
//
// Idempotency: idempotency_key is UNIQUE; replays MUST derive the same key
// from (incident_id, phase, event_type, attempt) so replays hit the
// UNIQUE constraint instead of inserting duplicate rows.
type Event struct {
	// ID is a snowflake (uint64) so multi-instance / archive merges do
	// not collide. Application code MUST NOT interpret this as a
	// timestamp; read loop_event_log.created_at for temporal order.
	ID int64 `gorm:"primaryKey;autoIncrement" json:"id"`

	// IncidentID is the cross-phase incident identifier. Stored as
	// VARCHAR(64) so external systems (alertmanager fingerprints,
	// harness case ids) can round-trip without base conversion.
	IncidentID string `gorm:"index;size:64;not null" json:"incident_id"`

	// TenantID is required for multi-tenant isolation. All read
	// queries MUST filter by tenant_id.
	TenantID string `gorm:"index;size:64;not null" json:"tenant_id"`

	// EventType is one of EventTypePhaseEntered / EventPhaseContractWritten /
	// EventPhaseFailed / EventPhasePaused / EventPhaseResumed / EventRollback /
	// EventRetryExhausted / EventCorrection. Use the constants below.
	EventType string `gorm:"size:32;not null;index" json:"event_type"`

	// Phase is the typed-string phase the event belongs to. VARCHAR(32)
	// — typed string (not iota) to make enum reorder safe.
	Phase string `gorm:"size:32;not null;index" json:"phase"`

	// IdempotencyKey is unique per logical write. Compose as
	//   fmt.Sprintf("%s:%s:%s:%d", incident_id, phase, event_type, attempt)
	// so replays hit the UNIQUE constraint instead of inserting duplicate rows.
	IdempotencyKey string `gorm:"uniqueIndex;size:64;not null" json:"idempotency_key"`

	// Payload is event-type-specific JSON (e.g. sub_task results, error
	// chain, contract_ref). Use jsonb on the DB side for index support.
	Payload string `gorm:"type:json" json:"payload,omitempty"`

	// TraceID is the OTel trace_id allowing cross-system log correlation.
	TraceID string `gorm:"size:64;index" json:"trace_id,omitempty"`

	// CreatedAt is the wall-clock time the event was persisted. The
	// append-only nature of the table means this column also doubles
	// as the canonical "when did this transition happen" timestamp.
	CreatedAt time.Time `gorm:"index;not null" json:"created_at"`
}

const IdempotencyKeyMaxLen = 64

// State is the derived snapshot of the most recent phase for an incident.
// loop_state is NOT source-of-truth — it is rebuilt from loop_event_log
// on crash recovery and otherwise lazy-updated per phase transaction.
//
// Web reads use this table for O(1) "show me the current phase" lookups
// without tailing the event log.
type State struct {
	// IncidentID is the primary key. One row per incident.
	IncidentID string `gorm:"primaryKey;size:64" json:"incident_id"`

	// CurrentPhase is the typed-string phase the orchestrator is in
	// (or stopped at). Empty string means "no events yet — orchestrator
	// has not run".
	CurrentPhase string `gorm:"size:32" json:"current_phase"`

	// LastEventID is the id of the most recent event written for this
	// incident. Useful for "what's new since cursor N" incremental reads.
	LastEventID int64 `json:"last_event_id"`

	// RetryCount tracks recovered→approved rollback attempts. Capped
	// at 3 by the orchestrator's rollback guard; overflow triggers
	// retry_exhausted and the loop enters the failed terminal state.
	RetryCount int `gorm:"default:0" json:"retry_count"`

	// UpdatedAt is the wall-clock time of the last state mutation.
	UpdatedAt time.Time `json:"updated_at"`
}

// Contract is one row of loop_contract — the per-phase output payload.
// Each phase writes one Contract when it transitions successfully;
// downstream phases read by (incident_id, phase, contract_type).
//
// Storage size policy:
//   - payload <= 64 KB → stored inline as JSONB (storage_backend="db")
//   - payload >  64 KB → persisted to OSS, payload holds the OSS key
//   - metadata, storage_backend="oss"
//
// See migration `0001-loop-event-log.sql` for the 64 KB threshold trigger.
type Contract struct {
	// ID is a snowflake (uint64) for cross-archive merge safety.
	ID int64 `gorm:"primaryKey;autoIncrement" json:"id"`

	// IncidentID is the owning incident. Indexed for per-incident reads.
	IncidentID string `gorm:"index;size:64;not null" json:"incident_id"`

	// TenantID is the owning tenant. Incident IDs are caller-supplied and
	// MUST NOT be treated as globally unique; contract reads filter on it.
	//
	// ⑥ tenant_id 强制：model 层不再 `default:''`，写路径必须有非空
	// tenantID（WriteContract 已 enforce）；读路径 ReadContractByID
	// 必须传 tenantID，否则 ⑥ 多租户安全修复不生效。
	TenantID string `gorm:"index;size:64;not null" json:"tenant_id"`

	// Phase is the producing phase. Combined with Type this forms the
	// natural lookup key.
	Phase string `gorm:"size:32;index;not null" json:"phase"`

	// Type is the contract discriminator: "RootCauseJSON" / "CritiqueScore" /
	// "VerifiedDelta" / "PostmortemDoc" / "ApprovalDecision" / "CorrelationSet".
	Type string `gorm:"size:32;not null" json:"type"`

	// SchemaVer is the contract schema version ("v1", "v1.1", ...).
	// ContractValidator rejects unknown / missing schema versions.
	SchemaVer string `gorm:"size:16;not null" json:"schema_version"`

	// Payload is the JSON-encoded contract. For storage_backend="oss"
	// this is a small JSON object {oss_key, size, content_type, sha256}.
	Payload string `gorm:"type:json" json:"payload"`

	// SizeBytes is the raw payload size in bytes. Recorded at write
	// time so the 64 KB threshold can be audited without re-reading.
	SizeBytes int `json:"size_bytes"`

	// StorageBackend is "db" (inline JSONB) or "oss" (offloaded to OSS).
	// Defaults to "db" for backward compatibility.
	StorageBackend string `gorm:"size:16;default:db" json:"storage_backend"`

	// CreatedAt is the write time. Used by the per-tenant purge sweep.
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}

// Loop event type constants. Use these instead of string literals so
// typos surface at compile time and grep finds the producer side.
const (
	EventTypePhaseEntered     = "phase_entered"
	EventPhaseContractWritten = "phase_contract_written"
	EventPhaseFailed          = "phase_failed"
	EventPhasePaused          = "phase_paused"
	EventPhaseResumed         = "phase_resumed"
	EventRollback             = "rollback"
	EventRetryExhausted       = "retry_exhausted"
	EventCorrection           = "correction"
)

// Loop terminal-state phases (loop_event_log accepts these as the
// "phase" column value on a failure event, but no transition rule
// allows leaving them).
const (
	PhaseFailed  = "failed"
	PhaseAborted = "aborted"
)

// Contract size policy constants. Payloads above InlinePayloadMaxBytes
// are offloaded to OSS by the contract repository; the inline cap keeps
// the loop_event_log / loop_contract tables from bloat.
const (
	// InlinePayloadMaxBytes is the threshold for JSONB inline storage.
	// Payloads larger than this are offloaded to OSS; the contract
	// row then holds the OSS key + metadata only.
	InlinePayloadMaxBytes = 64 * 1024

	// StorageBackendDB / StorageBackendOSS mark the storage destination
	// on the Contract.StorageBackend column.
	StorageBackendDB  = "db"
	StorageBackendOSS = "oss"
)

// TableName returns the explicit table name for the Event struct.
// GORM uses this when AutoMigrating; explicit naming avoids
// deriving "events" (which would clash with the existing alert.events
// table in some test schemas).
func (Event) TableName() string { return "loop_event_log" }

// TableName returns the explicit table name for the State struct.
func (State) TableName() string { return "loop_state" }

// TableName returns the explicit table name for the Contract struct.
func (Contract) TableName() string { return "loop_contract" }
