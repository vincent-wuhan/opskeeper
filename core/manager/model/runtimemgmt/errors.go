// Package runtimemgmt holds the durable persistence entities for the
// runtime management control plane (OpenSpec change `runtime-management-gap`,
// §2 Durable Task Lifecycle).
//
// §2.1 enumerates nine domain concepts that together form the durable
// task row plus its supporting contracts:
//
//	Task                — the central durable record (task.go)
//	TaskState           — task state machine (task_state.go)
//	IdempotencyContract — fingerprint committed before the first claim (task_idempotency.go)
//	AdmissionDecision   — slot-before-claim admission snapshot (task_admission.go)
//	Claim               — runtime claim generation + ownership (task_claim.go)
//	Lease               — prepare-lease and liveness-lease deadlines (task_lease.go)
//	Checkpoint          — resume pointer + payload reference (task_checkpoint.go)
//	AttemptRecord       — append of one attempted execution (task_attempt.go)
//	FailureClass        — terminal-failure taxonomy (failure_class.go)
//	TaskResult          — terminal result + sanitizer status (task_result.go)
//
// The package is strictly additive: a new `runtimemgmt_*` table family
// and matching Go types. It does NOT mutate or supersede the existing
// chat_task_executions / approvals / loop_event_log tables; the
// orchestrator keeps those for the seven-phase incident loop, and the
// runtime control plane only consults them via already-public APIs.
//
// Multi-tenant: every Task, Claim, Lease, Checkpoint, AttemptRecord
// and TaskResult row carries TenantID; queries MUST filter by tenant.
// The (tenant_id, task_id) and (tenant_id, runtime_id) composite
// indexes support tenant-scoped ops.
package runtimemgmt

import "errors"

// Sentinel errors used across the package. They are exported so
// callers (admission, claim, lease recovery) can branch on them with
// errors.Is and so tests can pin behaviour without string compares.
var (
	// ErrInvalidTransition is returned when a requested task state
	// transition is not in ValidTransitions; callers MUST treat this as
	// a permanent decision (no auto-retry).
	ErrInvalidTransition = errors.New("runtimemgmt: invalid task state transition")

	// ErrUnknownState is returned when an unrecognized state value is
	// encountered (e.g. an unmapped legacy state read from storage).
	ErrUnknownState = errors.New("runtimemgmt: unknown task state")

	// ErrIdempotencyRequired is returned by Claim / Start when the
	// runtime marks the task as mutating but no idempotency contract
	// fingerprint has been persisted. See spec requirement
	// "Mutating tasks declare idempotency before automatic retry".
	ErrIdempotencyRequired = errors.New("runtimemgmt: mutating task requires idempotency contract")

	// ErrLeaseExpired is returned when a lease deadline has elapsed
	// before the next renew / checkpoint / result call.
	ErrLeaseExpired = errors.New("runtimemgmt: lease expired")

	// ErrNoCapacity is returned by AdmissionDecision when the server
	// concurrency cap or runtime-reported free-slot cap is exhausted.
	// Maps to scenario "No capacity is available" in spec.md.
	ErrNoCapacity = errors.New("runtimemgmt: no capacity for slot-before-claim admission")

	// ErrClaimLost is returned when the task's claim generation has
	// advanced past the caller's view (a new claim took over after a
	// stale-lease recovery).
	ErrClaimLost = errors.New("runtimemgmt: claim generation has advanced")

	// ErrTerminalState is returned when a non-terminal mutator
	// (checkpoint, progress, lease renewal) targets a row that already
	// reached a terminal state.
	ErrTerminalState = errors.New("runtimemgmt: task already in a terminal state")

	// ErrResultNotReady is returned when a caller asks for the
	// terminal result before the task reached a terminal state.
	ErrResultNotReady = errors.New("runtimemgmt: task result not ready")

	// ErrSanitizerMissing is returned when terminal result is being
	// persisted without a sanitizer status decision (either sanitized
	// or explicitly marked sanitized_skipped with rationale).
	ErrSanitizerMissing = errors.New("runtimemgmt: terminal result requires sanitizer status decision")

	// ErrFailureClassInvalid is returned when a failure class value is
	// not part of the FailureClass* taxonomy.
	ErrFailureClassInvalid = errors.New("runtimemgmt: failure class is not in the taxonomy")
)
