// Package changewatcher 提供 edge agent 侧的变更监听能力。
//
// watcher.go 负责编排三个 tailer（journald / dockerd / packagemgr）。
// 平台策略：
//   - non-linux: 仅启动 packagemgr（journald / dockerd 不可用）
//   - linux:     启动全部三个（按 binary 是否存在自动跳过）
//
// 编排原则：
//   - 每个 tailer 独立的 context.WithCancel，单个 tailer 失败
//     不阻塞其他 tailer。
//   - errgroup 仅用于等待 Start 调用方的"启动信号"返回；运行
//     期间的错误被各 tailer 内部 logger 记录。
//   - Stop 调所有 cancel，确保优雅退出。
package changewatcher

import (
	"context"
	"log/slog"
	"sync"
)

// Watcher 编排多个 tailer 把 ChangeEvent 推到同一个 PushSink。
// 重复 Start 不会启动新一组 tailer；Start 之后必须 Stop 才能
// 再次 Start。
type Watcher struct {
	sink   PushSink
	logger *slog.Logger

	mu      sync.Mutex
	started bool
	cancels []context.CancelFunc
	wg      sync.WaitGroup
}

// New 返回一个未启动的 Watcher。logger 可为 nil（用 slog.Default()）。
func New(sink PushSink, logger *slog.Logger) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	if sink == nil {
		// 守护调用方：nil sink 会在第一次 Push 时 panic, 早 fail
		// 比晚 fail 好。
		panic("changewatcher: New requires a non-nil PushSink")
	}
	return &Watcher{sink: sink, logger: logger}
}

// Start 启动所有 tailer 并立即返回（非阻塞）。返回的 stop 函数
// 用于停止所有 tailer；多次调用 stop 是幂等的。
//
// 错误语义：Start 本身不返回 error——某个 tailer 启动失败会被
// 该 tailer 的 logger 记录，但不影响其他 tailer。这是设计选择：
// edge agent 必须能在缺 journald / dockerd 的环境里跑起来。
func (w *Watcher) Start(ctx context.Context) (stop func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		w.logger.Warn("changewatcher: Start called on already-started watcher; returning existing stop")
		return w.composeStop()
	}
	w.started = true

	stoppers := w.startTailersLocked(ctx)
	w.cancels = stoppers
	return w.composeStop()
}

// composeStop 必须在持有 w.mu 时调用 — 它从 w.cancels 读 slice。
func (w *Watcher) composeStop() func() {
	cancels := append([]context.CancelFunc(nil), w.cancels...)
	var once sync.Once
	return func() {
		once.Do(func() {
			for _, c := range cancels {
				c()
			}
		})
	}
}

// startTailersLocked 启动所有 tailer 并返回每个的 cancel func。
// 必须持有 w.mu。
func (w *Watcher) startTailersLocked(parent context.Context) []context.CancelFunc {
	stops := make([]context.CancelFunc, 0, 3)

	stops = append(stops, w.spawn(parent, "packagemgr", func(ctx context.Context) error {
		t := newPackageTailer(w.sink, w.logger)
		return t.Run(ctx)
	}))

	stops = append(stops, w.spawnLinuxTailers(parent)...)

	return stops
}

// spawn 启动一个独立 ctx 的 goroutine, 返回其 cancel。
func (w *Watcher) spawn(parent context.Context, name string, run func(context.Context) error) context.CancelFunc {
	ctx, cancel := context.WithCancel(parent)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("changewatcher: tailer panicked",
					slog.String("tailer", name),
					slog.Any("recover", r))
			}
		}()
		if err := run(ctx); err != nil && ctx.Err() == nil {
			// 父 ctx 还活着但 tailer 退出 — 记录但不 cancel 其他。
			w.logger.Warn("changewatcher: tailer exited",
				slog.String("tailer", name),
				slog.String("err", err.Error()))
		}
	}()
	return cancel
}

// Wait 阻塞直到所有 tailer goroutine 返回。主要给测试用。
func (w *Watcher) Wait() {
	w.wg.Wait()
}
