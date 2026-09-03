// Package loop — recovery.go
//
// recovered phase 的 PhaseWorker 实现（zero-manual-ops-loop Day 3）。
//
// recovered phase 的职责：
//
//  1. 读取上游 ApprovalDecision 拿到 target / resource_type / metrics
//  2. 调用 verify_recovery 工具（BaseTool 形式）拿到 VerifiedDelta
//  3. Verifier 解析 VerifiedDelta：
//     - passed=true  → Verdict{OK: true, Confidence: 1} → 推进 postmortem
//     - passed=false → Verdict{OK: false}              → 触发 recovered→approved rollback
//  4. rollback 之后：
//     - 调 StateStore.Increment 把 retry_count +1
//     - 若新 retry_count > MaxRetryCount → 触发 severity=dangerous
//     升级（写入 log + Verdict.Reasons；orchestrator 的 retry_exhausted
//     事件由下一轮 Run 产生）
//
// 设计依据：
//   - OpenSpec spec: recovery-verification
//   - design §A.3（adaptive baseline）+ §A.4（异步轮询）+ §D4（容差）
//   - orchestrator.go 已有的 rollbackEligible 5 条件 guard
//
// 不引入 monorepo 跨域 import：
//   - VerifyRecoveryCaller / RecoveryStateStore 都是本包内定义的
//     narrow interface；concrete 实现由 cmd/main.go 在 Day 5 集成时
//     注入（aiops/tools.VerifyRecoveryTool + InMemoryRecoveryStateStore）。
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/prom"
)

// RecoveredPhaseWorker 是 PhaseWorker 的 recovered phase 实现。
//
// 构造由 NewRecoveredPhaseWorker 完成；Verdict.OK 直接驱动
// orchestrator 的 nextPhase(recovered, contractValid) 决策。
type RecoveredPhaseWorker struct {
	// PhaseRef 固定 PhaseRecovered（compile-time 校验）。
	PhaseRef Phase

	// VerifierMs 用 RecoveredPhaseVerifierTimeoutMs（60s）覆盖默认值
	// 30s，因为 verify_recovery 涉及 baseline + current 双窗口 IO。
	VerifierMs int

	// verifyCaller 是 narrow interface（调用 verify_recovery 工具）。
	// nil = Planner 阶段直接拒绝，触发 phase_failed。
	verifyCaller VerifyRecoveryCaller

	// stateStore 是 retry_count 持久化接口。
	// nil = 跳过 retry_count 维护（仅用于单元测试基线 case）。
	stateStore RecoveryStateStore

	// approvedRefLoader 加载上游 ApprovalDecision（approval phase 写入）。
	// nil = Planner 拒绝；Phase 4 集成时会接到 ContractRepository。
	approvedRefLoader ApprovedDecisionLoader

	// clock 是时间源。nil = time.Now。tests 注入 fake 让 retry_count
	// 时间相关测试不依赖 wall-clock。
	clock func() time.Time

	// log 是 slog.Logger；nil = slog.Default()。
	log *slog.Logger

	// mu 保护以下字段：lastEscalation + escalationCache
	mu              sync.Mutex
	lastEscalation  map[string]time.Time
	escalationCache map[string]recoveryEscalation
}

// recoveryEscalation 是 retry_count 超 MaxRetryCount 后的升级记录。
// 在 state_store 上加一层 in-memory cache 以便 Verifier 决策时读；
// Day 4 migration incident_recovery_state 表建好后这块代码会
// 整体迁移过去。
type recoveryEscalation struct {
	incidentID string
	retryCount int
	at         time.Time
	reason     string
}

// NewRecoveredPhaseWorker 是生产构造器。
// verifyCaller / stateStore / approvedRefLoader 必填（monorepo 不让跨域
// import，所以 main.go 在 Day 5 集成时负责 wire-up）。
func NewRecoveredPhaseWorker(
	verifyCaller VerifyRecoveryCaller,
	stateStore RecoveryStateStore,
	approvedRefLoader ApprovedDecisionLoader,
	log *slog.Logger,
) (*RecoveredPhaseWorker, error) {
	if verifyCaller == nil {
		return nil, errors.New("loop: RecoveredPhaseWorker.verifyCaller is required")
	}
	if stateStore == nil {
		return nil, errors.New("loop: RecoveredPhaseWorker.stateStore is required")
	}
	if approvedRefLoader == nil {
		return nil, errors.New("loop: RecoveredPhaseWorker.approvedRefLoader is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &RecoveredPhaseWorker{
		PhaseRef:          PhaseRecovered,
		VerifierMs:        RecoveredPhaseVerifierTimeoutMs,
		verifyCaller:      verifyCaller,
		stateStore:        stateStore,
		approvedRefLoader: approvedRefLoader,
		clock:             func() time.Time { return time.Now().UTC() },
		log:               log,
		lastEscalation:    make(map[string]time.Time),
		escalationCache:   make(map[string]recoveryEscalation),
	}, nil
}

// Phase 实现 PhaseWorker 接口。
func (w *RecoveredPhaseWorker) Phase() Phase { return w.PhaseRef }

// Planner 准备 Plan：把上游 ApprovalDecision 读出来，列出 metric 子集。
//
// 步骤：
//  1. 通过 approvedRefLoader 取 ApprovalDecision（resource_type / target / metrics）
//  2. 把 target / resource_type / metrics / skill_id 装到 PlanStep.Args
//  3. Plan.Steps 唯一一项是 tool_call verify_recovery
func (w *RecoveredPhaseWorker) Planner(ctx context.Context, in PlanInput) (Plan, error) {
	if in.UpstreamContract == nil {
		return Plan{}, fmt.Errorf("%w: recovered phase needs ApprovalDecision upstream", ErrWorkerNotRegistered)
	}
	approval, err := w.approvedRefLoader.LoadApprovedDecision(ctx, in.TenantID, in.UpstreamContract.ID)
	if err != nil {
		return Plan{}, fmt.Errorf("loop: load ApprovalDecision (id=%d): %w", in.UpstreamContract.ID, err)
	}
	if approval == nil {
		return Plan{}, fmt.Errorf("%w: ApprovalDecision id=%d not found", ErrInvalidSchema, in.UpstreamContract.ID)
	}
	metrics := approval.VerifyMetrics
	if len(metrics) == 0 {
		// 没指定 metrics 时退回 resource_type 默认子集。
		metrics = defaultMetricsForResource(approval.ResourceType)
	}
	stepArgs := map[string]any{
		"skill_id":      approval.SkillID,
		"target":        approval.Target,
		"resource_type": approval.ResourceType,
		"tolerance":     approval.Tolerance,
		"metrics":       metrics,
	}
	return Plan{
		Steps: []PlanStep{{
			Kind:      "tool_call",
			Target:    ToolNameVerifyRecovery,
			Args:      stepArgs,
			TimeoutMs: RecoveredPhaseVerifierTimeoutMs,
		}},
		EstimatedCost: CostEstimate{USD: 0, Tokens: 0},
		Meta: map[string]any{
			"approval_id":       in.UpstreamContract.ID,
			"approval_decision": approval,
		},
	}, nil
}

// Executor 调用 verify_recovery 工具，把返回 JSON 解析成 loop.VerifiedDelta。
func (w *RecoveredPhaseWorker) Executor(ctx context.Context, plan Plan) (ExecResult, error) {
	if len(plan.Steps) == 0 {
		return ExecResult{}, fmt.Errorf("%w: planner returned empty steps", ErrPlanInvalid)
	}
	step := plan.Steps[0]
	argsJSON, err := json.Marshal(step.Args)
	if err != nil {
		return ExecResult{}, fmt.Errorf("%w: marshal verify_recovery args: %w", ErrPlanInvalid, err)
	}
	rawOut, err := w.verifyCaller.InvokeVerifyRecovery(ctx, string(argsJSON))
	if err != nil {
		return ExecResult{}, fmt.Errorf("loop: verify_recovery invocation: %w", err)
	}
	var vd VerifiedDelta
	if err := json.Unmarshal([]byte(rawOut), &vd); err != nil {
		return ExecResult{}, fmt.Errorf("%w: verify_recovery returned malformed JSON: %w", ErrInvalidSchema, err)
	}
	if err := ValidateVerifiedDelta(&vd); err != nil {
		return ExecResult{}, fmt.Errorf("%w: verify_recovery produced invalid contract: %w", ErrInvalidSchema, err)
	}
	return ExecResult{
		ContractRef: nil, // contract 由 Run 时机的 contract-writer 写入 loop_contract
		SideEffects: []SideEffect{{
			Kind:   "recovery_verification",
			Target: targetFromArgs(step.Args),
			Detail: map[string]any{
				"passed":         vd.Passed,
				"failed_metrics": vd.FailedMetrics,
				"deltas":         vd.Deltas,
				"retry_count":    vd.RetryCount,
				"warning_level":  vd.WarningLevel,
				"sampled_at":     w.clock(),
			},
		}},
		ToolReplay: []ToolReplayEntry{{
			Name:       ToolNameVerifyRecovery,
			ArgsJSON:   string(argsJSON),
			ResultJSON: rawOut,
			Status:     "success",
			LatencyMs:  0, // 单元测试不测 latency；Day 5 wire-up 时由 caller 注入
			Timestamp:  w.clock(),
		}},
		RawOutputs: map[string]any{
			"verified_delta": &vd,
		},
	}, nil
}

// Verifier 把 VerifiedDelta 翻译为 Verdict：
//   - Passed=true → OK=true, Confidence=vd.Tolerance（保守）
//   - Passed=false → OK=false；Reasons 列出 failed_metrics
//   - SeverityEscalated=true → Reasons 追加 "severity_escalated=dangerous"
//   - log 写一行 warn
//
// 副作用：每次调用 emit 一条 prom.ObserveLoopPhase(PhaseRecovered, dur, err)，
// 让 opskeeper self-health 能区分 recovered phase 三类结局：
//   - nil                          → 通过（postmortem 推进）
//   - errRolledBack{}              → 触发 recovered→approved rollback
//   - errSeverityEscalated{}       → retry_count > MaxRetryCount，severity=dangerous
//
// err 选用 sentinel 类型而非 fmt.Errorf 是因为 prom.ObserveLoopPhase 用
// errors.Is 分流；包装 string 会让所有路径都落到 "failed" 一桶。
func (w *RecoveredPhaseWorker) Verifier(ctx context.Context, result ExecResult) (vret Verdict, retErr error) {
	start := w.clock()
	defer func() {
		var tag error
		switch {
		case retErr != nil:
			tag = retErr
		case vret.OK:
			tag = nil
		default:
			// OK=false：区分"普通 rollback"和"severity escalation"
			raw, _ := result.RawOutputs["verified_delta"]
			if vd, ok := raw.(*VerifiedDelta); ok && vd.RetryCount > MaxRetryCount {
				tag = prom.ErrSeverityEscalated
			} else {
				tag = prom.ErrRolledBack
			}
		}
		prom.ObserveLoopPhase(string(PhaseRecovered), time.Since(start).Seconds(), tag)
	}()
	raw, ok := result.RawOutputs["verified_delta"]
	if !ok {
		return Verdict{}, fmt.Errorf("%w: Executor.RawOutputs missing verified_delta", ErrInvalidSchema)
	}
	vd, ok := raw.(*VerifiedDelta)
	if !ok {
		return Verdict{}, fmt.Errorf("%w: Executor.RawOutputs.verified_delta type=%T", ErrInvalidSchema, raw)
	}
	severityEscalated := vd.RetryCount > MaxRetryCount
	if vd.Passed {
		return Verdict{OK: true, Confidence: vd.Tolerance, Reasons: nil}, nil
	}
	// failed → 触发 rollback 路径
	reasons := make([]string, 0, len(vd.FailedMetrics)+1)
	for _, m := range vd.FailedMetrics {
		reasons = append(reasons, fmt.Sprintf("metric %q exceeded tolerance (delta in deltas map)", m))
	}
	if severityEscalated {
		reasons = append(reasons, fmt.Sprintf(
			"retry_count=%d exceeded MaxRetryCount=%d; severity escalated to dangerous",
			vd.RetryCount, MaxRetryCount,
		))
		w.log.Warn("loop.recovery.severity_escalated",
			slog.String("phase", string(PhaseRecovered)),
			slog.Int("retry_count", vd.RetryCount),
			slog.String("incident_id", resultSideEffectTarget(result)),
		)
	}
	return Verdict{OK: false, Confidence: vd.Tolerance, Reasons: reasons}, nil
}

// VerifierTimeoutMs 继承构造时设置的 RecoveredPhaseVerifierTimeoutMs。
func (w *RecoveredPhaseWorker) VerifierTimeoutMs() int {
	if w.VerifierMs > 0 {
		return w.VerifierMs
	}
	return RecoveredPhaseVerifierTimeoutMs
}

// --- rollback 后 retry_count 处理 --------------------------------------

// HandleRollback 是 recovered→approved 回滚后由 orchestrator 调用的钩子：
// 把 retry_count +1，超 MaxRetryCount 时升级 severity=dangerous。
//
// 返回值：
//   - newRetryCount：递增后的值
//   - escalated：    是否触发 severity 升级
//   - err：          StateStore 错误（增量失败不阻塞 rollback，但会
//     经 w.log 落 warn 行供 Day 5 接入 audit trail）
//
// 副作用：每次调用 emit 一条 prom.ObserveLoopPhase(PhaseRecovered, dur, err)，
// 让 self-health 区分"rollback 增量成功"和"severity escalation 已触发"。
func (w *RecoveredPhaseWorker) HandleRollback(ctx context.Context, incidentID string) (newRetryCount int, escalated bool, err error) {
	start := w.clock()
	defer func() {
		var tag error
		switch {
		case err != nil:
			tag = err
		case escalated:
			tag = prom.ErrSeverityEscalated
		default:
			tag = prom.ErrRolledBack
		}
		prom.ObserveLoopPhase(string(PhaseRecovered), time.Since(start).Seconds(), tag)
	}()
	if w.stateStore == nil {
		return 0, false, errors.New("loop: state store not configured")
	}
	newCount, storeErr := w.stateStore.Increment(ctx, incidentID)
	if storeErr != nil {
		w.log.Warn("loop.recovery.increment_retry_failed",
			slog.String("incident_id", incidentID),
			slog.String("error", storeErr.Error()),
		)
		return 0, false, storeErr
	}
	escalated = newCount > MaxRetryCount
	if escalated {
		w.recordEscalation(incidentID, newCount, "retry_count exceeded MaxRetryCount")
	}
	return newCount, escalated, nil
}

// recordEscalation 在 escalationCache 上落一条；incident_id 主键去重，
// 30s 内的重复升级只记一次（防 log flood）。
func (w *RecoveredPhaseWorker) recordEscalation(incidentID string, retryCount int, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, ok := w.lastEscalation[incidentID]; ok {
		if w.clock().Sub(last) < 30*time.Second {
			return
		}
	}
	now := w.clock()
	w.lastEscalation[incidentID] = now
	w.escalationCache[incidentID] = recoveryEscalation{
		incidentID: incidentID,
		retryCount: retryCount,
		at:         now,
		reason:     reason,
	}
	w.log.Warn("loop.recovery.severity_escalated",
		slog.String("incident_id", incidentID),
		slog.Int("retry_count", retryCount),
		slog.String("reason", reason),
	)
}

// EscalationSnapshot 是升级快照（暴露给 orchestrator / SPA）。
type EscalationSnapshot struct {
	IncidentID string    `json:"incident_id"`
	RetryCount int       `json:"retry_count"`
	At         time.Time `json:"at"`
	Reason     string    `json:"reason"`
}

// EscalationSnapshots 返回当前已记录的升级列表（Day 4 入库前临时）。
func (w *RecoveredPhaseWorker) EscalationSnapshots() []EscalationSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]EscalationSnapshot, 0, len(w.escalationCache))
	for _, e := range w.escalationCache {
		out = append(out, EscalationSnapshot{
			IncidentID: e.incidentID,
			RetryCount: e.retryCount,
			At:         e.at,
			Reason:     e.reason,
		})
	}
	return out
}

// ResetEscalationCache 在测试清理时调用；生产环境无外部入口。
func (w *RecoveredPhaseWorker) ResetEscalationCache() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastEscalation = make(map[string]time.Time)
	w.escalationCache = make(map[string]recoveryEscalation)
}

// --- Narrow interfaces --------------------------------------------------

// VerifyRecoveryCaller 是 verify_recovery BaseTool 的 narrow interface。
//
// 生产实现：Day 5 集成时由 main.go 构造 aiops/tools.VerifyRecoveryTool，
// 适配器把它的 InvokableRun 包成本接口。
//
// 测试实现：返回装好的 JSON。
type VerifyRecoveryCaller interface {
	InvokeVerifyRecovery(ctx context.Context, argsJSON string) (string, error)
}

// RecoveryStateStore 是 retry_count 持久化的 narrow interface。
//
// 接口对齐 aiops/tools.RecoveryStateStore 但在 loop 包内重声明以避免
// 跨域 import；Day 5 集成时由 adapter 把两者接起来（同一 in-memory 实现
// 同时实现两个接口，所以不需要 proxy）。
type RecoveryStateStore interface {
	Get(ctx context.Context, incidentID string) (int, error)
	Increment(ctx context.Context, incidentID string) (int, error)
	Reset(ctx context.Context, incidentID string) error
}

// ApprovedDecisionLoader 是 ApprovalDecision 的 narrow interface。
//
// Day 3 阶段 orchestrator 还没有 ContractRepository，所以 main.go 在
// Day 5 集成时把 loop_contractRepo 包成这个接口。
//
// 接口只暴露 LoadApprovedDecision；其他合约（RootCauseJSON /
// CritiqueScore 等）由各自的 worker 注入。
//
// 多租户安全：tenantID 必传（caller 是 Planner，in.TenantID 已在 scope）；
// 用于 reader 加 WHERE tenant_id = ? 过滤。⑥ 号差距的修复。
//
// 生产实现：DBApprovedDecisionLoader（db_approved_decision_loader.go），
// 通过 loopstore.ContractRepoDB.ReadContractByID 按 ID 读 loop_contract
// 行 + 反序列化 Payload JSON 得到 ApprovalDecision。DB 不通 / 行不存在
// / type 不匹配 / Payload 损坏四种异常路径都有明确语义：
//   - row == nil → (nil, nil)：Planner 走 default
//   - 其他错误  → error：必须停 phase（不再像 NoopApprovedDecisionLoader
//     那样把"合同不存在"与"DB 失败"混为一谈）
type ApprovedDecisionLoader interface {
	LoadApprovedDecision(ctx context.Context, tenantID string, contractID int64) (*ApprovalDecision, error)
}

// --- ApprovalDecision 本地类型 -------------------------------------------

// ApprovalDecision 是 approved phase 写入的合约；recovered phase 读取
// 它来推导 target / resource_type / metrics / tolerance。
//
// 字段对齐 spec 设计 §3.2（approved → recovered transition）。
// Day 3 阶段它是结构定义（不持久化），Day 5 集成时由 loop_contract
// 表写入。
type ApprovalDecision struct {
	// SchemaVersion 固定 "v1"。
	SchemaVersion string `json:"schema_version"`

	// SkillID 是上游根因 skill 的 id。
	SkillID string `json:"skill_id"`

	// Target 是修复动作的目标资源定位符。
	Target string `json:"target"`

	// ResourceType 是 host / pg / redis / k8s / app 之一。
	ResourceType string `json:"resource_type"`

	// Tolerance 是可选 override；0 表示用 metric 自身 default 或工具 default。
	Tolerance float64 `json:"tolerance,omitempty"`

	// VerifyMetrics 是 verify_recovery 要校验的 metric 子集；
	// 空表示用 defaultMetricsForResource(ResourceType)。
	VerifyMetrics []string `json:"verify_metrics,omitempty"`

	// ApprovedAt 是审批时间戳。
	ApprovedAt time.Time `json:"approved_at"`

	// ApprovedBy 是审批人（"auto" / user_id）。
	ApprovedBy string `json:"approved_by"`
}

// --- helpers ------------------------------------------------------------

// defaultMetricsForResource 是 per-resource 默认 verify 子集。
// 与 verify_recovery_basetool.go::ResourceMetricsAllowed 对齐；单源维护。
func defaultMetricsForResource(resourceType string) []string {
	switch resourceType {
	case "host":
		return []string{"cpu_usage", "mem_usage"}
	case "pg":
		return []string{"cpu_usage", "mem_usage", "qps", "latency_p99"}
	case "redis":
		return []string{"mem_usage", "qps", "latency_p99"}
	case "k8s":
		return []string{"cpu_usage", "mem_usage"}
	case "app":
		return []string{"qps", "latency_p99"}
	default:
		return nil
	}
}

// targetFromArgs 安全从 args 取 target 字段；缺失时返回 ""。
func targetFromArgs(args map[string]any) string {
	if args == nil {
		return ""
	}
	if s, ok := args["target"].(string); ok {
		return s
	}
	return ""
}

// resultSideEffectTarget 安全从 result.SideEffects 提取首个 target。
func resultSideEffectTarget(result ExecResult) string {
	if len(result.SideEffects) == 0 {
		return ""
	}
	return result.SideEffects[0].Target
}

// ToolNameVerifyRecovery 是 verify_recovery 的 wire name；与
// aiops/tools.ToolNameVerifyRecovery 保持同名常量避免跨域 import。
//
// 此常量在 loop 包内作为 PlanStep.Target 与 ToolReplayEntry.Name 的字面量；
// Day 5 集成时由 cmdpolicy 把 loop 包值与 aiops/tools 包值映射（同名即可）。
const ToolNameVerifyRecovery = "verify_recovery"
