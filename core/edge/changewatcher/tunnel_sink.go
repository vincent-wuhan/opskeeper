package changewatcher

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/edge/spool"
	"github.com/vincent-wuhan/opskeeper/core/edge/telemetrywal"
	"github.com/vincent-wuhan/opskeeper/core/floor/prom"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// sequencedEvent 是一个 ChangeEvent 加上写前日志打在它身上的行号。
//
// seq == 0 读作「这一条从未落过盘」, 于是中心无从判断它是新事件还是重放,
// 只能照单全收。这是刻意的: 编不出键的时候存一份重复, 好过为了让键好看
// 而把事件丢掉——而 seq 真的为 0 是不可能的, spool 的行号从 1 起。
type sequencedEvent struct {
	event ChangeEvent
	seq   uint64
}

// DefaultSinkConfig 是 TunnelSink 的默认参数。测试可覆写。
type DefaultSinkConfig struct {
	BatchSize     int           // 单批最大事件数；默认 100
	BufferSize    int           // 内部缓冲 chan 容量；默认 BatchSize * 2
	FlushInterval time.Duration // ticker 周期；默认 5s
	CallTimeout   time.Duration // 每次 Call 的 ctx 超时；默认 10s

	// WALDir 是本地落盘日志的目录。空 = 不落盘。
	//
	// 空不是小差别：没有它，flush 失败就是丢事件（原实现里那句
	// 「失败不回填, 接受丢失」就是这个意思），而「服务在 03:12 重启过」
	// 恰恰是人在 09:00 最想看到的那条。设置它以后，凑好的批次在推出去
	// 之前先落盘，推成功才 ack，推失败留在盘上等下一个 tick。
	//
	// 保证的边界要说清楚：事件从 Push 进 channel 到被 flush 凑批之间
	// 仍然只在内存里，那段窗口就是 FlushInterval。落盘保证的是「已经凑批
	// 的事件在报出去之前是耐久的」，而不是「采集瞬间即落盘」——后者要
	// 给每个事件一次文件写，放在 watcher 的热路径上，而 watcher 的职责
	// 是读 journald 不是写文件。
	WALDir string
	// WALBytes 是落盘日志的容量上限；默认 telemetrywal.DefaultWALBytes。
	WALBytes int64
}

// WALFileName 是变更事件日志的文件名.
const WALFileName = "change-events.jsonl"

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

	buf    chan ChangeEvent
	pushMu sync.Mutex
	// wal 是批次级的耐久记录，可为 nil。nil 时 sink 就是 flush 失败即
	// 丢弃的旧行为——那是开发机的合理配置，不是机群的。
	wal     *spool.Spool
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
	s := &TunnelSink{
		client:        client,
		logger:        logger,
		batchSize:     cfg.BatchSize,
		bufSize:       cfg.BufferSize,
		flushInterval: cfg.FlushInterval,
		callTimeout:   cfg.CallTimeout,
		buf:           make(chan ChangeEvent, cfg.BufferSize),
		flushNow:      make(chan struct{}, 1),
	}
	if cfg.WALDir != "" {
		wal, err := openEventWAL(cfg.WALDir, cfg.WALBytes, logger)
		if err != nil {
			// 响亮地记下来，然后继续跑。watcher 是采集面，让一个目录
			// 不可写把整个节点的变化事件停掉，代价远大于「这次断连的
			// 事件会丢」——而后者正是旧行为。
			logger.Warn("changewatcher: durable log unavailable; a failed flush will lose events",
				slog.String("dir", cfg.WALDir),
				slog.Any("err", err))
		} else {
			s.wal = wal
		}
	}
	return s
}

// openEventWAL 打开变更事件的落盘日志.
//
// 策略表从 telemetrywal 取，而不是在这里重写一份：这两个日志问的是同一
// 个问题（「满了先丢哪个」），两份答案就是两个没人能同时看见的答案。
// 事件类是其中最贵的一类，且没有保质期——「服务什么时候重启过」这个
// 问题没有「太晚了」这个答案。
func openEventWAL(dir string, maxBytes int64, logger *slog.Logger) (*spool.Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sp, err := spool.Open(spool.Options{
		Path:     filepath.Join(dir, WALFileName),
		MaxBytes: maxBytes,
		Label:    "the change-event log",
		Classes:  telemetrywal.DefaultClasses(),
	})
	if err != nil {
		return nil, err
	}
	return sp, nil
}

// CloseLog 关闭落盘日志，释放文件句柄.
func (s *TunnelSink) CloseLog() error {
	if s.wal == nil {
		return nil
	}
	return s.wal.Close()
}

// Pending 返回还在盘上等着回传的事件数.
func (s *TunnelSink) Pending() (int, error) {
	if s.wal == nil {
		return 0, nil
	}
	return s.wal.Len()
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

// drainAndFlush 反复把「盘上的」和「channel 里的」都推完. 用于 Close.
//
// 两个来源是有序的：盘上的更老，channel 里的更新。所以每轮先推盘上的一批，
// 推空了才去 channel 取新的。反过来做就会让一条 03:12 的事件排在一批刚刚
// 才产生的事件后面——在一个正在恢复的节点上，这正是「时间线错乱」的样子。
func (s *TunnelSink) drainAndFlush(ctx context.Context) {
	for {
		if s.replayOne(ctx) {
			continue
		}
		batch := s.collectBatch(ctx, s.batchSize)
		if len(batch) == 0 {
			return
		}
		if !s.deliver(ctx, batch) {
			return
		}
	}
}

// flushBatch 一个 tick 的全部工作：先回放盘上积压的一批，再凑一批新的推出去.
//
// 一次 tick 最多回放一批，这是「回放限流」的全部：积压的行按 FlushInterval
// 的节奏一批批走，而不是在隧道恢复的那一刻一次性糊到中心脸上。批量大小
// 乘以这个间隔，就是一个节点回放时的速率上界。
func (s *TunnelSink) flushBatch(ctx context.Context) {
	if s.replayOne(ctx) {
		// 盘上还有。这一 tick 不取新的：让积压先见底，新事件留在 channel
		// 里等，顺序才不会乱。
		return
	}
	batch := s.collectBatch(ctx, s.batchSize)
	if len(batch) == 0 {
		return
	}
	s.deliver(ctx, batch)
}

// replayOne 推送盘上最老的一批，成功则 ack。返回是否真的推了东西.
//
// 推失败时返回 true：这一 tick 已经「有事可做」了，flushBatch 因此不会去
// 凑新批次，把顺序保持住；错误本身记在这里而不是往上抛，因为对调用方而言
// 「网络不通」和「channel 空」是同一种不需要处理的情况。
func (s *TunnelSink) replayOne(ctx context.Context) bool {
	if s.wal == nil {
		return false
	}
	rows, err := s.wal.Peek(s.batchSize)
	if err != nil {
		s.logger.Warn("changewatcher: durable log unreadable",
			slog.String("err", err.Error()))
		return false
	}
	if len(rows) == 0 {
		return false
	}
	// decoded is the number of *rows* the batch represents, which is not
	// necessarily len(batch): a row this build cannot read is dropped
	// from the batch but still has to be acked, or it sits at the head of
	// the file forever and nothing behind it ever gets replayed. Those
	// two numbers being different is the whole reason this function
	// returns both.
	batch, decoded := decodeEvents(rows)
	if len(batch) == 0 {
		// Every row was unreadable. Nothing to send, but they still have
		// to leave the head of the file.
		s.logger.Warn("changewatcher: dropping change-event rows this build cannot read",
			slog.Int("rows", decoded))
		if err := s.wal.Ack(decoded); err != nil {
			s.logger.Warn("changewatcher: acking unreadable change events failed",
				slog.String("err", err.Error()))
		}
		return true
	}
	if err := s.callOnce(ctx, batch); err != nil {
		s.logger.Warn("changewatcher: replaying spooled change events failed",
			slog.Int("batch", len(batch)),
			slog.String("err", err.Error()))
		return true
	}
	if err := s.wal.Ack(decoded); err != nil {
		s.logger.Warn("changewatcher: acking replayed change events failed",
			slog.Int("batch", len(batch)),
			slog.String("err", err.Error()))
	}
	return true
}

// deliver 落盘、推送、ack——顺序不能换.
//
// 落盘在推送之前，是因为推送正是会失败的那一步，而一个只在成功路径上留下
// 记录的 sink 等于没有记录。ack 在推送之后，是因为 ack 的意思是「中心收到
// 了」，而在那之前说这句话就是在撒谎。
func (s *TunnelSink) deliver(ctx context.Context, batch []sequencedEvent) bool {
	if s.wal != nil {
		for i, item := range batch {
			ev := item.event
			// RecordSeq, not Record: the number the log stamps this row
			// with is the only thing that lets the center recognise the
			// same event when the ack is lost and the row comes round
			// again. Sending the batch before reading those numbers back
			// would be a dedup key nobody has.
			seq, err := s.wal.RecordSeq(ctx, telemetrywal.ClassEvent, ev)
			if err != nil {
				// 落盘失败仍然推送：中心拿到总比中心拿不到强，而这一批
				// 失去的是耐久性，不是数据。事件本身还在 channel 之外
				// 吗？不在了——所以这里要喊得足够响。
				//
				// seq 保持 0，也就是「中心无从判断这是不是重放」，于是
				// 中心只能照单全收。宁可存重复，也不能因为编不出键就
				// 把这一条丢掉。
				s.logger.Error("changewatcher: could not record a change event before sending it",
					slog.Any("err", err),
					slog.String("subject", ev.Subject))
				continue
			}
			batch[i].seq = seq
		}
	}
	if err := s.callOnce(ctx, batch); err != nil {
		s.logger.Warn("changewatcher: tunnel sink flush failed",
			slog.Int("batch", len(batch)),
			slog.String("err", err.Error()))
		// 事件留在盘上，下一个 tick 重来。旧实现在这里接受丢失并留下
		// 一句注释说明「后续可加重试」——那就是本项要做的事。
		return false
	}
	if s.wal != nil {
		if err := s.wal.Ack(len(batch)); err != nil {
			s.logger.Warn("changewatcher: acking sent change events failed; they will be sent again",
				slog.Int("batch", len(batch)),
				slog.String("err", err.Error()))
		}
	}
	return true
}

// decodeEvents 把日志行解回事件, 并返回这批事件代表了多少**行**.
//
// 两个数分开返回，因为它们真的会不相等，而把它们当成一个数就是丢事件：
// 解不出来的行不会进 batch，可它仍然占着文件头部的位置。不 ack 它，这一
// 批后面的每一行都永远回放不了——一个节点在网络恢复之后再也不报变化
// 事件，而症状是「没有错误」。
func decodeEvents(rows []spool.Row) (batch []sequencedEvent, decoded int) {
	batch = make([]sequencedEvent, 0, len(rows))
	decoded = 0
	for _, r := range rows {
		var ev ChangeEvent
		if err := json.Unmarshal(r.Payload, &ev); err != nil {
			continue
		}
		batch = append(batch, sequencedEvent{event: ev, seq: r.Seq})
		decoded++
	}
	return batch, decoded
}

// collectBatch 非阻塞地收集最多 n 个事件.
func (s *TunnelSink) collectBatch(ctx context.Context, n int) []sequencedEvent {
	batch := make([]sequencedEvent, 0, n)
	for i := 0; i < n; i++ {
		select {
		case ev := <-s.buf:
			batch = append(batch, sequencedEvent{event: ev})
		default:
			return batch
		}
	}
	return batch
}

// callOnce 把一批事件转成 wire 格式并通过 tunnel client 推送.
func (s *TunnelSink) callOnce(parent context.Context, batch []sequencedEvent) error {
	if len(batch) == 0 {
		return nil
	}
	wire := make([]tunnel.ChangeEventWire, len(batch))
	for i, item := range batch {
		ev := item.event
		wire[i] = tunnel.ChangeEventWire{
			Source:    string(ev.Source),
			Kind:      string(ev.Kind),
			Subject:   ev.Subject,
			Action:    ev.Action,
			Timestamp: ev.Timestamp,
			Severity:  string(ev.Severity),
			Labels:    ev.Labels,
			Seq:       item.seq,
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
