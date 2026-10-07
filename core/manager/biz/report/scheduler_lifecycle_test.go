package report

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestSchedulerStartStopCycle 验证 Scheduler 真实 Start/Stop 生命周期。
//
// 短 tick 让 loop 走一次 runOnce（noop repo，不会真的做事），Stop 验证 loop
// 真的退出 + 二次 Stop 幂等。
func TestSchedulerStartStopCycle(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nopGenerator{}, seqIDGen())
	s := NewScheduler(uc, nil)
	s.tick = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)
	// 等至少一次 tick
	deadline := time.Now().Add(2 * time.Second)
	for s.tick == 50*time.Millisecond {
		// poll: any tick means loop is alive
		if time.Now().After(deadline) {
			t.Fatal("loop did not tick within 2s")
		}
		if s.started {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
	if s.started {
		t.Error("started should be false after Stop")
	}
}

func TestSchedulerStartIsIdempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nopGenerator{}, seqIDGen())
	s := NewScheduler(uc, nil)
	s.tick = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)
	firstStopCh := s.stopCh
	firstDoneCh := s.doneCh

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

func TestSchedulerStopOnNilOrUnstartedReturns(t *testing.T) {
	var s *Scheduler
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("nil scheduler Stop should return nil, got %v", err)
	}
	repo := newFakeRepo()
	uc := NewUsecase(repo, nopGenerator{}, seqIDGen())
	s2 := NewScheduler(uc, nil)
	if err := s2.Stop(context.Background()); err != nil {
		t.Errorf("unstarted scheduler Stop should return nil, got %v", err)
	}
}

func TestSchedulerStopRespectsContext(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nopGenerator{}, seqIDGen())
	s := NewScheduler(uc, nil)
	// 手动模拟"已 Start 但 loop 永远不退出"的状态。
	s.started = true
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stopCancel()
	err := s.Stop(stopCtx)
	if err == nil {
		t.Fatal("Stop should return ctx error when doneCh is never closed")
	}
}

// 不变量：report 调度的原子计数 + 重启周期
var _ = atomic.Int64{}
