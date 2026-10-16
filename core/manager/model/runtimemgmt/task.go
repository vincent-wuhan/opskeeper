package runtimemgmt

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Task is the central durable row that §2.1 enumerates. The other
// eight domain concepts (state, idempotency, admission, claim, lease,
// checkpoint, attempt, failure-class, result) are split into their
// own tables for clean append-only semantics; the Task row carries
// the identity, current state, and pointer fields that join them.
//
// Multi-tenant: every query MUST filter on TenantID.
//
// Storage invariants (enforced by the orchestrator + repo layer, not
// at the row level):
//   - Once State is terminal (succeeded / failed / cancelled) only
//     the failure API may write the matching TaskResult row.
//   - AdmissionDecision must exist before State can become
//     TaskStateAdmitted.
//   - Claim must exist before State can become TaskStateClaimed.
//   - Lease.PrepareDeadline is set when Claim is written.
//   - IdempotencyContract must exist BEFORE the first Claim row IF
//     Safety == TaskSafetyMutating (the orchestrator's claim flow
//     enforces it; the row is opaque here).
type Task struct {
	// TaskID is the durable primary key. UUID v4 (see NewTaskID).
	// Tenant-scoped, not globally unique, so TenantID is part of
	// every read.
	TaskID string `gorm:"primaryKey;size:64"`

	TenantID string `gorm:"index;size:64;not null"`

	// IncidentID is the optional OpsKeeper incident id this task
	// belongs to. Empty for non-incident workflows
	// (infrastructure probes, preview-pg builds).
	IncidentID string `gorm:"index;size:64;default:''"`

	// Kind is the producer-supplied task type
	// ("read_only.diagnostic", "repair.preview.execute",
	// "agentteams.task", etc.). routing key for handlers; the
	// control plane is type-agnostic.
	Kind string `gorm:"size:64;not null;index"`

	// Safety classifies side-effect footprint (see task_safety.go).
	// Required to be one of TaskSafetyValues at insert time.
	Safety TaskSafety `gorm:"size:16;not null;index"`

	// State is the current durable phase. See task_state.go for the
	// allowed transitions.
	State TaskState `gorm:"size:16;not null;index"`

	// PayloadDigest is the SHA-256 hex digest of the producer-
	// supplied payload bytes. Recorded at insert time so the
	// admission/replay paths can verify they received the expected
	// input.
	PayloadDigest string `gorm:"size:64;not null;default:''"`

	// DesiredRuntimeSelector is the producer-supplied runtime
	// expression (e.g. "kind=chat_worker tier=ha") that admission
	// matched against. Free-form for v1.
	DesiredRuntimeSelector string `gorm:"size:128;not null;default:''"`

	// RuntimeID is the actual runtime that owns the active claim.
	// Empty before the first claim; populated and cleared on
	// recovery / supersede.
	RuntimeID string `gorm:"size:128;not null;default:'';index"`

	// WorkspaceID is the durable workspace identity (see §3). Empty
	// for tasks that do not allocate a workspace (e.g. short
	// inventory reads).
	WorkspaceID string `gorm:"size:64;not null;default:'';index"`

	// Priority is producer-supplied (0 = default). Used by the
	// admission service for tie-breaking; not a strict SLA. Indexed
	// alongside State so high-priority queues are cheap to find.
	Priority int `gorm:"not null;default:0;index"`

	// TraceID is the OTel trace_id (or empty). Joinable to the
	// runtime event timeline via (TraceID, CreatedAt).
	TraceID string `gorm:"size:64;index;default:''"`

	// CreatedAt is when the producer inserted the row.
	CreatedAt time.Time `gorm:"not null;index"`

	// UpdatedAt is the wall-clock time of the last State mutation.
	// The append-only child tables (Event, Checkpoint, AttemptRecord,
	// TaskResult) keep their own timestamps for ordering; this one
	// is the cheap "what's the current state?" lookup.
	UpdatedAt time.Time `gorm:"not null"`
}

// TableName pins the schema name. Migrations MUST keep this exact
// name; the orchestrator reads/writes by TaskID for state mutations.
func (Task) TableName() string { return "runtimemgmt_tasks" }

// ComputePayloadDigest is the canonical producer-side hasher. It
// accepts arbitrary bytes (typically the marshalled payload JSON)
// and returns the hex SHA-256 string the Task row stores. Splitting
// the helper out keeps marshalling tests honest about which bytes
// produced the digest.
func ComputePayloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// NewTask is the constructor the producer path uses. It fills the
// required identifiers, computes PayloadDigest, and stamps CreatedAt
// + UpdatedAt from the same wall-clock instant. State starts at
// TaskStateQueued (the admission service moves it to Admitted).
func NewTask(
	tenantID, incidentID, kind, payload string, payloadBytes []byte,
	safety TaskSafety, selector, traceID string, priority int, now time.Time,
) (Task, error) {
	if tenantID == "" || kind == "" {
		return Task{}, ErrUnknownState
	}
	if !IsTaskSafety(safety) {
		return Task{}, ErrUnknownState
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return Task{
		TaskID:                 NewTaskID(),
		TenantID:               tenantID,
		IncidentID:             incidentID,
		Kind:                   kind,
		Safety:                 safety,
		State:                  TaskStateQueued,
		PayloadDigest:          ComputePayloadDigest(payloadBytes),
		DesiredRuntimeSelector: selector,
		TraceID:                traceID,
		Priority:               priority,
		CreatedAt:              now,
		UpdatedAt:              now,
		// payload parameter is documentary; the digest is the
		// persisted fingerprint. Producers that want the raw bytes
		// recoverable should attach them via a sibling storage path.
	}, nil
}

// CanTransition validates the (current State -> next) edge using the
// same table IsValidTransition consults. Use this on every State
// write so a stale/illegal state mutation surfaces
// ErrInvalidTransition instead of silently corrupting the task.
func (t Task) CanTransition(next TaskState) error {
	if IsTerminal(t.State) {
		return ErrTerminalState
	}
	if !IsValidTransition(t.State, next) {
		return ErrInvalidTransition
	}
	return nil
}

// WithState returns a copy of t with State=next and UpdatedAt=now.
// Zero "now" means "use the current UTC clock". The original task
// value is not mutated; callers MUST persist the returned copy.
func (t Task) WithState(next TaskState, now time.Time) (Task, error) {
	if err := t.CanTransition(next); err != nil {
		return t, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	t.State = next
	t.UpdatedAt = now
	return t, nil
}
