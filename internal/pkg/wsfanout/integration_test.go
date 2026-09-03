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

// TestIntegration_CrossPodFanOut 验证 AIOps chat 跨副本 stop 的端到端行为。
func TestIntegration_CrossPodFanOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cli, _ := newTestRedis(t)
	met := wsfanout.NewMetrics(prometheus.NewRegistry())

	// pod-A 持有 session
	regA := wsfanout.NewRegistry(cli, "pod-A", met)
	ctrlA := wsfanout.NewControl(cli, "pod-A", met)

	// pod-B 接收 stop 请求
	regB := wsfanout.NewRegistry(cli, "pod-B", met)
	ctrlB := wsfanout.NewControl(cli, "pod-B", met)

	// pod-A 注册 session
	if err := regA.Register(ctx, wsfanout.KindAIOpsStream, "sess-001",
		map[string]string{"user_id": "42"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// pod-A 注册 stop handler
	var stopCalled atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	ctrlA.Subscribe(wsfanout.ActionStop, func(ctx context.Context, msg wsfanout.Message) {
		stopCalled.Store(true)
		wg.Done()
	})
	go ctrlA.SubscribeLoop(ctx)
	time.Sleep(100 * time.Millisecond)

	// pod-B 查 owner 并异步通知
	owner, kind, err := regB.Lookup(ctx, "sess-001")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if owner != "pod-A" {
		t.Fatalf("owner = %q, want pod-A", owner)
	}
	if kind != wsfanout.KindAIOpsStream {
		t.Errorf("kind = %q, want aiops_stream", kind)
	}
	if owner == ctrlB.PodID() {
		t.Fatal("test setup wrong: B thinks it owns")
	}

	if err := ctrlB.Send(ctx, owner, wsfanout.Message{
		Action:    wsfanout.ActionStop,
		SessionID: "sess-001",
		Reason:    "user_requested",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if waitTimeout(&wg, 2*time.Second) {
		t.Fatal("cross-pod stop handler not called within 2s")
	}
	if !stopCalled.Load() {
		t.Error("stopCalled = false")
	}
}

// TestIntegration_WebShellListAcrossPods 验证 WebShell list 跨副本 union。
func TestIntegration_WebShellListAcrossPods(t *testing.T) {
	ctx := context.Background()
	cli, _ := newTestRedis(t)
	met := wsfanout.NewMetrics(prometheus.NewRegistry())

	regA := wsfanout.NewRegistry(cli, "pod-A", met)
	regB := wsfanout.NewRegistry(cli, "pod-B", met)

	if err := regA.Register(ctx, wsfanout.KindWebShell, "ws-001", nil); err != nil {
		t.Fatal(err)
	}
	if err := regA.Register(ctx, wsfanout.KindWebShell, "ws-002", nil); err != nil {
		t.Fatal(err)
	}
	if err := regB.Register(ctx, wsfanout.KindWebShell, "ws-003", nil); err != nil {
		t.Fatal(err)
	}

	// 任意副本调 ScanByKind 都应看到全部 3 个
	infos, err := regA.ScanByKind(ctx, wsfanout.KindWebShell)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Errorf("total webshell sessions = %d, want 3", len(infos))
	}
	// 跨副本可查到 pod-B 的
	hasB := false
	for _, info := range infos {
		if info.PodID == "pod-B" && info.SessionID == "ws-003" {
			hasB = true
		}
	}
	if !hasB {
		t.Error("ScanByKind did not return pod-B's ws-003")
	}
}
