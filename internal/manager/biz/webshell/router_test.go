package webshell

import (
	"context"
	"log/slog"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"

	wsfanout "github.com/vincent-wuhan/opskeeper/internal/pkg/wsfanout"
)

// TestRouter_Kill_LocalOnly 验证未注入 fanout 时 Kill 走原路径。
func TestRouter_Kill_LocalOnly(t *testing.T) {
	r := NewRouter()
	sink := &fakeSink{}
	r.Register("ws-local", sink, ActiveSession{SessionID: "ws-local"})

	killed, pod, err := r.Kill("ws-local", "test")
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !killed || pod != "" {
		t.Errorf("local Kill: killed=%v pod=%q, want (true, \"\")", killed, pod)
	}
	if !sink.killCalled {
		t.Error("Kill hook not called")
	}
}

// TestRouter_Kill_UnknownSession 验证未知 session 返回 (false, "", nil)。
func TestRouter_Kill_UnknownSession(t *testing.T) {
	r := NewRouter()
	killed, pod, err := r.Kill("nope", "test")
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if killed || pod != "" {
		t.Errorf("unknown Kill: killed=%v pod=%q, want (false, \"\")", killed, pod)
	}
}

// fakeSink 实现 Sink + Killer。
type fakeSink struct {
	killCalled bool
	lastReason string
}

func (s *fakeSink) OnOutput(data []byte) error         { return nil }
func (s *fakeSink) OnExit(exitCode int, errMsg string) {}
func (s *fakeSink) Kill(reason string) {
	s.killCalled = true
	s.lastReason = reason
}

// TestRouter_WithFanout_NoopWhenNil 验证未注入 fanout 时 ListAllKind 走本地路径。
func TestRouter_WithFanout_NoopWhenNil(t *testing.T) {
	r := NewRouter()
	sink := &fakeSink{}
	r.Register("ws-001", sink, ActiveSession{SessionID: "ws-001"})

	got := r.ListAllKind(context.Background(), wsfanout.KindWebShell)
	if len(got) != 1 || got[0].SessionID != "ws-001" {
		t.Errorf("ListAllKind = %+v, want 1 entry ws-001", got)
	}
}

// TestRouter_WithFanout_DelegatesToWiring 验证注入 fanout 后 ListAllKind 跨副本可查到。
func TestRouter_WithFanout_DelegatesToWiring(t *testing.T) {
	// 共享 miniredis：两个 pod 看同一个 Redis
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	mkCli := func() *redisClient { return redisClientAt(mr.Addr()) }
	_ = mkCli // satisfy unused
	cli := redisClientAt(mr.Addr())
	met := wsfanout.NewMetrics(prometheus.NewRegistry())
	log := slog.Default()

	wA := wsfanout.NewWiring(cli, "pod-A", met, log)
	wB := wsfanout.NewWiring(cli, "pod-B", met, log)

	rA := NewRouter()
	rA.WithFanout(wA, log)
	rB := NewRouter()
	rB.WithFanout(wB, log)

	// pod-A 注册一个 session
	if err := wA.Register(context.Background(), wsfanout.KindWebShell, "ws-A", nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 另一个 pod-B 视角的 Router 应能跨副本看到
	got := rB.ListAllKind(context.Background(), wsfanout.KindWebShell)
	if len(got) != 1 || got[0].SessionID != "ws-A" || got[0].PodID != "pod-A" {
		t.Errorf("ListAllKind from pod-B = %+v, want 1 entry (ws-A, pod-A)", got)
	}
}
