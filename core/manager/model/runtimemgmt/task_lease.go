package runtimemgmt

import "time"

// Lease carries both prepare- and liveness-lease deadlines for one
// claim. It is split from the Claim row because (a) prepare-lease is
// bounded and short, while liveness-lease rolls with checkpoints,
// and (b) recovery wants to extend one without rewriting the other.
//
// Field semantics (design.md §Durable task model):
//   - Prepare deadline is when "preparing" must transition to
//     "running" or fail. It is set at claim time and never reset.
//   - Liveness deadline is rolled forward by each checkpoint,
//     progress, or lease-renew call.
//
// A zero time on either deadline means "not started yet" (claim has
// not assigned it) — the orchestrator MUST treat that as
// non-actionable rather than expired.
type Lease struct {
	// TaskID is the owning durable task; one row per task.
	// Indexed via primary-key on (TaskID, Generation) so a recovered
	// claim keeps its past lease history natural.
	TaskID string `gorm:"primaryKey;size:64"`

	// Generation matches the row's Claim.Generation. Together they
	// form the natural primary key.
	Generation int64 `gorm:"primaryKey"`

	TenantID string `gorm:"index;size:64;not null"`

	// PrepareDeadline is when the preparing state must transition to
	// running or be failed. Set at claim time and never reset.
	PrepareDeadline time.Time `gorm:"index"`

	// LivenessDeadline is when the running state must checkpoint
	// or be considered lost. Rolled forward by renew/checkpoint.
	LivenessDeadline time.Time `gorm:"index"`

	// ExtendedAt is the wall-clock time of the last successful
	// extend / checkpoint. Diagnostics only; not used for the
	// deadline math.
	ExtendedAt time.Time

	// Extensions counts how many times the orchestrator has rolled
	// the liveness lease forward. Bounded by the task kind's retry
	// budget (see AttemptRecord.MaxAttempts).
	Extensions int `gorm:"not null;default:0"`
}

// TableName pins the schema name. Migrations MUST keep this exact
// name; the orchestrator's lease sweeper reads by (task_id,
// generation).
func (Lease) TableName() string { return "runtimemgmt_leases" }

// HasPrepare reports whether the prepare deadline is set. The lease
// exists from claim time, but the deadline is only meaningful once
// the runtime has begun preparing inputs.
func (l Lease) HasPrepare() bool {
	return !l.PrepareDeadline.IsZero()
}

// HasLiveness reports whether the liveness deadline is set. Tasks in
// running state always have one; queued/admitted/terminal tasks
// return false.
func (l Lease) HasLiveness() bool {
	return !l.LivenessDeadline.IsZero()
}

// PrepareExpired reports whether the prepare deadline has elapsed at
// "now". A zero deadline returns false (callers must use HasPrepare
// first to disambiguate "not started" vs "not expired").
func (l Lease) PrepareExpired(now time.Time) bool {
	if l.PrepareDeadline.IsZero() {
		return false
	}
	return !now.Before(l.PrepareDeadline)
}

// LivenessExpired reports whether the liveness deadline has elapsed
// at "now". Same zero-deadline semantics as PrepareExpired.
func (l Lease) LivenessExpired(now time.Time) bool {
	if l.LivenessDeadline.IsZero() {
		return false
	}
	return !now.Before(l.LivenessDeadline)
}

// NewPrepareDeadline is the canonical claim-time preparation-deadline
// helper. It returns claimTime + ttl; ttl<=0 returns the zero time so
// callers can opt out of a prepare window for read-only tasks.
func NewPrepareDeadline(claimTime time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 || claimTime.IsZero() {
		return time.Time{}
	}
	return claimTime.Add(ttl).UTC()
}

// NewLivenessDeadline is the canonical first-running checkpoint
// helper. Returns startTime + ttl; ttl<=0 returns the zero time.
func NewLivenessDeadline(startTime time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 || startTime.IsZero() {
		return time.Time{}
	}
	return startTime.Add(ttl).UTC()
}

// ExtendLiveness rolls the deadline forward by ttl from "now",
// increments Extensions, and updates ExtendedAt. It returns a copy;
// callers MUST persist the result explicitly (the Lease receiver is
// not mutated so the same value can be used in audit logs).
//
// An expired lease is NOT extended here; the orchestrator MUST
// surface ErrLeaseExpired upstream and let recovery promote the
// claim rather than silently revive a stale row.
func (l Lease) ExtendLiveness(now time.Time, ttl time.Duration) (Lease, error) {
	if l.LivenessDeadline.IsZero() {
		return l, ErrLeaseExpired
	}
	if l.LivenessExpired(now) {
		return l, ErrLeaseExpired
	}
	if ttl <= 0 {
		return l, ErrLeaseExpired
	}
	l.LivenessDeadline = now.Add(ttl).UTC()
	l.ExtendedAt = now.UTC()
	l.Extensions++
	return l, nil
}
