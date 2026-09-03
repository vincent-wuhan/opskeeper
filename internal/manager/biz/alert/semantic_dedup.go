// Package alert - L4 Semantic Dedup（LLM 二次聚合层）
//
// 本文件实现 zero-manual-ops-loop Day 2 Task 2.1：在 L0 静态规则 + L1-L3 抑制后挂
// LLM 二次聚合钩子，把同根因的 incident 合并为同一 group。
//
// 关键约束（设计 B.3 / H.2）：
//   - 异步：worker pool + buffered channel；Enqueue 非阻塞（drop-on-full）
//   - 限流：worker 数 ≤ 4 / queue ≤ 256 / 单次 LLM timeout 8s
//   - 降级：LLM 失败 / circuit open → 返回 Fallback=true，passthrough 不合并
//   - 成本：日 ~200 批 × (3.5k input + 0.6k output) ≈ $4.75/天 → 月 ~$143
//
// 关键不阻塞（设计 H.2 Q2）：semantic_dedup 失败不阻塞 alert pipeline 写入，
// 仅失去合并能力；orchestrator 的 correlated 阶段仅消费已合并结果。
package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 默认 worker pool 参数（设计 B.3）。
const (
	defaultDedupWorkers    = 4
	defaultDedupQueueDepth = 256
	defaultDedupLLMTimeout = 8 * time.Second
	defaultDedupBatchCap   = 20 // 单批最多 incident 数
)

// Circuit breaker 默认参数（设计 B.5）。
const (
	defaultBreakerThreshold = 5 // 连续失败次数
	defaultBreakerCooldown  = 1 * time.Hour
)

// SemanticDedupPrompt LLM 聚合 prompt 模板（设计 B.2 中文版）。
//
// 输出 schema 严格 JSON，无注释；group_id 用短 hex 避免与 incident_id 冲突。
const SemanticDedupPrompt = `你是一个 SRE 告警聚合助手。请判断下面 N 条 alert 是否应合并为同一 incident 群。

## 输入 alerts

%s

## 判定维度（按重要性）

1. 同一资源（device_id / pod_name / pg_cluster）
2. 同一时间窗口（±10 分钟内首次触发）
3. 同一根因类别（资源耗尽 / 网络抖动 / 慢查询 / 锁竞争 / 配置错误 / 部署异常）
4. 同一服务（应用名 / namespace）

## 输出 schema（严格 JSON，无注释）

{
  "groups": [
    {
      "group_id": "grp_<8hex>",
      "alert_indexes": [1, 3],
      "primary_index": 1,
      "shared_root_cause_category": "pg.long_running_tx | redis.oom_eviction | host.cpu_saturation | network.flapping | ...",
      "rationale_short": "<≤40 字中文/英文>"
    }
  ],
  "ungrouped_indexes": [2],
  "confidence": 0.0
}

## 规则

- 严格按 JSON 输出，不要 markdown 代码块
- group_id 用短 hex（避免与 incident_id 冲突）
- primary_index 选最严重的或最早的
- confidence 取 0.0–1.0，反映判定确信度
- 单一 alert 也允许出现在 groups（groups=[{alert_indexes:[1]}], ungrouped_indexes=[]）`

// DedupRequest LLM 聚合请求。
//
// WindowKey 同一批 alert 同 ID（设计 B.3 命名）；用 YYYY-MM-DDTHH:MM ± 5min
// bucket 做聚合窗口标识。
type DedupRequest struct {
	WindowKey   string     // 聚合窗口标识
	Incidents   []Incident // L0-L3 已过滤的 incident 列表
	TriggeredAt time.Time
}

// Incident 跨层传递的最小 incident 字段（避免依赖 alert model.Incident 全字段）。
type Incident struct {
	ID           uint64
	TenantID     uint64
	Rule         string
	Severity     string
	Scope        string
	ScopeType    string
	DeviceID     *uint64
	Summary      string
	FirstFiredAt time.Time
	LastFiredAt  time.Time
	EventCount   uint64
	StaticRuleID string // L0 命中后写入（短路情形）
}

// DedupResult LLM 聚合结果。
type DedupResult struct {
	Groups     []IncidentGroup // 同根因合并组
	Confidence float64         // LLM 自评 0.0-1.0
	Fallback   bool            // true=降级（passthrough）
	Reason     string          // Fallback=true 时填原因：llm_unavailable / circuit_open / timeout / parse_error / queue_full
	BatchID    string          // 同 DedupRequest.WindowKey
	TokenUsed  int             // LLM 调用 token 数（降级时为 0）
	LatencyMs  int             // LLM 调用耗时（毫秒）
}

// IncidentGroup 合并后的事故组。
type IncidentGroup struct {
	GroupID                 string   // "grp_<8hex>"
	AlertIDs                []uint64 // 合并的 incident IDs
	PrimaryIncidentID       uint64   // 主 incident（最严重或最早）
	SharedRootCauseCategory string   // 根因类别（pg.long_running_tx / redis.oom_eviction / ...）
	Rationale               string   // ≤40 字解释
}

// LLMRequest LLM 调用请求（独立接口便于 mock）。
type LLMRequest struct {
	Prompt      string
	Temperature float64
	MaxTokens   int
}

// LLMResponse LLM 调用响应。
type LLMResponse struct {
	Content   string // 严格 JSON 字符串
	TokensIn  int
	TokensOut int
}

// LLMClient LLM provider 抽象。
//
// 生产实现 = chatruntime / internal/pkg/llm.Client；
// 测试用 fakeLLM 固定返回。
type LLMClient interface {
	Complete(ctx context.Context, req LLMRequest) (*LLMResponse, error)
}

// CircuitBreaker 简单熔断器（设计 B.5）。
//
// 状态机：
//
//	closed ──(连续失败≥threshold)──→ open (cooldown)
//	open ──(冷却 cooldown)──→ half-open
//	half-open ──(成功)──→ closed
//	half-open ──(再失败)──→ open (再冷却 cooldown)
//
// Day 2 简化版：仅实现 closed/open 两态；冷却结束自动重置失败计数。
type CircuitBreaker struct {
	mu            sync.Mutex
	failureCount  int
	lastFailureAt time.Time
	cooldown      time.Duration
	threshold     int
	now           func() time.Time // 测试注入
}

// NewCircuitBreaker 默认 5 连续失败 / 1h 冷却。
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		cooldown:  defaultBreakerCooldown,
		threshold: defaultBreakerThreshold,
		now:       time.Now,
	}
}

// WithThreshold 注入自定义阈值（测试）。
func (cb *CircuitBreaker) WithThreshold(t int) *CircuitBreaker {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.threshold = t
	return cb
}

// WithCooldown 注入自定义冷却时长（测试）。
func (cb *CircuitBreaker) WithCooldown(d time.Duration) *CircuitBreaker {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.cooldown = d
	return cb
}

// WithClock 注入固定时钟（测试）。
func (cb *CircuitBreaker) WithClock(now func() time.Time) *CircuitBreaker {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.now = now
	return cb
}

// RecordFailure 记录一次失败；达到阈值则进入 open 态。
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failureCount++
	cb.lastFailureAt = cb.now()
}

// RecordSuccess 记录一次成功；重置失败计数。
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failureCount = 0
}

// IsOpen 是否处于 open 态（拒绝调用）。
//
// 冷却结束后自动重置失败计数并返回 false（half-open 第一次调用允许通过，
// 若再失败则再次进入 open）。
func (cb *CircuitBreaker) IsOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.failureCount < cb.threshold {
		return false
	}
	// 已达阈值，检查冷却
	if cb.now().Sub(cb.lastFailureAt) > cb.cooldown {
		// 冷却结束，重置计数（半开状态；下一次失败会重新进入 open）
		cb.failureCount = 0
		return false
	}
	return true
}

// FailureCount 当前失败计数（测试可读）。
func (cb *CircuitBreaker) FailureCount() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.failureCount
}

// SemanticDedupService LLM 二次聚合服务（设计 B）。
type SemanticDedupService struct {
	workerCount    int           // 默认 4
	queueSize      int           // 默认 256
	llmTimeout     time.Duration // 默认 8s
	batchCap       int           // 默认 20
	llmClient      LLMClient     // 注入
	circuitBreaker *CircuitBreaker
	log            *slog.Logger
	now            func() time.Time

	// 异步队列
	queue chan DedupRequest
	wg    sync.WaitGroup

	// 生命周期
	stopOnce sync.Once
	stopped  chan struct{}

	// 指标（atomic；测试可读）
	droppedTotal   atomic.Int64
	processedTotal atomic.Int64
	fallbackTotal  atomic.Int64
}

// SemanticDedupConfig 注入配置。
type SemanticDedupConfig struct {
	Workers    int
	QueueDepth int
	LLMTimeout time.Duration
	BatchCap   int
	Log        *slog.Logger
	Now        func() time.Time
}

// NewSemanticDedupService 构造服务（必须 Inject LLMClient）。
func NewSemanticDedupService(client LLMClient, cfg SemanticDedupConfig) *SemanticDedupService {
	if client == nil {
		// nil client 时构造"全 fallback"服务；Enqueue 立即返 Fallback=true。
		// 便于 main.go 早期 wiring（LLM 未配置时）。
		return &SemanticDedupService{
			circuitBreaker: NewCircuitBreaker(),
			log:            cfg.Log,
			now:            cfg.Now,
		}
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaultDedupWorkers
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = defaultDedupQueueDepth
	}
	if cfg.LLMTimeout <= 0 {
		cfg.LLMTimeout = defaultDedupLLMTimeout
	}
	if cfg.BatchCap <= 0 {
		cfg.BatchCap = defaultDedupBatchCap
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	s := &SemanticDedupService{
		workerCount:    cfg.Workers,
		queueSize:      cfg.QueueDepth,
		llmTimeout:     cfg.LLMTimeout,
		batchCap:       cfg.BatchCap,
		llmClient:      client,
		circuitBreaker: NewCircuitBreaker(),
		log:            cfg.Log,
		now:            cfg.Now,
		queue:          make(chan DedupRequest, cfg.QueueDepth),
		stopped:        make(chan struct{}),
	}
	for i := 0; i < cfg.Workers; i++ {
		s.wg.Add(1)
		go s.workerLoop()
	}
	return s
}

// Enqueue 非阻塞入队（队列满则丢弃 + metric 计数 +1）。
//
// 返回 (result, true) 立即获得结果（如 circuit open / queue full / nil client 时
// 直接返回 Fallback=true 的结果），或 (nil, false) 表示已成功入队等待异步处理。
//
// 主流程推荐用法：
//
//	if s := dedup.Enqueue(req); s != nil {
//	    // 立即降级
//	} else {
//	    // 已入队，等异步回调 / 持久化轮询
//	}
func (s *SemanticDedupService) Enqueue(req DedupRequest) *DedupResult {
	if s == nil {
		return &DedupResult{Fallback: true, Reason: "service_not_initialized", BatchID: req.WindowKey}
	}
	if s.llmClient == nil {
		return &DedupResult{Fallback: true, Reason: "llm_unavailable", BatchID: req.WindowKey}
	}
	if s.circuitBreaker.IsOpen() {
		s.fallbackTotal.Add(1)
		return &DedupResult{Fallback: true, Reason: "circuit_open", BatchID: req.WindowKey}
	}
	if s.queue == nil {
		// 同步模式（worker pool 未启动）
		res, _ := s.Process(context.Background(), req)
		return res
	}
	select {
	case <-s.stopped:
		return &DedupResult{Fallback: true, Reason: "service_stopped", BatchID: req.WindowKey}
	default:
	}
	select {
	case s.queue <- req:
		s.processedTotal.Add(1)
		return nil
	default:
		// 队列满：drop + metric
		s.droppedTotal.Add(1)
		s.log.Warn("semantic_dedup: queue full, dropping",
			slog.String("batch_id", req.WindowKey),
			slog.Int("queue_depth", s.queueSize))
		return &DedupResult{Fallback: true, Reason: "queue_full", BatchID: req.WindowKey}
	}
}

// Process 同步处理单个 DedupRequest（供测试 / drain 路径用）。
//
// 返回的 DedupResult 始终非 nil（即便 LLM 失败也返 Fallback=true）。
func (s *SemanticDedupService) Process(ctx context.Context, req DedupRequest) (*DedupResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("semantic dedup service not initialized")
	}
	if s.llmClient == nil {
		return &DedupResult{Fallback: true, Reason: "llm_unavailable", BatchID: req.WindowKey}, nil
	}
	if s.circuitBreaker.IsOpen() {
		s.fallbackTotal.Add(1)
		return &DedupResult{Fallback: true, Reason: "circuit_open", BatchID: req.WindowKey}, nil
	}

	// 截断超过 batch cap 的请求（保留最新 cap 条）
	incidents := req.Incidents
	if len(incidents) > s.batchCap {
		s.log.Warn("semantic_dedup: batch exceeds cap, truncating",
			slog.String("batch_id", req.WindowKey),
			slog.Int("original", len(incidents)),
			slog.Int("cap", s.batchCap))
		incidents = incidents[len(incidents)-s.batchCap:]
	}

	prompt := s.renderPrompt(incidents)

	callCtx, cancel := context.WithTimeout(ctx, s.llmTimeout)
	defer cancel()

	start := s.now()
	resp, err := s.llmClient.Complete(callCtx, LLMRequest{
		Prompt:      prompt,
		Temperature: 0,
		MaxTokens:   1024,
	})
	latency := s.now().Sub(start)
	if err != nil {
		s.circuitBreaker.RecordFailure()
		s.fallbackTotal.Add(1)
		reason := "llm_unavailable"
		if callCtx.Err() == context.DeadlineExceeded {
			reason = "timeout"
		}
		s.log.Warn("semantic_dedup: llm call failed",
			slog.String("batch_id", req.WindowKey),
			slog.String("reason", reason),
			slog.Duration("latency", latency),
			slog.Any("err", err))
		return &DedupResult{
			Fallback:  true,
			Reason:    reason,
			BatchID:   req.WindowKey,
			LatencyMs: int(latency / time.Millisecond),
		}, nil
	}

	groups, confidence, err := parseDedupResponse(resp.Content, incidents)
	if err != nil {
		s.circuitBreaker.RecordFailure()
		s.fallbackTotal.Add(1)
		s.log.Warn("semantic_dedup: parse response failed",
			slog.String("batch_id", req.WindowKey),
			slog.Any("err", err))
		return &DedupResult{
			Fallback:  true,
			Reason:    "parse_error",
			BatchID:   req.WindowKey,
			LatencyMs: int(latency / time.Millisecond),
			TokenUsed: resp.TokensIn + resp.TokensOut,
		}, nil
	}

	s.circuitBreaker.RecordSuccess()
	return &DedupResult{
		Groups:     groups,
		Confidence: confidence,
		Fallback:   false,
		BatchID:    req.WindowKey,
		TokenUsed:  resp.TokensIn + resp.TokensOut,
		LatencyMs:  int(latency / time.Millisecond),
	}, nil
}

// renderPrompt 构造 prompt 输入（设计 B.2）。
func (s *SemanticDedupService) renderPrompt(incidents []Incident) string {
	var b strings.Builder
	for i, inc := range incidents {
		fmt.Fprintf(&b, "[%d] rule=%s severity=%s scope=%s/%s summary=%s\n",
			i+1, inc.Rule, inc.Severity, inc.Scope, inc.ScopeType, inc.Summary)
		if inc.DeviceID != nil {
			fmt.Fprintf(&b, "    device_id: %d\n", *inc.DeviceID)
		}
		if inc.StaticRuleID != "" {
			fmt.Fprintf(&b, "    static_rule_id: %s\n", inc.StaticRuleID)
		}
		fmt.Fprintf(&b, "    first_fired_at: %s last_fired_at: %s event_count: %d\n",
			inc.FirstFiredAt.UTC().Format(time.RFC3339),
			inc.LastFiredAt.UTC().Format(time.RFC3339),
			inc.EventCount)
	}
	return fmt.Sprintf(SemanticDedupPrompt, b.String())
}

// parseDedupResponse 解析 LLM JSON 响应。
//
// 容错：忽略 markdown 代码块包装；缺 confidence 字段默认 0.5；group_id 缺失
// 时自动生成短 hex。
func parseDedupResponse(content string, incidents []Incident) ([]IncidentGroup, float64, error) {
	content = strings.TrimSpace(content)
	// 去掉 ```json ... ``` 包装
	if strings.HasPrefix(content, "```") {
		if idx := strings.Index(content, "\n"); idx > 0 {
			content = content[idx+1:]
		}
		if strings.HasSuffix(content, "```") {
			content = content[:len(content)-3]
		}
		content = strings.TrimSpace(content)
	}
	if content == "" {
		return nil, 0, fmt.Errorf("empty LLM response")
	}

	var raw struct {
		Groups []struct {
			GroupID                 string `json:"group_id"`
			AlertIndexes            []int  `json:"alert_indexes"`
			PrimaryIndex            int    `json:"primary_index"`
			SharedRootCauseCategory string `json:"shared_root_cause_category"`
			Rationale               string `json:"rationale_short"`
		} `json:"groups"`
		UngroupedIndexes []int   `json:"ungrouped_indexes"`
		Confidence       float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return nil, 0, fmt.Errorf("unmarshal response: %w", err)
	}

	groups := make([]IncidentGroup, 0, len(raw.Groups))
	for _, g := range raw.Groups {
		alertIDs := make([]uint64, 0, len(g.AlertIndexes))
		for _, idx := range g.AlertIndexes {
			if idx >= 1 && idx <= len(incidents) {
				alertIDs = append(alertIDs, incidents[idx-1].ID)
			}
		}
		var primaryID uint64
		if g.PrimaryIndex >= 1 && g.PrimaryIndex <= len(incidents) {
			primaryID = incidents[g.PrimaryIndex-1].ID
		} else if len(alertIDs) > 0 {
			primaryID = alertIDs[0]
		}
		groups = append(groups, IncidentGroup{
			GroupID:                 g.GroupID,
			AlertIDs:                alertIDs,
			PrimaryIncidentID:       primaryID,
			SharedRootCauseCategory: g.SharedRootCauseCategory,
			Rationale:               g.Rationale,
		})
	}

	confidence := raw.Confidence
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	return groups, confidence, nil
}

// workerLoop worker pool 主循环。
func (s *SemanticDedupService) workerLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stopped:
			return
		case req, ok := <-s.queue:
			if !ok {
				return
			}
			// worker 上下文：超时 = llmTimeout
			ctx, cancel := context.WithTimeout(context.Background(), s.llmTimeout)
			_, _ = s.Process(ctx, req)
			cancel()
		}
	}
}

// Close 优雅关闭 worker pool。
func (s *SemanticDedupService) Close() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.queue != nil {
			close(s.stopped)
			close(s.queue)
		}
	})
	s.wg.Wait()
}

// Metrics 指标快照（测试 / debug 用）。
type DedupMetrics struct {
	ProcessedTotal int64
	DroppedTotal   int64
	FallbackTotal  int64
	QueueLen       int
	WorkerCount    int
}

// Metrics 返回当前指标。
func (s *SemanticDedupService) Metrics() DedupMetrics {
	if s == nil {
		return DedupMetrics{}
	}
	ql := 0
	if s.queue != nil {
		ql = len(s.queue)
	}
	return DedupMetrics{
		ProcessedTotal: s.processedTotal.Load(),
		DroppedTotal:   s.droppedTotal.Load(),
		FallbackTotal:  s.fallbackTotal.Load(),
		QueueLen:       ql,
		WorkerCount:    s.workerCount,
	}
}

// CircuitBreaker 暴露（测试可断言）。
func (s *SemanticDedupService) CircuitBreaker() *CircuitBreaker {
	if s == nil {
		return nil
	}
	return s.circuitBreaker
}
