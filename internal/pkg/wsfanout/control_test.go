package wsfanout_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/wsfanout"
)

func TestControl_SendSubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cli, _ := newTestRedis(t)
	met := wsfanout.NewMetrics(prometheus.NewRegistry())

	ctrlA := wsfanout.NewControl(cli, "pod-A", met)
	ctrlB := wsfanout.NewControl(cli, "pod-B", met)

	var received atomic.Int32
	var receivedSession atomic.Value
	var wg sync.WaitGroup
	wg.Add(1)
	ctrlA.Subscribe(wsfanout.ActionStop, func(ctx context.Context, msg wsfanout.Message) {
		receivedSession.Store(msg.SessionID)
		received.Add(1)
		wg.Done()
	})
	go ctrlA.SubscribeLoop(ctx)
	defer cancel()

	// 给 SubscribeLoop 一点时间建立连接
	time.Sleep(100 * time.Millisecond)

	if err := ctrlB.Send(ctx, "pod-A", wsfanout.Message{
		Action:    wsfanout.ActionStop,
		SessionID: "sess-001",
		Reason:    "user_requested",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if waitTimeout(&wg, 2*time.Second) {
		t.Fatal("handler not called within 2s")
	}
	if received.Load() != 1 {
		t.Errorf("received = %d, want 1", received.Load())
	}
	if got := receivedSession.Load(); got != "sess-001" {
		t.Errorf("received sessionID = %v, want sess-001", got)
	}
}

func TestControl_SendWrongTarget_NoHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cli, _ := newTestRedis(t)
	met := wsfanout.NewMetrics(prometheus.NewRegistry())

	ctrlA := wsfanout.NewControl(cli, "pod-A", met)
	ctrlX := wsfanout.NewControl(cli, "pod-X", met)

	// pod-A 没注册 handler；pod-X 也不订阅
	go ctrlA.SubscribeLoop(ctx)
	time.Sleep(100 * time.Millisecond)

	// 发到不存在的 pod：Redis 仍接受 PUBLISH；handler 未注册则静默丢弃
	if err := ctrlX.Send(ctx, "pod-A", wsfanout.Message{
		Action:    wsfanout.ActionStop,
		SessionID: "sess-001",
	}); err != nil {
		t.Errorf("Send: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
}

func TestControl_MultipleActions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cli, _ := newTestRedis(t)
	met := wsfanout.NewMetrics(prometheus.NewRegistry())
	ctrlA := wsfanout.NewControl(cli, "pod-A", met)
	ctrlB := wsfanout.NewControl(cli, "pod-B", met)

	var stopCount, killCount atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)

	ctrlA.Subscribe(wsfanout.ActionStop, func(ctx context.Context, msg wsfanout.Message) {
		stopCount.Add(1)
		wg.Done()
	})
	ctrlA.Subscribe(wsfanout.ActionKill, func(ctx context.Context, msg wsfanout.Message) {
		killCount.Add(1)
		wg.Done()
	})
	go ctrlA.SubscribeLoop(ctx)
	defer cancel()
	time.Sleep(100 * time.Millisecond)

	if err := ctrlB.Send(ctx, "pod-A", wsfanout.Message{
		Action: wsfanout.ActionStop, SessionID: "s1",
	}); err != nil {
		t.Fatalf("Send stop: %v", err)
	}
	if err := ctrlB.Send(ctx, "pod-A", wsfanout.Message{
		Action: wsfanout.ActionKill, SessionID: "s2",
	}); err != nil {
		t.Fatalf("Send kill: %v", err)
	}

	if waitTimeout(&wg, 2*time.Second) {
		t.Fatalf("not all handlers called: stop=%d kill=%d", stopCount.Load(), killCount.Load())
	}
	if stopCount.Load() != 1 || killCount.Load() != 1 {
		t.Errorf("stop=%d kill=%d, want 1, 1", stopCount.Load(), killCount.Load())
	}
}

func TestControl_SendRedisDown_FireAndForget(t *testing.T) {
	ctx := context.Background()
	cli, mr := newTestRedis(t)
	met := wsfanout.NewMetrics(prometheus.NewRegistry())
	ctrl := wsfanout.NewControl(cli, "pod-A", met)

	mr.Close()
	// Redis 不可达：Send 仍返回 nil（fire-and-forget）
	if err := ctrl.Send(ctx, "pod-B", wsfanout.Message{
		Action: wsfanout.ActionStop, SessionID: "s1",
	}); err != nil {
		t.Errorf("Send when Redis down: err = %v, want nil (fire-and-forget)", err)
	}
}

func TestControl_SubscribeLoopContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cli, _ := newTestRedis(t)
	met := wsfanout.NewMetrics(prometheus.NewRegistry())
	ctrl := wsfanout.NewControl(cli, "pod-A", met)

	done := make(chan struct{})
	go func() {
		ctrl.SubscribeLoop(ctx)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SubscribeLoop did not exit after ctx cancel")
	}
}

// waitTimeout 等待 wg 完成或超时。
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	c := make(chan struct{})
	go func() { wg.Wait(); close(c) }()
	select {
	case <-c:
		return false
	case <-time.After(d):
		return true
	}
}
