package decorator

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// Metrics 是 Adapter Tool 调用指标收集器。
//
// 接口与 Prometheus 兼容（counter / histogram）；骨架实现使用 atomic
// 计数器，避免在测试中拉起 Prometheus。生产实现可替换为 prometheus
// CounterVec / HistogramVec。
type Metrics struct {
	mu        sync.RWMutex
	counters  map[string]*int64 // tool → 调用次数
	errors    map[string]*int64 // tool → 错误次数
	durations map[string]*int64 // tool → 累计耗时（ms）
	count     int64             // 总调用次数
}

// NewMetrics 创建 Metrics。
func NewMetrics() *Metrics {
	return &Metrics{
		counters:  make(map[string]*int64),
		errors:    make(map[string]*int64),
		durations: make(map[string]*int64),
	}
}

// Record 记录一次调用。
func (m *Metrics) Record(tool string, durationMs int64, err error) {
	atomic.AddInt64(&m.count, 1)

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.counters[tool]; !ok {
		var c, e, d int64
		m.counters[tool] = &c
		m.errors[tool] = &e
		m.durations[tool] = &d
	}
	atomic.AddInt64(m.counters[tool], 1)
	atomic.AddInt64(m.durations[tool], durationMs)
	if err != nil {
		atomic.AddInt64(m.errors[tool], 1)
	}
}

// Snapshot 返回指标快照。
type Snapshot struct {
	TotalCalls  int64
	ToolMetrics map[string]ToolMetric
}

// ToolMetric 单个工具的指标。
type ToolMetric struct {
	Calls         int64
	Errors        int64
	DurationTotal int64
	AvgDurationMs float64
}

// Snapshot 生成快照。
func (m *Metrics) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snap := Snapshot{
		TotalCalls:  atomic.LoadInt64(&m.count),
		ToolMetrics: make(map[string]ToolMetric),
	}
	for tool, counter := range m.counters {
		calls := atomic.LoadInt64(counter)
		errs := atomic.LoadInt64(m.errors[tool])
		dur := atomic.LoadInt64(m.durations[tool])
		var avg float64
		if calls > 0 {
			avg = float64(dur) / float64(calls)
		}
		snap.ToolMetrics[tool] = ToolMetric{
			Calls:         calls,
			Errors:        errs,
			DurationTotal: dur,
			AvgDurationMs: avg,
		}
	}
	return snap
}

// MetricsDecorator 包装 inner 工具方法，记录调用指标。
type MetricsDecorator struct {
	Inner   ToolHandler
	Metrics *Metrics
}

// Handle 执行并记录。
func (m *MetricsDecorator) Handle(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	tool := toolNameFromArgs(args)
	started := time.Now()
	result, err := m.Inner(ctx, args)
	duration := time.Since(started).Milliseconds()
	m.Metrics.Record(tool, duration, err)
	return result, err
}

// WrapMetrics 装饰器工厂。
func WrapMetrics(inner ToolHandler, m *Metrics) ToolHandler {
	return (&MetricsDecorator{Inner: inner, Metrics: m}).Handle
}

// ApplyMetrics 装饰单个 Tool。
func ApplyMetrics(t registry.Tool, m *Metrics) registry.Tool {
	t.Handler = WrapMetrics(t.Handler, m)
	return t
}
