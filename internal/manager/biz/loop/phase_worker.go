// Package loop — phase_worker.go
//
// PhaseWorker is the orchestrator's stage-internal sub-orchestration
// abstraction. It follows v1 multi-agent-runtime's three-segment
// pipeline (Planner → Executor → Verifier) at the *philosophy* level
// only — the implementation in this file is intentionally minimal
// (default no-op) and the concrete per-phase workers (alert dedup,
// investigator, critic, flow.Execute, etc.) are wired in via
// RegisterPhaseWorker from the integration PRs.
//
// Key invariants (mirrored from the upstream design §5):
//
//  1. One Worker per Phase. The orchestrator holds a registry
//     (DefaultPhaseWorkerRegistry) keyed by Phase; lookup is O(1).
//  2. Idempotent. Workers MUST tolerate repeated Planner/Executor calls
//     with the same PlanInput (same incident_id + attempt + upstream
//     contract). The orchestrator's crash recovery leans on this.
//  3. Verifier deadline. Each Worker returns its own
//     VerifierTimeoutMs() so the cold-path / hot-path distinction can
//     express different deadlines (default 30s; recovered phase
//     overrides to 60s for cold LLM warm-ups).
//  4. No panic on missing Worker. If a phase is not registered the
//     orchestrator returns ErrWorkerNotRegistered; the test suite
//     covers this so the regression is loud.
package loop

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultVerifierTimeoutMs is the orchestrator-wide Verifier deadline
// floor. Per-phase Workers can extend via VerifierTimeoutMs().
const DefaultVerifierTimeoutMs = 30000

// RecoveredPhaseVerifierTimeoutMs is the recommended override for the
// recovered phase — verify_recovery pulls cold metrics with a 60s
// budget. Captured as a constant so tests assert against the value.
const RecoveredPhaseVerifierTimeoutMs = 60000

// PhaseWorker is the orchestrator's stage-internal sub-orchestration
// interface. Every phase of the seven-phase state machine maps to one
// registered Worker instance.
//
// Lifecycle (orchestrator-driven, single-thread per incident):
//
//	PlanInput arrvies from the orchestrator
//	  └─► Worker.Planner(ctx, in) => Plan
//	         └─► Worker.Executor(ctx, plan) => ExecResult
//	                └─► Worker.Verifier(ctx, result) => Verdict
//
// Worker.VerifierTimeoutMs() governs the Verifier wall-clock budget.
// The orchestrator wraps the Verifier call in a context.WithTimeout
// derived from the configured deadline; on timeout it reads the
// severity tier and dispatches one of the §5.4 fallback paths.
type PhaseWorker interface {
	// Phase returns the orchestrator phase this Worker handles.
	// Used by the registry to enforce 1-worker-per-phase on insert.
	Phase() Phase

	// Planner produces the execution plan for the phase. The plan
	// describes the sub-tasks the Worker intends to run (LLM calls,
	// tool calls, DAG node triggers, DB queries).
	//
	// Returning an error here causes the orchestrator to write
	// phase_failed (event_type="phase_failed", status="failed") and
	// exit the run; the planner is the safest place to fail fast
	// before any side effects.
	Planner(ctx context.Context, in PlanInput) (Plan, error)

	// Executor runs the plan. Implementation may be synchronous
	// (single round-trip) or asynchronous (goroutine-fed channel);
	// the orchestrator only waits for the returned ExecResult.
	//
	// SideEffects captured here are recorded in loop_event_log as
	// event_type="phase_contract_written" payloads; this is the
	// audit trail the postmortem phase reads.
	Executor(ctx context.Context, plan Plan) (ExecResult, error)

	// Verifier verifies the Executor result. Default deadline is
	// VerifierTimeoutMs() (in milliseconds). On timeout the
	// orchestrator falls back to the severity-tier policy (see
	// orchestrator.go::runVerifier).
	Verifier(ctx context.Context, result ExecResult) (Verdict, error)

	// VerifierTimeoutMs returns the Verifier deadline in
	// milliseconds. 0 means "use default 30000ms".
	VerifierTimeoutMs() int
}

// PlanInput is the orchestrator-composed input handed to a Worker.
// It carries the upstream contract reference (so the Worker can read
// the previous phase's output) plus the per-run identifiers needed
// for idempotency / tracing / multi-tenancy.
type PlanInput struct {
	// IncidentID identifies the closed-loop run. Required.
	IncidentID string

	// TenantID is the multi-tenant scope. Required for all read
	// queries the Worker makes.
	TenantID string

	// Phase is the phase being entered. Captured here so the Worker
	// does not need to call .Phase() to know which one it is.
	Phase Phase

	// Attempt is the 1-based attempt counter for the phase. Used by
	// the idempotency helper and the retry counter.
	Attempt int

	// UpstreamContract is the contract reference from the previous
	// phase (may be nil for the detected phase which has no upstream).
	// The Worker calls ContractRepository.Read to load the full payload.
	UpstreamContract *ContractRef

	// TraceID is the OTel trace_id propagated from the orchestrator's
	// Run call. Workers MUST thread this into outbound spans.
	TraceID string

	// AlertGroup and CorrelationHints are optional read-only MCP inputs.
	// When present, workers must treat them as more authoritative than
	// loader fallback values.
	AlertGroup       []string
	CorrelationHints map[string]any
}

// Plan is the structured output of Planner. The orchestrator does
// not interpret the plan; it simply forwards it to Executor.
type Plan struct {
	// Steps is the ordered sub-tasks. Planner MAY produce a single
	// step for trivial phases (e.g. detected) and a richly branching
	// list for complex phases (e.g. investigated).
	Steps []PlanStep

	// EstimatedCost is the Planner's self-reported cost estimate
	// (USD for LLM calls, IO call count, etc.). The orchestrator
	// surfaces this in metrics; it does not enforce limits.
	EstimatedCost CostEstimate

	// Meta is a free-form bag for planner-side annotations (selected
	// skill name, model id, hint messages). Not interpreted downstream.
	Meta map[string]any
}

// PlanStep is one sub-task inside a Plan.
type PlanStep struct {
	// Kind is one of "llm_call" / "tool_call" / "flow_dag" / "db_query" /
	// "skill_call". Validated only when the Executor cares; the
	// orchestrator does not enforce.
	Kind string

	// Target is the destination identifier (model id, tool name,
	// flow DAG id, table name, skill id).
	Target string

	// Args is the step-specific argument bag. Free-form map; the
	// Executor type-asserts per Kind.
	Args map[string]any

	// TimeoutMs is the per-step deadline. 0 means "no per-step cap,
	// use the orchestration context deadline".
	TimeoutMs int
}

// CostEstimate is the Planner's reported cost hint.
type CostEstimate struct {
	// USD is the estimated dollar cost (LLM tokens, etc.). 0 means
	// "no estimate available".
	USD float64

	// Tokens is the estimated token count (prompt + completion).
	Tokens int
}

// ExecResult is the structured output of Executor. The orchestrator
// forwards it to Verifier and persists SideEffects + ToolReplay.
type ExecResult struct {
	// ContractRef is the pointer to the loop_contract row the
	// Executor writes (or read for upstream). Optional; nil for
	// side-effect-only phases like detected.
	ContractRef *ContractRef

	// SideEffects is the effect log for the phase. Each entry is
	// persisted onto loop_event_log as event_type="phase_contract_written"
	// or recorded in the plan payload for the postmortem phase.
	SideEffects []SideEffect

	// ToolReplay is the canonical tool call sequence. The postmortem
	// phase and the Harness loop rubric both consume this.
	ToolReplay []ToolReplayEntry

	// RawOutputs is the planner/Executor's free-form output bag.
	// Not persisted; used for in-process handoff between Executor
	// and Verifier when both live in the same package.
	RawOutputs map[string]any
}

// SideEffect is one observable side effect produced by the Executor.
// The orchestrator's contract writer translates this into a loop_event
// payload.
type SideEffect struct {
	// Kind categorizes the side effect ("mutation", "notification",
	// "approval_request", "git_commit", ...).
	Kind string

	// Target is the resource identifier being affected.
	Target string

	// Detail is the side-effect-specific payload (e.g. the SQL
	// statement for a "mutation" kind). Free-form JSON.
	Detail map[string]any
}

// ToolReplayEntry is one replayable tool call. The postmortem phase
// embeds these in the postmortem markdown; the Harness rubric uses
// them to compute time_to_remediate.
type ToolReplayEntry struct {
	// Name is the tool name (e.g. "pg.terminate_long_tx").
	Name string

	// ArgsJSON is the JSON-encoded argument bag.
	ArgsJSON string

	// ResultJSON is the JSON-encoded result.
	ResultJSON string

	// Status is one of "success" / "failed" / "skipped".
	Status string

	// LatencyMs is the wall-clock latency of the call.
	LatencyMs int

	// Timestamp is the call time.
	Timestamp time.Time
}

// Verdict is the Verifier's structured output. The orchestrator
// consumes OK + Confidence to decide whether to advance to the next
// phase or hit the rollback / fail paths.
type Verdict struct {
	// OK is true when the Executor result passes verification.
	OK bool

	// Confidence is the Verifier's confidence in [0, 1]. The
	// orchestrated phase-guard thresholds (e.g. critiqued passes
	// only when Score >= 0.6) read this field.
	Confidence float64

	// Reasons is the issue list. Empty when OK=true; non-empty when
	// OK=false (the postmortem surface mines these reasons).
	Reasons []string

	// TimedOut is true when the verifier exceeded its deadline.
	// The orchestrator uses this flag to pick the severity-tier
	// fallback path (vs. treating the failure as a hard reject).
	TimedOut bool
}

// ContractRef is the cross-phase contract pointer. The Worker reads
// the actual payload via ContractRepository.Read(ContractRef.ID).
type ContractRef struct {
	// ID is the loop_contract.id value.
	ID int64

	// Type is the contract discriminator ("RootCauseJSON" / ...).
	// Mirrors loop_contract.type.
	Type string

	// SchemaVersion is the contract schema version ("v1" / ...).
	// Mirrors loop_contract.schema_version.
	SchemaVersion string
}

// ErrWorkerNotRegistered is returned by the orchestrator when it
// looks up a Worker for a Phase that has not been registered via
// RegisterPhaseWorker. The orchestrator wraps this with %w so the
// audit log captures the missing phase.
var ErrWorkerNotRegistered = errors.New("loop: phase worker not registered")

// ErrPlanInvalid is returned by Executor when the supplied Plan
// is malformed (e.g. referential integrity between Steps). The
// orchestrator wraps this with %w.
var ErrPlanInvalid = errors.New("loop: invalid plan")

// DefaultPhaseWorkerRegistry is the global registry mapping Phase to
// PhaseWorker. Production code calls RegisterPhaseWorker from
// integration init() (or from main.go's dependency wire-up) to
// populate it before the orchestrator runs.
//
// Tests inject a Worker per phase via the same registry; the
// orchestrator's Run function reads from this registry exclusively.
var (
	DefaultPhaseWorkerRegistry = make(map[Phase]PhaseWorker)

	// registryMu guards DefaultPhaseWorkerRegistry for concurrent
	// RegisterPhaseWorker calls during process startup. The map is
	// read-mostly at runtime; a sync.RWMutex would also work — a plain
	// Mutex keeps the code simple and avoids subtle timing bugs.
	registryMu sync.Mutex
)

// RegisterPhaseWorker installs w for w.Phase() in the registry. If
// a worker is already registered for the phase, the new one wins
// (the last writer wins) — this matches the "override in tests"
// pattern while keeping production deterministic (main.go wires
// exactly one per phase).
//
// Not safe to call concurrently for the same phase; concurrent
// registration for *different* phases is fine.
func RegisterPhaseWorker(w PhaseWorker) {
	registryMu.Lock()
	defer registryMu.Unlock()
	DefaultPhaseWorkerRegistry[w.Phase()] = w
}

// LookupPhaseWorker returns the Worker for p, or
// (nil, ErrWorkerNotRegistered) if absent. The returned Worker is
// the live registry pointer — callers MUST NOT mutate it.
func LookupPhaseWorker(p Phase) (PhaseWorker, error) {
	registryMu.Lock()
	defer registryMu.Unlock()
	w, ok := DefaultPhaseWorkerRegistry[p]
	if !ok {
		return nil, ErrWorkerNotRegistered
	}
	return w, nil
}

// UnregisterPhaseWorker removes the worker for p. Tests use this to
// reset state between sub-tests; production code does not call it.
func UnregisterPhaseWorker(p Phase) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(DefaultPhaseWorkerRegistry, p)
}

// --- BasePhaseWorker ----------------------------------------------------

// BasePhaseWorker is the default no-op implementation of PhaseWorker.
// Concrete per-phase workers (alert dedup, investigator, critic, etc.)
// live in their own packages and embed BasePhaseWorker to inherit
// the canonical field/timeout semantics — only the methods that
// actually do work need to be overridden.
//
// The base implementation is deliberately tiny so the test suite
// can assert "default behaviour is safe" without mocking each
// method individually.
type BasePhaseWorker struct {
	// PhaseRef is the phase this worker owns. Set by the embedding
	// struct's constructor. Required.
	PhaseRef Phase

	// VerifierMs is the per-worker verifier deadline override. 0
	// means "use DefaultVerifierTimeoutMs (30000ms)".
	VerifierMs int
}

// Phase returns the phase this worker handles.
func (b *BasePhaseWorker) Phase() Phase { return b.PhaseRef }

// Planner returns an empty plan. Override in concrete workers.
func (b *BasePhaseWorker) Planner(_ context.Context, _ PlanInput) (Plan, error) {
	return Plan{}, nil
}

// Executor returns an empty result. Override in concrete workers.
func (b *BasePhaseWorker) Executor(_ context.Context, _ Plan) (ExecResult, error) {
	return ExecResult{}, nil
}

// Verifier always returns OK=true with confidence 0.5. The orchestrator
// treats this as "no verifier override — advance to next phase".
// Override in concrete workers that have a real verification rule.
func (b *BasePhaseWorker) Verifier(_ context.Context, _ ExecResult) (Verdict, error) {
	return Verdict{OK: true, Confidence: 0.5}, nil
}

// VerifierTimeoutMs returns the configured override, falling back to
// DefaultVerifierTimeoutMs when zero.
func (b *BasePhaseWorker) VerifierTimeoutMs() int {
	if b.VerifierMs > 0 {
		return b.VerifierMs
	}
	return DefaultVerifierTimeoutMs
}

// NewVerifierContext returns a child context with the Verifier-deadline
// applied. The orchestrator calls this before invoking Verifier so
// the cancellation chain is explicit (and so tests can assert the
// deadline + cancel-func lifecycle).
//
// The cancel func is returned to the caller so the orchestrator can
// release it once Verifier returns (otherwise the context leaks
// across the per-phase loop run).
func (b *BasePhaseWorker) NewVerifierContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := time.Duration(b.VerifierTimeoutMs()) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Duration(DefaultVerifierTimeoutMs) * time.Millisecond
	}
	return context.WithTimeout(parent, timeout)
}
