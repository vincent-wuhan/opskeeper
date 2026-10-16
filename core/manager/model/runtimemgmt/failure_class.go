package runtimemgmt

// FailureClass is a closed taxonomy applied to a terminal Failed task.
// It is the input to the orchestrator's automatic-retry decision and
// to Dashboard's failure views, and the input that the failure API
// (tasks.md §2.3) records alongside the TaskResult.
//
// Values are typed-strings (not iota) so future additions stay
// backward compatible with rows already on disk. The constants below
// MUST stay in lock-step with FailureClassValues.
type FailureClass string

const (
	// FailureClassNoIdempotencyContract means the task was marked
	// mutating but no IdempotencyContract fingerprint was committed
	// before the first claim. Spec scenario
	// "A mutating task has no idempotency contract" requires this
	// exact class so retry stays terminal.
	FailureClassNoIdempotencyContract FailureClass = "no_idempotency_contract"

	// FailureClassLeaseExpired means the runtime lost the lease
	// (liveness or prepare) without producing a checkpoint; recovery
	// MUST classify this before retrying.
	FailureClassLeaseExpired FailureClass = "lease_expired"

	// FailureClassRuntimeLost means the claiming runtime went
	// recently_lost / offline before producing a checkpoint; recovery
	// may resume on a different runtime subject to task policy.
	FailureClassRuntimeLost FailureClass = "runtime_lost"

	// FailureClassAdmissionDenied means slot-before-claim admission
	// refused to queue the task. Distinct from ErrNoCapacity (which
	// is a transient signal); this class is terminal.
	FailureClassAdmissionDenied FailureClass = "admission_denied"

	// FailureClassWorkspaceUnavailable means the task's workspace
	// could not be acquired (lock busy, root missing). Terminal
	// unless workspace ownership is migrated.
	FailureClassWorkspaceUnavailable FailureClass = "workspace_unavailable"

	// FailureClassCredentialDenied means the credential broker
	// refused issuance for the requested scope (audit incomplete,
	// grant bound to a revoked task, etc.).
	FailureClassCredentialDenied FailureClass = "credential_denied"

	// FailureClassPluginIncompatible means the pinned plugin release
	// snapshot failed compatibility or capability verification.
	FailureClassPluginIncompatible FailureClass = "plugin_incompatible"

	// FailureClassSideEffectDuplicate means a duplicate side effect
	// was about to occur and the task was halted before issuance.
	FailureClassSideEffectDuplicate FailureClass = "side_effect_duplicate"

	// FailureClassWorkerError is the catch-all for worker-emitted
	// errors that are not classified into a tighter bucket. Carries
	// the worker's structured error in TaskResult.ErrorMessage.
	FailureClassWorkerError FailureClass = "worker_error"

	// FailureClassCancelled means the task reached Failed via
	// operator-initiated cancellation — distinct from the dedicated
	// TaskStateCancelled terminal so we keep Failed terminal semantics
	// available to recovery flows.
	FailureClassCancelled FailureClass = "cancelled"

	// FailureClassInternal means the control plane itself errored
	// (DB write, scheduler bug). Surfaced to oncall, never auto-retried.
	FailureClassInternal FailureClass = "internal"
)

// FailureClassValues is the closed enumeration, used by validation.
var FailureClassValues = []FailureClass{
	FailureClassNoIdempotencyContract,
	FailureClassLeaseExpired,
	FailureClassRuntimeLost,
	FailureClassAdmissionDenied,
	FailureClassWorkspaceUnavailable,
	FailureClassCredentialDenied,
	FailureClassPluginIncompatible,
	FailureClassSideEffectDuplicate,
	FailureClassWorkerError,
	FailureClassCancelled,
	FailureClassInternal,
}

// IsFailureClass reports whether s is a member of FailureClassValues.
// Used by the failure API to reject unknown labels.
func IsFailureClass(s FailureClass) bool {
	for _, v := range FailureClassValues {
		if v == s {
			return true
		}
	}
	return false
}

// IsClassified reports whether fc is one of the recognized classes.
// Returns ErrFailureClassInvalid for the empty string or any unknown
// value so callers can branch with errors.Is.
func (fc FailureClass) Validate() error {
	if fc == "" {
		return ErrFailureClassInvalid
	}
	if !IsFailureClass(fc) {
		return ErrFailureClassInvalid
	}
	return nil
}

// IsRetryable reports whether orchestrators MAY attempt automatic
// recovery for this class. Mutating tasks additionally require a
// pre-claim IdempotencyContract; read-only and disposable-preview
// tasks are always safe.
//
// The boolean is the v1 default; new failure classes added to the
// taxonomy MUST take a position here in the same change.
func (fc FailureClass) IsRetryable(taskSafety TaskSafety) bool {
	switch fc {
	case FailureClassNoIdempotencyContract,
		FailureClassAdmissionDenied,
		FailureClassCredentialDenied,
		FailureClassPluginIncompatible,
		FailureClassSideEffectDuplicate,
		FailureClassInternal:
		// Never auto-retry — these are policy-level refusals or
		// control-plane bugs that the operator must look at.
		return false
	case FailureClassLeaseExpired, FailureClassRuntimeLost:
		// Retry-eligible by class; mutating tasks additionally
		// require an IdempotencyContract (gated upstream).
		return true
	case FailureClassWorkspaceUnavailable:
		// Retry only when the safety class allows — read-only may
		// re-acquire; mutating tasks depend on workspace policy.
		return taskSafety == TaskSafetyReadOnly
	case FailureClassWorkerError:
		// Worker-emitted errors are retryable for read-only; mutating
		// tasks rely on idempotency.
		return taskSafety != TaskSafetyMutating
	case FailureClassCancelled:
		return false
	default:
		return false
	}
}
