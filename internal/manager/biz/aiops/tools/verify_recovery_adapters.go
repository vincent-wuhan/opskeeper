// Package tools — verify_recovery_adapters.go
//
// Adapters 把 aiops/tools 的 verify_recovery tool 桥接到
// biz/loop.VerifyRecoveryCaller narrow interface + biz/loop.RecoveryStateStore。
// 这样 biz/loop 不需要 import aiops/tools 的具体类型（保留 monorepo
// 边界），cmd/opskeeper/main.go 只需一次 wire-up。
//
// 设计：
//   - VerifyRecoveryCallerAdapter 包 InvokeVerifyRecovery(argsJSON) →
//     tool.InvokableRun(ctx, argsJSON)。
//   - RecoveryStateStoreAdapter 包 Get/Increment/Reset → tool's
//     in-memory store equivalents (Test 集成用)。
//   - NewDryRunMetricQuerier 给 dry-run 一个返回合成值的 querier，
//     让 recovered phase 在 metric 抓取层"软失败"时不阻塞整个
//     七阶段流水线（verifier 仍能产出 VerifiedDelta）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VerifyRecoveryCallerAdapter 把 *VerifyRecoveryTool 适配为
// biz/loop.VerifyRecoveryCaller narrow interface。
//
// 字段：
//   - Tool: 必填；nil 时 InvokeVerifyRecovery 返回 error。
//
// 注意：loop.VerifyRecoveryCaller 接口在 biz/loop 包内定义；本类型
// 只实现同名方法，依赖 main.go 显式 cast（见 cmd/opskeeper/main.go 的
// `loopWorkers, err := ... VerifyCaller: aiopstools.VerifyRecoveryCallerAdapter{...}`）。
type VerifyRecoveryCallerAdapter struct {
	Tool *VerifyRecoveryTool
}

// InvokeVerifyRecovery 实现 loop.VerifyRecoveryCaller。
func (a VerifyRecoveryCallerAdapter) InvokeVerifyRecovery(ctx context.Context, argsJSON string) (string, error) {
	if a.Tool == nil {
		return "", ErrVerifyRecoveryToolNil
	}
	return a.Tool.InvokableRun(ctx, argsJSON)
}

// ErrVerifyRecoveryToolNil is returned when the adapter is wired with
// a nil Tool. Mirrors the production wire-up discipline (no silent
// default behaviour).
var ErrVerifyRecoveryToolNil = newError("verify_recovery adapter: nil Tool")

// NewError is a tiny helper to avoid the errors import dance.
func newError(s string) error { return &simpleErr{s: s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

// RecoveryStateStoreAdapter 把 *InMemoryRecoveryStateStore 适配为
// biz/loop.RecoveryStateStore narrow interface。
//
// 字段：
//   - Inner: 必填；指向 InMemoryRecoveryStateStore（cmdpolicy 持久化
//     之前的 dry-run 存储）。生产环境替换为 DB-backed store。
type RecoveryStateStoreAdapter struct {
	Inner *InMemoryRecoveryStateStore
}

// Get 实现 loop.RecoveryStateStore。
func (a RecoveryStateStoreAdapter) Get(ctx context.Context, incidentID string) (int, error) {
	if a.Inner == nil {
		return 0, ErrStateStoreNil
	}
	return a.Inner.Get(ctx, incidentID)
}

// Increment 实现 loop.RecoveryStateStore。
func (a RecoveryStateStoreAdapter) Increment(ctx context.Context, incidentID string) (int, error) {
	if a.Inner == nil {
		return 0, ErrStateStoreNil
	}
	return a.Inner.Increment(ctx, incidentID)
}

// Reset 实现 loop.RecoveryStateStore。
func (a RecoveryStateStoreAdapter) Reset(ctx context.Context, incidentID string) error {
	if a.Inner == nil {
		return ErrStateStoreNil
	}
	return a.Inner.Reset(ctx, incidentID)
}

// ErrStateStoreNil is returned when the adapter is wired with a nil
// Inner store.
var ErrStateStoreNil = newError("recovery state store adapter: nil Inner")

// DryRunMetricQuerier 是一个返回合成值的 MetricQuerier 实现。它让
// recovered phase 在 production metric 后端未接好前也能完整跑通，
// 产出 baseline_avg=1.0 / compare_avg=1.0 / delta=0.0 的 VerifiedDelta，
// verifier 报告 passed=true。
//
// 生产环境替换为真正的 PromQL querier（host.cpu_usage → PromQL
// `host_cpu_usage_avg{target=...}` 等映射规则见 Day 4 design §A.3）。
type DryRunMetricQuerier struct {
	mu sync.Mutex
}

// NewDryRunMetricQuerier 构造 DryRunMetricQuerier。
func NewDryRunMetricQuerier() *DryRunMetricQuerier {
	return &DryRunMetricQuerier{}
}

// QueryMetric 实现 MetricQuerier。返回 baseline=1.0, compare=1.0,
// delta=0.0, sample_size=1, no error.
func (q *DryRunMetricQuerier) QueryMetric(_ context.Context, _ MetricQueryRequest) (MetricQueryResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = time.Now()
	return MetricQueryResult{
		BaselineAvg: 1.0,
		CurrentAvg:  1.0,
		SampleSize:  1,
	}, nil
}

// =====================================================================
// PrometheusMetricQuerier — MetricQuerier 的真实 PromQL 实现
// 设计依据：docs/superpowers/specs/2026-08-12-llm-worker-integration-design.md §6
// =====================================================================

// 默认 PromQL 单次查询超时。range query 30s 是业内 default；本实现只打
// instant query（avg_over_time），30s 足够覆盖网络抖动 + PromEngine 评估。
const defaultPromQueryTimeout = 30 * time.Second

// PrometheusMetricQuerier 是 MetricQuerier 的真实 PromQL 实现。
//
// 端点兼容：VictoriaMetrics / Prometheus / Thanos 全部支持
// /api/v1/query（{status, data:{resultType, result}}），同一份 converter
// 适用；不引入 client_golang、只用 stdlib net/http + encoding/json 避免
// 新增 go.mod 依赖。如未来需要 pool / retry / 数据压缩，再切到
// internal/pkg/promquery.Client。
//
// 复用基线：
//   - MetricSpecTable（cpu_usage / mem_usage / qps / latency_p99 → Source="promql"）；
//     任何 Source != "promql" 的 spec 直接 ErrMetricNotSupported，不发 IO。
//   - MetricQueryRequest 的 BaselineWindow / CompareWindow / Now 字段。
//
// PromQL 模板（design §6.2 + §A.3）：
//
//	baseline = avg_over_time(<metric>{target="<target>"}[<baseline_window>]) offset <compare_window>
//	current  = avg_over_time(<metric>{target="<target>"}[<compare_window>])
//
// 两条都跑在 /api/v1/query instant endpoint，evaluation time = Now。
// offset 写法避免 baseline 的窗口跨越到 compare_window（保证 baseline 严格
// 在告警前 compare_window 之前）。
//
// 错误分类（满足 spec §"metric adapter" Scenario）：
//   - endpoint 未配置 / metric 不支持 → 客户端错误（不发 IO）
//   - 不可达 / 网络错 → wrapped network error
//   - 5xx              → wrapped upstream error（含 status code）
//   - 4xx（含 Prom 业务错）→ wrapped query error（PromEnvelope.errorType/error）
//   - 非 JSON / 解析失败 → wrapped parse error
type PrometheusMetricQuerier struct {
	endpoint   string       // base URL，如 http://prometheus:9090（不带 /api/v1/query 后缀）
	httpClient *http.Client // 注入的 client；nil-safe 自动落到 defaultPromQueryTimeout
	auth       string       // Bearer token（可选）；空字符串时不加 Authorization 头
}

// NewPrometheusMetricQuerier 构造一个 querier。
//   - endpoint: Prometheus / VictoriaMetrics base URL（必填）。
//   - auth: 可选 Bearer token；空字符串表示无认证。
//   - httpClient: 可选；nil 时用 defaultPromQueryTimeout 的默认 client。
func NewPrometheusMetricQuerier(endpoint, auth string, httpClient *http.Client) *PrometheusMetricQuerier {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultPromQueryTimeout}
	}
	return &PrometheusMetricQuerier{
		endpoint:   strings.TrimRight(endpoint, "/"),
		httpClient: httpClient,
		auth:       auth,
	}
}

// ErrPromEndpointEmpty 在 QueryMetric 入口检查；空 endpoint 不发 IO。
var ErrPromEndpointEmpty = newError("prometheus query: endpoint is empty")

// ErrMetricNotSupported 标识 MetricSpecTable 中无该 (metric, resource_type) 映射，
// 或 Source != "promql"。与 base tool 的 ErrMetricNotAllowed 区分：前者是
// metric 名合法但本 adapter 不支持，后者是 metric 名本身不在 allowlist。
var ErrMetricNotSupported = newError("prometheus query: metric not supported for adapter")

// promMetricNameTable 把 (resource_type, metric) 映射到 exporter 实际的
// PromQL metric 名。key 用 "resource_type:metric" 复合键，避免双层 map
// 显式初始化复杂度。未命中时 lookupPromMetricName 返回 ("", false)，
// 由 QueryMetric 报 ErrMetricNotSupported。
//
// 命名约定：resource 段优先短名（node/container/pg/redis/app），
// 形态对齐常见 exporter（node_exporter / postgres_exporter /
// redis_exporter / 自研 app 业务指标）。非穷举映射，未列出的
// (resource, metric) 组合由调用方报错回 orchestrator，让 harness 能
// 显式 fail 而不是静默错位。
var promMetricNameTable = map[string]string{
	// host
	"host:cpu_usage": "node_cpu_usage_percent",
	"host:mem_usage": "node_memory_usage_percent",
	// k8s
	"k8s:cpu_usage": "container_cpu_usage_percent",
	"k8s:mem_usage": "container_memory_usage_percent",
	// pg
	"pg:cpu_usage":   "pg_cpu_usage_percent",
	"pg:mem_usage":   "pg_memory_usage_percent",
	"pg:qps":         "pg_stat_activity_requests_per_second",
	"pg:latency_p99": "pg_query_latency_p99_milliseconds",
	// redis
	"redis:mem_usage":   "redis_memory_usage_percent",
	"redis:qps":         "redis_commands_per_second",
	"redis:latency_p99": "redis_command_latency_p99_milliseconds",
	// app
	"app:qps":         "app_requests_per_second",
	"app:latency_p99": "app_request_latency_p99_milliseconds",
}

// lookupPromMetricName 返回 (promql metric name, ok)。未命中时
// QueryMetric 报 ErrMetricNotSupported。
func lookupPromMetricName(resourceType, metric string) (string, bool) {
	name, ok := promMetricNameTable[resourceType+":"+metric]
	return name, ok
}

// QueryMetric 实现 MetricQuerier。一次调用同时跑 baseline + current 两条
// PromQL（串行，便于把 5xx 错误归到第一条失败的查询），返回 MetricQueryResult。
//
// 流程：
//  1. entry check：endpoint 非空、spec 存在、(metric, resource_type) 有映射
//  2. baseline / current PromQL 拼装
//  3. /api/v1/query?query=<expr>&time=<now>  GET（带可选 Authorization）
//  4. 响应解析：PromEnvelope → vector 平均 → MetricQueryResult
func (q *PrometheusMetricQuerier) QueryMetric(ctx context.Context, req MetricQueryRequest) (MetricQueryResult, error) {
	if q == nil || q.endpoint == "" {
		return MetricQueryResult{}, ErrPromEndpointEmpty
	}
	spec, ok := MetricSpecTable[req.Metric]
	if !ok || spec.Source != "promql" {
		return MetricQueryResult{}, fmt.Errorf("%w: metric=%q source=%q", ErrMetricNotSupported, req.Metric, spec.Source)
	}
	metricName, ok := lookupPromMetricName(req.ResourceType, req.Metric)
	if !ok {
		return MetricQueryResult{}, fmt.Errorf("%w: resource_type=%q metric=%q", ErrMetricNotSupported, req.ResourceType, req.Metric)
	}
	if req.BaselineWindow <= 0 || req.CompareWindow <= 0 {
		return MetricQueryResult{}, fmt.Errorf("%w: window must be positive (baseline=%s compare=%s)", ErrMetricNotSupported, req.BaselineWindow, req.CompareWindow)
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	baselineExpr := fmt.Sprintf(
		`avg_over_time(%s{target=%q}[%s]) offset %s`,
		metricName, req.Target, formatPromDuration(req.BaselineWindow), formatPromDuration(req.CompareWindow),
	)
	currentExpr := fmt.Sprintf(
		`avg_over_time(%s{target=%q}[%s])`,
		metricName, req.Target, formatPromDuration(req.CompareWindow),
	)

	baselineAvg, baselineSample, err := q.runInstantQuery(ctx, baselineExpr, now)
	if err != nil {
		return MetricQueryResult{}, fmt.Errorf("prometheus query: baseline: %w", err)
	}
	currentAvg, currentSample, err := q.runInstantQuery(ctx, currentExpr, now)
	if err != nil {
		return MetricQueryResult{}, fmt.Errorf("prometheus query: current: %w", err)
	}

	// sample_size 取 current 窗口的样本数（与 design §A.3 一致）；
	// current 缺失时回落到 baselineSample，保持 postmortem 有信号。
	if currentSample == 0 {
		currentSample = baselineSample
	}
	return MetricQueryResult{
		BaselineAvg: baselineAvg,
		CurrentAvg:  currentAvg,
		SampleSize:  currentSample,
	}, nil
}

// formatPromDuration 把 time.Duration 渲染成 PromQL [N] 内的字符串。
// Go 默认的 time.Duration.String() 形如 "5m0s"，PromQL 兼容；为了
// 短窗（< 1s）输出稳定，特意取整到秒。
func formatPromDuration(d time.Duration) string {
	secs := int64(d / time.Second)
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("%ds", secs)
}

// promEnvelope 是 Prometheus /api/v1/query 响应的 wire envelope。
// status="success" 时 data 才有内容；error 时走 errorType / error。
type promEnvelope struct {
	Status    string          `json:"status"`
	Data      *promVectorData `json:"data,omitempty"`
	ErrorType string          `json:"errorType,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// promVectorData 是 instant query 的 data 段，resultType 必须是 "vector"。
// 我们忽略 ResultType 字段（client 端已知是 "vector"），只解析 result。
type promVectorData struct {
	ResultType string       `json:"resultType"`
	Result     []promSample `json:"result"`
}

// promSample 是 instant vector 的一条样本。Value 是字符串形式的浮点
// （Prom 规定带引号），parse 时用 ParseFloat。
type promSample struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"` // [unix_ts_string, value_string]
}

// runInstantQuery 打到 /api/v1/query，解析响应为 avg + sample_size。
// 5xx / 4xx / parse 失败分别 wrap 成不同错误类型，调用方用 errors.Is 分类。
func (q *PrometheusMetricQuerier) runInstantQuery(ctx context.Context, expr string, ts time.Time) (avg float64, sampleSize int, err error) {
	u, err := url.Parse(q.endpoint + "/api/v1/query")
	if err != nil {
		return 0, 0, fmt.Errorf("build url: %w", err)
	}
	q2 := u.Query()
	q2.Set("query", expr)
	if !ts.IsZero() {
		q2.Set("time", strconv.FormatFloat(float64(ts.UnixNano())/1e9, 'f', -1, 64))
	}
	u.RawQuery = q2.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, 0, fmt.Errorf("build request: %w", err)
	}
	if q.auth != "" {
		req.Header.Set("Authorization", "Bearer "+q.auth)
	}

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("network: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode >= 500 {
		return 0, 0, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncateErrBody(string(body), 256))
	}

	var env promEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, 0, fmt.Errorf("parse json: %w", err)
	}
	if resp.StatusCode >= 400 || env.Status == "error" {
		return 0, 0, fmt.Errorf("query error: type=%s msg=%s", env.ErrorType, env.Error)
	}
	if env.Data == nil {
		return 0, 0, fmt.Errorf("parse: missing data")
	}
	if env.Data.ResultType != "vector" {
		return 0, 0, fmt.Errorf("parse: expected resultType=vector, got %q", env.Data.ResultType)
	}

	samples := env.Data.Result
	if len(samples) == 0 {
		// 空 vector = 目标不存在 / 无样本；调用方拿到 0,0 自行 decide
		// 不算协议错误（Prom 200 OK + data.result=[] 是合法）。
		return 0, 0, nil
	}

	sum := 0.0
	for _, s := range samples {
		if len(s.Value) < 2 {
			continue
		}
		vStr, ok := s.Value[1].(string)
		if !ok {
			continue
		}
		v, parseErr := strconv.ParseFloat(vStr, 64)
		if parseErr != nil {
			continue
		}
		sum += v
	}
	return sum / float64(len(samples)), len(samples), nil
}

// truncateErrBody 把长字符串收尾到 maxLen 内，超出加 "..."。用于把上游
// 错误原文控制在一行内，避免 metric log 爆 buffer。
func truncateErrBody(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
