package runtimemgmt

// TaskState is the durable phase of one task row. The state machine is
// the v1 set defined by OpenSpec design.md §Durable task model:
//
//	queued -> admitted -> claimed -> preparing -> running
//	  -> succeeded | failed | cancelled
//
// Constants are typed-strings (NOT a numeric iota) so future enum
// reorders stay backward-compatible in storage and over the API.
type TaskState string

const (
	// TaskStateQueued is the row's birth state: persisted by the
	// producer that requested the work, before admission has run.
	TaskStateQueued TaskState = "queued"

	// TaskStateAdmitted is reached after slot-before-claim admission
	// passes and the runtime selector is satisfied. From here, claim
	// is allowed.
	TaskStateAdmitted TaskState = "admitted"

	// TaskStateClaimed means a specific runtime owns this task via
	// the Claim row (claim generation + claimed-by). The prepare
	// lease is now ticking.
	TaskStateClaimed TaskState = "claimed"

	// TaskStatePreparing is the brief window during which the runtime
	// resolves inputs, workspace, plugins and credentials. A short
	// prepare-lease bounds it. From here only Running or Failed.
	TaskStatePreparing TaskState = "preparing"

	// TaskStateRunning is the steady-state execution. Liveness lease
	// must be renewed by checkpoints.
	TaskStateRunning TaskState = "running"

	// TaskStateSucceeded is a terminal state. The TaskResult row
	// holds the sanitized outcome.
	TaskStateSucceeded TaskState = "succeeded"

	// TaskStateFailed is a terminal state. The TaskResult row holds
	// the FailureClass and reason; mutating tasks auto-retry is
	// gated by IdempotencyContract.
	TaskStateFailed TaskState = "failed"

	// TaskStateCancelled is a terminal state for operator-initiated
	// cancellation. Survives across node / worker restarts.
	TaskStateCancelled TaskState = "cancelled"
)

// AllTaskStates is the closed set of task states for membership checks
// (e.g. validation in scan / repair jobs). Keep aligned with the
// individual constants above — adding a state MUST append to both.
var AllTaskStates = []TaskState{
	TaskStateQueued,
	TaskStateAdmitted,
	TaskStateClaimed,
	TaskStatePreparing,
	TaskStateRunning,
	TaskStateSucceeded,
	TaskStateFailed,
	TaskStateCancelled,
}

// ValidTransitions is the authoritative adjacency map. Keys MUST be
// non-terminal states; values are the allowed next states for that
// phase. Terminal states have no entries.
//
// Paths encoded:
//
//	queued     -> admitted, failed, cancelled
//	admitted   -> claimed, failed, cancelled
//	claimed    -> preparing, failed, cancelled
//	preparing  -> running,  failed, cancelled
//	running    -> succeeded, failed, cancelled
//
// The map is intentionally explicit; do NOT encode closures or
// fallbacks. Stale-claim recovery uses the From-state observed at
// recovery time, not a back-edges from terminal.
var ValidTransitions = map[TaskState]map[TaskState]struct{}{
	TaskStateQueued: {
		TaskStateAdmitted:  {},
		TaskStateFailed:    {},
		TaskStateCancelled: {},
	},
	TaskStateAdmitted: {
		TaskStateClaimed:   {},
		TaskStateFailed:    {},
		TaskStateCancelled: {},
	},
	TaskStateClaimed: {
		TaskStatePreparing: {},
		TaskStateFailed:    {},
		TaskStateCancelled: {},
	},
	TaskStatePreparing: {
		TaskStateRunning:   {},
		TaskStateFailed:    {},
		TaskStateCancelled: {},
	},
	TaskStateRunning: {
		TaskStateSucceeded: {},
		TaskStateFailed:    {},
		TaskStateCancelled: {},
	},
}

// IsValidTransition reports whether the (from -> to) edge is allowed by
// ValidTransitions. Unknown "from" values return false; the caller is
// expected to surface ErrInvalidTransition rather than logging-and-
// continuing silently.
func IsValidTransition(from, to TaskState) bool {
	if _, ok := ValidTransitions[from]; !ok {
		return false
	}
	_, ok := ValidTransitions[from][to]
	return ok
}

// IsTerminal reports whether s is one of succeeded / failed / cancelled.
// Read-only paths use this to decide whether to surface recovery
// affordances or stop polling.
func IsTerminal(s TaskState) bool {
	switch s {
	case TaskStateSucceeded, TaskStateFailed, TaskStateCancelled:
		return true
	default:
		return false
	}
}

// Normalize maps unknown or empty state values to the safe sentinel
// behaviour: returns ErrUnknownState without side effects. Callers
// that must continue (e.g. legacy replayers) should treat the error
// as "skip this row" and record the observation.
func Normalize(s string) (TaskState, error) {
	switch TaskState(s) {
	case TaskStateQueued,
		TaskStateAdmitted,
		TaskStateClaimed,
		TaskStatePreparing,
		TaskStateRunning,
		TaskStateSucceeded,
		TaskStateFailed,
		TaskStateCancelled:
		return TaskState(s), nil
	default:
		return "", ErrUnknownState
	}
}
