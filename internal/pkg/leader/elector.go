package leader

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/redislock"
)

// electLoop 是单个 role 的 election 主循环。
//
// 设计要点：
//   - state.start 在独立 goroutine 内调用（避免阻塞 electLoop）
//   - state.start 应"快速返回"（典型实现：内部 go actualWork() 后立刻返回 nil）
//   - 若 state.start 返回 error：释放锁 + backoff 重试
//   - 若 state.start 超过 startTimeout 未返回：视为已就绪，继续
//   - renewLoop 在 electLoop goroutine 内运行（保持锁续约）
//   - 任意时刻 ctx 取消 / MarkDraining / renew 失败 → 释放锁 + 调 stop → 退出
func (m *Manager) electLoop(ctx context.Context, role Role) {
	defer m.wg.Done()

	state := m.getState(role)
	if state == nil {
		return
	}

	log := slog.Default().With(slog.String("comp", "leader"), slog.String("role", string(role)))

	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-m.Quit():
			log.Info("electLoop: quit, exit")
			return
		case <-ctx.Done():
			log.Info("electLoop: ctx done, exit")
			return
		default:
		}

		if ctx.Err() != nil {
			log.Info("electLoop: ctx done, exit")
			return
		}
		if m.IsDraining() {
			log.Info("electLoop: draining, exit")
			return
		}

		// 抢锁
		key := string(role)
		lock, err := redislock.TryAcquire(ctx, m.cli, key, m.owner, m.ttl)
		if err != nil {
			if errors.Is(err, redislock.ErrLockHeld) {
				log.Debug("lock held, retry later")
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				continue
			}
			log.Error("redislock.TryAcquire failed", slog.Any("err", err))
			state.setErr(err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}

		log.Info("acquired leadership", slog.String("instance_id", m.instanceID))
		backoff = 1 * time.Second

		// 启动 worker（异步 + 超时）
		workerCtx, workerCancel := context.WithCancel(context.Background())
		startResult := make(chan error, 1)
		go func() {
			var err error
			if state.start != nil {
				err = state.start(workerCtx)
			}
			startResult <- err
		}()

		// 等 start 返回（带超时）
		var startErr error
		select {
		case startErr = <-startResult:
			// start 已返回
		case <-time.After(m.startTimeout):
			// start 仍在运行；视为已就绪
			log.Debug("worker start timeout, assume running")
		}

		if startErr != nil {
			log.Error("worker start failed", slog.Any("err", startErr))
			state.setErr(startErr)
			state.setLeader(false)
			state.setRunning(false)
			_ = lock.Release(ctx)
			workerCancel()
			// startResult 已被上面 select 消费，buffer 空；start goroutine 已退出
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}

		state.setErr(nil)
		state.setLeader(true)
		state.setRunning(true)
		m.notifyBecome(role)

		// renew ticker loop（阻塞直到失锁或让位）
		lost := m.renewLoop(ctx, workerCtx, role, lock)

		// 失锁后清理
		state.setLeader(false)
		state.setRunning(false)
		workerCancel()
		// 注：startResult 已被上面 select 消费一次，buffer 空；这里不再等待
		// （workerCancel 已发出停止信号，worker goroutine 会自然退出）

		if state.stop != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := state.stop(stopCtx); err != nil {
				log.Error("worker stop failed", slog.Any("err", err))
			}
			stopCancel()
		}
		m.notifyLose(role)

		if lost {
			log.Info("lost leadership, retry acquire")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		} else {
			log.Info("resigned, exit electLoop")
			return
		}
	}
}

// renewLoop 是已获 leader 后的后台 renew 循环。
//
// 返回值：
//   - true：renew 失败（自然失锁），需要重新抢
//   - false：ctx 取消 / workerCtx 取消 / MarkDraining（主动让位），退出
func (m *Manager) renewLoop(ctx context.Context, workerCtx context.Context, role Role, lock *redislock.Lock) bool {
	log := slog.Default().With(slog.String("comp", "leader"), slog.String("role", string(role)))

	interval := m.renewInterval
	if interval <= 0 {
		interval = m.ttl / 3
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.Quit():
			log.Info("renewLoop: quit, resign")
			m.releaseQuietly(lock)
			return false
		case <-ctx.Done():
			log.Info("renewLoop: ctx done, resign")
			m.releaseQuietly(lock)
			return false
		case <-workerCtx.Done():
			log.Info("renewLoop: workerCtx done, resign")
			m.releaseQuietly(lock)
			return false
		case <-ticker.C:
			renCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := lock.Renew(renCtx, m.ttl)
			cancel()
			if err != nil {
				log.Warn("lock.Renew failed", slog.Any("err", err))
				return true // 失锁
			}
			if m.IsDraining() {
				log.Info("renewLoop: draining detected, resign")
				m.releaseQuietly(lock)
				return false
			}
		}
	}
}

// releaseQuietly 释放锁，错误仅记录日志。
func (m *Manager) releaseQuietly(lock *redislock.Lock) {
	relCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := lock.Release(relCtx); err != nil {
		slog.Warn("lock.Release failed", slog.Any("err", err))
	}
}

// notifyBecome 触发 onBecome 回调。
func (m *Manager) notifyBecome(role Role) {
	state := m.getState(role)
	if state == nil {
		return
	}
	m.mu.RLock()
	subs := append([]Subscribers(nil), state.subs...)
	m.mu.RUnlock()

	if len(subs) == 0 {
		return
	}
	go func() {
		for _, sub := range subs {
			if sub.OnBecome != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := sub.OnBecome(ctx); err != nil {
					slog.Warn("subscriber OnBecome failed",
						slog.String("role", string(role)),
						slog.Any("err", err))
				}
				cancel()
			}
		}
	}()
}

// notifyLose 触发 onLose 回调。
func (m *Manager) notifyLose(role Role) {
	state := m.getState(role)
	if state == nil {
		return
	}
	m.mu.RLock()
	subs := append([]Subscribers(nil), state.subs...)
	m.mu.RUnlock()

	if len(subs) == 0 {
		return
	}
	go func() {
		for _, sub := range subs {
			if sub.OnLose != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := sub.OnLose(ctx); err != nil {
					slog.Warn("subscriber OnLose failed",
						slog.String("role", string(role)),
						slog.Any("err", err))
				}
				cancel()
			}
		}
	}()
}

// getState 取出 role 的 state（线程安全）。
func (m *Manager) getState(role Role) *electorState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.roles[role]
}
