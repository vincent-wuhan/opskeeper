package leader

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestManager(t *testing.T, opts ...Option) (*Manager, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(func() { mr.Close() })

	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = cli.Close() })

	allOpts := append([]Option{WithTTL(2 * time.Second), WithRenewInterval(500 * time.Millisecond)}, opts...)
	mgr := NewManager(cli, allOpts...)
	return mgr, mr, cli
}

func TestNewManager(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	if mgr.InstanceID() == "" {
		t.Error("InstanceID is empty")
	}
	if mgr.Owner().ID == "" {
		t.Error("Owner.ID is empty")
	}
	if mgr.IsLeaderAny() {
		t.Error("fresh manager should not be leader")
	}
	if mgr.IsDraining() {
		t.Error("fresh manager should not be draining")
	}
	if len(mgr.WorkersRunning()) != 0 {
		t.Errorf("WorkersRunning on fresh manager = %d entries, want 0", len(mgr.WorkersRunning()))
	}
}

func TestRegister_PanicOnNilBoth(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when both start and stop are nil")
		}
	}()
	mgr.Register("test:role", nil, nil)
}

func TestRegister_PanicOnDuplicate(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mgr.Register("test:dup", func(ctx context.Context) error { return nil }, nil)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate Register")
		}
	}()
	mgr.Register("test:dup", func(ctx context.Context) error { return nil }, nil)
}

func TestSubscribe_PanicOnUnknownRole(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on Subscribe unknown role")
		}
	}()
	mgr.Subscribe("test:unknown", Subscribers{})
}

func TestSingleInstance_BecomesLeader(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	var started atomic.Int32
	var stopped atomic.Int32
	mgr.Register("test:start",
		func(ctx context.Context) error {
			started.Add(1)
			// 实际 worker 在 goroutine 内运行；start 立即返回
			go func() {
				<-ctx.Done()
			}()
			return nil
		},
		func(ctx context.Context) error {
			stopped.Add(1)
			return nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	// 等 leader 选举完成
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		isL := mgr.IsLeader("test:start")
		sN := started.Load()
		t.Logf("poll: IsLeader=%v started=%d", isL, sN)
		if isL && sN == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !mgr.IsLeader("test:start") {
		t.Fatal("expected to become leader")
	}
	t.Logf("after leader check: IsLeader=true, started=%d, running=%v", started.Load(), mgr.WorkersRunning()["test:start"])
	if started.Load() != 1 {
		t.Errorf("start invoked %d times, want 1", started.Load())
	}
	if !mgr.WorkersRunning()["test:start"] {
		t.Error("WorkersRunning[test:start] should be true")
	}

	t.Log("about to cancel()")
	cancel()
	t.Log("cancel() returned, calling mgr.Close()")
	_ = mgr.Close()
	t.Logf("mgr.Close() returned, stopped=%d", stopped.Load())

	if stopped.Load() != 1 {
		t.Errorf("stop invoked %d times, want 1", stopped.Load())
	}
}

func TestThreeInstances_OnlyOneLeader(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	defer mr.Close()

	cli1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cli2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cli3 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer cli1.Close()
	defer cli2.Close()
	defer cli3.Close()

	mgrs := []*Manager{
		NewManager(cli1, WithTTL(2*time.Second), WithRenewInterval(500*time.Millisecond), WithInstanceID("inst-1")),
		NewManager(cli2, WithTTL(2*time.Second), WithRenewInterval(500*time.Millisecond), WithInstanceID("inst-2")),
		NewManager(cli3, WithTTL(2*time.Second), WithRenewInterval(500*time.Millisecond), WithInstanceID("inst-3")),
	}

	var startedCount atomic.Int32
	startFn := func(ctx context.Context) error {
		startedCount.Add(1)
		// worker 在 goroutine 内运行；start 立即返回
		go func() { <-ctx.Done() }()
		return nil
	}
	for _, m := range mgrs {
		m.Register("test:3way", startFn, nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, m := range mgrs {
		go m.Run(ctx)
	}

	// 等选举稳定
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		leaderCount := 0
		for _, m := range mgrs {
			if m.IsLeader("test:3way") {
				leaderCount++
			}
		}
		if leaderCount == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	leaderCount := 0
	var leaderID string
	for _, m := range mgrs {
		if m.IsLeader("test:3way") {
			leaderCount++
			leaderID = m.InstanceID()
		}
	}
	if leaderCount != 1 {
		t.Fatalf("leader count = %d, want 1", leaderCount)
	}
	t.Logf("leader is %s, start invoked %d times", leaderID, startedCount.Load())

	cancel()
	for _, m := range mgrs {
		_ = m.Close()
	}
}

func TestFailover_OnLeaderClose(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	defer mr.Close()

	cli1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cli2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer cli1.Close()
	defer cli2.Close()

	mgr1 := NewManager(cli1, WithTTL(2*time.Second), WithRenewInterval(500*time.Millisecond), WithInstanceID("inst-1"))
	mgr2 := NewManager(cli2, WithTTL(2*time.Second), WithRenewInterval(500*time.Millisecond), WithInstanceID("inst-2"))

	mgr1.Register("test:failover", func(ctx context.Context) error { go func() { <-ctx.Done() }(); return nil }, nil)
	mgr2.Register("test:failover", func(ctx context.Context) error { go func() { <-ctx.Done() }(); return nil }, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr1.Run(ctx)
	go mgr2.Run(ctx)

	// 等 leader1 抢到
	deadline := time.Now().Add(3 * time.Second)
	initialLeader := (*Manager)(nil)
	for time.Now().Before(deadline) {
		if mgr1.IsLeader("test:failover") {
			initialLeader = mgr1
			break
		}
		if mgr2.IsLeader("test:failover") {
			initialLeader = mgr2
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if initialLeader == nil {
		t.Fatal("no leader elected initially")
	}
	t.Logf("initial leader: %s", initialLeader.InstanceID())

	// 主动关闭 leader（mgr.Close 会 quit electLoop 并 MarkDraining）。
	// 模拟 leader 进程崩溃。
	if initialLeader == mgr1 {
		_ = mgr1.Close()
	} else {
		_ = mgr2.Close()
	}

	// 等另一个实例接管
	deadline = time.Now().Add(5 * time.Second)
	newLeader := (*Manager)(nil)
	for time.Now().Before(deadline) {
		if initialLeader == mgr1 && mgr2.IsLeader("test:failover") {
			newLeader = mgr2
			break
		}
		if initialLeader == mgr2 && mgr1.IsLeader("test:failover") {
			newLeader = mgr1
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if newLeader == nil {
		t.Fatal("failover did not happen within 5s")
	}
	t.Logf("new leader after failover: %s", newLeader.InstanceID())

	cancel()
}

func TestResignAll_ClearsLeadership(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mgr.Register("test:resign",
		func(ctx context.Context) error { go func() { <-ctx.Done() }(); return nil },
		nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	// 等成为 leader
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.IsLeader("test:resign") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !mgr.IsLeader("test:resign") {
		t.Fatal("expected to become leader before resign")
	}

	// ResignAll
	resignCtx, resignCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resignCancel()
	if err := mgr.ResignAll(resignCtx); err != nil {
		t.Fatalf("ResignAll: %v", err)
	}

	if mgr.IsLeader("test:resign") {
		t.Error("after ResignAll, IsLeader should be false")
	}
	if !mgr.IsDraining() {
		t.Error("after ResignAll, IsDraining should be true")
	}

	cancel()
}

func TestSubscribe_OnBecomeOnLose(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	var becomeCount atomic.Int32
	var loseCount atomic.Int32
	mgr.Register("test:sub",
		func(ctx context.Context) error { go func() { <-ctx.Done() }(); return nil },
		nil,
	)
	mgr.Subscribe("test:sub", Subscribers{
		OnBecome: func(ctx context.Context) error {
			becomeCount.Add(1)
			return nil
		},
		OnLose: func(ctx context.Context) error {
			loseCount.Add(1)
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	// 等 onBecome
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if becomeCount.Load() == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if becomeCount.Load() != 1 {
		t.Fatalf("onBecome invoked %d times, want 1", becomeCount.Load())
	}

	// ResignAll 触发 onLose
	resignCtx, resignCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resignCancel()
	_ = mgr.ResignAll(resignCtx)

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if loseCount.Load() == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if loseCount.Load() != 1 {
		t.Errorf("onLose invoked %d times, want 1", loseCount.Load())
	}

	cancel()
}

func TestStartFailure_ReleasesAndRetries(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	var attempts atomic.Int32
	mgr.Register("test:failstart",
		func(ctx context.Context) error {
			attempts.Add(1)
			return errors.New("start intentionally fails")
		},
		nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	// 等若干次重试（默认 backoff=1s，所以至少等 1.5s）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if attempts.Load() >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if got := attempts.Load(); got < 2 {
		t.Errorf("expected at least 2 start attempts, got %d", got)
	}
	if mgr.IsLeader("test:failstart") {
		t.Error("should never become leader when start always fails")
	}

	cancel()
}

func TestMarkDraining_Idempotent(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mgr.MarkDraining()
	mgr.MarkDraining() // 二次调用应不 panic
	if !mgr.IsDraining() {
		t.Error("IsDraining should be true after MarkDraining")
	}
}

func TestWorkersRunning_InitialEmpty(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	if got := mgr.WorkersRunning(); len(got) != 0 {
		t.Errorf("fresh manager WorkersRunning len = %d, want 0", len(got))
	}
}
