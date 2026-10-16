package runtimemgmt

import "time"

// Claim is the durable runtime-claim record. There is at most one
// active claim per task; replaced claims keep their history in the
// RecoveryLog table (sibling model in a later slice). The
// Claim.Generation counter advances every time the orchestrator hands
// the task to a new claimer; runtime checks it on every
// checkpoint/progress/result call to detect a stale-lease recovery
// that already promoted someone else.
//
// Spec scenario "Worker restarts after checkpoint" relies on
// Generation + ClaimedBy semantics: a worker recovering from a
// checkpoint MUST observe the same (Generation, ClaimedBy) pair that
// produced the checkpoint; otherwise it sees ErrClaimLost instead of
// duplicating side effects.
type Claim struct {
	// TaskID is the owning durable task; one row per task.
	TaskID string `gorm:"primaryKey;size:64"`

	TenantID string `gorm:"index;size:64;not null"`

	// Generation is incremented every time a new claim supersedes
	// the prior one (stale-lease, manual takeover, node restart
	// that promoted a backup). The runtime's checkpoint / progress
	// / result calls MUST echo the value they observed at claim
	// time; the orchestrator rejects on mismatch.
	Generation int64 `gorm:"not null;default:0"`

	// ClaimedBy is the runtime identity string ("runtime:edge:abc",
	// "runtime:worker:harness:42", etc.) that owns the current
	// generation. Indexed so dashboards can drill by runtime.
	ClaimedBy string `gorm:"size:128;not null;index"`

	// ClaimedAt is the wall-clock time the runtime accepted the
	// claim. Distinguishes "when did generation N start" from the
	// prepare-lease deadline, which is claimed_at + prepare_lease.
	ClaimedAt time.Time `gorm:"index;not null"`

	// LeasedUntil is the rolling liveness-lease deadline. The
	// runtime must renew / checkpoint before this time or the
	// orchestrator can safely expire the claim and recover.
	LeasedUntil time.Time `gorm:"index;not null"`

	// OwnerLeaseID is the most recent lease extension token. The
	// orchestrator writes a new token every time it advances
	// LeasedUntil; the runtime echoes it on its next checkpoint.
	// Empty before the first renewal.
	OwnerLeaseID string `gorm:"size:64;not null;default:''"`
}

// TableName pins the schema name. Migrations MUST keep this exact
// name; the orchestrator's claim/lease state machine reads by task_id.
func (Claim) TableName() string { return "runtimemgmt_claims" }

// IsStale reports whether the claim's lease is past its deadline at
// "now". A stale claim may be safely recovered (advanced) by the
// orchestrator's stale-lease sweeper.
func (c Claim) IsStale(now time.Time) bool {
	if c.LeasedUntil.IsZero() {
		return true
	}
	return !now.Before(c.LeasedUntil)
}

// BelongsToRuntime reports whether candidate (the runtime id echoed
// back by an arriving checkpoint/progress/result call) matches the
// claim. The runtime identity comparison is exact: a re-registration
// or recovery promotes a new generation rather than reuse this one.
func (c Claim) BelongsToRuntime(candidate string) bool {
	if c.ClaimedBy == "" || candidate == "" {
		return false
	}
	return c.ClaimedBy == candidate
}

// AdvanceTo is the recovery-time helper. It returns a new Claim with
// Generation+1, ClaimedBy=nextRuntime, ClaimedAt=now, LeasedUntil
// reset by the caller, and OwnerLeaseID blanked. The pre-existing
// row is preserved in the recovery log (a separate table added in a
// later slice); this helper only computes the replacement record.
//
// The orchestrator MUST persist the recovery log entry in the same
// transaction as the Claim write.
func (c Claim) AdvanceTo(nextRuntime string, now time.Time, newLeasedUntil time.Time) Claim {
	return Claim{
		TaskID:       c.TaskID,
		TenantID:     c.TenantID,
		Generation:   c.Generation + 1,
		ClaimedBy:    nextRuntime,
		ClaimedAt:    now.UTC(),
		LeasedUntil:  newLeasedUntil.UTC(),
		OwnerLeaseID: "",
	}
}
