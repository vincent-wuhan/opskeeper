package systemupgrade

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type stubChecker struct {
	calls atomic.Int64
	err   error
}

func (s *stubChecker) Check(context.Context) (*Info, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	return &Info{CurrentVersion: "v0.0.0"}, nil
}

func TestSupervisorStartStopCycle(t *testing.T) {
	c := &stubChecker{}
	sup := NewSupervisor(SupervisorConfig{Service: c, Interval: 50 * time.Millisecond})
	if sup == nil {
		t.Fatal("NewSupervisor returned nil for non-nil Service")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 等至少 2 次 tick
	deadline := time.Now().Add(2 * time.Second)
	for c.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("loop did not tick twice within 2s (calls=%d)", c.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
	if sup.started.Load() {
		t.Error("started should be false after Stop")
	}
}

func TestSupervisorStartIsIdempotent(t *testing.T) {
	c := &stubChecker{}
	sup := NewSupervisor(SupervisorConfig{Service: c, Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup.Start(ctx)
	firstStopCh := sup.stopCh
	firstDoneCh := sup.doneCh

	sup.Start(ctx)
	if sup.stopCh != firstStopCh {
		t.Error("Start overwrote stopCh on idempotent call")
	}
	if sup.doneCh != firstDoneCh {
		t.Error("Start overwrote doneCh on idempotent call")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer stopCancel()
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestSupervisorStopsOnContextCancel(t *testing.T) {
	c := &stubChecker{}
	sup := NewSupervisor(SupervisorConfig{Service: c, Interval: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	sup.Start(ctx)
	cancel() // ctx 取消 → loop 退出

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer stopCancel()
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestSupervisorStopsOnLeaderLoss(t *testing.T) {
	c := &stubChecker{}
	sup := NewSupervisor(SupervisorConfig{Service: c, Interval: 50 * time.Millisecond})
	sup.Start(context.Background())

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer stopCancel()
	// 不取消 ctx,只 Stop
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 已 stopped 的 supervisor 不应被再次启动(语义上的 guard;leader.Manager
	// 在新一次获选时会调 Start,先 Stop 即可)
	sup.Start(context.Background())
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop after restart: %v", err)
	}
}

func TestSupervisorStopOnNilOrUnstartedReturns(t *testing.T) {
	var sup *Supervisor
	if err := sup.Stop(context.Background()); err != nil {
		t.Errorf("nil supervisor Stop should return nil, got %v", err)
	}
	c := &stubChecker{}
	sup2 := NewSupervisor(SupervisorConfig{Service: c, Interval: time.Hour})
	if err := sup2.Stop(context.Background()); err != nil {
		t.Errorf("unstarted supervisor Stop should return nil, got %v", err)
	}
}

func TestSupervisorSurvivesCheckerError(t *testing.T) {
	c := &stubChecker{err: errors.New("upstream down")}
	sup := NewSupervisor(SupervisorConfig{Service: c, Interval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Start(ctx)

	deadline := time.Now().Add(1 * time.Second)
	for c.calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("loop did not tick 3 times with errors: calls=%d", c.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer stopCancel()
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
