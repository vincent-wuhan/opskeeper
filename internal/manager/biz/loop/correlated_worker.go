// Package loop — correlated_worker.go
//
// Correlated phase 的 PhaseWorker 实现（zero-manual-ops-loop · path A ·
// llm-worker-integration 批次 2 / subagent 6）。
//
// Correlated phase 的职责（设计 §4.2 + 编排 spec "七阶段状态机"）：
//
//  1. 读取上游 DetectionEvent（detected phase 写入的 alert payload）
//  2. 通过 alertRepo.FindByLabelsetkey 拉同一 labelsetkey 窗口内的历史 alert
//  3. 把"当前 alert + 历史 alerts"打包成 LLM prompt，调用 LLMCaller 拿到
//     语义相似度判断 + 根因假设（输出符合 CorrelatedGroup schema）
//  4. Executor 持久化 CorrelatedGroup（本批只 console log；Day 5 集成时
//     由 main.go 接到 contract_repository 写 loop_contract）
//  5. Verifier 校验 schema + 必要字段（minItems、confidence 区间）
//
// 设计依据：
//   - OpenSpec delta spec: closed-loop-orchestrator §"correlated"
//   - Design §3.2（LLMCaller 共享抽象）+ §4.2（CorrelatedGroup schema）
//   - phase_worker.go::PhaseWorker 三段式契约
//
// 不引入 monorepo 跨域 import：
//   - AlertRepository / CurrentDetectionEventLoader 是本包内定义的
//     narrow interface；concrete 实现由 cmd/main.go 在 Day 5 集成时
//     注入（aiops/alerts 适配成对应接口）。
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// CorrelatedGroup 是 correlated phase 的合约输出（设计 §4.2）。
//
// JSON schema 摘要（详见 Design Doc §4.2 完整定义）：
//
//	required: incident_id, alert_ids, root_hypothesis
//	optional: confidence (0-1)
//
// incident_id 与本批 incident 关联；alert_ids 至少 1 项；root_hypothesis
// 是 LLM 给的简短根因假设（一句话）。confidence 是 LLM 自我报告的可信度。
type CorrelatedGroup struct {
	// IncidentID 是本批 incident 的唯一标识。
	IncidentID string `json:"incident_id"`

	// AlertIDs 是被合并到本 group 的 alert id 列表；至少 1 项。
	AlertIDs []string `json:"alert_ids"`

	// RootHypothesis 是 LLM 给的根因假设（1-2 句话）。
	RootHypothesis string `json:"root_hypothesis"`

	// Confidence 是 LLM 报告的语义相似度可信度，区间 [0, 1]；0 = 完全不相关，
	// 1 = 完全同一根因。可选字段；缺省视为 0。
	Confidence float64 `json:"confidence,omitempty"`

	// ResourceType 是被 group 命中的资源类型（host / pg / redis / k8s /
	// app 之一）；investigated phase 据此挑 investigator 工具子集。
	// 可选字段；缺省时 investigated worker 退回 host 兜底。
	ResourceType string `json:"resource_type,omitempty"`

	// Target 是被 group 命中的资源定位符（如 host:i-0abc123）；investigated
	// phase 用作 SideEffect.Target。可选字段。
	Target string `json:"target,omitempty"`

	// TimeWindow 是 group 命中的时间窗（与首条 DetectionEvent 的
	// detected_at 对齐）；investigated phase 据此设 InvestigatorToolset 的
	// 样本区间。可选字段；缺省时 investigated worker 用 PlanInput 的兜底。
	TimeWindow TimeWindow `json:"time_window,omitempty"`
}

// DetectionEvent 是 detected phase 写入的 alert payload 的最小本地形态。
//
// 为什么在 loop 包内重声明：
//   - monorepo 约束禁止 internal/<domain> 跨域 import；
//   - AlertRepository 接口需要 DetectionEvent 参数；
//   - Day 5 集成时由 adapter 把 aiops/alerts.DetectionEvent 与本类型互转。
//
// 字段对齐 Design §4.1 DetectionEvent schema 的最小子集（alert_id /
// severity / resource / labelsetkey / detected_at）；raw_payload 由
// 相关方按需扩展。
type DetectionEvent struct {
	// AlertID 是 alert 在 alertmanager 内的唯一 id。
	AlertID string `json:"alert_id"`

	// Severity 是 alert 严重等级（info / warning / error / critical）。
	Severity string `json:"severity"`

	// Resource 是 resource type: pg / redis / k8s / host / app。
	Resource string `json:"resource"`

	// LabelSetKey 是 alert 标签集合的规范化 key（用于合并判定）。
	LabelSetKey string `json:"labelsetkey"`

	// DetectedAt 是 alert 触发时间。
	DetectedAt time.Time `json:"detected_at"`

	// Summary 是 1 句话 alert 摘要（便于 LLM 快速理解）。
	Summary string `json:"summary,omitempty"`
}

// AlertRepository 是 alert 持久化的 narrow interface。
//
// 接口对齐 aiops/alerts.AlertRepository 但在 loop 包内重声明以避免
// 跨域 import；Day 5 集成时由 adapter 把两者接起来（同一 in-memory 实现
// 同时实现两个接口，所以不需要 proxy）。
//
// FindByLabelsetkey 返回自 since 以来同 labelsetkey 的历史 alert 列表；
// 用于 correlated phase 判断"当前 alert 是否与历史 alert 共享根因"。
type AlertRepository interface {
	FindByLabelsetkey(ctx context.Context, key string, since time.Time) ([]DetectionEvent, error)
}

// CurrentDetectionEventLoader 是从 PlanInput 解析出当前 DetectionEvent
// 的 narrow interface。Planner 调它拿到本批 incident 的"当前 alert"。
//
// Day 5 集成时 main.go 注入一个 contractRepo-backed 实现（从 loop_contract
// 表读 DetectionEvent JSON）。本批测试通过 WithCurrentDetectionEventLoader
// 注入 fake。
type CurrentDetectionEventLoader interface {
	Load(ctx context.Context, in PlanInput) (DetectionEvent, error)
}

// CorrelatedPhaseWorker 是 PhaseWorker 的 correlated phase 实现。
//
// 构造由 NewCorrelatedPhaseWorker 完成；Plan / ExecResult / Verdict
// 严格遵循 phase_worker.go::PhaseWorker 三段式契约。
type CorrelatedPhaseWorker struct {
	// BasePhaseWorker 继承 Phase + VerifierMs 字段 + 默认方法。
	BasePhaseWorker

	// caller 是 LLMCaller（必须）；Planner 通过它请求根因假设。
	caller LLMCaller

	// alertRepo 是 alert 持久化 narrow interface（必须）。
	alertRepo AlertRepository

	// currentEventLoader 从 PlanInput 解析出当前 DetectionEvent；
	// 缺省时 Planner 报错（要求显式注入）。
	currentEventLoader CurrentDetectionEventLoader

	// historicalWindow 是历史 alert 回溯窗口（默认 24h）。
	historicalWindow time.Duration

	// clock 是时间源；nil = time.Now。tests 注入 fake 让 schema 校验
	// 时间相关字段时不需要 wall-clock。
	clock func() time.Time

	// log 是 slog.Logger；nil = slog.Default()。
	log *slog.Logger
}

// CorrelatedPhaseWorkerOption 是 NewCorrelatedPhaseWorker 的可选项。
type CorrelatedPhaseWorkerOption func(*CorrelatedPhaseWorker)

// WithCorrelatedHistoricalWindow 覆盖历史 alert 回溯窗口。
func WithCorrelatedHistoricalWindow(d time.Duration) CorrelatedPhaseWorkerOption {
	return func(w *CorrelatedPhaseWorker) { w.historicalWindow = d }
}

// WithCorrelatedClock 注入 fake clock（测试用）。
func WithCorrelatedClock(clock func() time.Time) CorrelatedPhaseWorkerOption {
	return func(w *CorrelatedPhaseWorker) { w.clock = clock }
}

// WithCorrelatedLogger 注入 slog logger。
func WithCorrelatedLogger(log *slog.Logger) CorrelatedPhaseWorkerOption {
	return func(w *CorrelatedPhaseWorker) { w.log = log }
}

// WithCurrentDetectionEventLoader 注入当前 DetectionEvent 解析器。
//
// Day 5 集成时由 main.go 注入 contractRepo-backed 实现；测试用 fake 实现。
// 不注入时 Planner 在 PlanInput 缺上游 contract 时 fail-fast。
func WithCurrentDetectionEventLoader(loader CurrentDetectionEventLoader) CorrelatedPhaseWorkerOption {
	return func(w *CorrelatedPhaseWorker) { w.currentEventLoader = loader }
}

// NewCorrelatedPhaseWorker 是生产构造器。
//
// caller / alertRepo 必填（Planner 阶段就要用到）；其余选项通过
// CorrelatedPhaseWorkerOption 注入。
func NewCorrelatedPhaseWorker(caller LLMCaller, alertRepo AlertRepository, opts ...CorrelatedPhaseWorkerOption) (*CorrelatedPhaseWorker, error) {
	if caller == nil {
		return nil, errors.New("loop: CorrelatedPhaseWorker.caller is required")
	}
	if alertRepo == nil {
		return nil, errors.New("loop: CorrelatedPhaseWorker.alertRepo is required")
	}
	w := &CorrelatedPhaseWorker{
		BasePhaseWorker:  BasePhaseWorker{PhaseRef: PhaseCorrelated, VerifierMs: 60_000},
		caller:           caller,
		alertRepo:        alertRepo,
		historicalWindow: 24 * time.Hour,
		clock:            func() time.Time { return time.Now().UTC() },
		log:              slog.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	return w, nil
}

// correlatedOutputSchema 是 CorrelatedGroup 的 JSON-Schema 描述，传给
// LLMCaller.OutputSchema。LLMCaller 用它做结构化输出校验；本 Worker 在
// Verifier 阶段还会做语义校验（minItems、confidence 区间）。
//
// 注：loop 包内 hand-rolled schema validator 不支持 minItems / minimum /
// maximum，所以这部分约束在 Verifier 阶段手工补齐（ValidateCorrelatedGroup）。
// incident_id 允许模型省略并由 Executor 用可信 PlanInput 回填；Verifier
// 仍强制最终 CorrelatedGroup 非空，避免小模型漏字段导致闭环中断。
const correlatedOutputSchema = `{
  "type": "object",
  "required": ["alert_ids", "root_hypothesis"],
  "properties": {
    "incident_id":      {"type": "string"},
    "alert_ids":        {"type": "array", "items": {"type": "string"}},
    "root_hypothesis":  {"type": "string"},
    "confidence":       {"type": "number"}
  }
}`

// Planner 把"上游 DetectionEvent + 同 labelsetkey 历史 alerts"打包
// 成 LLM prompt，并描述下一步要执行的 LLM call。
//
// 步骤：
//  1. 通过 currentEventLoader 读出当前 alert（detected phase 写入）；
//     缺 contract / contract 解析失败时直接拒（fail-fast）。
//  2. 通过 alertRepo.FindByLabelsetkey 拉 since=now-historicalWindow 以来
//     同 labelsetkey 的历史 alert。
//  3. 拼 system + user prompt：包含 incident_id / labelsetkey / 当前 alert
//     摘要 / 历史 alert 列表（按 detected_at 倒序，最多 20 条），让 LLM
//     输出 CorrelatedGroup JSON。
//  4. Plan.Steps 唯一一项是 llm_call，把 prompt + schema 装到 Args。
func (w *CorrelatedPhaseWorker) Planner(ctx context.Context, in PlanInput) (Plan, error) {
	if in.IncidentID == "" {
		return Plan{}, fmt.Errorf("%w: correlated planner: incident_id missing", ErrPlanInvalid)
	}

	loader := w.currentEventLoader
	if loader == nil {
		return Plan{}, fmt.Errorf("%w: current DetectionEvent loader not configured", ErrPlanInvalid)
	}
	current, err := loader.Load(ctx, in)
	if err != nil {
		return Plan{}, fmt.Errorf("loop: correlated planner load current: %w", err)
	}
	if current.LabelSetKey == "" {
		return Plan{}, fmt.Errorf("%w: current DetectionEvent missing labelsetkey", ErrPlanInvalid)
	}

	since := w.clock().Add(-w.historicalWindow)
	historical, err := w.alertRepo.FindByLabelsetkey(ctx, current.LabelSetKey, since)
	if err != nil {
		return Plan{}, fmt.Errorf("loop: correlated planner FindByLabelsetkey: %w", err)
	}
	// 排序：detected_at 倒序（最新在前），方便 LLM 优先看最近事件。
	sort.SliceStable(historical, func(i, j int) bool {
		return historical[i].DetectedAt.After(historical[j].DetectedAt)
	})
	// 截断：最多 20 条历史 alert，避免 prompt 过长。
	if len(historical) > 20 {
		historical = historical[:20]
	}

	systemPrompt := w.renderSystemPrompt()
	userPrompt := w.renderUserPrompt(in.IncidentID, current, historical)

	return Plan{
		Steps: []PlanStep{{
			Kind:   "llm_call",
			Target: "correlated",
			Args: map[string]any{
				"phase":            string(PhaseCorrelated),
				"incident_id":      in.IncidentID,
				"labelsetkey":      current.LabelSetKey,
				"current_alert_id": current.AlertID,
				"system_prompt":    systemPrompt,
				"user_prompt":      userPrompt,
				"output_schema":    correlatedOutputSchema,
				"historical_count": len(historical),
			},
		}},
		EstimatedCost: CostEstimate{USD: 0, Tokens: 0},
		Meta: map[string]any{
			"labelsetkey":      current.LabelSetKey,
			"current_alert_id": current.AlertID,
			"historical_count": len(historical),
		},
	}, nil
}

// renderSystemPrompt 是 system-turn 消息：定义 LLM 的角色和输出格式约束。
func (w *CorrelatedPhaseWorker) renderSystemPrompt() string {
	return strings.Join([]string{
		"You are the alert-correlation engine of an SRE closed-loop system.",
		"Your task: given one new alert and a history of recent alerts sharing the same label set key,",
		"decide whether the new alert belongs to the same incident as the historical alerts.",
		"Return a JSON object that matches the contract schema exactly.",
		"Do not add prose outside the JSON. Do not nest the JSON inside markdown fences.",
	}, " ")
}

// renderUserPrompt 是 user-turn 消息：包含 incident_id / labelsetkey /
// 当前 alert 详情 / 历史 alert 列表（最多 20 条）。
func (w *CorrelatedPhaseWorker) renderUserPrompt(incidentID string, current DetectionEvent, historical []DetectionEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "incident_id: %s\n", incidentID)
	fmt.Fprintf(&b, "labelsetkey: %s\n", current.LabelSetKey)
	b.WriteString("current_alert:\n")
	fmt.Fprintf(&b, "  alert_id: %s\n", current.AlertID)
	fmt.Fprintf(&b, "  severity: %s\n", current.Severity)
	fmt.Fprintf(&b, "  resource: %s\n", current.Resource)
	fmt.Fprintf(&b, "  detected_at: %s\n", current.DetectedAt.UTC().Format(time.RFC3339))
	if current.Summary != "" {
		fmt.Fprintf(&b, "  summary: %s\n", current.Summary)
	}
	b.WriteString("historical_alerts:\n")
	if len(historical) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for i, h := range historical {
			fmt.Fprintf(&b, "  - [%d] alert_id=%s severity=%s resource=%s detected_at=%s",
				i, h.AlertID, h.Severity, h.Resource, h.DetectedAt.UTC().Format(time.RFC3339))
			if h.Summary != "" {
				fmt.Fprintf(&b, " summary=%q", h.Summary)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nDecide: do all of these alerts share the same root cause?\n")
	b.WriteString("If yes, list all alert_ids (including current) in alert_ids and give a one-sentence root_hypothesis.\n")
	b.WriteString("If no, set alert_ids=[current.alert_id] and explain in root_hypothesis why you excluded the historical ones.\n")
	b.WriteString("Set confidence in [0, 1] reflecting how sure you are about the root cause hypothesis.\n")
	return b.String()
}

// Executor 调用 LLMCaller 跑 PlanStep 描述的 LLM call，把 CorrelatedGroup
// 装到 ExecResult.RawOutputs["correlated_group"] + SideEffects 写一条
// phase_contract_written 事件（detail 含 group 字段；持久化由
// orchestrator 的 contract writer 在 Day 5 集成时完成）。
//
// 失败语义：
//   - LLM 返回的 JSON 缺 alert_ids 字段 → ErrSchemaInvalid（来自 LLMCaller）
//   - LLM 调用连续 transient 失败 → 透传 wrapped error
func (w *CorrelatedPhaseWorker) Executor(ctx context.Context, plan Plan) (ExecResult, error) {
	if len(plan.Steps) == 0 {
		return ExecResult{}, fmt.Errorf("%w: correlated executor: empty steps", ErrPlanInvalid)
	}
	step := plan.Steps[0]

	systemPrompt, _ := step.Args["system_prompt"].(string)
	userPrompt, _ := step.Args["user_prompt"].(string)
	schema, _ := step.Args["output_schema"].(string)
	labelsetkey, _ := step.Args["labelsetkey"].(string)
	currentAlertID, _ := step.Args["current_alert_id"].(string)
	incidentID, _ := step.Args["incident_id"].(string)

	out, err := w.caller.Call(ctx, CallInput{
		Phase:        PhaseCorrelated,
		SystemPrompt: systemPrompt,
		Prompt:       userPrompt,
		OutputSchema: schema,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("loop: correlated llm call: %w", err)
	}

	var group CorrelatedGroup
	if err := json.Unmarshal(out.Raw, &group); err != nil {
		return ExecResult{}, fmt.Errorf("%w: correlated unmarshal LLM output: %w", ErrInvalidSchema, err)
	}

	// 兜底：若 LLM 漏填 incident_id，用上游 incident_id 补上（LLM 自填易
	// 出错；plan 输入是 single source of truth）。
	if group.IncidentID == "" && incidentID != "" {
		group.IncidentID = incidentID
	}
	// 兜底：若 LLM 把 alert_ids 漏填当前 alert，至少把 currentAlertID
	// 放进 alert_ids，确保 Verifier 不会因 alert_ids 空数组而拒绝。
	if len(group.AlertIDs) == 0 && currentAlertID != "" {
		group.AlertIDs = []string{currentAlertID}
	}
	payloadJSON, err := json.Marshal(group)
	if err != nil {
		return ExecResult{}, fmt.Errorf("loop: marshal CorrelatedGroup contract: %w", err)
	}

	return ExecResult{
		SideEffects: []SideEffect{{
			Kind:   "phase_contract_written",
			Target: group.IncidentID,
			Detail: map[string]any{
				"contract_type":   "CorrelatedGroup",
				"alert_ids":       group.AlertIDs,
				"root_hypothesis": group.RootHypothesis,
				"confidence":      group.Confidence,
				"labelsetkey":     labelsetkey,
				"payload":         string(payloadJSON),
			},
		}},
		ToolReplay: []ToolReplayEntry{},
		RawOutputs: map[string]any{
			"correlated_group": &group,
			"raw_llm_output":   out.Raw,
		},
	}, nil
}

// Verifier 验证 Executor 产出的 CorrelatedGroup 满足 schema + 必要字段：
//   - incident_id / root_hypothesis 非空
//   - alert_ids 至少 1 项
//   - confidence 区间 [0, 1]（LLMCaller schema validator 不支持 numeric bounds）
//
// 失败时返回 Verdict{OK:false} + Reasons；orchestrator 写到 phase_failed 事件。
func (w *CorrelatedPhaseWorker) Verifier(_ context.Context, result ExecResult) (Verdict, error) {
	raw, ok := result.RawOutputs["correlated_group"]
	if !ok {
		return Verdict{}, fmt.Errorf("%w: Executor.RawOutputs missing correlated_group", ErrInvalidSchema)
	}
	group, ok := raw.(*CorrelatedGroup)
	if !ok {
		return Verdict{}, fmt.Errorf("%w: Executor.RawOutputs.correlated_group type=%T", ErrInvalidSchema, raw)
	}
	if err := ValidateCorrelatedGroup(group); err != nil {
		return Verdict{OK: false, Confidence: 0, Reasons: []string{err.Error()}}, nil
	}
	// 通过校验 → OK=true；confidence 直接采用 LLM 自报。
	return Verdict{OK: true, Confidence: group.Confidence}, nil
}

// VerifierTimeoutMs 固定返回 60_000（correlated 阶段 LLM cold start
// 偏长；与 recovered 阶段对齐）。
func (w *CorrelatedPhaseWorker) VerifierTimeoutMs() int {
	return 60_000
}

// --- validation --------------------------------------------------------

// ErrCorrelatedSchemaInvalid 是 CorrelatedGroup 验证失败的 sentinel。
// Validator 包成 %w，Verifier / 外部消费者用 errors.Is 判定。
var ErrCorrelatedSchemaInvalid = errors.New("loop: correlated group schema invalid")

// ValidateCorrelatedGroup 校验 CorrelatedGroup 满足 schema + 必要字段：
//   - incident_id 非空
//   - alert_ids 至少 1 项
//   - root_hypothesis 非空
//   - confidence ∈ [0, 1]（可选字段；存在时校验）
//
// 缺 alert_ids 字段 / alert_ids 为空数组 / 字段类型错误均返回错误。
func ValidateCorrelatedGroup(g *CorrelatedGroup) error {
	if g == nil {
		return fmt.Errorf("%w: nil CorrelatedGroup", ErrCorrelatedSchemaInvalid)
	}
	if g.IncidentID == "" {
		return fmt.Errorf("%w: incident_id missing", ErrCorrelatedSchemaInvalid)
	}
	if len(g.AlertIDs) < 1 {
		return fmt.Errorf("%w: alert_ids empty (need >= 1)", ErrCorrelatedSchemaInvalid)
	}
	for i, id := range g.AlertIDs {
		if id == "" {
			return fmt.Errorf("%w: alert_ids[%d] is empty string", ErrCorrelatedSchemaInvalid, i)
		}
	}
	if g.RootHypothesis == "" {
		return fmt.Errorf("%w: root_hypothesis missing", ErrCorrelatedSchemaInvalid)
	}
	if g.Confidence < 0 || g.Confidence > 1 {
		return fmt.Errorf("%w: confidence=%v out of [0,1]", ErrCorrelatedSchemaInvalid, g.Confidence)
	}
	return nil
}

// NoopAlertRepository is the AlertRepository no-op implementation.
// 留 wire-up 默认（PhaseWorkerDeps.AlertRepo 未传时由 phase_workers.go 兜底）。
type NoopAlertRepository struct{}

func (NoopAlertRepository) FindByLabelsetkey(_ context.Context, _ string, _ time.Time) ([]DetectionEvent, error) {
	return nil, nil
}
