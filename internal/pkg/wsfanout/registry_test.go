package wsfanout_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/wsfanout"
)

// newTestRedis 起一个 miniredis + redis.Client。t.Cleanup 自动收尾。
func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = cli.Close()
		mr.Close()
	})
	return cli, mr
}

func newTestRegistry(t *testing.T, podID string) (*wsfanout.Registry, *miniredis.Miniredis) {
	cli, mr := newTestRedis(t)
	reg := wsfanout.NewRegistry(cli, podID, wsfanout.NewMetrics(prometheus.NewRegistry()),
		wsfanout.WithTTL(2*time.Second))
	return reg, mr
}

func TestNewPodID(t *testing.T) {
	pid1 := wsfanout.NewPodID("")
	if pid1 == "" {
		t.Fatal("NewPodID empty string")
	}
	pid2 := wsfanout.NewPodID("")
	if pid1 == pid2 {
		t.Errorf("expected unique pod IDs, got identical: %s", pid1)
	}

	override := "manager-0"
	if got := wsfanout.NewPodID(override); got != override {
		t.Errorf("override = %q, want %q", got, override)
	}
}

func TestRegistry_RegisterLookup(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestRegistry(t, "pod-A")

	extra := map[string]string{"user_id": "42"}
	if err := reg.Register(ctx, wsfanout.KindAIOpsStream, "sess-001", extra); err != nil {
		t.Fatalf("Register: %v", err)
	}

	pod, kind, err := reg.Lookup(ctx, "sess-001")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if pod != "pod-A" {
		t.Errorf("pod = %q, want pod-A", pod)
	}
	if kind != wsfanout.KindAIOpsStream {
		t.Errorf("kind = %q, want aiops_stream", kind)
	}
}

func TestRegistry_LookupMissing(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestRegistry(t, "pod-A")
	pod, kind, err := reg.Lookup(ctx, "nope")
	if err != nil {
		t.Fatalf("Lookup missing: %v", err)
	}
	if pod != "" || kind != "" {
		t.Errorf("Lookup missing = (%q, %q), want (\"\", \"\")", pod, kind)
	}
}

func TestRegistry_RegisterConflict(t *testing.T) {
	ctx := context.Background()
	cli, _ := newTestRedis(t)

	regA := wsfanout.NewRegistry(cli, "pod-A", wsfanout.NewMetrics(prometheus.NewRegistry()))
	regB := wsfanout.NewRegistry(cli, "pod-B", wsfanout.NewMetrics(prometheus.NewRegistry()))

	if err := regA.Register(ctx, wsfanout.KindAIOpsStream, "sess-001", nil); err != nil {
		t.Fatalf("A Register: %v", err)
	}
	err := regB.Register(ctx, wsfanout.KindAIOpsStream, "sess-001", nil)
	if err == nil {
		t.Fatal("B Register should fail with ErrSessionOwned")
	}
	if !wsfanout.IsErrSessionOwned(err) {
		t.Errorf("B Register err = %v, want ErrSessionOwned", err)
	}
	// A 仍持有
	pod, _, _ := regA.Lookup(ctx, "sess-001")
	if pod != "pod-A" {
		t.Errorf("A still owns, got pod=%q", pod)
	}
}

func TestRegistry_RegisterIdempotent(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestRegistry(t, "pod-A")

	// 同一 pod 多次 Register 应幂等通过
	if err := reg.Register(ctx, wsfanout.KindAIOpsStream, "sess-001", nil); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := reg.Register(ctx, wsfanout.KindAIOpsStream, "sess-001", nil); err != nil {
		t.Errorf("idempotent Register: %v", err)
	}
}

func TestRegistry_Unregister(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestRegistry(t, "pod-A")

	if err := reg.Register(ctx, wsfanout.KindAIOpsStream, "sess-001", nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Unregister(ctx, "sess-001"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	pod, _, _ := reg.Lookup(ctx, "sess-001")
	if pod != "" {
		t.Errorf("after Unregister, Lookup pod = %q, want empty", pod)
	}
}

func TestRegistry_TTLExpiry(t *testing.T) {
	ctx := context.Background()
	cli, mr := newTestRedis(t)
	reg := wsfanout.NewRegistry(cli, "pod-A", wsfanout.NewMetrics(prometheus.NewRegistry()),
		wsfanout.WithTTL(1*time.Second))

	if err := reg.Register(ctx, wsfanout.KindAIOpsStream, "sess-001", nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// miniredis 主动快进时间
	mr.FastForward(2 * time.Second)

	pod, _, err := reg.Lookup(ctx, "sess-001")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if pod != "" {
		t.Errorf("after TTL expiry, pod = %q, want empty", pod)
	}
}

func TestRegistry_HeartbeatThrottle(t *testing.T) {
	ctx := context.Background()
	cli, _ := newTestRedis(t)
	reg := wsfanout.NewRegistry(cli, "pod-A", wsfanout.NewMetrics(prometheus.NewRegistry()),
		wsfanout.WithTTL(5*time.Second))

	if err := reg.Register(ctx, wsfanout.KindAIOpsStream, "sess-001", nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// 第二次调用应在 1 分钟内被 throttle
	if err := reg.Heartbeat(ctx, "sess-001"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	// 模拟不同时间点的多次调用：仅第一次实际写 Redis
	for i := 0; i < 10; i++ {
		if err := reg.Heartbeat(ctx, "sess-001"); err != nil {
			t.Fatalf("Heartbeat[%d]: %v", i, err)
		}
	}
	// 验证 session 仍存在
	pod, _, _ := reg.Lookup(ctx, "sess-001")
	if pod != "pod-A" {
		t.Errorf("after Heartbeat, pod = %q", pod)
	}
}

func TestRegistry_ScanByKind(t *testing.T) {
	ctx := context.Background()
	cli, _ := newTestRedis(t)
	reg := wsfanout.NewRegistry(cli, "pod-A", wsfanout.NewMetrics(prometheus.NewRegistry()))

	for i := 0; i < 5; i++ {
		id := "aiops-" + string(rune('A'+i))
		if err := reg.Register(ctx, wsfanout.KindAIOpsStream, id, nil); err != nil {
			t.Fatalf("Register %s: %v", id, err)
		}
	}
	for i := 0; i < 3; i++ {
		id := "ws-" + string(rune('A'+i))
		if err := reg.Register(ctx, wsfanout.KindWebShell, id, nil); err != nil {
			t.Fatalf("Register %s: %v", id, err)
		}
	}

	infos, err := reg.ScanByKind(ctx, wsfanout.KindAIOpsStream)
	if err != nil {
		t.Fatalf("ScanByKind: %v", err)
	}
	if len(infos) != 5 {
		t.Errorf("aiops sessions = %d, want 5", len(infos))
	}
	for _, info := range infos {
		if info.Kind != wsfanout.KindAIOpsStream {
			t.Errorf("info.Kind = %q", info.Kind)
		}
	}

	wsInfos, err := reg.ScanByKind(ctx, wsfanout.KindWebShell)
	if err != nil {
		t.Fatalf("ScanByKind webshell: %v", err)
	}
	if len(wsInfos) != 3 {
		t.Errorf("webshell sessions = %d, want 3", len(wsInfos))
	}
}

func TestRegistry_RedisDown(t *testing.T) {
	ctx := context.Background()
	cli, mr := newTestRedis(t)
	reg := wsfanout.NewRegistry(cli, "pod-A", wsfanout.NewMetrics(prometheus.NewRegistry()))

	if err := reg.Register(ctx, wsfanout.KindAIOpsStream, "sess-001", nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mr.Close() // 关闭 miniredis，模拟 Redis 不可达

	// Register 失败返回 error，调用方可继续业务
	err := reg.Register(ctx, wsfanout.KindAIOpsStream, "sess-002", nil)
	if err == nil {
		t.Error("Register after Redis close: expected error")
	}
}
