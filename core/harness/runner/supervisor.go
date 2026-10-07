// supervisor.go — leader-only gating for the harness case runner.
//
// harness case runs are on-demand (one Run() call per invocation), so
// there's no background loop to Start/Stop in the classical sense.
// What the leader.Manager needs is a hook to flip a "this replica is
// the leader for harness:runner" flag on election win / loss so the
// HTTP handler that dispatches case runs can reject (or redirect)
// requests landing on follower replicas.
//
// Phase 2 scope: wire the flag + Start/Stop callbacks. Phase 4 will
// teach the dispatch HTTP handler to read IsActive() and respond 503
// / 302 to non-leader replicas.
package runner

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// supervisor 跟踪当前进程是否持有 harness:runner 角色。 包级单例
// 由 init() 初始化,这是包内可接受的例外(AGENTS.md 允许 collector
// 风格的只读单例;gating flag 是 register-time set / read-only after)。
var supervisor = struct {
	sync.RWMutex
	active atomic.Bool
	log    *slog.Logger
}{
	log: slog.Default().With(slog.String("comp", "harness-supervisor")),
}

// IsActive reports whether the local replica is the leader for the
// harness:runner role. Followers return false; the leader returns
// true. HTTP handlers dispatching case runs consult this flag to
// decide whether to accept the run or reject / redirect.
//
// Single-replica deployments (the historical MVP shape) get true
// unconditionally because the leader.Manager is not started.
func IsActive() bool {
	return supervisor.active.Load()
}

// HarnessStart is the leader.Manager.Register start callback. Sets
// the gating flag and logs the acquisition. Returns nil immediately
// (Start is "fire-and-forget" — there's no goroutine to wait on).
//
// When leader.Manager is disabled (the embedded compose, single
// replica, or unit tests that flip OPSKEEPER_LEADER_ENABLED=false),
// the legacy "always-active" path is used: harness case runs go
// through on every replica. To preserve that behaviour, callers
// who construct Supervisor directly (not via the leader.Manager)
// should call Activate() once on boot.
func HarnessStart(_ context.Context) error {
	supervisor.active.Store(true)
	supervisor.log.Info("harness:runner acquired leadership")
	return nil
}

// HarnessStop is the leader.Manager.Register stop callback. Clears
// the gating flag. Idempotent.
func HarnessStop(_ context.Context) error {
	supervisor.active.Store(false)
	supervisor.log.Info("harness:runner released leadership")
	return nil
}

// Activate flips the gating flag to true. Use this on the boot
// path of single-replica deployments (or unit tests) that don't
// run a leader.Manager. Safe to call multiple times.
func Activate() {
	supervisor.active.Store(true)
}

// Deactivate flips the gating flag to false. Mirrors Activate for
// symmetry; mostly used in tests.
func Deactivate() {
	supervisor.active.Store(false)
}
