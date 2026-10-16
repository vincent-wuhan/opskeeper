package runtimemgmt

import "time"

// AttemptRecord is one attempted execution of a task. A task can have
// multiple attempts (initial + retries); each becomes one
// AttemptRecord row. Auto-retry is gated by IdempotencyContract +
// FailureClass.IsRetryable.
//
// The row also doubles as the per-attempt safety / classification
// snapshot, so post-mortem and the failure API can render the retry
// decision tree without joining on each retry call.
type AttemptRecord struct {
	// TaskID + AttemptNumber composite primary key (1-based). The
	// TaskID field is a VARCHAR(64), the AttemptNumber is int.
	TaskID string `gorm:"primaryKey;size:64"`

	// AttemptNumber is 1-based. The orchestrator reserves 0 for
	// "no attempts yet" in projected state but does not persist
	// it.
	AttemptNumber int `gorm:"primaryKey"`

	TenantID string `gorm:"index;size:64;not null"`

	// MaxAttempts is the upper bound the orchestrator will retry
	// this task. Read at first attempt and snapshotted into every
	// retry so a runtime config change does not retroactively
	// expand the budget.
	MaxAttempts int `gorm:"not null;default:1"`

	// ParentRetryID optionally links this attempt to the prior
	// failed attempt within the same task. Empty on the initial
	// attempt; populated on retries so dashboards can render a
	// retry tree.
	ParentRetryID string `gorm:"size:64;index;default:''"`

	// StartedAt is the wall-clock time the attempt began (state
	// transitions into preparing). Indexed for SLA / latency views.
	StartedAt time.Time `gorm:"index;not null"`

	// EndedAt is when the attempt reached a terminal state (success
	// or classified failure). Indexed alongside StartedAt so retry
	// trees can show attempt duration.
	EndedAt *time.Time `gorm:"index"`

	// Outcome is empty while the attempt is in flight; on terminal
	// state it carries one of AttemptOutcomeSucceeded,
	// AttemptOutcomeFailed, AttemptOutcomeCancelled,
	// AttemptOutcomeSuperseded. "Superseded" is the terminal a
	// stale-lease promotion marks on the prior attempt.
	Outcome string `gorm:"size:16;not null;default:''"`

	// FailureClass is empty on success. On terminal failure it
	// carries the FailureClass taxonomy value the orchestrator
	// assigned at classification time.
	FailureClass FailureClass `gorm:"size:32;not null;default:''"`

	// FailureReason is the human-readable error message paired with
	// FailureClass. Worker-emitted strings are sanitized before
	// being persisted; see TaskResult.SanitizerStatus for the gate.
	FailureReason string `gorm:"type:text"`

	// RuntimeID is the runtime that owned this attempt. Mirrors
	// Claim.ClaimedBy at attempt start; kept on the AttemptRecord
	// for replay / post-mortem queries so they do not have to
	// re-walk the Claim history table.
	RuntimeID string `gorm:"size:128;not null;index"`

	// ParentTaskID optionally links a follow-on task (e.g. a retry
	// that the orchestrator decided to re-materialize as a new
	// durable row). Distinct from ParentRetryID (which is
	// in-task) so an attempt-tree and a follow-on-task-tree can
	// co-exist.
	ParentTaskID string `gorm:"size:64;index;default:''"`
}

// TableName pins the schema name. Migrations MUST keep this exact
// name; the orchestrator reads by (task_id, attempt_number).
func (AttemptRecord) TableName() string { return "runtimemgmt_attempt_records" }

// Attempt outcomes — typed strings for forward compatibility. Applies
// to AttemptRecord.Outcome column.
const (
	AttemptOutcomeSucceeded  = "succeeded"
	AttemptOutcomeFailed     = "failed"
	AttemptOutcomeCancelled  = "cancelled"
	AttemptOutcomeSuperseded = "superseded"
)

// IsTerminalOutcome reports whether outcome is one of the four
// terminal states an AttemptRecord can carry.
func IsTerminalOutcome(outcome string) bool {
	switch outcome {
	case AttemptOutcomeSucceeded,
		AttemptOutcomeFailed,
		AttemptOutcomeCancelled,
		AttemptOutcomeSuperseded:
		return true
	default:
		return false
	}
}

// FailureClassOf returns the persisted failure class if the attempt
// ended in a terminal failure; empty on success / superseded /
// cancelled. The helper exists because dashboards / replayer code
// otherwise has to compare Outcome twice.
func (a AttemptRecord) FailureClassOf() FailureClass {
	if a.Outcome == AttemptOutcomeFailed {
		return a.FailureClass
	}
	return ""
}

// HasRetryBudget reports whether the task has at least one more
// attempt available (AttemptNumber < MaxAttempts). The orchestrator
// uses this together with failure class to decide between
// retry-via-checkpoint, retry-via-new-attempt, and terminal-failed.
func (a AttemptRecord) HasRetryBudget() bool {
	if a.MaxAttempts <= 0 {
		return false
	}
	return a.AttemptNumber < a.MaxAttempts
}

// Elapsed reports the wall-clock duration the attempt was in flight.
// Returns zero if the attempt has not ended yet.
func (a AttemptRecord) Elapsed() time.Duration {
	if a.EndedAt == nil {
		return 0
	}
	return a.EndedAt.Sub(a.StartedAt)
}
