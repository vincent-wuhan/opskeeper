package flow

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/flow"
)

// nopSchedulerStateRepo 满足 ScheduleStateRepo 接口但不返回任何持久化状态。
type nopSchedulerStateRepo struct{}

func (nopSchedulerStateRepo) LoadScheduleStates(context.Context) ([]*model.FlowScheduleNextFire, error) {
	return nil, nil
}
func (nopSchedulerStateRepo) UpsertScheduleState(context.Context, *model.FlowScheduleNextFire) error {
	return nil
}
func (nopSchedulerStateRepo) DeleteScheduleStatesNotIn(context.Context, []ScheduleStateKey) error {
	return nil
}

// stubFlowRepo 满足 Repo 接口的最小子集：scheduler 只调 ListEnabled。
type stubFlowRepo struct {
	listCalls atomic.Int64
}

func (r *stubFlowRepo) Create(context.Context, *model.Flow) error        { return nil }
func (r *stubFlowRepo) Update(context.Context, *model.Flow) error        { return nil }
func (r *stubFlowRepo) Get(context.Context, uint64) (*model.Flow, error) { return nil, nil }
func (r *stubFlowRepo) List(context.Context, int, int) ([]*model.Flow, int64, error) {
	return nil, 0, nil
}
func (r *stubFlowRepo) ListEnabled(context.Context) ([]*model.Flow, error) {
	r.listCalls.Add(1)
	return nil, nil
}
func (r *stubFlowRepo) Delete(context.Context, uint64) error { return nil }

type stubRunRepo struct{}

func (stubRunRepo) CreateRun(context.Context, *model.FlowRun) error { return nil }
func (stubRunRepo) UpdateRun(context.Context, *model.FlowRun) error { return nil }
func (stubRunRepo) GetRun(context.Context, string) (*model.FlowRun, error) {
	return nil, nil
}
func (stubRunRepo) ListRuns(context.Context, uint64, int) ([]*model.FlowRun, error) {
	return nil, nil
}
func (stubRunRepo) CreateNode(context.Context, *model.FlowRunNode) error { return nil }
func (stubRunRepo) UpdateNode(context.Context, *model.FlowRunNode) error { return nil }
func (stubRunRepo) ListNodes(context.Context, string) ([]*model.FlowRunNode, error) {
	return nil, nil
}
func (stubRunRepo) SweepStaleRunning(context.Context, string) (int64, error) { return 0, nil }
func (stubRunRepo) PruneRuns(context.Context, time.Time) (int64, error)      { return 0, nil }

// TestSchedulerStopExitsRunningLoop 验证 Start 启动 loop 后 Stop 能干净退出。
//
// loop 的 interval 设为 1h，自然 tick 不会发生；我们用 100ms 短 interval
// 走一次 tick 路径来断言 uc.ListEnabledFlows 被调过，再 Stop 验证 doneCh 真的
// 关闭（loop 退出）。
func TestSchedulerStopExitsRunningLoop(t *testing.T) {
	repo := &stubFlowRepo{}
	uc := NewUsecase(repo, stubRunRepo{}, nil, slog.Default())
	s := NewScheduler(uc, nopSchedulerStateRepo{}, slog.Default())
	s.interval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)
	// 等至少一次 tick
	deadline := time.Now().Add(2 * time.Second)
	for repo.listCalls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("loop did not tick within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// 二次 Stop 应当立即返回 nil（idempotent）
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
	// started 在 Stop 之后被复位（供 leader 重新获选后再次 Start）
	// 但当前实现是 no-reset：leader.Manager 不会重入 Start；保留为 false
	// 反而更安全（防止过期 start 覆盖新 doneCh）。
	if s.started {
		t.Error("started should be false after Stop")
	}
}

func TestSchedulerStartIsIdempotent(t *testing.T) {
	repo := &stubFlowRepo{}
	uc := NewUsecase(repo, stubRunRepo{}, nil, slog.Default())
	s := NewScheduler(uc, nopSchedulerStateRepo{}, slog.Default())
	s.interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)
	firstStopCh := s.stopCh
	firstDoneCh := s.doneCh

	// 第二次 Start 应当 no-op：stopCh/doneCh 不被覆盖
	s.Start(ctx)
	if s.stopCh != firstStopCh {
		t.Error("Start overwrote stopCh on idempotent call")
	}
	if s.doneCh != firstDoneCh {
		t.Error("Start overwrote doneCh on idempotent call")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestSchedulerStopOnNilReturns(t *testing.T) {
	var s *Scheduler
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("nil scheduler Stop should return nil, got %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("idempotent nil scheduler Stop should return nil, got %v", err)
	}
}

func TestSchedulerStopRespectsContext(t *testing.T) {
	s := NewScheduler(nil, nopSchedulerStateRepo{}, slog.Default())
	s.started = true
	s.stopCh = make(chan struct{})
	// doneCh 永远不会被关闭 → Stop 应当返回 ctx 错误
	s.doneCh = make(chan struct{})

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stopCancel()
	err := s.Stop(stopCtx)
	if err == nil {
		t.Fatal("Stop should return ctx error when doneCh is never closed")
	}
}
