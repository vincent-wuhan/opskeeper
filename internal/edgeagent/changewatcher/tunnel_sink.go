package changewatcher

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/prom"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tunnel"
)

// DefaultSinkConfig 是 TunnelSink 的默认参数。测试可覆写。
type DefaultSinkConfig struct {
	BatchSize     int           // 单批最大事件数；默认 100
	BufferSize    int           // 内部缓冲 chan 容量；默认 BatchSize * 2
	FlushInterval time.Duration // ticker 周期；默认 5s
	CallTimeout   time.Duration // 每次 Call 的 ctx 超时；默认 10s
}

func (c *DefaultSinkConfig) apply() {
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.BufferSize <= 0 {
		c.BufferSize = c.BatchSize * 2
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 5 * time.Second
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 10 * time.Second
	}
}

// TunnelSinkSinkConfig 是带名字的别名以避免与类型名冲突。
type TunnelSinkSinkConfig = DefaultSinkConfig

// TunnelSink 把 changewatcher 的 ChangeEvent 批量推送到 manager。
//
// 设计要点：
//   - Push 非阻塞, buf 满则 drop oldest 并累加 drop counter (可观测)
//   - 后台 Run goroutine 周期 flush 或满批 flush
//   - Close 强制 drain 残余, 不丢最后一波事件
//   - 失败 call 不阻塞 watch, 仅 log warn
type TunnelSink struct {
	client tunnel.Client
	logger *slog.Logger

	batchSize     int
	bufSize       int
	flushInterval time.Duration
	callTimeout   time.Duration

	buf     chan ChangeEvent
	pushMu  sync.Mutex
	dropped atomic.Uint64
	flushed atomic.Uint64
	closed  atomic.Bool

	// flushNow 用来在测试或 Close 时强制 flush.
	flushNow chan struct{}
}

// NewTunnelSink 构造一个 sink. 需在 Start 前调 Run 启动后台 flush 循环.
func NewTunnelSink(client tunnel.Client, logger *slog.Logger, cfg TunnelSinkSinkConfig) *TunnelSink {
	cfg.apply()
	if logger == nil {
		logger = slog.Default()
	}
	return &TunnelSink{
		client:        client,
		logger:        logger,
		batchSize:     cfg.BatchSize,
		bufSize:       cfg.BufferSize,
		flushInterval: cfg.FlushInterval,
		callTimeout:   cfg.CallTimeout,
		buf:           make(chan ChangeEvent, cfg.BufferSize),
		flushNow:      make(chan struct{}, 1),
	}
}

// Push 把事件放入缓冲. buf 满时 drop oldest 并累加 drop counter, 自身不阻塞.
func (s *TunnelSink) Push(ctx context.Context, ev ChangeEvent) error {
	if s.closed.Load() {
		return nil // 关闭后的事件直接丢弃
	}
	select {
	case s.buf <- ev:
		// 通知后台: 检查是否满批.
		select {
		case s.flushNow <- struct{}{}:
		default:
		}
		return nil
	default:
		// 缓冲满, drop oldest. 用 non-blocking select 拿一个, 留出空位.
		select {
		case <-s.buf:
			s.dropped.Add(1)
		default:
		}
		// 再尝试放.
		select {
		case s.buf <- ev:
			return nil
		default:
			// 极端 race: 已经满, 丢弃本事件.
			s.dropped.Add(1)
			if prom.ChangeEventsPushedTotal != nil {
				prom.ChangeEventsPushedTotal.WithLabelValues("drop").Inc()
			}
			return nil
		}
	}
}

// Run 启动后台 flush 循环. ctx 取消时返回 (残余会丢弃, 调 Close 强制 drain).
func (s *TunnelSink) Run(ctx context.Context) {
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.flushBatch(ctx)
		case <-s.flushNow:
			s.flushBatch(ctx)
		}
	}
}

// Close 强制 drain 残余并标记关闭. 返回前会做最后一次 flush.
func (s *TunnelSink) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil // 已关闭
	}
	// 用 background ctx 因为调用方的 ctx 可能已 cancel.
	ctx, cancel := context.WithTimeout(context.Background(), s.callTimeout)
	defer cancel()
	s.drainAndFlush(ctx)
	close(s.buf)
	return nil
}

// drainAndFlush 把 buf 中所有事件凑成最多 batchSize 一批 flush 出去.
// 用于 Close 时的强制 drain.
func (s *TunnelSink) drainAndFlush(ctx context.Context) {
	for {
		batch := s.collectBatch(ctx, s.batchSize)
		if len(batch) == 0 {
			return
		}
		if err := s.callOnce(ctx, batch); err != nil {
			s.logger.Warn("changewatcher: tunnel sink final flush failed",
				slog.Int("batch", len(batch)),
				slog.String("err", err.Error()))
			return
		}
	}
}

// flushBatch 凑一批并 push. 凑不满 batchSize 也 flush (ticker 触发时).
func (s *TunnelSink) flushBatch(ctx context.Context) {
	batch := s.collectBatch(ctx, s.batchSize)
	if len(batch) == 0 {
		return
	}
	if err := s.callOnce(ctx, batch); err != nil {
		s.logger.Warn("changewatcher: tunnel sink flush failed",
			slog.Int("batch", len(batch)),
			slog.String("err", err.Error()))
		// 失败不回填, 接受丢失 (后续可加重试, 不在本 change 范围)
	}
}

// collectBatch 非阻塞地收集最多 n 个事件.
func (s *TunnelSink) collectBatch(ctx context.Context, n int) []ChangeEvent {
	batch := make([]ChangeEvent, 0, n)
	for i := 0; i < n; i++ {
		select {
		case ev := <-s.buf:
			batch = append(batch, ev)
		default:
			return batch
		}
	}
	return batch
}

// callOnce 把一批事件转成 wire 格式并通过 tunnel client 推送.
func (s *TunnelSink) callOnce(parent context.Context, batch []ChangeEvent) error {
	if len(batch) == 0 {
		return nil
	}
	wire := make([]tunnel.ChangeEventWire, len(batch))
	for i, ev := range batch {
		wire[i] = tunnel.ChangeEventWire{
			Source:    string(ev.Source),
			Kind:      string(ev.Kind),
			Subject:   ev.Subject,
			Action:    ev.Action,
			Timestamp: ev.Timestamp,
			Severity:  string(ev.Severity),
			Labels:    ev.Labels,
		}
	}
	req := tunnel.PushChangeEventsRequest{Events: wire}
	var resp tunnel.PushChangeEventsResponse
	ctx, cancel := context.WithTimeout(parent, s.callTimeout)
	defer cancel()
	if err := s.client.Call(ctx, tunnel.MethodPushChangeEvents, &req, &resp); err != nil {
		return err
	}
	s.flushed.Add(uint64(resp.Accepted))
	if prom.ChangeEventsPushedTotal != nil {
		prom.ChangeEventsPushedTotal.WithLabelValues("ok").Add(float64(resp.Accepted))
	}
	if resp.Rejected > 0 {
		if prom.ChangeEventsPushedTotal != nil {
			prom.ChangeEventsPushedTotal.WithLabelValues("reject").Add(float64(resp.Rejected))
		}
	}
	if resp.Rejected > 0 {
		s.logger.Warn("changewatcher: tunnel sink partial reject",
			slog.Uint64("accepted", uint64(resp.Accepted)),
			slog.Uint64("rejected", uint64(resp.Rejected)))
	}
	return nil
}

// Dropped 返回累计 drop 计数（用于 metrics / log).
func (s *TunnelSink) Dropped() uint64 { return s.dropped.Load() }

// Flushed 返回累计成功 flush 计数.
func (s *TunnelSink) Flushed() uint64 { return s.flushed.Load() }
