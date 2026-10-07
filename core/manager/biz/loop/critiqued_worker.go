// Package loop — critiqued_worker.go
//
// critiqued phase 的 PhaseWorker 实现（LLM-driven，路径 A 集成批次 2）。
//
// critiqued phase 的职责：
//
//  1. 读取上游 RootCauseJSON（investigated phase 写入），把根因 / 证据链 /
//     修复选项 拼成 prompt。
//  2. 调用 LLMCaller.Call 让 LLM 给 RootCauseJSON 打 3 个维度的分：
//     - accuracy       根因是否命中真实根因
//     - completeness   evidence_chain 是否足以支撑结论
//     - actionability  remediation_options 是否可执行
//  3. Verifier 把 3 维都校 [0, 1] 后写 Verdict；通过则推进到 approved，
//     失败则触发 phase_failed（orchestrator 写 loop_event）。
//
// 设计依据：
//   - OpenSpec delta spec：closed-loop-orchestrator §"LLM-driven worker"
//   - 共享 LLMCaller 抽象（llm_caller.go）：schema 验证 / retry / 成本
//     埋点 都通过 LLMCaller 自动获得，本 worker 只负责 prompt + 字段语义。
//   - 不引入 monorepo 跨域 import：RootCauseRefLoader 是本包内定义的
//     narrow interface，concrete 实现由 cmd/main.go 在集成时注入。
//
// 与已有 CritiqueScore（contract.go）的关系：contract.go 的 CritiqueScore
// 是跨 phase 合约（SchemaVersion / Verdict / Score / Reasons ...），下游
// approved / postmortem 消费；本文件定义的 CritiqueDimensions 是 LLM 的
// 3 维评分，worker 内部使用。两套类型并存是路径 A 集成期的中间状态；后续
// batch 会把 CritiqueDimensions 合并进 CritiqueScore.Fields 或单独持久化。
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// CritiqueDimensions 是 LLM 对 RootCauseJSON 的三维评分输出，区间 [0, 1]。
//
// JSON 字段：
//   - accuracy       0-1，根因与真实根因的吻合度
//   - completeness   0-1，evidence_chain 完整性
//   - actionability  0-1，remediation_options 可执行度
//
// 字段名直接对应 LLM 输出 JSON 的 key；schema 校验在 LLMCaller.Call
// 内部完成（schema 包含 minimum=0, maximum=1），本 worker 仍做一道
// 冗余校验（Verifier）以防调用方传非 schema 校验路径。
type CritiqueDimensions struct {
	Accuracy      float64 `json:"accuracy"`
	Completeness  float64 `json:"completeness"`
	Actionability float64 `json:"actionability"`
}

// critiqueDimensionsJSONSchema 是 LLMCaller 用的 JSON Schema 契约。
// minimum/maximum 字段由 llm_caller_schema.go 校验（路径 A 集成批次 2
// 在该 validator 加上 numeric bounds 支持）。
const critiqueDimensionsJSONSchema = `{
  "type": "object",
  "required": ["accuracy", "completeness", "actionability"],
  "properties": {
    "accuracy":       {"type": "number", "minimum": 0, "maximum": 1},
    "completeness":   {"type": "number", "minimum": 0, "maximum": 1},
    "actionability":  {"type": "number", "minimum": 0, "maximum": 1}
  }
}`

// critiqueSystemPrompt 是给 LLM 的 system-turn 指令。
const critiqueSystemPrompt = "You are a senior SRE reviewing an auto-generated root cause analysis. Score it on three dimensions in [0, 1]: accuracy (root cause matches reality), completeness (evidence chain is sufficient), actionability (remediation options are executable). Respond with only the JSON object."

// CritiquedPhaseWorker 是 PhaseWorker 的 critiqued phase 实现。
//
// 构造由 NewCritiquedPhaseWorker 完成；Planner 负责读上游 + LLM 评审，
// Executor 占位持久化，Verifier 校验 3 维落在 [0, 1]。
type CritiquedPhaseWorker struct {
	BasePhaseWorker

	// caller 是共享 LLMCaller（llm_caller.go）。Planner 用它调 LLM。
	// nil = Planner 立即拒绝。
	caller LLMCaller

	// upstreamLoader 加载上游 RootCauseJSON。
	// nil = Planner 拒绝（与 recovered 的 ApprovedDecisionLoader 同策略）。
	upstreamLoader RootCauseRefLoader

	// clock 时间源。nil = time.Now。
	clock func() time.Time

	// log 结构化日志。nil = slog.Default()。
	log *slog.Logger
}

// CritiqueOption 是 CritiquedPhaseWorker 的 functional option。
// 用专用名（不复用 llm_caller.Option）是因为 llm_caller.Option 签名为
// func(*llmCaller)，类型不同但同包同名会冲突，故加 Critique 前缀。
type CritiqueOption func(*CritiquedPhaseWorker)

// WithCritiqueClock 注入时间源；测试需要确定性时间戳时使用。
func WithCritiqueClock(fn func() time.Time) CritiqueOption {
	return func(w *CritiquedPhaseWorker) { w.clock = fn }
}

// WithCritiqueLogger 注入 slog.Logger；nil 走 slog.Default().
func WithCritiqueLogger(log *slog.Logger) CritiqueOption {
	return func(w *CritiquedPhaseWorker) { w.log = log }
}

// WithCritiqueUpstreamLoader 注入上游 RootCauseJSON 加载器。
// 不注入 = Planner 拒绝（fail-fast，避免无上游就乱调 LLM）。
func WithCritiqueUpstreamLoader(loader RootCauseRefLoader) CritiqueOption {
	return func(w *CritiquedPhaseWorker) { w.upstreamLoader = loader }
}

// NewCritiquedPhaseWorker 是生产构造器。
// caller 必填；其他依赖通过 CritiqueOption 注入。
func NewCritiquedPhaseWorker(caller LLMCaller, opts ...CritiqueOption) *CritiquedPhaseWorker {
	w := &CritiquedPhaseWorker{
		BasePhaseWorker: BasePhaseWorker{
			PhaseRef:   PhaseCritiqued,
			VerifierMs: critiquedPhaseVerifierTimeoutMs,
		},
		caller: caller,
		clock:  func() time.Time { return time.Now().UTC() },
		log:    slog.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	return w
}

// critiquedPhaseVerifierTimeoutMs 是该 phase 的 Verifier deadline。
// 60s 覆盖默认 30s：LLM 重试 + schema 校验的最坏路径在该上限内。
const critiquedPhaseVerifierTimeoutMs = 60_000

// critiqueRawKey 是 Plan.Meta 里放 LLM 原始 JSON 的 key（Planner→Verifier 链路用）。
const critiqueRawKey = "critique_raw_json"

// critiqueDimensionsKey 是 Plan.Meta / ExecResult.RawOutputs 里放
// 已解析 CritiqueDimensions 的 key。
const critiqueDimensionsKey = "critique_dimensions"

// Planner 读上游 RootCauseJSON，渲染 prompt，调 LLMCaller.Call，把 raw JSON
// 和已解析的 CritiqueDimensions 装到 Plan.Meta 供后续阶段消费。
//
// 错误语义：
//   - caller == nil                  → ErrPlanInvalid（programmer error）
//   - in.UpstreamContract == nil      → ErrPlanInvalid（critiqued 必须有上游）
//   - upstreamLoader.LoadRootCause != nil  → wrapped（DB / IO 失败）
//   - upstreamLoader returns nil      → ErrPlanInvalid（无上游数据）
//   - LLMCaller.Call returns err     → wrapped（覆盖 timeout / schema / API 错误）
//   - LLMCaller.Call 返回 raw 反序列化错误 → wrapped ErrSchemaInvalid（兜底）
func (w *CritiquedPhaseWorker) Planner(ctx context.Context, in PlanInput) (Plan, error) {
	if w.caller == nil {
		return Plan{}, fmt.Errorf("%w: critiqued planner: LLMCaller not configured", ErrPlanInvalid)
	}
	if in.UpstreamContract == nil {
		return Plan{}, fmt.Errorf("%w: critiqued planner: upstream RootCauseJSON contract missing", ErrPlanInvalid)
	}
	if w.upstreamLoader == nil {
		return Plan{}, fmt.Errorf("%w: critiqued planner: RootCauseRefLoader not configured", ErrPlanInvalid)
	}

	rc, err := w.upstreamLoader.LoadRootCause(ctx, in.UpstreamContract.ID)
	if err != nil {
		return Plan{}, fmt.Errorf("loop: critiqued planner load RootCauseJSON: %w", err)
	}
	if rc == nil {
		return Plan{}, fmt.Errorf("%w: critiqued planner: no RootCauseJSON for contract id=%d", ErrPlanInvalid, in.UpstreamContract.ID)
	}
	if verr := ValidateRootCauseJSON(rc); verr != nil {
		return Plan{}, fmt.Errorf("%w: critiqued planner: %v", ErrPlanInvalid, verr)
	}

	userPrompt := renderCritiqueUserPrompt(in.IncidentID, rc)

	out, err := w.caller.Call(ctx, CallInput{
		Phase:        PhaseCritiqued,
		Prompt:       userPrompt,
		SystemPrompt: critiqueSystemPrompt,
		OutputSchema: critiqueDimensionsJSONSchema,
		TimeoutMs:    critiquedPhaseVerifierTimeoutMs,
	})
	if err != nil {
		// LLMCaller 已经 wrap 过 ErrSchemaInvalid / ErrSchemaUnparseable；
		// 这里原样透传错误链让 Verifier 用 errors.Is 判别。
		return Plan{}, fmt.Errorf("loop: critiqued planner LLM call: %w", err)
	}

	// 兜底反序列化：LLMCaller.Call 内部已做 schema 校验 + JSON 提取，
	// 这里的 unmarshal 失败只发生在 schema 通过但 JSON 字节被外部修改
	// 的极端情况。仍保留 err 路径以便回溯。
	var dims CritiqueDimensions
	if uerr := json.Unmarshal(out.Raw, &dims); uerr != nil {
		return Plan{}, fmt.Errorf("%w: critiqued planner unmarshal: %v (raw=%s)", ErrSchemaInvalid, uerr, string(out.Raw))
	}

	return Plan{
		Steps: []PlanStep{{
			Kind:   "llm_call",
			Target: "critique_llm",
			Args: map[string]any{
				"upstream_contract_id": in.UpstreamContract.ID,
				"upstream_schema":      in.UpstreamContract.SchemaVersion,
				"critique_schema":      critiqueDimensionsJSONSchema,
				"tokens_in":            out.TokensIn,
				"tokens_out":           out.TokensOut,
				"cost_usd":             out.CostUSD,
				"latency_ms":           out.LatencyMs,
				"model_timestamp_unix": w.clock().Unix(),
			},
		}},
		EstimatedCost: CostEstimate{
			USD:    out.CostUSD,
			Tokens: out.TokensIn + out.TokensOut,
		},
		Meta: map[string]any{
			critiqueRawKey:        string(out.Raw),
			critiqueDimensionsKey: &dims,
			"tokens_in":           out.TokensIn,
			"tokens_out":          out.TokensOut,
			"cost_usd":            out.CostUSD,
			"latency_ms":          out.LatencyMs,
		},
	}, nil
}

// Executor 占位持久化：从 Plan.Meta 取出 CritiqueDimensions，包成
// ExecResult 返回。DB 持久化在 path A 集成期后续 batch 接到
// loop_contract repository（与 recovered / approved worker 一致）。
//
// 真实落地时（不在本 batch 范围）：
//   - 把 CritiqueDimensions 序列化成 loop_contract row（type="CritiqueScore"）
//   - 把上游 RootCauseJSON id + 三个维度写进 loop_event_log
//   - 用 Plan.Steps[0].Args 的 tokens / cost / latency 写 audit log
//
// 本 batch 只确保 orchestrator 拿到的 ExecResult 结构正确；持久化逻辑
// 待 ContractRepository 注入后由 cmd/main.go 在集成时补上 adapter。
func (w *CritiquedPhaseWorker) Executor(_ context.Context, plan Plan) (ExecResult, error) {
	rawMeta, ok := plan.Meta[critiqueRawKey]
	if !ok {
		return ExecResult{}, fmt.Errorf("%w: critiqued executor: Plan.Meta missing %q", ErrPlanInvalid, critiqueRawKey)
	}
	raw, ok := rawMeta.(string)
	if !ok {
		return ExecResult{}, fmt.Errorf("%w: critiqued executor: Plan.Meta[%q] type=%T", ErrPlanInvalid, critiqueRawKey, rawMeta)
	}

	dimsVal, ok := plan.Meta[critiqueDimensionsKey]
	if !ok {
		return ExecResult{}, fmt.Errorf("%w: critiqued executor: Plan.Meta missing %q", ErrPlanInvalid, critiqueDimensionsKey)
	}
	dims, ok := dimsVal.(*CritiqueDimensions)
	if !ok {
		return ExecResult{}, fmt.Errorf("%w: critiqued executor: Plan.Meta[%q] type=%T", ErrPlanInvalid, critiqueDimensionsKey, dimsVal)
	}

	// Schema 重新校验一次：Planner 已经在 LLMCaller 里跑过，但 Executor
	// 必须独立判 schema，否则 Plan.Meta 被外部篡改时无法兜底。
	if verr := validateCritiqueDimensionsRaw(raw); verr != nil {
		return ExecResult{}, fmt.Errorf("%w: critiqued executor re-validate: %v", ErrSchemaInvalid, verr)
	}

	// 占位持久化：把 CritiqueDimensions + 元数据装进 SideEffect.Detail，
	// 等 ContractRepository adapter 落地时直接把 Detail 当作 row payload。
	side := SideEffect{
		Kind:   "phase_contract_written",
		Target: "CritiqueScore",
		Detail: map[string]any{
			"accuracy":      dims.Accuracy,
			"completeness":  dims.Completeness,
			"actionability": dims.Actionability,
			"raw_json":      raw,
		},
	}

	return ExecResult{
		SideEffects: []SideEffect{side},
		RawOutputs: map[string]any{
			critiqueDimensionsKey: dims,
			critiqueRawKey:        raw,
		},
	}, nil
}

// Verifier 把 CritiqueDimensions 落到 [0, 1] 校验，并返回 Verdict：
//   - 全部 3 维在 [0, 1] → OK=true, Confidence=mean(dims), Reasons=nil
//   - 任一维超出         → OK=false, Reasons 列出越界字段
//   - 拿不到 dims         → OK=false, Reasons=["missing_critique_dimensions"]
func (w *CritiquedPhaseWorker) Verifier(_ context.Context, result ExecResult) (Verdict, error) {
	val, ok := result.RawOutputs[critiqueDimensionsKey]
	if !ok {
		return Verdict{
			OK:       false,
			Reasons:  []string{"missing_critique_dimensions"},
			TimedOut: false,
		}, nil
	}
	dims, ok := val.(*CritiqueDimensions)
	if !ok {
		return Verdict{
			OK:      false,
			Reasons: []string{fmt.Sprintf("critique_dimensions_type_invalid: got %T", val)},
		}, nil
	}
	if dims == nil {
		return Verdict{OK: false, Reasons: []string{"critique_dimensions_nil"}}, nil
	}

	var (
		outOfRange []string
		sum        float64
		count      int
	)
	check := func(name string, v float64) {
		count++
		sum += v
		if v < 0 || v > 1 {
			outOfRange = append(outOfRange, fmt.Sprintf("%s=%v out of [0,1]", name, v))
		}
	}
	check("accuracy", dims.Accuracy)
	check("completeness", dims.Completeness)
	check("actionability", dims.Actionability)

	if len(outOfRange) > 0 {
		return Verdict{
			OK:         false,
			Confidence: 0,
			Reasons:    outOfRange,
		}, nil
	}

	mean := 0.0
	if count > 0 {
		mean = sum / float64(count)
	}
	return Verdict{
		OK:         true,
		Confidence: mean,
		Reasons:    nil,
	}, nil
}

// VerifierTimeoutMs 覆盖 BasePhaseWorker 的 VerifierMs。
// 60s 给 LLM 重试 + schema 校验留足余量；用 BasePhaseWorker 的字段
// 走统一 path，main.go 不需要为 critiqued 做特殊处理。
func (w *CritiquedPhaseWorker) VerifierTimeoutMs() int {
	if v := w.BasePhaseWorker.VerifierTimeoutMs(); v > 0 {
		return v
	}
	return critiquedPhaseVerifierTimeoutMs
}

// --- helpers ------------------------------------------------------------

// renderCritiqueUserPrompt 把 RootCauseJSON 序列化成 user prompt。
//
// 渲染策略：JSON 序列化整个 RootCauseJSON（schema v1），让 LLM 直接读结构。
// 不做手工拼接 / 模板渲染是因为 LLM 对 JSON 输入最稳定；后续 batch 接到
// prompt registry 时换成模板引擎也只是换实现，wire 形状不变。
func renderCritiqueUserPrompt(incidentID string, rc *RootCauseJSON) string {
	rcJSON, _ := json.Marshal(rc)
	return fmt.Sprintf(
		"incident_id=%s\n\nReview the following root cause analysis. Return JSON only.\n\n%s",
		incidentID,
		string(rcJSON),
	)
}

// validateCritiqueDimensionsRaw 重新跑 schema 校验 + bounds 检查。
// Executor 在拿不到 LLMCaller 的情况下用这道兜底；逻辑上跟 schema 校验器
// 等价，但直接读 []byte 避免重复构建 *schemaNode。
func validateCritiqueDimensionsRaw(raw string) error {
	if raw == "" {
		return errors.New("empty critique JSON")
	}
	var dims map[string]any
	if err := json.Unmarshal([]byte(raw), &dims); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	for _, field := range []string{"accuracy", "completeness", "actionability"} {
		v, ok := dims[field]
		if !ok {
			return fmt.Errorf("missing required field %q", field)
		}
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("field %q not a number (got %T)", field, v)
		}
		if f < 0 || f > 1 {
			return fmt.Errorf("field %q=%v out of [0,1]", field, f)
		}
	}
	return nil
}

// --- Narrow interfaces --------------------------------------------------

// RootCauseRefLoader 加载上游 RootCauseJSON 合约（investigated phase 写入）。
//
// 接口只暴露 LoadRootCause 一个方法；其他合约（ApprovalDecision /
// CritiqueScore 等）由各自的 worker 注入。
//
// 行为契约：
//   - contractID 不存在 → 返回 (nil, nil)，让 Planner 拒绝
//   - DB / IO 错误       → 返回 wrapped error
//   - 多 tenant 隔离     → 实现层必须按 in.TenantID 过滤
//
// Day 5 集成时由 cmd/main.go 注入 loop_contract repo 的 adapter。
type RootCauseRefLoader interface {
	LoadRootCause(ctx context.Context, contractID int64) (*RootCauseJSON, error)
}

// NoopRootCauseRefLoader 返回 (nil, nil)，让 Planner 在缺真实 loader 时
// fail-fast。生产环境替换为 DB-backed loader。
type NoopRootCauseRefLoader struct{}

func (NoopRootCauseRefLoader) LoadRootCause(_ context.Context, _ int64) (*RootCauseJSON, error) {
	return nil, nil
}
