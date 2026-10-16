package runtimemgmt

// TaskSafety classifies the side-effect footprint of a task. It is
// the input to the automatic-retry gate and to admission/quota
// decisions. Defined by design.md §Durable task model and spec
// requirement "Mutating tasks declare idempotency before automatic
// retry".
//
// Three values cover the v1 surface:
//   - ReadOnly:    probe / diagnostic / inventory. Always safely
//                  retryable.
//   - Disposable:  a disposable preview runtime (preview-pg, scratch
//                  runner). Side effects scoped to the runtime; safe
//                  to drop on retry.
//   - Mutating:    durable side effects outside the runtime. Requires
//                  an IdempotencyContract fingerprint BEFORE the first
//                  claim to be eligible for automatic retry.
type TaskSafety string

const (
	TaskSafetyReadOnly   TaskSafety = "read_only"
	TaskSafetyDisposable TaskSafety = "disposable"
	TaskSafetyMutating   TaskSafety = "mutating"
)

// TaskSafetyValues is the closed enumeration. Used by validation.
var TaskSafetyValues = []TaskSafety{
	TaskSafetyReadOnly,
	TaskSafetyDisposable,
	TaskSafetyMutating,
}

// IsTaskSafety reports whether s is in TaskSafetyValues. Empty
// strings and unknown values return false.
func IsTaskSafety(s TaskSafety) bool {
	for _, v := range TaskSafetyValues {
		if v == s {
			return true
		}
	}
	return false
}

// RequiresIdempotencyContract returns true when a task of this safety
// class cannot be safely auto-retried without a pre-claim
// IdempotencyContract. Read-only and disposable tasks do not require
// one (the failure-class gate is the only constraint).
func (s TaskSafety) RequiresIdempotencyContract() bool {
	return s == TaskSafetyMutating
}
