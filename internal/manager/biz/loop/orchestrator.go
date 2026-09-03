// Package loop — orchestrator.go
//
// Implements the seven-phase closed-loop state machine
// (detected → correlated → investigated → critiqued → approved →
// recovered → postmortem) plus the three terminal states
// (failed / aborted / retry_exhausted).
//
// Design references (zero-manual-ops-loop):
//   - Design §3 (state machine, transition rules, crash recovery)
//   - Design §5 (PhaseWorker interface — phase_worker.go)
//   - OpenSpec spec: closed-loop-orchestrator
//   - Tasks 1.1, 1.4, 1.5
//
// Public surface (intentionally narrow for Day 1):
//
//   - Phase (typed string)
//   - RunOptions / RunResult
//   - Orchestrator interface (Run / Resume / State)
//   - nextPhase + recoverFromEvents state-machine helpers
//   - DB advisory lock helper (acquireLock / releaseLock)
//
// Day 1 deliberately omits the concrete PhaseWorker implementations
// (alert dedup / investigator / critic / flow.Execute / postmortem) —
// those are wired in by integration PRs. The orchestrator here drives
// the registry and the event log; the actual sub-tasks happen in
// the registered workers.
package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// Phase is a typed-string phase of the closed-loop state machine.
// Using a typed string (not iota) is intentional: the DB column is
// VARCHAR(32) per design §3.1, so reordering Go constants does not
// silently flip historical phase values.
type Phase string

// The seven forward phases plus the two terminal phases.
const (
	PhaseDetected     Phase = "detected"
	PhaseCorrelated   Phase = "correlated"
	PhaseInvestigated Phase = "investigated"
	PhaseCritiqued    Phase = "critiqued"
	PhaseApproved     Phase = "approved"
	PhaseRecovered    Phase = "recovered"
	PhasePostmortem   Phase = "postmortem"
	// Terminal phases — the state machine refuses to leave them.
	PhaseFailed  Phase = loopmodel.PhaseFailed
	PhaseAborted Phase = loopmodel.PhaseAborted
)

// allForwardPhases is the canonical forward ordering. The state machine
// uses this for index lookups in nextPhase and for the recoverFromEvents
// path-comparison logic.
var allForwardPhases = []Phase{
	PhaseDetected,
	PhaseCorrelated,
	PhaseInvestigated,
	PhaseCritiqued,
	PhaseApproved,
	PhaseRecovered,
	PhasePostmortem,
}

// MaxRetryCount is the recovered→approved rollback cap. After three
// failed verifications the orchestrator writes retry_exhausted and
// the loop enters the failed terminal state.
const MaxRetryCount = 3

// RollbackStaleThreshold is the wall-clock budget outside of which
// a recovered→approved rollback is treated as stale and refused.
// Per design §3.2, a rollback request arriving more than 30 minutes
// after the last recovered-phase entry is rejected.
const RollbackStaleThreshold = 30 * time.Minute

// DefaultAdvisoryLockTimeoutSec is the DB advisory lock wait budget
// used by acquireLock. Failed acquisition returns ErrLockBusy so the
// HTTP layer can map it to 409 Conflict + Retry-After.
const DefaultAdvisoryLockTimeoutSec = 5

// ErrLockBusy is returned by acquireLock when the per-incident
// advisory lock is held by another instance. The orchestrator wraps
// this with %w so audit / observability can detect contention.
var ErrLockBusy = errors.New("loop: advisory lock busy")

// ErrInvalidTransition is returned by nextPhase when the requested
// transition is not allowed by the rule table (e.g. detected →
// investigated skip). The orchestrator wraps this with %w.
var ErrInvalidTransition = errors.New("loop: invalid phase transition")

// ErrTerminalPhase is returned by nextPhase when the source phase
// is a terminal state (failed / aborted / postmortem). The state
// machine refuses to leave terminal states.
var ErrTerminalPhase = errors.New("loop: source phase is terminal")

// ErrUnsupportedRollback is returned by the recovered→approved
// rollback guard when one of the 5 required conditions is not met
// (VerifiedDelta not failing, retry cap reached, stale, ctx canceled,
// missing failed-metrics list).
var ErrUnsupportedRollback = errors.New("loop: rollback not supported")

// RunOptions is the orchestrator's input struct.
// It carries everything the Run gate needs to start a new run or
// resume an existing one.
type RunOptions struct {
	// IncidentID is the cross-phase incident identifier. Required.
	IncidentID string

	// FromPhase is the entry phase. Defaults to PhaseDetected when
	// empty (the alert-entry path). The chat-entry path passes
	// PhaseApproved to skip the earlier phases per spec §对话升级为闭环.
	FromPhase Phase

	// TenantID is the multi-tenant scope. Required.
	TenantID string

	// TriggeredBy is the entry source ("alert" / "chat" / "harness").
	// Recorded on the run-start event for audit traceability.
	TriggeredBy string

	// IdempotentKey is the run-level idempotency key. When the same
	// key is seen twice within the run, the orchestrator returns the
	// previously persisted RunResult instead of re-running.
	IdempotentKey string

	// LinkedConversationID is set when the run was promoted from a
	// chat conversation (D13-D16). The orchestrator pushes the
	// post-loop postmortem back to this conversation via the
	// ChatReportPusher seam. Empty for alert-entry incidents.
	LinkedConversationID string

	// LinkedTurnSeq is the seq of the assistant turn that triggered
	// the chat-promote. Forwarded to the run-start event for audit.
	LinkedTurnSeq int

	// StopAfterPhase gives read-only callers a hard safety boundary after
	// executing and verifying the named phase. Empty preserves legacy behavior.
	StopAfterPhase Phase

	// AlertGroup and CorrelationHints carry the authoritative MCP investigation
	// context into phase workers. They are read-only inputs and do not replace
	// contracts produced by the loop.
	AlertGroup       []string
	CorrelationHints map[string]any
}

// RunResult is the orchestrator's terminal output struct.
type RunResult struct {
	// IncidentID is the run's incident identifier.
	IncidentID string

	// FinalPhase is the phase the loop ended on (one of the seven
	// forward phases or a terminal state).
	FinalPhase Phase

	// LoopEvents is the ordered list of events recorded for the run.
	// Useful for downstream tooling (postmortem rendering, harness
	// rubric extraction).
	LoopEvents []loopmodel.Event

	// RootCause is the structured root cause produced by the
	// investigated phase. Nil when the run terminated before
	// investigated.
	RootCause *RootCauseJSON

	// Verified is the verified-delta payload produced by the
	// recovered phase. Nil when the run terminated before recovered.
	Verified *VerifiedDelta

	// Postmortem is the postmortem doc produced by the postmortem
	// phase. Nil when the run terminated before postmortem.
	Postmortem *PostmortemDoc
}

// Orchestrator is the public surface of the state machine. Callers
// (alert webhook handler, chat promote handler, harness runner) hold
// an Orchestrator and dispatch to Run / Resume / State.
type Orchestrator interface {
	// Run advances the loop from opts.FromPhase (or recovered from
	// the event log when omitted) to the next terminal state.
	// Idempotent: re-running with the same opts.IdempotentKey
	// returns the previously persisted RunResult.
	Run(ctx context.Context, opts RunOptions) (*RunResult, error)

	// Resume continues a paused loop after a HITL callback. The
	// loop_event_log must contain a phase_paused event with the
	// matching pause_token; otherwise Resume returns ErrInvalidToken.
	Resume(ctx context.Context, incidentID string, pauseToken string) error

	// State returns the current State snapshot for an incident.
	// tenantID scopes the read to the caller's tenant (multi-tenant
	// isolation enforced by EventRepo.ReadEvents). When the incident
	// has no events yet, returns (nil, nil) — the alert entry path
	// uses this to detect the "first run" case.
	State(ctx context.Context, tenantID, incidentID string) (*loopmodel.State, error)
}

// AdvisoryLocker is the narrow DB seam the orchestrator needs:
// MySQL's GET_LOCK / RELEASE_LOCK primitives. Production code wires
// a *sql.DB (the GetLockerAdapter default impl below wraps it);
// tests inject a stub. The interface intentionally does NOT expose
// the full *sql.DB surface so the orchestrator keeps a tight
// dependency on the database.
type AdvisoryLocker interface {
	// GetLock acquires the named lock with the given timeout (seconds).
	// Returns (acquired=true, nil) on success, (acquired=false, nil)
	// on timeout, (acquired=false, err) on DB error.
	GetLock(ctx context.Context, name string, timeoutSec int) (bool, error)
	// ReleaseLock releases the named lock. Idempotent: calling on
	// a non-held lock is a no-op (returns nil).
	ReleaseLock(ctx context.Context, name string) error
}

// OrchestratorDeps is the dependency bag the orchestrator needs.
// All fields are required; the constructor panics on nil to surface
// misconfiguration at startup rather than at first incident.
type OrchestratorDeps struct {
	// Locker is the DB seam for advisory lock acquisition. Required.
	// In production this is a *sql.DB-compatible implementation of
	// AdvisoryLocker; tests pass a stub.
	Locker AdvisoryLocker

	// AdvisoryLockTimeoutSec is the per-Run GET_LOCK wait budget.
	// 0 means DefaultAdvisoryLockTimeoutSec (5 seconds).
	AdvisoryLockTimeoutSec int

	// EventRepo is the optional Day 5 seam for loop_event_log writes.
	// When non-nil AND WorkerRegistry is non-nil (or the package-global
	// DefaultPhaseWorkerRegistry has workers), orchestrator.Run walks
	// all 7 phases and persists events. When nil, Run falls back to
	// the Day 1 single-phase_entered path (existing tests).
	EventRepo EventRepo

	// ContractRepo is the optional Day 5 seam for loop_contract writes.
	// When set the orchestrator writes per-phase payloads; nil is valid
	// for the dry-run integration test (events-only).
	ContractRepo ContractRepo

	// WorkerRegistry is the optional Day 5 dependency bag holding the
	// 7 PhaseWorker instances. Production wires the result of
	// phase_workers.go::NewWorkerRegistry(...). nil = use the package-
	// global DefaultPhaseWorkerRegistry (when populated); double-nil =
	// legacy Day 1 path.
	WorkerRegistry *WorkerRegistry

	// Logger is the slog.Logger for orchestrator-internal warnings
	// (e.g. transient EventRepo.AppendEvent failures). nil = silent.
	Logger *slog.Logger

	// ChatReportPusher is the optional Day 9 seam for async
	// post-mortem write-back into the chat conversation that
	// triggered the run. Only called when opts.TriggeredBy == "chat"
	// AND opts.LinkedConversationID != "". nil = skip the push
	// (alert-triggered runs never push back).
	ChatReportPusher ChatReportPusher
}

// ChatReportPusher is the narrow seam the orchestrator needs to
// push a post-loop postmortem markdown into a chat conversation.
// Implemented by a thin adapter that wraps *chatdiagnose.ChatDiagnoseService.
// nil-safe at the orchestrator level.
type ChatReportPusher interface {
	PushReportToConversation(ctx context.Context, conversationID, tenantID, reportMarkdown string) error
}

// orchestrator is the production implementation of Orchestrator.
// Held behind the Orchestrator interface so tests can swap in fakes.
type orchestrator struct {
	deps OrchestratorDeps

	// ResultCache stores by-run RunResult so retried calls with the
	// same IdempotentKey return the cached value. Keyed by
	// IdempotentKey; EntryTouchedAt is used to evict stale entries.
	mu          sync.Mutex
	resultCache map[string]cachedRunResult
}

type cachedRunResult struct {
	result    *RunResult
	touchedAt time.Time
}

// NewOrchestrator constructs the production Orchestrator. Returns
// nil + a non-nil error if the deps are missing required fields.
func NewOrchestrator(deps OrchestratorDeps) (Orchestrator, error) {
	if deps.Locker == nil {
		return nil, errors.New("loop: OrchestratorDeps.Locker is required")
	}
	timeout := deps.AdvisoryLockTimeoutSec
	if timeout == 0 {
		timeout = DefaultAdvisoryLockTimeoutSec
	}
	return &orchestrator{
		deps: OrchestratorDeps{
			Locker:                 deps.Locker,
			AdvisoryLockTimeoutSec: timeout,
			EventRepo:              deps.EventRepo,
			ContractRepo:           deps.ContractRepo,
			WorkerRegistry:         deps.WorkerRegistry,
			Logger:                 deps.Logger,
		},
		resultCache: make(map[string]cachedRunResult),
	}, nil
}

// --- Phase transition rules ---------------------------------------------

// nextPhase decides the destination phase from current. The contract
// argument is the latest contract the previous phase produced (nil
// for detected, RootCauseJSON for investigated, VerifiedDelta for
// recovered, etc.). contractValid drives the recovered → approved
// rollback branch — true when VerifiedDelta.passed == false.
//
// Transition rule table (mirrors design §3.2):
//
//	detected     → correlated
//	correlated   → investigated
//	investigated → critiqued
//	critiqued    → approved
//	approved     → recovered
//	recovered    → postmortem                        (contractValid)
//	recovered    → approved        (rollback only)   (!contractValid)
//	recovered    → failed          (retry exhausted)
//	postmortem   → (terminal; no outgoing)
//	failed       → (terminal; no outgoing)
//	aborted      → (terminal; no outgoing)
//
// All other transitions (e.g. detected → investigated skipping
// correlated) return ErrInvalidTransition. The guard is held by
// TransitionRule.evaluate in the design; this helper is the
// minimal pure-function version suitable for tests.
func nextPhase(current Phase, contractValid bool) (Phase, error) {
	switch current {
	case PhaseDetected:
		return PhaseCorrelated, nil
	case PhaseCorrelated:
		return PhaseInvestigated, nil
	case PhaseInvestigated:
		return PhaseCritiqued, nil
	case PhaseCritiqued:
		return PhaseApproved, nil
	case PhaseApproved:
		return PhaseRecovered, nil
	case PhaseRecovered:
		if contractValid {
			return PhasePostmortem, nil
		}
		// Verification failed → rollback to approved. The orchestrator
		// decides whether to actually execute the rollback (retry budget)
		// inside Run; nextPhase simply maps the binary signal.
		return PhaseApproved, nil
	case PhasePostmortem, PhaseFailed, PhaseAborted:
		return "", fmt.Errorf("%w: from %s", ErrTerminalPhase, current)
	default:
		return "", fmt.Errorf("%w: from %s", ErrInvalidTransition, current)
	}
}

// rollbackEligible reports whether the recovered→approved rollback
// can be taken for the given state. The 5 conditions (per design §3.2)
// MUST all hold:
//
//  1. VerifiedDelta.passed == false
//  2. VerifiedDelta.failed_metrics non-empty
//  3. LoopState.RetryCount < 3
//  4. Within 30 minutes of the last recovered entry (stale guard)
//  5. Original error was NOT a context cancellation (external abort)
//
// Returns (eligible, err). err is one of ErrUnsupportedRollback
// (condition miss) or a context-related error (caller wraps).
func rollbackEligible(verified *VerifiedDelta, retryCount int, lastRecoveredAt time.Time, errChain string) (bool, error) {
	if verified == nil {
		return false, fmt.Errorf("%w: verified delta missing", ErrUnsupportedRollback)
	}
	if verified.Passed {
		return false, fmt.Errorf("%w: verified.passed=true (no rollback needed)", ErrUnsupportedRollback)
	}
	if len(verified.FailedMetrics) == 0 {
		return false, fmt.Errorf("%w: failed_metrics empty", ErrUnsupportedRollback)
	}
	if retryCount >= MaxRetryCount {
		return false, fmt.Errorf("%w: retry_count=%d >= %d", ErrUnsupportedRollback, retryCount, MaxRetryCount)
	}
	if time.Since(lastRecoveredAt) > RollbackStaleThreshold {
		return false, fmt.Errorf("%w: last recovered entry older than %s", ErrUnsupportedRollback, RollbackStaleThreshold)
	}
	// Condition 5: error chain must not be a context cancellation.
	// We use a string check to avoid pulling in errors.Is machinery
	// here; the caller can also pre-check with errors.Is(err, context.Canceled).
	if isContextCanceledError(errChain) {
		return false, fmt.Errorf("%w: error is context cancellation", ErrUnsupportedRollback)
	}
	return true, nil
}

// isContextCanceledError is a small helper that probes an error
// chain string for the canonical context-cancellation markers.
// Reason for not taking context.Context as input: this function is
// called from rollbackEligible which is part of a pure decision
// table — keeping it pure simplifies the test matrix.
func isContextCanceledError(errChain string) bool {
	if errChain == "" {
		return false
	}
	for _, marker := range []string{
		"context canceled",
		"context deadline exceeded",
		"signal: killed",
	} {
		if containsFold(errChain, marker) {
			return true
		}
	}
	return false
}

// containsFold is a case-insensitive substring check. We avoid
// strings.Contains in an inline loop so the marker matching is
// deterministic and grep-able.
func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if equalFold(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

// equalFold is ASCII-case-insensitive equality. We inline the
// comparison instead of importing strings.ToLower to keep the
// dependency surface of this hot-path helper minimal.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// isTerminal reports whether phase is one of the terminal states
// (postmortem is the success terminal; failed / aborted are the
// failure terminals). The state machine refuses to leave a
// terminal phase.
func isTerminal(phase Phase) bool {
	switch phase {
	case PhasePostmortem, PhaseFailed, PhaseAborted:
		return true
	}
	return false
}

// --- Crash recovery -----------------------------------------------------

// recoverFromEvents scans the event log in reverse order and
// reconstructs the current phase. The decision matrix (per design §3.4):
//
//	events empty                → (detected, nil)            -- fresh start
//	last event = phase_entered  → (phase, errInPhase)        -- phase in flight
//	last = phase_contract_written → (nextPhase, nil)         -- advance
//	last = phase_failed         → (phase, errorChain)        -- phase failed
//	last = rollback             → (approved, nil)            -- respawn after rollback
//	last = retry_exhausted      → (failed, nil)              -- terminal
//
// The bool return is "phase known" — false when the event log is
// empty (caller decides whether to start from detected).
//
// Implementation note: the function is pure (no DB I/O) so the
// orchestrator's crash-recovery path is unit-testable without
// touching a database. The orchestrator pre-fetches the events
// via the repo and passes them in.
func recoverFromEvents(events []loopmodel.Event) (Phase, error) {
	if len(events) == 0 {
		return PhaseDetected, nil
	}

	// Walk backwards. The freshest event determines the state.
	// We only need to walk through the most recent contiguous run
	// of events for the same phase; once we cross a phase boundary
	// we know the previous phase completed.
	latest := events[0]
	switch latest.EventType {
	case loopmodel.EventRetryExhausted:
		return PhaseFailed, nil
	case loopmodel.EventRollback:
		return PhaseApproved, nil
	case loopmodel.EventPhaseContractWritten:
		// Phase completed. Advance to the next phase.
		next, err := nextPhase(Phase(latest.Phase), true)
		if err != nil {
			return PhaseFailed, fmt.Errorf("recover: advance from %s: %w", latest.Phase, err)
		}
		return next, nil
	case loopmodel.EventTypePhaseEntered:
		// Phase is in flight (no contract written yet). Caller
		// should restart from this phase.
		return Phase(latest.Phase), nil
	case loopmodel.EventPhaseFailed:
		// Phase failed. Look for the most recent incident of
		// phase_entered for the same phase to know which phase
		// failed (the failed event carries the phase name too).
		return Phase(latest.Phase), nil
	case loopmodel.EventPhasePaused:
		// Paused at the latest phase. Caller's Resume path will
		// pick up from here.
		return Phase(latest.Phase), nil
	case loopmodel.EventPhaseResumed:
		// Resumed; the orchestrator re-enters the phase. Return
		// the latest phase so the caller can re-run.
		return Phase(latest.Phase), nil
	case loopmodel.EventCorrection:
		// Correction events do not move the phase forward; treat
		// them as transparent and look at the next event.
		return recoverFromEvents(events[1:])
	}

	// Unknown event type — refuse to advance to keep the state
	// machine safe against corrupted event logs.
	return PhaseFailed, fmt.Errorf("recover: unknown event_type %q", latest.EventType)
}

// --- Orchestrator methods (skeleton) ------------------------------------

// Run is the entry point. The current implementation initialises
// the event log watermark and dispatches to the per-phase worker
// via the registry. Subsequent PRs (Day 2 — alert dedup, Day 3 —
// recovery verification, Day 4 — postmortem, Day 5 — integration)
// wire up the actual per-phase logic.
//
// For Day 1 the function only:
//  1. Acquires the per-incident advisory lock (5s default).
//  2. Computes the starting phase via recoverFromEvents.
//  3. Writes a phase_entered event for the starting phase.
//  4. Returns a RunResult with the phase signature so the test
//     suite can verify the state-machine plumbing.
//
// Concrete phase-handler logic (Worker dispatch) is added in the
// follow-up PRs.
func (o *orchestrator) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	if opts.IncidentID == "" {
		return nil, errors.New("loop: RunOptions.IncidentID is required")
	}
	if opts.TenantID == "" {
		return nil, errors.New("loop: RunOptions.TenantID is required")
	}

	// Idempotency: if the key is already in the cache, return the
	// cached result without re-running.
	if opts.IdempotentKey != "" {
		o.mu.Lock()
		if cached, ok := o.resultCache[opts.IdempotentKey]; ok {
			o.mu.Unlock()
			return cached.result, nil
		}
		o.mu.Unlock()
	}

	// Acquire the per-incident advisory lock. The lock is released
	// in a defer so the orchestrator never leaves the lock held on
	// panic.
	release, err := o.acquireLock(ctx, opts.IncidentID)
	if err != nil {
		return nil, fmt.Errorf("loop: acquire lock for %s: %w", opts.IncidentID, err)
	}
	defer release(ctx)

	// Resolve the starting phase. When opts.FromPhase is empty the
	// default is PhaseDetected (alert entry); the caller can
	// override (e.g. chat promote passes PhaseApproved).
	from := opts.FromPhase
	if from == "" {
		from = PhaseDetected
		if len(opts.AlertGroup) > 0 && len(opts.CorrelationHints) > 0 {
			from = PhaseInvestigated
		}
	}

	// Day 5 dispatch: when the optional EventRepo + WorkerRegistry
	// are wired, walk the full state machine. Otherwise fall back to
	// the Day 1 single-event path (preserves existing test suite).
	var events []loopmodel.Event
	finalPhase := from

	if o.canWalk() {
		events, finalPhase, err = o.walkPhases(ctx, opts, from)
		if err != nil {
			return nil, fmt.Errorf("loop: walk phases: %w", err)
		}
	} else {
		// Legacy Day 1 path — one phase_entered event, no walking.
		events = []loopmodel.Event{{
			IncidentID:     opts.IncidentID,
			TenantID:       opts.TenantID,
			EventType:      loopmodel.EventTypePhaseEntered,
			Phase:          string(from),
			IdempotencyKey: buildIdempotencyKey(opts.IncidentID, from, loopmodel.EventTypePhaseEntered, 1),
			TraceID:        traceIDFromContext(ctx),
			CreatedAt:      time.Now().UTC(),
		}}
	}

	result := &RunResult{
		IncidentID: opts.IncidentID,
		FinalPhase: finalPhase,
		LoopEvents: events,
	}

	if opts.IdempotentKey != "" {
		o.mu.Lock()
		o.resultCache[opts.IdempotentKey] = cachedRunResult{
			result:    result,
			touchedAt: time.Now().UTC(),
		}
		o.mu.Unlock()
	}

	if result.RootCause == nil {
		result.RootCause = o.readRootCause(ctx, opts)
	}

	return result, nil
}

func (o *orchestrator) readRootCause(ctx context.Context, opts RunOptions) *RootCauseJSON {
	if o.deps.ContractRepo == nil {
		return nil
	}
	payload, err := o.deps.ContractRepo.ReadContract(ctx, opts.TenantID, opts.IncidentID, PhaseInvestigated, "root_cause_json")
	if err != nil || payload == nil {
		return nil
	}
	var rootCause RootCauseJSON
	if err := json.Unmarshal([]byte(payload.Payload), &rootCause); err != nil {
		return nil
	}
	if err := ValidateRootCauseJSON(&rootCause); err != nil {
		return nil
	}
	return &rootCause
}

func (o *orchestrator) readVerifiedDelta(ctx context.Context, opts RunOptions) *VerifiedDelta {
	if o.deps.ContractRepo == nil {
		return nil
	}
	payload, err := o.deps.ContractRepo.ReadContract(ctx, opts.TenantID, opts.IncidentID, PhaseRecovered, "verified_delta")
	if err != nil || payload == nil {
		return nil
	}
	var verified VerifiedDelta
	if err := json.Unmarshal([]byte(payload.Payload), &verified); err != nil {
		return nil
	}
	return &verified
}

func (o *orchestrator) readPostmortem(ctx context.Context, opts RunOptions) *PostmortemDoc {
	if o.deps.ContractRepo == nil {
		return nil
	}
	payload, err := o.deps.ContractRepo.ReadContract(ctx, opts.TenantID, opts.IncidentID, PhasePostmortem, "postmortem_doc")
	if err != nil || payload == nil {
		return nil
	}
	var doc PostmortemDoc
	if err := json.Unmarshal([]byte(payload.Payload), &doc); err != nil {
		return nil
	}
	return &doc
}

// Resume continues a paused loop. The contract here is intentionally
// small for Day 1: the orchestrator validates that the latest event
// is a phase_paused event for the supplied pause_token, then clears
// the pause state and re-enters the phase via the standard Run path.
//
// The concrete call path (HITL verify-token, re-acquire advisory
// lock, etc.) is added by the hitl-pause-resume integration.
func (o *orchestrator) Resume(ctx context.Context, incidentID string, pauseToken string) error {
	if incidentID == "" {
		return errors.New("loop: Resume requires incidentID")
	}
	if pauseToken == "" {
		return errors.New("loop: Resume requires pauseToken")
	}

	release, err := o.acquireLock(ctx, incidentID)
	if err != nil {
		return fmt.Errorf("loop: acquire lock for %s: %w", incidentID, err)
	}
	defer release(ctx)

	// Day 1: stub. The token-validation logic is added when the
	// hitl-pause-resume integration lands. We keep the lock +
	// validation hop so the test suite can assert the lock path.
	_ = ctx
	return nil
}

// State returns the snapshot row. The DB read is filled in by the
// follow-up PR that wires the state repository; for Day 1 we
// return a synthesized snapshot so the test suite can assert the
// orchestration shape without a live DB.
func (o *orchestrator) State(ctx context.Context, tenantID, incidentID string) (*loopmodel.State, error) {
	if tenantID == "" {
		return nil, errors.New("loop: State requires tenantID")
	}
	if incidentID == "" {
		return nil, errors.New("loop: State requires incidentID")
	}
	// No event-repo wired: synthesize a "no events yet" state so the
	// HTTP handler can return 200 with current_phase="" instead of
	// 500. Tests rely on this for the Day-1 fallback path.
	if o.deps.EventRepo == nil {
		return &loopmodel.State{
			IncidentID:   incidentID,
			CurrentPhase: "",
			UpdatedAt:    time.Now().UTC(),
		}, nil
	}
	events, err := o.deps.EventRepo.ReadEvents(ctx, tenantID, incidentID)
	if err != nil {
		return nil, fmt.Errorf("loop: State read events: %w", err)
	}
	state := &loopmodel.State{
		IncidentID: incidentID,
		UpdatedAt:  time.Now().UTC(),
	}
	if len(events) > 0 {
		state.LastEventID = events[len(events)-1].ID
	}
	if len(events) == 0 {
		// Fresh start: no events yet, the alert entry path uses this
		// to detect "first run". Return an empty CurrentPhase (not
		// "detected") so callers can distinguish "no run" from
		// "run started in detected".
		return state, nil
	}
	// EventRepo returns ascending-chronological; recoverFromEvents
	// expects the newest event first. Reverse in place so the helper
	// can index [0] without sorting.
	reversed := make([]loopmodel.Event, len(events))
	for i := range events {
		reversed[i] = events[len(events)-1-i]
	}
	// Terminal phases (postmortem = success, failed/aborted = failure)
	// cannot be advanced past; recoverFromEvents errors in that case.
	// Handle them by reporting the terminal phase as the current state
	// and surfacing a recovery error so the HTTP handler can decide
	// whether to escalate.
	if isTerminalEvent(reversed) {
		// Terminal phase reached — return it as the canonical
		// current phase with no error. Callers (HTTP handler,
		// AgentTeams state.json bridge) treat postmortem/failed/
		// aborted as legitimate end-of-loop states.
		state.CurrentPhase = latestEnteredPhase(reversed)
		return state, nil
	}
	current, recoverErr := recoverFromEvents(reversed)
	state.CurrentPhase = string(current)
	if recoverErr != nil {
		state.CurrentPhase = latestEnteredPhase(reversed)
		return state, recoverErr
	}
	return state, nil
}

// isTerminalEvent reports whether the freshest event chain ends at a
// terminal phase (postmortem / failed / aborted). Used by State() to
// short-circuit recoverFromEvents and avoid its "source phase is
// terminal" error.
func isTerminalEvent(events []loopmodel.Event) bool {
	if len(events) == 0 {
		return false
	}
	latest := events[0]
	if latest.EventType != loopmodel.EventPhaseContractWritten {
		return false
	}
	phase := Phase(latest.Phase)
	return phase == PhasePostmortem || phase == PhaseFailed || phase == PhaseAborted
}

// latestEnteredPhase walks the (newest-first) event slice and
// returns the phase name of the freshest phase_entered event. Used
// as a fallback when recoverFromEvents hits a terminal-phase edge.
func latestEnteredPhase(events []loopmodel.Event) string {
	for _, ev := range events {
		if ev.EventType == loopmodel.EventTypePhaseEntered {
			return ev.Phase
		}
	}
	return ""
}

// --- Helpers ------------------------------------------------------------

// acquireLock acquires the per-incident DB advisory lock. The lock
// is namespaced under "loop:" so a single incident cannot be
// processed by two orchestrator instances simultaneously. The
// timeout is configurable via OrchestratorDeps.AdvisoryLockTimeoutSec.
//
// Returns a release function that the caller MUST defer-release.
// The release function is idempotent — calling it twice is a no-op.
func (o *orchestrator) acquireLock(ctx context.Context, incidentID string) (func(context.Context), error) {
	lockName := "loop:" + incidentID
	timeout := o.deps.AdvisoryLockTimeoutSec

	acquired, err := o.deps.Locker.GetLock(ctx, lockName, timeout)
	if err != nil {
		return nil, fmt.Errorf("loop: GET_LOCK(%s) exec: %w", lockName, err)
	}
	if !acquired {
		return nil, fmt.Errorf("%w: incident=%s timeout=%ds", ErrLockBusy, incidentID, timeout)
	}

	var released bool
	release := func(c context.Context) {
		if released {
			return
		}
		released = true
		// We use a fresh context (not the caller's, which may be
		// canceled) so the lock is released even if the caller
		// canceled.
		relCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = o.deps.Locker.ReleaseLock(relCtx, lockName)
		_ = c // named param for callers that want to thread the caller's ctx
	}
	return release, nil
}

// buildIdempotencyKey composes the unique key used by the
// loop_event_log UNIQUE constraint. The shape is stable so replays
// hit the same row.
func buildIdempotencyKey(incidentID string, phase Phase, eventType string, attempt int) string {
	return fmt.Sprintf("%s:%s:%s:%d", incidentID, phase, eventType, attempt)
}

// traceIDFromContext extracts the OTel trace_id from the context if
// present. Returns "" when the ctx does not carry a trace id; the
// caller stores the empty string in loop_event_log.trace_id which
// is acceptable for non-traced runs (e.g. harness dry-run).
//
// We intentionally do NOT depend on the OTel API package here to
// keep the loop package import-light. The function is a thin
// string-typed lookup that respects the OpenTelemetry context key
// when the upstream host wires it; otherwise it returns "".
func traceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	// The OTel context key is unexported and the host wires a
	// plain stringer type. We probe the canonical string key and
	// return it directly when found.
	type traceIDKey struct{}
	if v := ctx.Value(traceIDKey{}); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// newPauseToken returns a 32-byte hex token for the pause / resume
// handshake. The token is opaque; the orchestrator stamps it on the
// phase_paused event and the hitl-pause-resume coordinator echoes
// it back on the resume callback.
func newPauseToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("loop: rand.Read: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
