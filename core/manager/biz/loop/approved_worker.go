// Package loop — approved_worker.go
//
// Day 5 integration: the approved phase's PhaseWorker implementation
// (Task 5.4). Bridges the orchestrator's stage-internal pipeline
// (Planner → Executor → Verifier) with the HITL pause/resume hook
// from hitl-pause-resume.
//
// 路径 A 集成批次 3 在原有能力上扩展 LLM 风险评估：
//
//  1. Planner: 读 upstream critiqued phase 的 CritiqueDimensions，
//     根据 Actionability 推 severity tier，把 tier 装进 Plan.Meta
//     供 Executor / Verifier 共享。
//  2. Executor: 从 Plan.Meta 读 severity，调 PauseHook.Evaluate
//     拿到 pauseRequired + pause_token，按结果选 ApprovedExecAdvance
//     / ApprovedExecPause。
//  3. Verifier: 镜像 Executor 的 ApprovedExecDecision 决定 OK。
//
// Behavior:
//
//  1. Planner: snapshot the upstream RootCauseJSON / RemediationOptions
//     and decide severity tier. Returns a Plan with one Step whose
//     target = "pause_hook".
//  2. Executor: invoke PauseHook.Evaluate. If pauseRequired=true, the
//     executor returns an ExecResult with RawOutputs["pause_required"]
//     = true + pause_token in SideEffects[0].Target. The
//     orchestrator's Run sees this and writes phase_paused then
//     exits. If false, Executor proceeds to invoke the remediation
//     tool (Day 7+ wires concrete tools; default is a no-op).
//  3. Verifier: a fast policy check (severity + remediation count).
//     Default Verdict{OK: true} → advance to recovered.
//
// The pause_token handshake matches the orchestrator's newPauseToken
// helper so the HITL coordinator can echo it back via Resume.
package loop

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// approvedDefaultSeverity is the fallback severity used when no
// upstream critique is available (critiqueLoader is nil OR
// LoadCritiqueDimensions returned an error / nil dims). Matches v1
// path-A-阶段 2 任务 2.5 的默认 PauseHook 行为。
const approvedDefaultSeverity = "safe"

// 路径 A 集成 §5.2：Actionability → severity tier 阈值。
//   - actionability < 0.5  → dangerous（remediation options 难落地，危险）
//   - 0.5 ≤ a < 0.8        → mutating（部分可执行，需人审）
//   - a ≥ 0.8              → safe（可自动执行）
const (
	approvedDangerousTier = "dangerous"
	approvedMutatingTier  = "mutating"
	approvedSafeTier      = "safe"

	approvedDangerousActionabilityMax = 0.5
	approvedSafeActionabilityMin      = 0.8
)

// ApprovedPhaseWorker is the approved phase Worker. It owns the
// PauseHook + critiqueLoader + clock + log dependencies so the
// orchestrator's Run doesn't need to know about HITL specifics.
type ApprovedPhaseWorker struct {
	BasePhaseWorker
	pauseHook      PauseHook
	critiqueLoader ApprovedCritiqueLoader
	clock          func() time.Time
	log            *slog.Logger

	// remediationLoader reads the candidate fixes the investigator
	// proposed. nil → the Planner reports zero options and the Executor
	// has nothing to dispatch, which is a phase failure rather than a
	// silent advance.
	remediationLoader RemediationOptionLoader

	// invoker performs the selected action. The default is
	// UnavailableInvoker, which refuses — see remediation.go for why the
	// default has to be a refusal and not a success.
	invoker RemediationInvoker

	// incident / tenant carry the run's scope from the Plan into the
	// Executor. The orchestrator calls Planner and Executor separately and
	// PlanInput does not reach the Executor, so the Planner stashes them
	// here. This is a per-run field, not shared state: the loop runs one
	// incident at a time per worker instance, and a concurrent run against
	// the same worker would be a bug worth surfacing rather than
	// papering over with a lock.
	incidentID string
	tenantID   string
}

// ApprovedCritiqueLoader 加载 critiqued phase 输出的 CritiqueDimensions
// narrow interface。生产环境由 cmd/main.go 在集成时注入
// ContractRepo.ReadCritiqueDimensions adapter；dry-run / 测试用
// NoopApprovedCritiqueLoader。
//
// 与已有 CritiqueLoader（loaders.go）的区别：本接口返回 LLM
// CritiqueDimensions（路径 A 集成期内同名冲突，已 rename 避免），而
// CritiqueLoader 返回 contract.CritiqueScore。后续批次合并两者时，
// ApprovedCritiqueLoader 退化为从 CritiqueScore.Fields 抽 Actionability。
//
// 实现约定：
//
//   - 返回 (nil, nil) 当 critiqued phase 没跑过 / 没输出（Planner 退化 v1）
//   - 返回 err 当 upstream load 失败（Planner log warn + 退化 v1）
type ApprovedCritiqueLoader interface {
	LoadCritiqueDimensions(ctx context.Context, incidentID string) (*CritiqueDimensions, error)
}

// NoopApprovedCritiqueLoader 是 ApprovedCritiqueLoader 的 no-op 实现。
// 返回 (nil, nil) — Planner 把它等同于 "upstream critique 缺失"，落回
// v1 severity="safe" 行为，让 dry-run integration test 不依赖 critique。
type NoopApprovedCritiqueLoader struct{}

func (NoopApprovedCritiqueLoader) LoadCritiqueDimensions(_ context.Context, _ string) (*CritiqueDimensions, error) {
	return nil, nil
}

// severityFromActionability 根据 §5.2 把 Actionability [0,1] 映射成 3 类
// severity tier。out-of-range（负数 / >1）按端点归一化：负数 → dangerous，
// 大于 1 → safe。
func severityFromActionability(actionability float64) string {
	switch {
	case actionability < approvedDangerousActionabilityMax:
		return approvedDangerousTier
	case actionability < approvedSafeActionabilityMin:
		return approvedMutatingTier
	default:
		return approvedSafeTier
	}
}

// ApprovedOption 是 ApprovedPhaseWorker 的 functional option。
// 用专用名以避免与 Critic*Option / Recovered*Option 同包同名冲突。
type ApprovedOption func(*ApprovedPhaseWorker)

// WithApprovedCritiqueLoader 注入 upstream critique 加载器。
// 不注入 = NoopApprovedCritiqueLoader（Planner 退化 v1 行为）。
func WithApprovedCritiqueLoader(loader ApprovedCritiqueLoader) ApprovedOption {
	return func(w *ApprovedPhaseWorker) {
		if loader != nil {
			w.critiqueLoader = loader
		}
	}
}

// WithRemediationLoader 注入 upstream 修复选项加载器。
// 不注入 = 无选项可派发（Executor 会以明确原因失败，不会假装成功）。
func WithRemediationLoader(loader RemediationOptionLoader) ApprovedOption {
	return func(w *ApprovedPhaseWorker) {
		if loader != nil {
			w.remediationLoader = loader
		}
	}
}

// WithRemediationInvoker 注入修复动作执行器。
// 不注入 = UnavailableInvoker（fail-closed：报告失败而不是静默推进）。
func WithRemediationInvoker(invoker RemediationInvoker) ApprovedOption {
	return func(w *ApprovedPhaseWorker) {
		if invoker != nil {
			w.invoker = invoker
		}
	}
}

// WithApprovedLogger 注入 slog.Logger；nil 走 slog.Default()。
func WithApprovedLogger(log *slog.Logger) ApprovedOption {
	return func(w *ApprovedPhaseWorker) {
		if log != nil {
			w.log = log
		}
	}
}

// WithApprovedClock 注入时间源；测试需要确定性时间戳时使用。
func WithApprovedClock(fn func() time.Time) ApprovedOption {
	return func(w *ApprovedPhaseWorker) {
		if fn != nil {
			w.clock = fn
		}
	}
}

// NewApprovedPhaseWorker constructs the approved phase worker.
//
// pauseHook / clock / log 仍走原来的位置参数（不动签名以免破坏
// phase_workers.go 等直接字段初始化的场景）；critiqueLoader 通过
// ApprovedOption 注入（不传 = NoopApprovedCritiqueLoader）。
func NewApprovedPhaseWorker(pauseHook PauseHook, clock func() time.Time, log *slog.Logger, opts ...ApprovedOption) (*ApprovedPhaseWorker, error) {
	if pauseHook == nil {
		pauseHook = NoopPauseHook{}
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if log == nil {
		log = slog.Default()
	}
	w := &ApprovedPhaseWorker{
		BasePhaseWorker: BasePhaseWorker{PhaseRef: PhaseApproved},
		pauseHook:       pauseHook,
		critiqueLoader:  NoopApprovedCritiqueLoader{},
		clock:           clock,
		log:             log,
		invoker:         UnavailableInvoker{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	return w, nil
}

// ApprovedExecDecision is the canonical RawOutputs["decision"] value
// the approved worker emits. The orchestrator's Run checks it on the
// executor output and decides between phase_paused vs advance.
type ApprovedExecDecision string

const (
	// ApprovedExecAdvance means the remediation proceeds; advance to
	// the recovered phase.
	ApprovedExecAdvance ApprovedExecDecision = "advance"
	// ApprovedExecPause means the pause hook required human sign-off;
	// the orchestrator writes phase_paused + the pause_token and exits.
	ApprovedExecPause ApprovedExecDecision = "pause"
	// ApprovedExecAutoApprove means the upstream RootCauseJSON marked
	// this remediation auto_approve=true; advance without consulting
	// the pause hook (still writes the contract).
	ApprovedExecAutoApprove ApprovedExecDecision = "auto_approve"
)

// ApprovedPauseTokenField is the SideEffect.Target field that carries
// the hex pause_token when ApprovedExecDecision == "pause".
const ApprovedPauseTokenField = "pause_token"

// ApprovedDecisionRawKey is the RawOutputs map key carrying the
// ApprovedExecDecision string. Orchestrator reads it after Executor.
const ApprovedDecisionRawKey = "approved_decision"

// approvedMetaSeverity 是 Planner→Executor 间传递 severity tier 的 Meta key。
// Executor 读它来填 PauseInput.Severity，不再次重算 tier。
const approvedMetaSeverity = "approved_severity"

// approvedMetaRemediationCount 是 Planner→Executor 间传递 remediation
// option 数目的 Meta key。当前 ApprovedPhaseWorker 不读 upstream
// RootCauseJSON，所以始终是 0；保留 key 是为后续批次引入
// RootCauseRefLoader 时无需再改 Executor 接口。
const approvedMetaRemediationCount = "approved_remediation_count"

// approvedMetaActionability 是 Planner→Verifier / 调试链路观察用的
// Actionability 原始值，悬浮在 Plan.Meta。
const approvedMetaActionability = "approved_actionability"

// approvedMetaRemediationOptions 是 Planner 写进 Plan.Meta 的上游修复
// 选项，Executor 从这里读取。走 Plan.Meta 而不是重新读一次 loader，是为了让
// 审批所依据的选项与被执行的选项在同一次读取内确定：中间若上游被改写，
// 审批的和执行的不再是同一份数据。
const approvedMetaRemediationOptions = "approved_remediation_options"

// approvedMetaRemediationStatus / Message / Action 是 Executor 写回
// RawOutputs 的派发结果，Verifier 与 orchestrator 读。
const (
	approvedMetaRemediationStatus  = "remediation_status"
	approvedMetaRemediationMessage = "remediation_message"
	approvedMetaRemediationAction  = "remediation_action"
)

// approvedMetaRemediationTool / Argv are what a downstream consumer (the
// crystalliser) needs to turn a verified recovery into a runbook
// declaration: the registry name it reads the tool's class by, and the
// literal vector a node would re-run. They ride on the phase_contract_written
// event payload rather than on a side table so the recorded trace of a run
// is self-contained: a reader of the event does not need to re-run the
// dispatch to learn what it did.
//
// The class is deliberately absent. It is a property the registry states
// about the tool, so a copy on the event would be a second answer to the
// same question and the two could drift; a reader takes it from the
// registry by name.
const (
	approvedMetaRemediationTool = "remediation_tool"
	approvedMetaRemediationArgv = "remediation_argv"
)

// approvedAutoApprover 是 auto_approve=true 动作的审计署名。它是一个
// 字面量而不是空串：审计链上必须能区分"策略自动批准"与"某个人批准"，
// 而"policy:auto"让前者不冒充人。
const approvedAutoApprover = "policy:auto"

// approvedPolicyApprover 预留给 resume 路径上的人工批准。
const approvedPolicyApprover = "policy:pending-human"

// approvedRemediationReason 是写进审计记录的固定理由。它记录的是
// "由 closed loop 按策略执行"，而不是把上游的 action 名重复一遍——
// action 名已经在 side effect 的 target 里了。
const approvedRemediationReason = "closed loop: pre-approved remediation dispatched by policy"

// approvedMetaPauseRequired 是 Executor 写回 ExecResult.RawOutputs 的
// pause 决策 flag（true / false），便于 Verifier / orchestrator
// / Harness rubric 一次性判定是否有人工介入。Pause token 仍走
// SideEffects[0].Target 以兼容现有 contract-writer。
const approvedMetaPauseRequired = "pause_required"

// Planner 流程（路径 A 集成批次 3）：
//
//  1. 调 critiqueLoader.LoadCritiqueDimensions(ctx, in.IncidentID)
//  2. nil dims / 加载错误 → log warn + 退化 v1（severity="safe"）
//  3. dims != nil → severityFromActionability(dims.Actionability)
//  4. severity / remediation_count / actionability 装进 Plan.Meta
//  5. Plan.Steps[0] 是 "skill_call" target="pause_hook"，Args 拷贝
//     severity / remediation_count 兼容 v1 pauseHook Eval signature。
func (w *ApprovedPhaseWorker) Planner(ctx context.Context, in PlanInput) (Plan, error) {
	severity := approvedDefaultSeverity
	remediationCount := 0
	var actionability *float64

	// The Planner keeps the run's scope for the Executor, which is called
	// separately and never sees PlanInput.
	w.incidentID = in.IncidentID
	w.tenantID = in.TenantID

	// Load the proposed fixes. This is the value the severity tier is
	// partly about: a run with three proposed actions and a run with one
	// are not the same approval decision, and reporting zero here made
	// the pause hook's remediation_count argument a constant.
	var proposed []RemediationOption
	if w.remediationLoader != nil {
		opts, err := w.remediationLoader.LoadRemediationOptions(ctx, in.TenantID, in.IncidentID)
		if err != nil {
			// A loader failure is not a reason to proceed with no
			// options: that is indistinguishable from "the investigator
			// proposed nothing", and it would advance a run whose
			// approval was decided without reading what it was approving.
			return Plan{}, fmt.Errorf("loop: approved planner: load remediation options: %w", err)
		}
		proposed = opts
	}
	remediationCount = len(proposed)

	// LLM 风险评估：基于 critiqued phase 上游 CritiqueDimensions。
	if w.critiqueLoader != nil {
		dims, err := w.critiqueLoader.LoadCritiqueDimensions(ctx, in.IncidentID)
		switch {
		case err != nil:
			w.log.WarnContext(ctx, "loop: approved planner: load critique failed; fallback to default severity",
				"incident_id", in.IncidentID,
				"err", err.Error())
		case dims == nil:
			// 上游 critique 缺失（critiqued phase 没跑 / 被跳过），退化 v1。
		default:
			severity = severityFromActionability(dims.Actionability)
			a := dims.Actionability
			actionability = &a
		}
	}

	meta := map[string]any{
		approvedMetaSeverity:           severity,
		approvedMetaRemediationCount:   remediationCount,
		approvedMetaRemediationOptions: proposed,
	}
	if actionability != nil {
		meta[approvedMetaActionability] = *actionability
	}

	return Plan{
		Steps: []PlanStep{{
			Kind:   "skill_call",
			Target: "pause_hook",
			Args: map[string]any{
				"severity":          severity,
				"remediation_count": remediationCount,
			},
		}},
		Meta: meta,
	}, nil
}

// Executor consults the PauseHook with the severity the Planner
// computed from upstream critique. Returns:
//
//   - ApprovedExecPause + pause_token in SideEffects[0].Target when
//     the hook demands human sign-off (orchestrator writes
//     phase_paused + aborts).
//   - ApprovedExecAdvance / ApprovedExecAutoApprove otherwise (the
//     orchestrator writes phase_contract_written + advances to
//     recovered).
func (w *ApprovedPhaseWorker) Executor(ctx context.Context, plan Plan) (ExecResult, error) {
	severity := approvedDefaultSeverity
	if v, ok := plan.Meta[approvedMetaSeverity].(string); ok && v != "" {
		severity = v
	}

	pauseRequired, token, err := w.pauseHook.Evaluate(ctx, PauseInput{
		Phase:            PhaseApproved,
		Actor:            "auto",
		Severity:         severity,
		RemediationCount: remediationCountFromPlan(plan),
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("loop: approved pause hook: %w", err)
	}
	if pauseRequired {
		if token == "" {
			t, terr := newPauseToken()
			if terr != nil {
				return ExecResult{}, fmt.Errorf("loop: mint pause token: %w", terr)
			}
			token = t
		}
		return ExecResult{
			SideEffects: []SideEffect{{
				Kind:   "approval_request",
				Target: ApprovedPauseTokenField,
				Detail: map[string]any{"value": token},
			}},
			RawOutputs: map[string]any{
				ApprovedDecisionRawKey:    string(ApprovedExecPause),
				approvedMetaPauseRequired: true,
				"pause_token":             token,
				approvedMetaSeverity:      severity,
			},
		}, nil
	}
	// The pause hook cleared the run, so the approved action may now be
	// performed. This is the branch that used to return here having done
	// nothing at all.
	dispatch, err := w.dispatchRemediation(ctx, plan)
	if err != nil {
		// The partial result is returned alongside the error rather than
		// discarded. dispatchRemediation has already written the
		// ToolReplay entry for the attempt, and throwing it away would
		// leave the phase_failed event with no record of what was tried —
		// which downstream reads identically to "nothing was attempted".
		//
		// The decision key is deliberately absent: a failed dispatch is
		// not an advance, and the Verifier keys off its absence.
		return dispatch, err
	}
	dispatch.RawOutputs[ApprovedDecisionRawKey] = string(ApprovedExecAdvance)
	dispatch.RawOutputs[approvedMetaPauseRequired] = false
	dispatch.RawOutputs[approvedMetaSeverity] = severity
	return dispatch, nil
}

// dispatchRemediation selects and performs the approved action.
//
// Four outcomes, and the distinction between them is the whole point:
//
//   - dispatched successfully → advance; the recovered phase verifies it
//   - dispatched and failed    → the phase fails; the failure is in
//     evidence before the recovery verifier runs, rather than after it
//     has drawn a conclusion from unchanged metrics
//   - nothing eligible         → the phase fails; a run whose every
//     proposed action needs a human must stop for one
//   - no loader wired          → the phase fails; this is a deployment
//     fault and pretending otherwise produces a postmortem that claims a
//     fix nobody applied
func (w *ApprovedPhaseWorker) dispatchRemediation(ctx context.Context, plan Plan) (ExecResult, error) {
	proposed, _ := plan.Meta[approvedMetaRemediationOptions].([]RemediationOption)
	if w.remediationLoader == nil {
		return ExecResult{}, fmt.Errorf(
			"loop: approved phase cannot dispatch a remediation: no remediation option loader is wired "+
				"(%d option(s) proposed); refusing to advance a run that would apply nothing", len(proposed))
	}
	selected, err := SelectRemediation(proposed)
	if err != nil {
		return ExecResult{}, err
	}

	approver := approvedAutoApprover
	if !selected.AutoApprove {
		// Unreachable through SelectRemediation today, but the branch
		// exists because the resume path will grow: a human approving via
		// pause token is not auto_approve, and when that lands the
		// approver must be their id rather than a literal.
		approver = approvedPolicyApprover
	}
	start := w.clock()
	outcome, invokeErr := w.invoker.Invoke(ctx, RemediationRequest{
		IncidentID: w.incidentID,
		TenantID:   w.tenantID,
		Option:     selected,
		Approver:   approver,
		Reason:     approvedRemediationReason,
	})
	if invokeErr != nil {
		// A failure is recorded before it is returned. The phase_failed
		// event and the ToolReplay entry are the two places a
		// postmortem can later find out what was attempted; an error
		// that only reached a log line leaves the incident looking
		// like it never got that far.
		outcome = RemediationOutcome{
			Status:  RemediationStatusFailed,
			Message: invokeErr.Error(),
			Args:    outcome.Args,
			Result:  outcome.Result,
		}
	}
	replay, side := RecordRemediation(outcome, start, w.clock)
	replay.Name = selected.Action
	side.Target = selected.Target
	side.Detail["action"] = selected.Action
	side.Detail["risk"] = selected.Risk
	side.Detail["auto_approve"] = selected.AutoApprove
	side.Detail["approver"] = approver

	rawOutputs := map[string]any{
		approvedMetaRemediationStatus:  outcome.Status,
		approvedMetaRemediationMessage: outcome.Message,
		approvedMetaRemediationAction:  selected.Action,
	}
	// The class, tool and argv are recorded only when the replay actually
	// carries a vector to re-run. Writing an empty argv here would let a
	// downstream reader treat "reached its change through an API" as "a
	// runbook with no arguments", which the ledger refuses anyway — but
	// recording the absence explicitly is what tells the two apart at the
	// event.
	if len(replay.Argv) > 0 {
		rawOutputs[approvedMetaRemediationArgv] = replay.Argv
		rawOutputs[approvedMetaRemediationTool] = replay.RegisteredTool
	}
	result := ExecResult{
		SideEffects: []SideEffect{side},
		ToolReplay:  []ToolReplayEntry{replay},
		RawOutputs:  rawOutputs,
	}
	if outcome.Status != RemediationStatusSuccess || invokeErr != nil {
		return result, fmt.Errorf("%w: %s: %s", ErrRemediationFailed, selected.Action, outcome.Message)
	}
	return result, nil
}

// remediationCountFromPlan 安全地从 Plan.Meta 读 remediation count，
// 缺字段或类型不符返回 0。后续批次可能由 Planner 从 upstream
// RootCauseJSON.RemediationOptions 推出非 0 值。
func remediationCountFromPlan(plan Plan) int {
	v, ok := plan.Meta[approvedMetaRemediationCount]
	if !ok {
		return 0
	}
	n, ok := v.(int)
	if !ok {
		return 0
	}
	return n
}

// Verifier returns OK=true when the executor reported advance /
// auto_approve; OK=false when the executor demanded pause (the
// orchestrator should NOT advance, it should record paused).
//
// We keep this fast — the pause decision already happened in the
// Executor; Verifier just mirrors the result so the orchestrator's
// Verifier deadline path has something to time-out against.
func (w *ApprovedPhaseWorker) Verifier(_ context.Context, result ExecResult) (Verdict, error) {
	decisionRaw, _ := result.RawOutputs[ApprovedDecisionRawKey]
	decision, _ := decisionRaw.(string)
	switch ApprovedExecDecision(decision) {
	case ApprovedExecAdvance, ApprovedExecAutoApprove:
		// An advance must be backed by a dispatch. This check is what
		// makes the fix durable: a later edit that restores a no-op
		// executor, or a pause hook that clears a run with no eligible
		// action, is caught here rather than being written into a
		// postmortem as a fix.
		status, _ := result.RawOutputs[approvedMetaRemediationStatus].(string)
		if status != RemediationStatusSuccess {
			action, _ := result.RawOutputs[approvedMetaRemediationAction].(string)
			message, _ := result.RawOutputs[approvedMetaRemediationMessage].(string)
			return Verdict{OK: false, Reasons: []string{
				fmt.Sprintf("advance without a successful dispatch: action=%q status=%q message=%q", action, status, message),
			}}, nil
		}
		return Verdict{OK: true, Confidence: 1.0}, nil
	case ApprovedExecPause:
		return Verdict{OK: false, Reasons: []string{"pause_required"}}, nil
	default:
		return Verdict{OK: false, Reasons: []string{"unknown_decision"}}, nil
	}
}

// VerifierTimeoutMs — approved phase uses the default 30s; pause
// evaluation is in-process and should not block long.
func (w *ApprovedPhaseWorker) VerifierTimeoutMs() int {
	if v := w.BasePhaseWorker.VerifierTimeoutMs(); v > 0 {
		return v
	}
	return DefaultVerifierTimeoutMs
}
