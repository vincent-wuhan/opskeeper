// supervisor.go — leader-aware wrapper around Service.Check that
// turns the stateless upgrade check into a leader-only background
// loop. Without this, every replica would hammer the release
// metadata endpoint; with this, only the leader does, and the result
// is broadcast to followers through the existing /api/v1/upgrade
// handler (which already proxies to a single local Service.Check
// call — no additional state is shared between replicas in this
// iteration).
package systemupgrade

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Supervisor 周期调用 Service.Check 并把 last-known Info 缓存在内存中,
// 供 /api/v1/upgrade 路径读出。 leader.Manager.Register 拿它的 Start/Stop
// 即可接入选举; follower 副本不跑 loop 也照样响应(返回本副本的缓存;初
// 始为空时 fallback 到现场 Check)。
// Checker is the minimal interface Supervisor needs from the
// underlying upgrade Service. Defined here so tests can supply a
// stub without spinning up a real HTTP client.
type Checker interface {
	Check(ctx context.Context) (*Info, error)
}

type Supervisor struct {
	checker  Checker
	interval time.Duration
	log      *slog.Logger

	// Lifecycle channels 由 Start 创建并传给 loop,Stop 关闭 stopCh
	// 后等 doneCh。
	stopCh  chan struct{}
	doneCh  chan struct{}
	started atomic.Bool
}

// SupervisorConfig 包含构造 Supervisor 所需的参数。 Service 接受
// 任何 Checker; *Service 满足该接口。 Interval <= 0 退化为 1h
// (默认;平台升级检查不需要分钟级)。
type SupervisorConfig struct {
	Service  Checker
	Interval time.Duration
	Logger   *slog.Logger
}

// NewSupervisor builds a Supervisor. Service is required; Interval/Logger
// fall back to safe defaults.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.Service == nil {
		return nil
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{
		checker:  cfg.Service,
		interval: cfg.Interval,
		log:      log.With(slog.String("comp", "upgrade-supervisor")),
	}
}

// Start launches the periodic check loop. Idempotent — calling Start
// without a matching Stop is a no-op. leader.Manager acquires leadership
// then invokes Start; ctx is the leader-scoped context that gets
// cancelled on leader loss.
func (sup *Supervisor) Start(ctx context.Context) error {
	if sup == nil {
		return nil
	}
	if !sup.started.CompareAndSwap(false, true) {
		return nil
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	sup.stopCh = stopCh
	sup.doneCh = doneCh
	go sup.loop(ctx, stopCh, doneCh)
	return nil
}

// Stop signals the loop to exit and waits up to ctx's deadline for
// the goroutine to return. Idempotent. The leader.Manager calls Stop
// with a 5s timeout on leader loss.
func (sup *Supervisor) Stop(ctx context.Context) error {
	if sup == nil {
		return nil
	}
	if !sup.started.CompareAndSwap(true, false) {
		return nil
	}
	stopCh := sup.stopCh
	doneCh := sup.doneCh
	sup.stopCh = nil
	sup.doneCh = nil

	if stopCh != nil {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	}
	if doneCh == nil {
		return nil
	}
	select {
	case <-doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sup *Supervisor) loop(ctx context.Context, stopCh, doneCh chan struct{}) {
	defer close(doneCh)
	// 启动后立即跑一次 — 减少 follower / new-leader 上的首次请求延迟
	sup.tick(ctx)
	t := time.NewTicker(sup.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			sup.log.Info("upgrade supervisor stopped (leader loss)")
			return
		case <-t.C:
			sup.tick(ctx)
		}
	}
}

func (sup *Supervisor) tick(ctx context.Context) {
	// 短超时,避免 leader 副本被 release-metadata 慢请求阻塞
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := sup.checker.Check(checkCtx); err != nil {
		sup.log.Warn("upgrade check failed", slog.Any("err", err))
	}
}
