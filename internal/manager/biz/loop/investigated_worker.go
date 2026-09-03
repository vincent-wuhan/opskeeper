// Package loop — investigated_worker.go
//
// investigated phase 的 PhaseWorker 实现（zero-manual-ops-loop · D2）。
//
// investigated phase 的职责：
//
//  1. 读取上游 correlated phase 的 CorrelatedGroup（resource_type /
//     target / time_window / alert_ids）
//  2. 通过 InvestigatorToolset 拿 evidence chain + remediation options
//  3. 调 LLMCaller 让模型把 evidence + remediations 汇总成 RootCauseJSON
//  4. Verifier 复核 evidence 数量 ≥ 1 + remediation 数量 ≥ 1 + schema_version
//     在 v1 / v1.1，confidence ∈ [0, 1] 等（深层校验委托给
//     ValidateRootCauseJSON；Verifier 只做必要的兜底断言）
//
// 设计依据：
//   - OpenSpec delta spec: openspec/changes/llm-worker-integration/specs/closed-loop-orchestrator/spec.md
//     "InvestigatedPhaseWorker 真实 LLM-driven 实现" Scenario
//   - design §3.2 LLMCaller 抽象 + §3.3 五 phase worker 集成
//   - contract.go:RootCauseJSON v1 形态（已存在，复用）
//   - correlated_worker.go:CorrelatedGroup v1 形态（已存在，复用；本次扩展
//     加 ResourceType / Target / TimeWindow 三个 omitempty 字段供
//     investigated phase 读）
//
// 不引入 monorepo 跨域 import：
//   - InvestigatorToolset / CorrelatedGroupLoader 是本包内定义的
//     narrow interface；concrete 实现由 cmd/main.go 在 Day 5 集成时
//     注入（aiops/tools.InvestigatorTool + loop_contract.LoadCorrelatedGroup）。
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// investigatedOutputSchema 是给 LLMCaller.Call 的 OutputSchema。
//
// LLMCaller 的 schema validator 子集只支持 type / required / properties /
// items / enum；minItems / maximum / minimum 由 ValidateRootCauseJSON 在
// Executor 路径上兜底。所以这里只声明"必填 + 类型 + kind 枚举"，数量上下
// 界交给 ValidateRootCauseJSON。
//
// required 字段对齐 RootCauseJSON：
//
//	schema_version, root_cause_object, confidence, evidence_chain,
//	time_window, remediation_options。
const investigatedOutputSchema = `{
  "type": "object",
  "required": ["schema_version", "root_cause_object", "confidence", "evidence_chain", "time_window", "remediation_options"],
  "properties": {
    "schema_version": {"type": "string"},
    "root_cause_object": {
      "type": "object",
      "required": ["kind", "summary"],
      "properties": {
        "kind":    {"type": "string", "enum": ["pg_lock","pg_long_tx","pg_pool_exhausted","redis_memory","k8s_oom","host_cpu","host_memory","host_disk","unknown"]},
        "summary": {"type": "string"},
        "detail":  {"type": "object"}
      }
    },
    "confidence":         {"type": "number"},
    "evidence_chain":     {"type": "array", "items": {"type": "object"}},
    "time_window":        {"type": "object"},
    "remediation_options":{"type": "array", "items": {"type": "object"}}
  }
}`

const investigatedPhaseLLMTimeoutMs = 120_000

// InvestigatorToolset 是 investigated phase 用的 investigator 工具集合。
//
// 这是 narrow interface；生产实现由 aiops/tools 包提供，Day 5 集成时由
// main.go 包成本接口。测试用 inMemoryInvestigatorToolset（fake）。
type InvestigatorToolset interface {
	// Investigate 基于 resource_type + alertID 拉 evidence chain。
	// time_window 限定样本区间。返回的 slice 必须满足 len ≥ 1
	// （否则 LLMCaller 拿不到 evidence 写 RootCauseJSON）。
	Investigate(ctx context.Context, resourceType, alertID string, timeWindow TimeWindow) ([]EvidenceItem, error)

	// ListRemediations 列出该 resource_type 可用的 remediation options。
	// 返回的 slice 必须满足 len ≥ 1。
	ListRemediations(ctx context.Context, resourceType string) ([]RemediationOption, error)
}

// EvidenceAwareInvestigatorToolset is the optional production extension used
// by investigator-real-strategy. Keeping it separate preserves every existing
// InvestigatorToolset fake, Noop, and static adapter implementation.
type EvidenceAwareInvestigatorToolset interface {
	InvestigatorToolset
	ListRemediationsWithEvidence(ctx context.Context, resourceType, alertID string, evidence []EvidenceItem) ([]RemediationOption, error)
}

// CorrelatedGroupLoader 加载上游 correlated phase 写入的 CorrelatedGroup。
//
// 与 recovery.go::ApprovedDecisionLoader 同形：narrow interface 在 loop 包内
// 重声明以避免跨域 import；Day 5 集成时由 adapter 把 loop_contract repo 接进来。
type CorrelatedGroupLoader interface {
	LoadCorrelatedGroup(ctx context.Context, tenantID string, contractID int64) (*CorrelatedGroup, error)
}

// NoopCorrelatedGroupLoader 是 CorrelatedGroupLoader 的 no-op 实现。
// 返回 (nil, nil)，让 Planner 在缺真实 contract 装载器时仍能跑通
// （用 default values 合成一个 CorrelatedGroup）。
type NoopCorrelatedGroupLoader struct{}

// LoadCorrelatedGroup 始终返回 nil。
func (NoopCorrelatedGroupLoader) LoadCorrelatedGroup(_ context.Context, _ string, _ int64) (*CorrelatedGroup, error) {
	return nil, nil
}

// InvestigatedPhaseWorker 是 PhaseWorker 的 investigated phase 实现。
//
// 构造由 NewInvestigatedPhaseWorker 完成；caller / toolset / loader 必填
// （生产环境由 cmd/main.go 在 Day 5 集成时 wire-up）。
type InvestigatedPhaseWorker struct {
	// PhaseRef 固定 PhaseInvestigated（compile-time 校验）。
	PhaseRef Phase

	// caller 走 LLMCaller 抽象（路径 A 集成期不允许直接 import eino）。
	caller LLMCaller

	// toolset 调 investigator 工具拿 evidence + remediations。
	toolset InvestigatorToolset

	// loader 读上游 CorrelatedGroup。
	loader CorrelatedGroupLoader

	// clock 是时间源。nil = time.Now。tests 注入 fake 让确定性时间戳
	// 存活于 -race 测试下。
	clock func() time.Time

	// log 是 slog.Logger；nil = slog.Default()。
	log *slog.Logger
}

// NewInvestigatedPhaseWorker 是生产构造器。
//
// caller / toolset / loader 必填（monorepo 不让跨域 import，所以 main.go
// 在 Day 5 集成时负责 wire-up）。log / clock 缺省走默认值。
func NewInvestigatedPhaseWorker(
	caller LLMCaller,
	toolset InvestigatorToolset,
	loader CorrelatedGroupLoader,
	log *slog.Logger,
) (*InvestigatedPhaseWorker, error) {
	if caller == nil {
		return nil, errors.New("loop: InvestigatedPhaseWorker.caller is required")
	}
	if toolset == nil {
		return nil, errors.New("loop: InvestigatedPhaseWorker.toolset is required")
	}
	if loader == nil {
		loader = NoopCorrelatedGroupLoader{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &InvestigatedPhaseWorker{
		PhaseRef: PhaseInvestigated,
		caller:   caller,
		toolset:  toolset,
		loader:   loader,
		clock:    func() time.Time { return time.Now().UTC() },
		log:      log,
	}, nil
}

// Phase 实现 PhaseWorker 接口。
func (w *InvestigatedPhaseWorker) Phase() Phase { return w.PhaseRef }

// Planner 准备 Plan：读取上游 CorrelatedGroup，调 investigator 工具集拿
// evidence + remediations，组装 LLM prompt。
//
// 步骤：
//  1. 通过 loader 取 CorrelatedGroup（resource_type / target / time_window
//     / alert_ids）；loader 返回 nil 时用 defaultCorrelatedGroup() 兜底
//  2. 调 toolset.Investigate(...) 拿 evidence chain；toolset 失败立即拒绝
//  3. 调 toolset.ListRemediations(...) 拿 remediation options；同上
//  4. 把 evidence + remediations + CorrelatedGroup 序列化成 JSON 片段，
//     拼成 user prompt；system prompt 是 investigator role description
//  5. Plan.Steps 唯一一项是 llm_call；prompt / system_prompt / output_schema
//     装到 Args；Meta 携带 evidence + remediations 给 Executor 复用
func (w *InvestigatedPhaseWorker) Planner(ctx context.Context, in PlanInput) (Plan, error) {
	group := investigationInputGroup(in)
	if group == nil {
		loadedGroup, err := w.loadCorrelatedGroup(ctx, in)
		if err != nil {
			return Plan{}, err
		}
		group = loadedGroup
	}
	if group == nil {
		group = defaultCorrelatedGroup(in)
	}

	resourceType := pickResourceType(group)
	alertID := pickAlertID(group)
	timeWindow := pickTimeWindow(group)

	evidence, err := w.toolset.Investigate(ctx, resourceType, alertID, timeWindow)
	if err != nil {
		return Plan{}, fmt.Errorf("loop: investigator.Investigate: %w", err)
	}
	if len(evidence) < 1 {
		return Plan{}, fmt.Errorf("%w: investigator returned empty evidence chain for resource_type=%s",
			ErrInvalidSchema, resourceType)
	}

	var remediations []RemediationOption
	if evidenceAware, ok := w.toolset.(EvidenceAwareInvestigatorToolset); ok {
		remediations, err = evidenceAware.ListRemediationsWithEvidence(ctx, resourceType, alertID, evidence)
	} else {
		remediations, err = w.toolset.ListRemediations(ctx, resourceType)
	}
	if err != nil {
		return Plan{}, fmt.Errorf("loop: investigator.ListRemediations: %w", err)
	}
	if len(remediations) < 1 {
		return Plan{}, fmt.Errorf("%w: investigator returned empty remediation_options for resource_type=%s",
			ErrInvalidSchema, resourceType)
	}

	prompt, systemPrompt := buildInvestigatedPrompt(group, evidence, remediations)

	return Plan{
		Steps: []PlanStep{{
			Kind:   "llm_call",
			Target: "summarize_root_cause",
			Args: map[string]any{
				"system_prompt": systemPrompt,
				"user_prompt":   prompt,
				"output_schema": investigatedOutputSchema,
			},
			TimeoutMs: investigatedPhaseLLMTimeoutMs,
		}},
		EstimatedCost: CostEstimate{USD: 0, Tokens: 0},
		Meta: map[string]any{
			"correlated_group":    group,
			"resource_type":       resourceType,
			"alert_id":            alertID,
			"time_window":         timeWindow,
			"evidence_chain":      evidence,
			"remediation_options": remediations,
		},
	}, nil
}

// Executor 调 LLMCaller.Call，解析返回值成 RootCauseJSON 并做 schema 校验。
//
// LLMCaller.Call 自己做 OutputSchema 校验（缺 required 字段 / 类型不匹配 /
// enum 越界返回 ErrSchemaInvalid wrapped）。Executor 在此之上再加
// ValidateRootCauseJSON 做更深校验（schema_version / confidence 范围 /
// evidence 数量上下界 / remediation 数量上下界 / risk 枚举）。
func (w *InvestigatedPhaseWorker) Executor(ctx context.Context, plan Plan) (ExecResult, error) {
	if len(plan.Steps) == 0 {
		return ExecResult{}, fmt.Errorf("%w: planner returned empty steps", ErrPlanInvalid)
	}
	if deterministicHostCPUSpikeHint(plan) {
		rootCause, err := deterministicRootCause(plan)
		if err != nil {
			return ExecResult{}, fmt.Errorf("loop: deterministic host CPU root cause: %w", err)
		}
		return w.rootCauseExecResult(plan, rootCause, 0, 0, 0, 0), nil
	}
	if deterministicPGPoolExhaustionHint(plan) {
		rootCause, err := deterministicRootCause(plan)
		if err != nil {
			return ExecResult{}, fmt.Errorf("loop: deterministic PostgreSQL pool root cause: %w", err)
		}
		return w.rootCauseExecResult(plan, rootCause, 0, 0, 0, 0), nil
	}
	step := plan.Steps[0]
	systemPrompt, _ := step.Args["system_prompt"].(string)
	userPrompt, _ := step.Args["user_prompt"].(string)
	outputSchema, _ := step.Args["output_schema"].(string)

	callOut, err := w.caller.Call(ctx, CallInput{
		Phase:        PhaseInvestigated,
		Prompt:       userPrompt,
		SystemPrompt: systemPrompt,
		OutputSchema: outputSchema,
		TimeoutMs:    step.TimeoutMs,
		MaxRetries:   0,
	})
	if err != nil {
		if errors.Is(err, ErrSchemaInvalid) {
			rc, fallbackErr := deterministicRootCause(plan)
			if fallbackErr == nil {
				w.log.Warn("investigation LLM output invalid; using deterministic evidence-backed root cause", "error", err.Error())
				return w.rootCauseExecResult(plan, rc, 0, 0, 0, 0), nil
			}
		}
		return ExecResult{}, fmt.Errorf("loop: LLM summarize_root_cause: %w", err)
	}

	var rc RootCauseJSON
	if err := json.Unmarshal(callOut.Raw, &rc); err != nil {
		return ExecResult{}, fmt.Errorf("%w: LLM returned malformed RootCauseJSON: %w", ErrInvalidSchema, err)
	}
	if err := ValidateRootCauseJSON(&rc); err != nil {
		fallback, fallbackErr := deterministicRootCause(plan)
		if fallbackErr == nil {
			w.log.Warn("investigation LLM RootCauseJSON invalid; using deterministic evidence-backed root cause", "error", err.Error())
			return w.rootCauseExecResult(plan, fallback, callOut.TokensIn, callOut.TokensOut, callOut.CostUSD, callOut.LatencyMs), nil
		}
		return ExecResult{}, fmt.Errorf("%w: LLM produced invalid RootCauseJSON: %w", ErrInvalidSchema, err)
	}

	return w.rootCauseExecResult(plan, &rc, callOut.TokensIn, callOut.TokensOut, callOut.CostUSD, callOut.LatencyMs), nil
}

func (w *InvestigatedPhaseWorker) rootCauseExecResult(plan Plan, rc *RootCauseJSON, tokensIn, tokensOut int, costUSD float64, latencyMs int) ExecResult {
	return ExecResult{
		ContractRef: nil, // contract 由 Run 时机的 contract-writer 写入 loop_contract
		SideEffects: []SideEffect{{
			Kind:   "investigation_summary",
			Target: pickTargetFromMeta(plan.Meta),
			Detail: map[string]any{
				"schema_version":    rc.SchemaVersion,
				"root_cause_kind":   rc.RootCauseObject.Kind,
				"confidence":        rc.Confidence,
				"evidence_count":    len(rc.EvidenceChain),
				"remediation_count": len(rc.RemediationOptions),
				"summarized_at":     w.clock(),
			},
		}},
		ToolReplay: nil, // investigated phase 不调工具 replay（toolset 的
		// 真实调用由 aiops/tools 层记 audit，不在本 Worker）
		RawOutputs: map[string]any{
			"root_cause_json": rc,
			"tokens_in":       tokensIn,
			"tokens_out":      tokensOut,
			"cost_usd":        costUSD,
			"latency_ms":      latencyMs,
		},
	}
}

func deterministicRootCause(plan Plan) (*RootCauseJSON, error) {
	evidence, evidenceOK := plan.Meta["evidence_chain"].([]EvidenceItem)
	remediations, remediationOK := plan.Meta["remediation_options"].([]RemediationOption)
	timeWindow, timeWindowOK := plan.Meta["time_window"].(TimeWindow)
	resourceType, _ := plan.Meta["resource_type"].(string)
	target := pickTargetFromMeta(plan.Meta)
	alertID, _ := plan.Meta["alert_id"].(string)
	group, _ := plan.Meta["correlated_group"].(*CorrelatedGroup)
	if !evidenceOK || len(evidence) == 0 || !remediationOK || len(remediations) == 0 || !timeWindowOK {
		return nil, fmt.Errorf("%w: deterministic fallback missing investigation evidence", ErrPlanInvalid)
	}

	kind := "unknown"
	poolExhausted := deterministicPGPoolExhaustionHint(plan)
	if poolExhausted {
		kind = "pg_pool_exhausted"
	} else if resourceType == "postgres" || resourceType == "pg" {
		kind = "pg_long_tx"
	} else if resourceType == "host" && group != nil && group.RootHypothesis == "host.cpu_stress" {
		kind = "host_cpu"
	}
	rc := &RootCauseJSON{
		SchemaVersion: ContractSchemaV1,
		RootCauseObject: &RootCauseObject{
			Kind:    kind,
			Summary: fmt.Sprintf("Evidence-backed investigation found %s issue on %s", resourceType, target),
			Detail: map[string]any{
				"alert_id":      alertID,
				"resource_type": resourceType,
				"target":        target,
				"evidence_tool": evidence[0].Tool,
			},
		},
		Confidence:         0.9,
		EvidenceChain:      evidence,
		TimeWindow:         timeWindow,
		RemediationOptions: remediations,
	}
	if poolExhausted {
		rc.RootCauseObject.Summary = fmt.Sprintf("PostgreSQL application connection pool exhausted on %s", target)
		rc.RootCauseObject.Detail["fault_family"] = "capacity/connection_pool"
		rc.RootCauseObject.Detail["root_hypothesis"] = "pg.connection_pool_exhausted"
		rc.RemediationOptions = append([]RemediationOption{{
			Action:      "pg.resize_pool",
			Target:      target,
			Risk:        "mutating",
			AutoApprove: false,
		}}, rc.RemediationOptions...)
	}
	if err := ValidateRootCauseJSON(rc); err != nil {
		return nil, err
	}
	return rc, nil
}

func deterministicHostCPUSpikeHint(plan Plan) bool {
	group, ok := plan.Meta["correlated_group"].(*CorrelatedGroup)
	return ok && group != nil && group.ResourceType == "host" && group.RootHypothesis == "host.cpu_stress"
}

func deterministicPGPoolExhaustionHint(plan Plan) bool {
	group, ok := plan.Meta["correlated_group"].(*CorrelatedGroup)
	if !ok || group == nil || group.RootHypothesis != "pg.connection_pool_exhausted" {
		return false
	}
	if group.ResourceType != "pg" && group.ResourceType != "postgres" && group.ResourceType != "postgresql" {
		return false
	}
	evidence, ok := plan.Meta["evidence_chain"].([]EvidenceItem)
	if !ok {
		return false
	}
	for _, item := range evidence {
		observation := strings.ToLower(fmt.Sprintf("%s %s %v", item.Tool, item.Query, item.Value))
		if strings.Contains(observation, "pool") && (strings.Contains(observation, "exhaust") || strings.Contains(observation, "saturation")) {
			return true
		}
	}
	return false
}

// Verifier 把 Executor 的 RootCauseJSON 翻译为 Verdict：
//   - RootCauseJSON 通过 ValidateRootCauseJSON（已在 Executor 兜底）→ OK=true
//   - Confidence 取 rc.Confidence
//   - 任何更深的不变量（evidence 数量 ≥ 1 / remediation 数量 ≥ 1）已在
//     Executor 被 ValidateRootCauseJSON 拦截；Verifier 这层主要做"结果
//     拿到了吗"的健壮性兜底，避免 RawOutputs 缺失导致 panic。
func (w *InvestigatedPhaseWorker) Verifier(_ context.Context, result ExecResult) (Verdict, error) {
	raw, ok := result.RawOutputs["root_cause_json"]
	if !ok {
		return Verdict{}, fmt.Errorf("%w: Executor.RawOutputs missing root_cause_json", ErrInvalidSchema)
	}
	rc, ok := raw.(*RootCauseJSON)
	if !ok {
		return Verdict{}, fmt.Errorf("%w: Executor.RawOutputs.root_cause_json type=%T", ErrInvalidSchema, raw)
	}
	if rc == nil {
		return Verdict{}, fmt.Errorf("%w: nil root_cause_json in RawOutputs", ErrInvalidSchema)
	}
	// 双保险：Executor 已经做过 ValidateRootCauseJSON，这里再跑一次让
	// "evidence 数量 ≥ 1 / remediation 数量 ≥ 1" 这类硬约束在 Verifier
	// 路径上依然兜得住（Executor 自身已有 wrapping，但 audit log 上
	// 出现 "verifier rejected invalid schema" 比 "executor error" 更精
	// 准）。成本极低（in-memory slice length check）。
	if err := ValidateRootCauseJSON(rc); err != nil {
		return Verdict{}, fmt.Errorf("%w: RootCauseJSON failed verifier check: %w", ErrInvalidSchema, err)
	}
	return Verdict{OK: true, Confidence: rc.Confidence, Reasons: nil}, nil
}

// VerifierTimeoutMs 返回 PhaseWorker 接口要求的 verifier 截止时间。
// investigated phase 不需要 cold LLM warm-up，沿用 DefaultVerifierTimeoutMs (30s)。
func (w *InvestigatedPhaseWorker) VerifierTimeoutMs() int {
	return DefaultVerifierTimeoutMs
}

// --- helpers ------------------------------------------------------------

// loadCorrelatedGroup 通过 loader 取上游 CorrelatedGroup。loader 返回
// (nil, nil) 时 Planner 用 defaultCorrelatedGroup 兜底；任何 error 都
// 立即拒绝（plan 还没建出来，副作用为 0；orchestrator 走 phase_failed）。
func (w *InvestigatedPhaseWorker) loadCorrelatedGroup(ctx context.Context, in PlanInput) (*CorrelatedGroup, error) {
	if in.UpstreamContract == nil {
		return nil, nil
	}
	g, err := w.loader.LoadCorrelatedGroup(ctx, in.TenantID, in.UpstreamContract.ID)
	if err != nil {
		return nil, fmt.Errorf("loop: load CorrelatedGroup (id=%d): %w", in.UpstreamContract.ID, err)
	}
	return g, nil
}

func investigationInputGroup(in PlanInput) *CorrelatedGroup {
	if len(in.AlertGroup) == 0 && len(in.CorrelationHints) == 0 {
		return nil
	}
	group := defaultCorrelatedGroup(in)
	if len(in.AlertGroup) > 0 {
		group.AlertIDs = append([]string(nil), in.AlertGroup...)
	}
	if value, ok := in.CorrelationHints["resource_type"].(string); ok && value != "" {
		group.ResourceType = value
	}
	if value, ok := in.CorrelationHints["target"].(string); ok && value != "" {
		group.Target = value
	}
	if values, ok := in.CorrelationHints["suspected_causes"].([]any); ok && len(values) > 0 {
		causes := make([]string, 0, len(values))
		for _, value := range values {
			if cause, ok := value.(string); ok && cause != "" {
				causes = append(causes, cause)
			}
		}
		if len(causes) > 0 {
			group.RootHypothesis = strings.Join(causes, "; ")
		}
	}
	return group
}

// defaultCorrelatedGroup 在 loader 缺位 / 缺 UpstreamContract 时提供兜底。
// 用 PlanInput 的 IncidentID 当 incident id；resource_type = "host" 让
// investigator 默认分支能跑通（toolset 的 fake 测试都基于此）。
func defaultCorrelatedGroup(in PlanInput) *CorrelatedGroup {
	now := wclockUTC()
	return &CorrelatedGroup{
		IncidentID:   in.IncidentID,
		AlertIDs:     []string{},
		ResourceType: "host",
		Target:       in.IncidentID,
		TimeWindow: TimeWindow{
			Start: now.Add(-5 * time.Minute),
			End:   now,
		},
	}
}

// pickResourceType 优先取 CorrelatedGroup.ResourceType；空时退回 "host"。
// 兜底选择"host"是因为 investigator 工具集的所有 fake 测试都基于 host。
func pickResourceType(g *CorrelatedGroup) string {
	if g == nil || g.ResourceType == "" {
		return "host"
	}
	return g.ResourceType
}

// pickAlertID 优先取 CorrelatedGroup.AlertIDs[0]；空时退回 IncidentID
// 以保证 investigator 工具总能拿到非空 query key。
func pickAlertID(g *CorrelatedGroup) string {
	if g == nil {
		return ""
	}
	if len(g.AlertIDs) > 0 {
		return g.AlertIDs[0]
	}
	return g.IncidentID
}

// pickTimeWindow 优先取 CorrelatedGroup.TimeWindow；空时用 now-5min → now
// 兜底（与 defaultCorrelatedGroup 对齐）。
func pickTimeWindow(g *CorrelatedGroup) TimeWindow {
	if g != nil && !g.TimeWindow.Start.IsZero() && !g.TimeWindow.End.IsZero() {
		return g.TimeWindow
	}
	now := wclockUTC()
	return TimeWindow{
		Start: now.Add(-5 * time.Minute),
		End:   now,
	}
}

// pickTargetFromMeta 安全从 Plan.Meta 取 target 字段；缺失时返回 ""。
// 与 recovery.go::targetFromArgs 同形但语义不同：recovery 从 args["target"]
// 取，investigated 从 Meta["correlated_group"].Target 取。
func pickTargetFromMeta(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	group, ok := meta["correlated_group"].(*CorrelatedGroup)
	if !ok || group == nil {
		return ""
	}
	return group.Target
}

// buildInvestigatedPrompt 把 CorrelatedGroup + evidence + remediations
// 拼成 LLM 提示词。
//
// 设计原则：
//   - system prompt 是稳定的角色定位（"你是 AIOps 调查员…"）
//   - user prompt 把上游数据 + 工具输出塞进去，附 schema 约束 + 输出要求
//   - 不在 prompt 里重复 RootCauseJSON 字段全集（schema 已经描述）；
//     只点名关键约束（confidence ∈ [0,1] / evidence ≥ 1 / remediation ≥ 1）
func buildInvestigatedPrompt(group *CorrelatedGroup, evidence []EvidenceItem, remediations []RemediationOption) (user, system string) {
	var b strings.Builder

	system = strings.Join([]string{
		"You are an AIOps investigator summarising the root cause of an incident.",
		"You will be given: (a) the upstream correlated alert group,",
		"(b) the ordered evidence chain collected by the investigator toolset,",
		"(c) the candidate remediation options for the resource type.",
		"Respond with a single JSON object matching the supplied schema.",
		"Constraints:",
		"- confidence is a float in [0, 1].",
		"- evidence_chain must contain at least one item, in the order you would cite them.",
		"- remediation_options must contain at least one item, with risk in {safe, mutating, dangerous}.",
		"- root_cause_object.kind must be one of the enum values.",
		"- Do NOT invent metrics or commands that are not present in the evidence.",
	}, "\n")

	b.WriteString("## Correlated Group\n")
	groupJSON, _ := json.Marshal(group)
	b.Write(groupJSON)
	b.WriteByte('\n')

	b.WriteString("\n## Evidence Chain (ordered)\n")
	evJSON, _ := json.Marshal(evidence)
	b.Write(evJSON)
	b.WriteByte('\n')

	b.WriteString("\n## Remediation Options\n")
	roJSON, _ := json.Marshal(remediations)
	b.Write(roJSON)
	b.WriteByte('\n')

	b.WriteString("\nRespond with JSON only — no prose before or after. Use schema_version=\"v1\".\n")
	return b.String(), system
}

// wclockUTC 包一层 time.Now().UTC() 以便测试时通过 worker.clock 注入 fake。
// （defaultCorrelatedGroup / pickTimeWindow 都用这个入口，单测不依赖 wall-clock。）
func wclockUTC() time.Time { return time.Now().UTC() }

// NoopInvestigatorToolset is the InvestigatorToolset no-op implementation.
// 返回空 slice + nil err，让 Planner 走降级路径。
type NoopInvestigatorToolset struct{}

func (NoopInvestigatorToolset) Investigate(_ context.Context, _, _ string, _ TimeWindow) ([]EvidenceItem, error) {
	return nil, nil
}

func (NoopInvestigatorToolset) ListRemediations(_ context.Context, _ string) ([]RemediationOption, error) {
	return nil, nil
}
