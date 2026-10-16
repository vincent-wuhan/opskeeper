package runtimemgmt

import "time"

// Checkpoint is one durable resume pointer recorded by the worker.
// A task can hold many checkpoints (one per attempt, plus optional
// intra-attempt progress checkpoints). The latest one for the active
// (task_id, attempt_number) pair is the resume target; older
// checkpoints are kept for replay / audit.
//
// Spec scenario "Worker restarts after checkpoint" relies on this:
// on restart, the worker reads the latest Checkpoint row for its
// claim and resumes from PayloadRef/ResumeHint instead of starting
// over. Mutating workers MUST also compare
// IdempotencyContract.Fingerprint to confirm the resume target
// matches the original side-effect surface.
type Checkpoint struct {
	// ID is an auto-incremented row id. The natural lookup is
	// (TaskID, AttemptNumber, Sequence), but a surrogate id keeps
	// the checkpoint rows append-only-friendly and easy to garbage
	// collect via DELETE on the storage layer once a new attempt
	// supersedes them.
	ID int64 `gorm:"primaryKey;autoIncrement"`

	// TaskID is the owning task.
	TaskID string `gorm:"index;size:64;not null"`

	TenantID string `gorm:"index;size:64;not null"`

	// AttemptNumber ties the checkpoint to one AttemptRecord. 1-based
	// so attempt 0 (the initial queued row) is unambiguous.
	AttemptNumber int `gorm:"not null;default:1"`

	// Sequence numbers checkpoints inside one attempt: 1 for the
	// first checkpoint of the attempt, then monotonic. AutoIncrement
	// per (TaskID, AttemptNumber).
	Sequence int `gorm:"not null;default:1"`

	// Generation mirrors Claim.Generation at write time. Recovery
	// skips checkpoints whose generation is behind the current
	// claim's generation (stale-lease → new claimer → old
	// checkpoint from a different runtime).
	Generation int64 `gorm:"not null;default:0"`

	// Phase is the optional worker-supplied phase label
	// ("collecting_evidence", "writing_result"). Free-form short
	// string for diagnostics, not part of the resume contract.
	Phase string `gorm:"size:64;not null;default:''"`

	// ResumeHint is the worker-supplied opaque cursor the next
	// claim reads first ("evidence_id=42", "page_token=..."). The
	// orchestrator passes it through verbatim; storage stays opaque
	// to the control plane.
	ResumeHint string `gorm:"type:text"`

	// PayloadRef points to the durable payload (workspace artefact,
	// OSS key, or DB row id) that backs the resume. The runtime
	// owns the semantics of this ref; the control plane only
	// persists it.
	PayloadRef string `gorm:"size:255;not null;default:''"`

	// RecordedAt is wall-clock when the worker wrote this row.
	RecordedAt time.Time `gorm:"index;not null"`

	// RecordedBy mirrors Claim.ClaimedBy at write time, so post-
	// mortem queries can show which runtime produced a checkpoint
	// without joining the Claim row.
	RecordedBy string `gorm:"size:128;not null;default:''"`
}

// TableName pins the schema name. Migrations MUST keep this exact
// name; the orchestrator reads by (task_id, attempt_number, sequence).
func (Checkpoint) TableName() string { return "runtimemgmt_checkpoints" }

// ResumeFor returns the latest checkpoint row for (TaskID,
// AttemptNumber) ordered by Sequence desc. The result is the resume
// target for the next worker; nil means no resume pointer yet.
//
// NOTE: callers pass a slice of Checkpoint for that task/attempt and
// the function returns the best one without scanning the storage
// layer; the actual repository implementation owns the SQL.
func ResumeFor(checkpoints []Checkpoint) *Checkpoint {
	var best *Checkpoint
	for i := range checkpoints {
		c := &checkpoints[i]
		if c == nil {
			continue
		}
		if best == nil || c.Sequence > best.Sequence {
			best = c
		}
	}
	return best
}

// IsForCurrentClaim reports whether the checkpoint's generation
// matches the live Claim.Generation. Stale-claim recovery uses this
// to skip checkpoints written by the prior claimer.
func (c Checkpoint) IsForCurrentClaim(claimGeneration int64) bool {
	return c.Generation == claimGeneration && c.Generation != 0
}
