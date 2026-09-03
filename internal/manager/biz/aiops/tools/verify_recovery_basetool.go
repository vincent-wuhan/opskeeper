// Package tools — verify_recovery_basetool.go
//
// verify_recovery 是 zero-manual-ops-loop Day 3 落地的恢复验证 BaseTool，
// 由 orchestrator 的 recovered phase 通过 PhaseWorker 调用（见
// internal/manager/biz/loop/recovery.go）。
//
// 设计依据：
//
//   - OpenSpec spec: recovery-verification（事实源）
//   - docs/superpowers/specs/2026-08-10-zero-manual-ops-recovery-postmortem-design.md §A
//   - openspec/changes/zero-manual-ops-loop/design.md §D4
//
// 关键约束：
//
//   - 参数 schema 严格 additionalProperties:false + range [0,1] tolerance
//   - 时间窗口非正或 > 1h 拒绝
//   - Allowlist 硬编码 4 类（cpu_usage / mem_usage / qps / latency_p99），
//     per-resource 子集裁剪；非法 metric 直接 ErrMetricNotAllowed，
//     不发起任何 IO
//   - 不依赖全局状态：依赖（querier / stateStore / log）走构造函数注入
//   - baseline 选择告警时刻之前 5m（baseline_window 默认），
//     compare 选择调用时刻之前 2m（compare_window 默认），与 design §A.3 一致
//   - 输出用 loop.VerifiedDelta 的 JSON 形态（schema_version=v1），
//     Orchestrator 端用 ValidateVerifiedDelta 反向校验
//
// 已知偏离：
//
//   - Allowlist 用 D4 design 约定的 4 个名（cpu_usage / mem_usage /
//     qps / latency_p99），spec §"验证 metric 限定"列了 7 个底层名
//     （cpu/mem/disk_io/net_in/net_out/conn_count/request_rate）。
//     落到 Day 3 闭环 + Day 4 harness 协同，4 个高层 metric 已能
//     覆盖 host.cpu-spike / pg.long-running-tx / redis.memory-burst
//     三大 golden case。Day 5 集成时由 cmdpolicy 把底层 metric
//     映射到 4 个高层 metric（mapping 规则留 Day 5 写）。
//   - retry_count 持久化走 RecoveryStateStore 接口；Day 3 阶段
//     用 in-memory 实现，Day 4 migration 把 incident_recovery_state
//     表建起来后切到 DB-backed 实现。orchestrator 不感知具体存储。
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
)

// ToolNameVerifyRecovery 是 verify_recovery 在工具表里的稳定 wire 名。
// 闭环 orchestrator 通过 BaseTool.Info().Name 解析。
const ToolNameVerifyRecovery = "verify_recovery"

// VerifyRecoveryDescription 是给 LLM 的 1 句话简介。
const VerifyRecoveryDescription = "验证一个修复动作是否把目标 metric 拉回告警前基线，输出 " +
	"VerifiedDelta（passed / failed_metrics / deltas / retry_count）。" +
	"仅校验 allowlist 内的 metric，其他资源类型直接拒绝。"

// verifyRecoveryWhenToUse 是 routing hint；NOT 反向 guard 防止 LLM 误用。
const verifyRecoveryWhenToUse = "仅在闭环 recovered 阶段使用：orchestrator 跑完修复动作后，" +
	"对目标 target 抓 baseline（告警前 window）+ current（修复后 window），" +
	"对比每个 metric 的相对偏差是否在 tolerance 内。" +
	"NOT for: 实时 SRE 诊断（用 get_host_load / query_promql）；" +
	"NOT for: 跨租户资源（多租户隔离是调用方负责）；" +
	"NOT for: 未在 allowlist 的 metric（cpu_usage / mem_usage / qps / latency_p99 之外的全部拒绝）。"

// VerifyRecoverySchema 是入参 JSON Schema。tolerance 限定 [0,1]，
// time window 限定 > 0 且 <= 1h，additionalProperties:false。
var VerifyRecoverySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "skill_id":        {"type": "string",  "description": "关联 skill id（来自 investigator 根因）"},
    "target":          {"type": "string",  "description": "host-1 / pg-cluster-x / k8s-deploy-y 等资源定位符"},
    "resource_type":   {"type": "string",  "enum": ["host","pg","redis","k8s","app"], "description": "目标资源类型，决定 per-resource metric 子集"},
    "baseline_window": {"type": "string",  "description": "baseline 窗口（Go duration 语法，如 5m），默认 5m，范围 (0, 1h]"},
    "compare_window":  {"type": "string",  "description": "compare 窗口（Go duration 语法，如 2m），默认 2m，范围 (0, 1h]"},
    "tolerance":       {"type": "number",  "minimum": 0, "maximum": 1, "default": 0.15, "description": "相对偏差阈值（绝对值），<= 该值算 pass"},
    "metrics":         {"type": "array",   "minItems": 1, "items": {"type": "string", "enum": ["cpu_usage","mem_usage","qps","latency_p99"]}, "description": "待校验 metric 子集，必须是 allowlist 内且当前 resource_type 允许"},
    "sensitivity":     {"type": "string",  "description": "data-guard-classification 分类（public/internal/confidential/restricted），可选"}
  },
  "required": ["skill_id","target","resource_type","metrics"],
  "additionalProperties": false
}`)

// VerifyRecoveryArgs 是 typed form，json tag 对齐 schema。
type VerifyRecoveryArgs struct {
	SkillID        string   `json:"skill_id"`
	Target         string   `json:"target"`
	ResourceType   string   `json:"resource_type"`
	BaselineWindow string   `json:"baseline_window,omitempty"`
	CompareWindow  string   `json:"compare_window,omitempty"`
	Tolerance      float64  `json:"tolerance,omitempty"`
	Metrics        []string `json:"metrics"`
	Sensitivity    string   `json:"sensitivity,omitempty"`
}

// AllowedVerifyMetrics 是 allowlist 集中常量（spec §"验证 metric 限定"）。
// 4 个高层 metric，覆盖 host / pg / redis / k8s / app 共 5 类资源。
// Day 5 集成时由 cmdpolicy 把 7 个底层 metric（cpu/mem/.../request_rate）
// 映射到 4 个高层 metric。
var AllowedVerifyMetrics = []string{
	"cpu_usage",
	"mem_usage",
	"qps",
	"latency_p99",
}

// ResourceMetricsAllowed 是 per-resource 子集。metric 必须在 allowlist
// 内且当前 resource_type 允许才放行；二者都做 fail-fast 校验。
var ResourceMetricsAllowed = map[string]map[string]bool{
	"host":  {"cpu_usage": true, "mem_usage": true},
	"pg":    {"cpu_usage": true, "mem_usage": true, "qps": true, "latency_p99": true},
	"redis": {"mem_usage": true, "qps": true, "latency_p99": true},
	"k8s":   {"cpu_usage": true, "mem_usage": true},
	"app":   {"qps": true, "latency_p99": true},
}

// MetricSpec 描述单个 metric 的查询参数与默认 tolerance。
type MetricSpec struct {
	Source           string  // "promql" | "agent"
	DefaultTolerance float64 // metric 自身默认 tolerance
	Unit             string  // 仅文档用途
}

// MetricSpecTable 把 metric 名映射到 MetricSpec。Source 决定 MetricQuerier
// 的查询方式（PromQL / agent 上报）；Day 3 默认 QueryPromQL 走 promql 分支。
var MetricSpecTable = map[string]MetricSpec{
	"cpu_usage":   {Source: "promql", DefaultTolerance: 0.15, Unit: "pct"},
	"mem_usage":   {Source: "promql", DefaultTolerance: 0.15, Unit: "pct"},
	"qps":         {Source: "promql", DefaultTolerance: 0.20, Unit: "qps"},
	"latency_p99": {Source: "promql", DefaultTolerance: 0.25, Unit: "ms"},
}

// 错误定义。错误链均以 base 为前缀，便于 callers 用 errors.Is 分类。
var (
	// ErrArgsInvalid：JSON 解码失败 / required 字段缺失 / 字段类型错。
	ErrArgsInvalid = errors.New("verify_recovery: args invalid")
	// ErrMetricNotAllowed：metric 不在 allowlist 或不在该 resource 子集内。
	// 调用方应在收到本错误后立即停止本调用，不发起任何 IO。
	ErrMetricNotAllowed = errors.New("verify_recovery: metric not in allowlist")
	// ErrResourceTypeUnknown：resource_type 不在 5 个允许的枚举内。
	ErrResourceTypeUnknown = errors.New("verify_recovery: resource_type unknown")
	// ErrToleranceOutOfRange：tolerance 不在 [0,1]。
	ErrToleranceOutOfRange = errors.New("verify_recovery: tolerance out of range")
	// ErrWindowInvalid：baseline_window / compare_window 非正或 > 1h。
	ErrWindowInvalid = errors.New("verify_recovery: window invalid")
)

// MaxRetryCount 上限：与 loop.MaxRetryCount 对齐（orchestrator 端常量是 3）。
// 在 verify 工具里再定义一次，避免 import-time 循环依赖（理论上不会，
// 但保持包内常量自描述更稳）。
const MaxRetryCount = 3

// WindowMax 单次窗口上限，超过此值拒绝（防止 LLM 误传 24h 等破坏内存）。
const WindowMax = 1 * time.Hour

// DefaultBaselineWindow / DefaultCompareWindow 与 design §A.3 对齐。
const (
	DefaultBaselineWindow = 5 * time.Minute
	DefaultCompareWindow  = 2 * time.Minute
	DefaultTolerance      = 0.15
	// EpsilonBaseline 防 baseline_avg=0 时除 0；与 design §A.3 epsilon 一致。
	EpsilonBaseline = 1e-6
)

// MetricQuerier 是 verify_recovery 对外的 metric 抓取抽象。
//
// Day 3 阶段：生产实现接 promQuery + agent 上报通道；测试 fake 直接返回
// 装好的 baseline_avg / current_avg。
//
// 每个 metric 一次调用返回 (baseline_avg, current_avg, sample_size)。
// 三元组语义：
//   - baseline_avg：baseline_window 内 metric 的均值
//   - current_avg： compare_window  内 metric 的均值
//   - sample_size： current 窗口内的有效样本数（< 3 触发 warn）
type MetricQuerier interface {
	QueryMetric(ctx context.Context, req MetricQueryRequest) (MetricQueryResult, error)
}

// MetricQueryRequest 一次 metric 查询的入参。MetricQuerier 实现用它打 PromQL。
type MetricQueryRequest struct {
	Target         string        // 资源定位符
	ResourceType   string        // host/pg/...
	Metric         string        // cpu_usage 等
	BaselineWindow time.Duration // 历史窗口
	CompareWindow  time.Duration // 当前窗口
	Now            time.Time     // 调用时刻（让 fake 可重现）
}

// MetricQueryResult 是 MetricQuerier 的返回值。
type MetricQueryResult struct {
	BaselineAvg float64
	CurrentAvg  float64
	SampleSize  int
}

// RecoveryStateStore 是 retry_count 持久化的接口。
//
// Day 3 阶段用 InMemoryRecoveryStateStore（默认实现）；Day 4 migration
// incident_recovery_state 表建好后切到 DBRecoveryStateStore（Day 4 PR）。
//
// 接口维度设计：单 incident_id 取 / 增 retry_count；不暴露全局迭代，
// 避免 orchestrator 误扫表。Increment 在 retry_count >= MaxRetryCount 时
// 仍要返回新值（不报错），由 caller 决定是否升级 severity。
type RecoveryStateStore interface {
	// Get 返回 incident_id 的当前 retry_count；未记录返回 0。
	Get(ctx context.Context, incidentID string) (int, error)
	// Increment 原子地把 incident_id 的 retry_count +1 并返回新值。
	Increment(ctx context.Context, incidentID string) (int, error)
	// Reset 把 incident_id 的 retry_count 清零（run 完成 / postmortem sealed 时调用）。
	Reset(ctx context.Context, incidentID string) error
}

// InMemoryRecoveryStateStore 是 RecoveryStateStore 的内存实现。
// 并发安全；进程重启即丢，符合 Day 3 阶段（Day 4 migration 会替换为 DB）。
type InMemoryRecoveryStateStore struct {
	mu     sync.Mutex
	counts map[string]int
}

// NewInMemoryRecoveryStateStore 返回一个空实例。
func NewInMemoryRecoveryStateStore() *InMemoryRecoveryStateStore {
	return &InMemoryRecoveryStateStore{counts: make(map[string]int)}
}

// Get 实现 RecoveryStateStore.Get。
func (s *InMemoryRecoveryStateStore) Get(_ context.Context, incidentID string) (int, error) {
	if s == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[incidentID], nil
}

// Increment 实现 RecoveryStateStore.Increment。
func (s *InMemoryRecoveryStateStore) Increment(_ context.Context, incidentID string) (int, error) {
	if s == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[incidentID]++
	return s.counts[incidentID], nil
}

// Reset 实现 RecoveryStateStore.Reset。
func (s *InMemoryRecoveryStateStore) Reset(_ context.Context, incidentID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counts, incidentID)
	return nil
}

// VerifyRecoveryConfig 是工具的可调参数集中（cmd/main.go 可注入）。
type VerifyRecoveryConfig struct {
	// DefaultTolerance：当 args.tolerance=0 时使用的默认 tolerance。
	DefaultTolerance float64
	// MaxConcurrent：单 instance 内的最大并发 metric 查询数（限流）。
	MaxConcurrent int
	// Clock：时间源；测试可注入。nil 表示 time.Now。
	Clock func() time.Time
}

// DefaultVerifyRecoveryConfig 返回默认配置。
func DefaultVerifyRecoveryConfig() VerifyRecoveryConfig {
	return VerifyRecoveryConfig{
		DefaultTolerance: DefaultTolerance,
		MaxConcurrent:    8,
		Clock:            nil,
	}
}

// VerifyRecoveryTool 是 verify_recovery 的 BaseTool 实现。
//
// 编译期接口断言：必须实现 basetool.BaseTool。
var _ basetool.BaseTool = (*VerifyRecoveryTool)(nil)

// VerifyRecoveryTool 是结构化注入版。Querier / StateStore / Config 全部
// 由构造函数注入，不持有任何全局可变状态。
type VerifyRecoveryTool struct {
	querier    MetricQuerier
	stateStore RecoveryStateStore
	log        *slog.Logger
	cfg        VerifyRecoveryConfig
	// sem 是并发限流信号量（nil-safe）；MaxConcurrent<=0 视为无限。
	sem chan struct{}
}

// NewVerifyRecoveryTool 构造一个工具实例。
// querier / stateStore 必填；cfg 为零值时用 DefaultVerifyRecoveryConfig()。
func NewVerifyRecoveryTool(querier MetricQuerier, stateStore RecoveryStateStore, log *slog.Logger, cfg VerifyRecoveryConfig) *VerifyRecoveryTool {
	if log == nil {
		log = slog.Default()
	}
	if cfg.DefaultTolerance == 0 {
		cfg.DefaultTolerance = DefaultTolerance
	}
	if cfg.MaxConcurrent < 0 {
		cfg.MaxConcurrent = 0
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	t := &VerifyRecoveryTool{
		querier:    querier,
		stateStore: stateStore,
		log:        log,
		cfg:        cfg,
	}
	if cfg.MaxConcurrent > 0 {
		t.sem = make(chan struct{}, cfg.MaxConcurrent)
	}
	return t
}

// Info 返回工具元数据（pure read-only）。
func (t *VerifyRecoveryTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameVerifyRecovery,
		Description: VerifyRecoveryDescription,
		WhenToUse:   verifyRecoveryWhenToUse,
		Parameters:  VerifyRecoverySchema,
		Class:       "read",
	}, nil
}

// VerifyOutcome 是工具内部分流后的产出。InvokableRun 返回它的 JSON 序列化。
//
// 字段对齐 loop.VerifiedDelta 的 wire shape，但保留 tool-internal 状态
// （retryCount 来自 StateStore），orchestrator 端再包成 VerifiedDelta 入
// loop_contract。
type VerifyOutcome struct {
	// SchemaVersion 固定 "v1"（McClintock 的 deviation：v1 不是 1.0）。
	SchemaVersion string `json:"schema_version"`
	// Passed 是所有 metric 都不超 tolerance 时的总体结论。
	Passed bool `json:"passed"`
	// FailedMetrics 是超 tolerance 的 metric 名子集；Passed=true 时为空。
	FailedMetrics []string `json:"failed_metrics,omitempty"`
	// Deltas 是每个 metric 的相对偏差 |current-baseline| / baseline。
	Deltas map[string]float64 `json:"deltas"`
	// SampleSize 是 current 窗口样本数（取所有 metric 中最小值，反映
	// 最弱信号；postmortem 用它做"低样本告警"）。
	SampleSize int `json:"sample_size"`
	// Tolerance 是实际生效的 tolerance（user override > metric default > 全局 default）。
	Tolerance float64 `json:"tolerance"`
	// RetryCount 是当前 incident 的累计重试次数（StateStore 查询）。
	RetryCount int `json:"retry_count"`
	// WarningLevel 三档：pass / warn / fail，与 contract.WarningLevel 对齐。
	WarningLevel string `json:"warning_level"`
	// SeverityEscalated 表示 retry_count 超过 MaxRetryCount 后的强制升级。
	SeverityEscalated bool `json:"severity_escalated"`
}

// InvokableRun 是 BaseTool 的执行入口。
//
// 步骤：
//  1. JSON 解码 + fail-fast 校验（allowlist / tolerance / window）
//  2. 取 retry_count（StateStore）
//  3. 并发抓每个 metric 的 baseline + current（限流）
//  4. 计算每个 metric 的相对偏差 + warning_level + passed
//  5. 超 MaxRetryCount 时 SeverityEscalated=true
//  6. 序列化 VerifyOutcome 返回
//
// 错误处理：args 校验失败 / IO 失败时，error 链包含 base error；
// Passed / Deltas 等结构化字段只输出 OK 的部分，不暴露内部 IO 错误给 LLM
// （error 链带 %w 即可，LLM 也能读到错误字符串）。
func (t *VerifyRecoveryTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	if t.querier == nil {
		return "", fmt.Errorf("%w: querier not configured", ErrArgsInvalid)
	}
	if t.stateStore == nil {
		return "", fmt.Errorf("%w: state store not configured", ErrArgsInvalid)
	}

	args, err := parseAndValidateVerifyArgs(argsJSON)
	if err != nil {
		return "", err
	}

	now := t.cfg.Clock()
	tolerance := args.Tolerance
	if tolerance == 0 {
		tolerance = t.cfg.DefaultTolerance
	}

	retryCount, err := t.stateStore.Get(ctx, args.SkillID)
	if err != nil {
		// retry_count 不可读不应阻塞 verify：降级为 0，记录 warn。
		t.log.Warn("verify_recovery: state store get failed; degrading to 0",
			slog.String("incident_id", args.SkillID),
			slog.String("error", err.Error()))
		retryCount = 0
	}

	deltas := make(map[string]float64, len(args.Metrics))
	failed := make([]string, 0)
	minSample := 0
	for _, m := range args.Metrics {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("%w: ctx canceled", err)
		}
		baselineAvg, currentAvg, sample, err := t.queryOne(ctx, args, m, now)
		if err != nil {
			// 单个 metric IO 失败：标 failed 并带 metric 名到 error 链。
			return "", fmt.Errorf("%w: metric=%s: %w", ErrArgsInvalid, m, err)
		}
		rel := relativeDeviation(baselineAvg, currentAvg)
		deltas[m] = rel

		metricTol := tolerance
		if spec, ok := MetricSpecTable[m]; ok && spec.DefaultTolerance > 0 && tolerance == t.cfg.DefaultTolerance {
			// 用户未显式传 tolerance 时，用 metric 自身 default（design §A.3）。
			metricTol = spec.DefaultTolerance
		}
		// design §A.3: passed = all metric status != "fail"
		//   status = "fail" when delta > 2*tolerance
		//   status = "warn" when tolerance < delta <= 2*tolerance (NOT failed)
		//   status = "pass" when delta <= tolerance (NOT failed)
		if rel > 2*metricTol {
			failed = append(failed, m)
		}
		if minSample == 0 || sample < minSample {
			minSample = sample
		}
	}

	warning := classifyWarningLevel(deltas, args.Metrics, tolerance)
	passed := len(failed) == 0

	out := VerifyOutcome{
		SchemaVersion:     loop.ContractSchemaV1,
		Passed:            passed,
		FailedMetrics:     failed,
		Deltas:            deltas,
		SampleSize:        minSample,
		Tolerance:         tolerance,
		RetryCount:        retryCount,
		WarningLevel:      warning,
		SeverityEscalated: retryCount > MaxRetryCount,
	}

	// 校验产出对齐 loop.VerifiedDelta contract，避免 orchestrator 端再 fail。
	// 这里用 InternalVerifiedDelta 是为了复用 ValidateVerifiedDelta 但不
	// 把 StateStore 内部状态泄漏到 loop 包。
	vd := loop.VerifiedDelta{
		SchemaVersion: out.SchemaVersion,
		Passed:        out.Passed,
		FailedMetrics: out.FailedMetrics,
		Deltas:        out.Deltas,
		SampleSize:    out.SampleSize,
		Tolerance:     out.Tolerance,
		RetryCount:    out.RetryCount,
		WarningLevel:  out.WarningLevel,
	}
	if err := loop.ValidateVerifiedDelta(&vd); err != nil {
		return "", fmt.Errorf("%w: produced VerifiedDelta invalid: %w", ErrArgsInvalid, err)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("%w: marshal outcome: %w", ErrArgsInvalid, err)
	}
	return string(raw), nil
}

// queryOne 抓单个 metric 的 baseline + current，带并发限流。
func (t *VerifyRecoveryTool) queryOne(ctx context.Context, args VerifyRecoveryArgs, metric string, now time.Time) (float64, float64, int, error) {
	if t.sem != nil {
		select {
		case t.sem <- struct{}{}:
			defer func() { <-t.sem }()
		case <-ctx.Done():
			return 0, 0, 0, ctx.Err()
		}
	}
	bw, _ := time.ParseDuration(args.BaselineWindow)
	cw, _ := time.ParseDuration(args.CompareWindow)
	req := MetricQueryRequest{
		Target:         args.Target,
		ResourceType:   args.ResourceType,
		Metric:         metric,
		BaselineWindow: bw,
		CompareWindow:  cw,
		Now:            now,
	}
	res, err := t.querier.QueryMetric(ctx, req)
	if err != nil {
		return 0, 0, 0, err
	}
	return res.BaselineAvg, res.CurrentAvg, res.SampleSize, nil
}

// parseAndValidateVerifyArgs 解码 + 严格校验 argsJSON。
// 任一校验失败：返回 (zero, error) 并通过 %w 链接 base error。
func parseAndValidateVerifyArgs(argsJSON string) (VerifyRecoveryArgs, error) {
	var args VerifyRecoveryArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: bad json: %w", ErrArgsInvalid, err)
	}
	if args.SkillID == "" {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: skill_id required", ErrArgsInvalid)
	}
	if args.Target == "" {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: target required", ErrArgsInvalid)
	}
	if args.ResourceType == "" {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: resource_type required", ErrArgsInvalid)
	}
	if _, ok := ResourceMetricsAllowed[args.ResourceType]; !ok {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: resource_type=%q", ErrResourceTypeUnknown, args.ResourceType)
	}
	if len(args.Metrics) == 0 {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: metrics array empty", ErrArgsInvalid)
	}
	if args.BaselineWindow == "" {
		args.BaselineWindow = DefaultBaselineWindow.String()
	}
	if args.CompareWindow == "" {
		args.CompareWindow = DefaultCompareWindow.String()
	}
	baselineD, err := time.ParseDuration(args.BaselineWindow)
	if err != nil {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: baseline_window=%q parse: %w", ErrWindowInvalid, args.BaselineWindow, err)
	}
	if baselineD <= 0 || baselineD > WindowMax {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: baseline_window=%s out of (0, 1h]", ErrWindowInvalid, baselineD)
	}
	compareD, err := time.ParseDuration(args.CompareWindow)
	if err != nil {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: compare_window=%q parse: %w", ErrWindowInvalid, args.CompareWindow, err)
	}
	if compareD <= 0 || compareD > WindowMax {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: compare_window=%s out of (0, 1h]", ErrWindowInvalid, compareD)
	}
	args.BaselineWindow = baselineD.String()
	args.CompareWindow = compareD.String()
	if args.Tolerance < 0 || args.Tolerance > 1 {
		return VerifyRecoveryArgs{}, fmt.Errorf("%w: tolerance=%v", ErrToleranceOutOfRange, args.Tolerance)
	}
	// allowlist + per-resource 子集校验，fail-fast 在 IO 之前。
	for _, m := range args.Metrics {
		if !isAllowedMetric(m) {
			return VerifyRecoveryArgs{}, fmt.Errorf("%w: metric=%q (allowed=%v)", ErrMetricNotAllowed, m, AllowedVerifyMetrics)
		}
		if !ResourceMetricsAllowed[args.ResourceType][m] {
			return VerifyRecoveryArgs{}, fmt.Errorf("%w: metric=%q not in resource_type=%q subset", ErrMetricNotAllowed, m, args.ResourceType)
		}
	}
	return args, nil
}

// isAllowedMetric 是 O(1) allowlist 查询。
func isAllowedMetric(m string) bool {
	_, ok := MetricSpecTable[m]
	return ok
}

// relativeDeviation 计算 |current - baseline| / max(baseline, eps)。
// baseline_avg=0 时退化为 EpsilonBaseline 防 NaN。
func relativeDeviation(baseline, current float64) float64 {
	denom := baseline
	if denom < 0 {
		denom = -denom
	}
	if denom < EpsilonBaseline {
		denom = EpsilonBaseline
	}
	diff := current - baseline
	if diff < 0 {
		diff = -diff
	}
	return diff / denom
}

// classifyWarningLevel 按 design §A.3 三档规则。
//
//	delta_pct <= tolerance               → "pass"
//	tolerance < delta_pct <= 2*tolerance → "warn"
//	delta_pct > 2*tolerance              → "fail"
//
// 整体 warning_level 取所有 metric 中最高档（fail > warn > pass）。
func classifyWarningLevel(deltas map[string]float64, metrics []string, tolerance float64) string {
	worst := "pass"
	for _, m := range metrics {
		d := deltas[m]
		if d > 2*tolerance {
			return "fail"
		}
		if d > tolerance {
			worst = "warn"
		}
	}
	return worst
}

// IncrementRetryCount 暴露给 orchestrator 端：rollback 后调用一次。
//
// 返回值 = 新 retry_count。并发安全（StateStore 内部加锁）。
// 超 MaxRetryCount 时仍递增；caller 据此决定是否升级 severity=dangerous。
func (t *VerifyRecoveryTool) IncrementRetryCount(ctx context.Context, incidentID string) (int, error) {
	if t.stateStore == nil {
		return 0, fmt.Errorf("%w: state store not configured", ErrArgsInvalid)
	}
	return t.stateStore.Increment(ctx, incidentID)
}

// ResetRetryCount 暴露给 orchestrator 端：postmortem sealed 或 run 完成时清零。
func (t *VerifyRecoveryTool) ResetRetryCount(ctx context.Context, incidentID string) error {
	if t.stateStore == nil {
		return fmt.Errorf("%w: state store not configured", ErrArgsInvalid)
	}
	return t.stateStore.Reset(ctx, incidentID)
}

// GetRetryCount 暴露给 orchestrator 端：rollback 决策时读当前值。
func (t *VerifyRecoveryTool) GetRetryCount(ctx context.Context, incidentID string) (int, error) {
	if t.stateStore == nil {
		return 0, nil
	}
	return t.stateStore.Get(ctx, incidentID)
}
