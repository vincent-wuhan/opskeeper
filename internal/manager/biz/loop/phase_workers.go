// Package loop — phase_workers.go
//
// Day 5 integration: default PhaseWorker factory wiring the seven
// phases (detected → correlated → investigated → critiqued →
// approved → recovered → postmortem) to a minimal-but-real Worker
// instance each. The factory exists so:
//
//  1. The orchestrator always has a Worker per phase → Run can walk
//     the full state machine without panicking on missing registry.
//  2. The dry-run integration test exercises the seven-phase
//     pipeline without LLM or external DB.
//  3. Per-phase concrete logic (alert dedup, investigator, critic,
//     flow.Execute, postmortem) replaces the no-op defaults in
//     Day 6–10 follow-ups without touching the orchestrator.
//
// Design references:
//
//   - Design §5 (PhaseWorker three-segment pipeline: Planner →
//     Executor → Verifier).
//   - Spec closed-loop-orchestrator §"七阶段状态机".
//   - Tasks 5.1, 5.2, 5.3, 5.4 (Day 5).
//
// HITL pause (Task 5.4): the approved phase worker delegates the
// pre-execution hook to a PauseHook. The hitl-pause-resume
// integration implements the hook; in tests + dry-runs the hook is
// a no-op so the approved phase auto-advances.
package loop

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// FlowRunner is the narrow seam the per-phase Worker uses to invoke
// the in-phase sub-task DAG (Task 5.2). Production wires the
// flow/engine.go Engine wrapped behind a DAG descriptor; tests pass
// a fake that records calls. nil = skip DAG (still valid; the
// minimal default worker never invokes the runner).
type FlowRunner interface {
	RunDAG(ctx context.Context, incidentID, phase string, plan Plan) error
}

// PauseHook is the HITL pause seam the approved phase invokes before
// advancing to recovered (Task 5.4). The default no-op returns nil
// immediately; the hitl-pause-resume integration returns a paused
// sentinel when severity policy demands human approval.
type PauseHook interface {
	// Evaluate returns (pauseRequired, pauseToken, err). When
	// pauseRequired=true the orchestrator writes a phase_paused
	// event and aborts the run; the resume path resumes from the
	// stored pause_token.
	Evaluate(ctx context.Context, in PauseInput) (bool, string, error)
}

// PauseInput is the per-phase evaluation context handed to PauseHook.
type PauseInput struct {
	IncidentID string
	TenantID   string
	Phase      Phase
	// Severity is the upstream-derived risk tier ("safe" /
	// "mutating" / "dangerous"). The hook may consult this to
	// decide whether to require human sign-off.
	Severity string
	// RemediationCount is the count of remediation_options on the
	// upstream RootCauseJSON; some hooks gate on "≥1 mutating".
	RemediationCount int
	// Actor is the user_id or "auto" that triggered the loop.
	Actor string
}

// NoopPauseHook returns immediately with pauseRequired=false. The
// default for dry-runs + unit tests; production replaces with the
// hitl-pause-resume impl.
type NoopPauseHook struct{}

func (NoopPauseHook) Evaluate(_ context.Context, _ PauseInput) (bool, string, error) {
	return false, "", nil
}

// NoopFlowRunner is the in-phase DAG seam's no-op impl. Mirrors
// NoopPauseHook: present so the factory always builds a valid Worker
// without forcing callers to wire a real flow.Engine.
type NoopFlowRunner struct{}

func (NoopFlowRunner) RunDAG(_ context.Context, _, _ string, _ Plan) error {
	return nil
}

// PhaseWorkerDeps is the dependency bag the factory needs. Fields
// are optional — the factory falls back to no-op / base behaviour
// when a dep is nil so the dry-run integration test never panics.
type PhaseWorkerDeps struct {
	// VerifyCaller is the Day 3 narrow interface that drives the
	// recovered phase Worker. Required for the recovered phase to
	// be functional; nil → recovered uses BasePhaseWorker (always
	// passes verification).
	VerifyCaller VerifyRecoveryCaller

	// StateStore is the retry_count persistence seam the recovered
	// phase reads/writes. Same fallback as VerifyCaller.
	StateStore RecoveryStateStore

	// ApprovedRefLoader reads the upstream ApprovalDecision so the
	// recovered phase knows the target / resource_type / metrics.
	// Same fallback as VerifyCaller.
	ApprovedRefLoader ApprovedDecisionLoader

	// FlowRunner drives the in-phase DAG sub-task. nil → use
	// NoopFlowRunner so workers that don't need a DAG aren't
	// forced to wire one.
	FlowRunner FlowRunner

	// PauseHook is the HITL pause gate for the approved phase. nil
	// → use NoopPauseHook.
	PauseHook PauseHook

	// Logger is the slog.Logger; nil → slog.Default().
	Logger *slog.Logger

	// Clock is the wall-clock source; nil → time.Now. Tests inject
	// a fake so deterministic timestamps survive race tests.
	Clock func() time.Time

	// LLMCaller 注入 5 worker（detected / correlated / investigated /
	// critiqued / postmortem）。nil → 5 worker fallback 到 BasePhaseWorker
	// 占位（保留 v1 行为兼容）。
	LLMCaller LLMCaller

	// 各 worker 自己的窄依赖（nil → Noop 占位，wire-up 兜底）。
	AlertRepo                   AlertRepository             // correlated worker
	CurrentDetectionEventLoader CurrentDetectionEventLoader // correlated worker（读 detected contract）
	InvestigatorToolset         InvestigatorToolset         // investigated worker
	CorrelatedGroupLoader       CorrelatedGroupLoader       // investigated worker（读上游 CorrelatedGroup）
	GitArtifactSink             GitArtifactSink             // postmortem worker
	UpstreamContractLoader      UpstreamContractLoader      // postmortem worker（读 3 个上游 contract bundle）
	ApprovedCritiqueLoader      ApprovedCritiqueLoader      // approved worker（基于 CritiqueDimensions 算 severity）
	PatternWriter               PatternWriter               // postmortem worker（KB write-back hook，nil → 跳过）
}

// DefaultPhaseWorkerFactory builds the seven default PhaseWorker
// instances. The recovered phase is upgraded to RecoveredPhaseWorker
// when VerifyCaller+StateStore+ApprovedRefLoader are all present;
// otherwise it falls back to a BasePhaseWorker with the recovered
// verifier timeout. The other six phases use the BasePhaseWorker
// shape; concrete logic is filled in by Day 6–10 follow-ups.
func DefaultPhaseWorkerFactory(deps PhaseWorkerDeps) (map[Phase]PhaseWorker, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := deps.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	flow := deps.FlowRunner
	if flow == nil {
		flow = NoopFlowRunner{}
	}
	pause := deps.PauseHook
	if pause == nil {
		pause = NoopPauseHook{}
	}

	workers := make(map[Phase]PhaseWorker, len(allForwardPhases))

	// Approved → recovered: HITL pause gate (Task 5.4).
	// ApprovedPhaseWorker 已用 Option 注入 ApprovedCritiqueLoader；nil loader → NoopApprovedCritiqueLoader。
	approvedCritiqueLoader := deps.ApprovedCritiqueLoader
	approved, err := NewApprovedPhaseWorker(
		pause, clock, logger,
		WithApprovedCritiqueLoader(approvedCritiqueLoader),
	)
	if err != nil {
		return nil, fmt.Errorf("loop: build approved worker: %w", err)
	}
	workers[PhaseApproved] = approved

	// LLM-driven 5 worker（detected / correlated / investigated / critiqued / postmortem）。
	// deps.LLMCaller == nil → fallback 到 BasePhaseWorker（v1 行为兼容）。
	if deps.LLMCaller != nil {
		// Detected → correlated
		workers[PhaseDetected] = NewDetectedPhaseWorker(deps.LLMCaller)

		// Correlated → investigated
		alertRepo := deps.AlertRepo
		if alertRepo == nil {
			alertRepo = NoopAlertRepository{}
		}
		currentEventLoader := deps.CurrentDetectionEventLoader
		correlated, err := NewCorrelatedPhaseWorker(deps.LLMCaller, alertRepo,
			WithCurrentDetectionEventLoader(currentEventLoader))
		if err != nil {
			return nil, fmt.Errorf("loop: build correlated worker: %w", err)
		}
		workers[PhaseCorrelated] = correlated

		// Investigated → critiqued
		toolset := deps.InvestigatorToolset
		if toolset == nil {
			toolset = NoopInvestigatorToolset{}
		}
		groupLoader := deps.CorrelatedGroupLoader
		if groupLoader == nil {
			groupLoader = NoopCorrelatedGroupLoader{}
		}
		investigated, err := NewInvestigatedPhaseWorker(deps.LLMCaller, toolset, groupLoader, logger)
		if err != nil {
			return nil, fmt.Errorf("loop: build investigated worker: %w", err)
		}
		workers[PhaseInvestigated] = investigated

		// Critiqued → approved
		workers[PhaseCritiqued] = NewCritiquedPhaseWorker(deps.LLMCaller)

		// Postmortem → terminal
		gitSink := deps.GitArtifactSink
		if gitSink == nil {
			gitSink = NoopGitArtifactSink{}
		}
		upstreamLoader := deps.UpstreamContractLoader
		if upstreamLoader == nil {
			upstreamLoader = NoopUpstreamContractLoader{}
		}
		postmortem, err := NewPostmortemPhaseWorker(deps.LLMCaller, gitSink, upstreamLoader, deps.PatternWriter, logger)
		if err != nil {
			return nil, fmt.Errorf("loop: build postmortem worker: %w", err)
		}
		workers[PhasePostmortem] = postmortem
	} else {
		// v1 行为：5 phase BasePhaseWorker 占位（dry-run integration test 仍能跑通）。
		workers[PhaseDetected] = &BasePhaseWorker{PhaseRef: PhaseDetected}
		workers[PhaseCorrelated] = &BasePhaseWorker{PhaseRef: PhaseCorrelated}
		workers[PhaseInvestigated] = &BasePhaseWorker{PhaseRef: PhaseInvestigated}
		workers[PhaseCritiqued] = &BasePhaseWorker{PhaseRef: PhaseCritiqued}
		workers[PhasePostmortem] = &BasePhaseWorker{PhaseRef: PhasePostmortem}
	}

	// Recovered → postmortem OR rollback to approved.
	if deps.VerifyCaller != nil && deps.StateStore != nil && deps.ApprovedRefLoader != nil {
		rec, err := NewRecoveredPhaseWorker(
			deps.VerifyCaller,
			deps.StateStore,
			deps.ApprovedRefLoader,
			logger,
		)
		if err != nil {
			return nil, fmt.Errorf("loop: build recovered worker: %w", err)
		}
		workers[PhaseRecovered] = rec
	} else {
		// Fallback: BasePhaseWorker with the recovered verifier
		// timeout so a no-DB dry-run still walks past recovered.
		workers[PhaseRecovered] = &BasePhaseWorker{
			PhaseRef:   PhaseRecovered,
			VerifierMs: RecoveredPhaseVerifierTimeoutMs,
		}
	}

	// Postmortem: terminal phase; default no-op Worker (Day 4
	// concrete impl fills in the markdown rendering).
	workers[PhasePostmortem] = &BasePhaseWorker{PhaseRef: PhasePostmortem}

	return workers, nil
}

// RegisterDefaultPhaseWorkers wires the seven default workers into
// the package-global DefaultPhaseWorkerRegistry. Called once from
// cmd/opskeeper/main.go at startup; tests skip this and pass their own
// WorkerRegistry via OrchestratorDeps.WorkerRegistry.
//
// Safe to call multiple times — the registry is last-writer-wins.
func RegisterDefaultPhaseWorkers(deps PhaseWorkerDeps) error {
	workers, err := DefaultPhaseWorkerFactory(deps)
	if err != nil {
		return err
	}
	for _, w := range workers {
		RegisterPhaseWorker(w)
	}
	return nil
}

// UnregisterDefaultPhaseWorkers removes every phase worker installed
// by RegisterDefaultPhaseWorkers. Tests use it to reset global state
// between sub-tests; production never calls it.
func UnregisterDefaultPhaseWorkers() {
	for _, p := range allForwardPhases {
		UnregisterPhaseWorker(p)
	}
}
