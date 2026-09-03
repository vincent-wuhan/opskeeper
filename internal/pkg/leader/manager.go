// Package leader 提供基于 Redis 的 leader election 能力。
//
// 设计依据：docs/superpowers/specs/2026-07-15-platform-base-ha-design.md §3.2
//
// 用法：
//
//	mgr := leader.NewManager(redisCli, leader.WithTTL(15*time.Second), leader.WithRenewInterval(5*time.Second))
//	mgr.Register("scheduler:flow", flowScheduler.Start, flowScheduler.Stop)
//	mgr.Register("harness:runner", harnessRunner.Start, harnessRunner.Stop)
//	go mgr.Run(ctx)
//	defer mgr.ResignAll(ctx)
package leader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/redislock"
)

// Role 标识一个 leader-only worker 的逻辑角色。
//
// 命名约定：<subsystem>:<scope>，如 scheduler:flow / harness:runner / upgrade:checker。
// role 是本 manager 内唯一的字符串；Redis key 为 opskeeper:leader:<role>。
type Role string

// WorkerFunc 是 worker 的 Start/Stop 函数签名。
//
// Start 在本实例成为该 role 的 leader 时调用；返回 error 时视为启动失败，自动让位。
// Stop 在本实例失去 leader 身份时调用；返回 error 仅记录日志。
type WorkerFunc func(ctx context.Context) error

// Subscribers 在 leader 状态变更时触发的回调。
//
// onBecome 在成为 leader 后调用；onLose 在失去 leader 后调用。
// 二者均在独立 goroutine 中调用，失败只记日志。
type Subscribers struct {
	OnBecome WorkerFunc
	OnLose   WorkerFunc
}

// Option 是 Manager 的构造选项。
type Option func(*Manager)

// WithTTL 设置 leader 锁的 TTL（默认 15s）。
func WithTTL(ttl time.Duration) Option {
	return func(m *Manager) { m.ttl = ttl }
}

// WithRenewInterval 设置后台 renew 间隔（默认 TTL/3）。
func WithRenewInterval(d time.Duration) Option {
	return func(m *Manager) { m.renewInterval = d }
}

// WithInstanceID 设置显式的 instance ID（默认 <hostname>-<uuid[:8]>）。
func WithInstanceID(id string) Option {
	return func(m *Manager) { m.instanceID = id }
}

// WithStartTimeout 设置 state.start 的就绪超时（默认 5s）。
//
// 如果 state.start 在该超时内未返回，视为已就绪（典型用法：start 内部 go actualWork 后立即返回 nil）。
// 如果 state.start 在该超时前返回 error，视为 start 失败，释放锁并重试。
func WithStartTimeout(d time.Duration) Option {
	return func(m *Manager) { m.startTimeout = d }
}

// Manager 管理多个 role 的 leader election。
//
// 一个 Manager 对应一个 opskeeper manager 进程；不同 role 可在不同进程上独立 leader。
type Manager struct {
	cli           *redis.Client
	ttl           time.Duration
	renewInterval time.Duration
	startTimeout  time.Duration
	instanceID    string
	owner         redislock.Owner

	mu       sync.RWMutex
	roles    map[Role]*electorState
	draining bool

	wg     sync.WaitGroup
	stopCh chan struct{}
	once   sync.Once

	// quitCh 在 Close 时关闭；electLoop/renewLoop select 监听它。
	// 与 ctx 不同：Close 调用方可能没有 ctx 引用，需要一个不依赖 ctx 的退出信号。
	quitCh   chan struct{}
	quitOnce sync.Once
}

// electorState 是单个 role 的内部状态。
type electorState struct {
	start WorkerFunc
	stop  WorkerFunc
	subs  []Subscribers

	isLeader bool   // guarded by electorState.mu
	running  bool   // guarded by electorState.mu
	errStr   string // guarded by electorState.mu
	mu       sync.Mutex
}

func (s *electorState) setLeader(v bool)  { s.mu.Lock(); s.isLeader = v; s.mu.Unlock() }
func (s *electorState) getLeader() bool   { s.mu.Lock(); defer s.mu.Unlock(); return s.isLeader }
func (s *electorState) setRunning(v bool) { s.mu.Lock(); s.running = v; s.mu.Unlock() }
func (s *electorState) getRunning() bool  { s.mu.Lock(); defer s.mu.Unlock(); return s.running }
func (s *electorState) setErr(err error) {
	s.mu.Lock()
	if err != nil {
		s.errStr = err.Error()
	} else {
		s.errStr = ""
	}
	s.mu.Unlock()
}
func (s *electorState) getErr() string { s.mu.Lock(); defer s.mu.Unlock(); return s.errStr }

// NewManager 构造 Manager。
func NewManager(cli *redis.Client, opts ...Option) *Manager {
	host, _ := os.Hostname()
	m := &Manager{
		cli:           cli,
		ttl:           15 * time.Second,
		renewInterval: 5 * time.Second,
		startTimeout:  5 * time.Second,
		instanceID:    fmt.Sprintf("%s-%s", host, uuid.NewString()[:8]),
		owner:         redislock.NewOwner(),
		roles:         make(map[Role]*electorState),
		stopCh:        make(chan struct{}),
		quitCh:        make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Quit 返回 manager 的 quit channel（仅 Close 关闭它）。
// 可用于外部 select 监听 Close 信号。
func (m *Manager) Quit() <-chan struct{} { return m.quitCh }

// InstanceID 返回本进程唯一标识。
func (m *Manager) InstanceID() string { return m.instanceID }

// Owner 返回本进程作为 leader 候选者时的 owner（用于诊断）。
func (m *Manager) Owner() redislock.Owner { return m.owner }

// Register 注册一个 leader-only worker。
//
// 必须在 Run 之前调用；之后注册的 role 在 Run 启动后不会被处理。
// start / stop 不可同时为 nil；至少要有一个非 nil（推荐两个都有）。
func (m *Manager) Register(role Role, start, stop WorkerFunc) {
	if start == nil && stop == nil {
		panic(fmt.Sprintf("leader: Register(%q): start and stop both nil", role))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.roles[role]; exists {
		panic(fmt.Sprintf("leader: Register(%q): duplicate", role))
	}
	m.roles[role] = &electorState{
		start: start,
		stop:  stop,
	}
}

// Subscribe 注册 leader 状态变更回调。
//
// 可多次调用；多个 subscriber 按注册顺序串行触发。
// 回调在独立 goroutine 中调用；失败只记日志。
func (m *Manager) Subscribe(role Role, subs Subscribers) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.roles[role]
	if !ok {
		panic(fmt.Sprintf("leader: Subscribe(%q): role not registered", role))
	}
	state.subs = append(state.subs, subs)
}

// IsLeaderAny 返回本进程是否至少是某个 role 的 leader。
//
// 用于 /readyz 的 follower 副本判定（follower 副本 IsLeaderAny=false）。
func (m *Manager) IsLeaderAny() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.roles {
		if s.getLeader() {
			return true
		}
	}
	return false
}

// IsLeader 返回本进程是否持有特定 role。
func (m *Manager) IsLeader(role Role) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.roles[role]; ok {
		return s.getLeader()
	}
	return false
}

// WorkersRunning 返回所有 role 当前是否在运行。
//
// 用于 /api/v1/cluster/status 与 /readyz 的 WorkersChecker。
func (m *Manager) WorkersRunning() map[Role]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[Role]bool, len(m.roles))
	for role, s := range m.roles {
		out[role] = s.getRunning()
	}
	return out
}

// MarkDraining 标记 manager 进入 draining 状态（用于 SIGTERM 优雅下线）。
//
// 状态变更后 IsLeaderAny 仍返回 true（已持锁），但 WorkersRunning 标记停止接收新任务。
func (m *Manager) MarkDraining() {
	m.mu.Lock()
	m.draining = true
	m.mu.Unlock()
}

// IsDraining 返回是否处于 draining 状态。
func (m *Manager) IsDraining() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.draining
}

// Run 启动所有 role 的 election goroutine。
//
// 每个 role 一个 goroutine；本方法启动后立即返回，调用方应阻塞在 ctx.Done()。
func (m *Manager) Run(ctx context.Context) {
	m.mu.RLock()
	roles := make([]Role, 0, len(m.roles))
	for r := range m.roles {
		roles = append(roles, r)
	}
	m.mu.RUnlock()

	for _, role := range roles {
		m.wg.Add(1)
		go m.electLoop(ctx, role)
	}
}

// Close 标记 manager 停止并等待所有 election goroutine 退出。
//
// 行为：
//  1. 关闭 quitCh（所有 electLoop/renewLoop 在 select 中感知并退出）
//  2. 标记 draining（让在 renewLoop 里的 goroutine 也走 resign 路径）
//  3. 等待 wg（所有 goroutine 退出）
//
// 注意：Close 不取消调用方传入的 ctx；如需在 Close 之外也取消 ctx，请调用方自己处理。
func (m *Manager) Close() error {
	m.quitOnce.Do(func() { close(m.quitCh) })
	m.MarkDraining()
	m.once.Do(func() { close(m.stopCh) })
	m.wg.Wait()
	return nil
}

// ResignAll 主动让出所有 role 的 leader 身份。
//
// 用于 SIGTERM 优雅下线；调用后 IsLeaderAny 返回 false（停 renew + 释放锁）。
// 各 role 的 stop 也会被调用。
//
// 带 ctx 超时；超时时未完成让位的 role 继续后台让位。
func (m *Manager) ResignAll(ctx context.Context) error {
	m.MarkDraining()
	m.mu.RLock()
	roles := make([]Role, 0, len(m.roles))
	for r := range m.roles {
		roles = append(roles, r)
	}
	m.mu.RUnlock()

	// 等 electLoop 检测到 draining 后自然退出并完成 stop + Release。
	// ResignAll 的语义是"让位"，不是"硬杀"；electLoop 会在下个 tick 检测到 IsDraining 后
	// 释放锁、调 stop、退出。
	return m.waitForLeaderExit(ctx)
}

// waitForLeaderExit 阻塞直到所有 role 的 leader 状态都为 false，或 ctx 取消。
func (m *Manager) waitForLeaderExit(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	if ctx != nil {
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
	}
	for {
		if !m.IsLeaderAny() {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("leader: resign timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
