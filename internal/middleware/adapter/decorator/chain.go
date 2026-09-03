package decorator

import (
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// ChainConfig 是装饰器链配置。
type ChainConfig struct {
	AuditSink AuditSink
	Limiter   Limiter
	Metrics   *Metrics
	Timeout   TimeoutConfig
	// 顺序：timeout → ratelimit → audit → metrics → handler
	// 这样 metrics 包含全部耗时（含 timeout cancel）
	// audit 记录最终结果（成功 / 失败）
}

// TimeoutConfig 超时配置（per-tool 可覆盖）。
type TimeoutConfig struct {
	Default time.Duration
	PerTool map[string]time.Duration
}

// NewChainConfig 创建默认配置。
func NewChainConfig() ChainConfig {
	return ChainConfig{
		AuditSink: &SlogAuditSink{Logger: slog.Default()},
		Limiter:   NoopLimiter{},
		Metrics:   NewMetrics(),
		Timeout: TimeoutConfig{
			Default: DefaultTimeout,
			PerTool: map[string]time.Duration{},
		},
	}
}

// WrapTool 对单个 Tool 应用完整装饰器链。
//
// 顺序：Metrics → Audit → RateLimit → Timeout → handler（外到内）
// 实际调用：
//  1. MetricsDecorator 开始计时
//  2. AuditDecorator 开始审计
//  3. RateLimitDecorator 限流检查
//  4. TimeoutDecorator 设置 deadline
//  5. handler 实际执行
func WrapTool(t registry.Tool, cfg ChainConfig) registry.Tool {
	h := t.Handler
	// Timeout（最内层）
	timeout := cfg.Timeout.Default
	if v, ok := cfg.Timeout.PerTool[t.Name]; ok {
		timeout = v
	}
	h = WrapTimeout(h, timeout)
	// RateLimit
	if cfg.Limiter != nil {
		h = WrapRateLimit(h, cfg.Limiter)
	}
	// Audit
	if cfg.AuditSink != nil {
		h = WrapAudit(h, cfg.AuditSink)
	}
	// Metrics（最外层，统计总耗时）
	if cfg.Metrics != nil {
		h = WrapMetrics(h, cfg.Metrics)
	}
	t.Handler = h
	return t
}

// WrapAll 对 Registry 中所有 Tool 应用装饰器链。
func WrapAll(reg *registry.Registry, cfg ChainConfig) {
	for _, name := range reg.ListTools("") {
		t, ok := reg.GetTool(name)
		if !ok {
			continue
		}
		reg.ReplaceTool(WrapTool(t, cfg))
	}
}
