package probes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
)

// ---- stub Checker ----

type stubChecker struct {
	name  string
	err   error
	delay time.Duration
	calls atomic.Int64
}

func (s *stubChecker) Name() string { return s.name }
func (s *stubChecker) Check(ctx context.Context) error {
	s.calls.Add(1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.err
}

// ---- Liveness tests ----

func TestLivenessAlways200(t *testing.T) {
	p := NewProbes(&stubChecker{name: "fail", err: errors.New("nope")})
	w := httptest.NewRecorder()
	p.LivenessHandler()(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Errorf("liveness = %d, want 200", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("liveness body = %q, want ok", w.Body.String())
	}
}

// ---- Readiness tests ----

func TestReadinessAllPass200(t *testing.T) {
	c1 := &stubChecker{name: "a"}
	c2 := &stubChecker{name: "b"}
	p := NewProbes(c1, c2)

	w := httptest.NewRecorder()
	p.ReadinessHandler("inst-1", func() string { return "leader" }, nil)(
		w, httptest.NewRequest("GET", "/readyz", nil))

	if w.Code != 200 {
		t.Fatalf("readyz = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp ReadinessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Ready {
		t.Error("Ready should be true")
	}
	if resp.Role != "leader" {
		t.Errorf("Role = %q, want leader", resp.Role)
	}
	if resp.InstanceID != "inst-1" {
		t.Errorf("InstanceID = %q", resp.InstanceID)
	}
	if len(resp.Checks) != 2 {
		t.Errorf("Checks len = %d, want 2", len(resp.Checks))
	}
	for name, check := range resp.Checks {
		if !check.OK {
			t.Errorf("check %s not OK: %s", name, check.Error)
		}
	}
}

func TestReadinessOneFails503(t *testing.T) {
	c1 := &stubChecker{name: "db"}
	c2 := &stubChecker{name: "redis", err: errors.New("connection refused")}
	p := NewProbes(c1, c2)

	w := httptest.NewRecorder()
	p.ReadinessHandler("inst-1", nil, nil)(
		w, httptest.NewRequest("GET", "/readyz", nil))

	if w.Code != 503 {
		t.Fatalf("readyz = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var resp ReadinessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Ready {
		t.Error("Ready should be false")
	}
	if resp.Checks["db"].OK != true {
		t.Error("db should be OK")
	}
	if resp.Checks["redis"].OK != false {
		t.Error("redis should fail")
	}
	if resp.Checks["redis"].Error == "" {
		t.Error("redis error message should be populated")
	}
}

func TestReadinessEmptyProbes200(t *testing.T) {
	p := NewProbes()
	w := httptest.NewRecorder()
	p.ReadinessHandler("inst", nil, nil)(
		w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 200 {
		t.Errorf("empty probes = %d, want 200", w.Code)
	}
}

func TestReadinessTimeoutProducesError(t *testing.T) {
	// Checker that sleeps 200ms; runCheckers enforces 500ms timeout —
	// this should succeed. But a checker sleeping 600ms should fail.
	fast := &stubChecker{name: "fast", delay: 50 * time.Millisecond}
	slow := &stubChecker{name: "slow", delay: 600 * time.Millisecond}
	p := NewProbes(fast, slow)

	w := httptest.NewRecorder()
	p.ReadinessHandler("inst", nil, nil)(
		w, httptest.NewRequest("GET", "/readyz", nil))

	if w.Code != 503 {
		t.Fatalf("readyz = %d, want 503 (slow checker should timeout)", w.Code)
	}
	var resp ReadinessResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Checks["fast"].OK != true {
		t.Error("fast checker should pass")
	}
	if resp.Checks["slow"].OK != false {
		t.Error("slow checker should timeout → fail")
	}
}

func TestReadinessRunsCheckersInParallel(t *testing.T) {
	// 4 checkers each sleeping 100ms. Sequential would take 400ms;
	// parallel finishes in ~100ms. We assert < 300ms wall.
	checkers := make([]Checker, 4)
	for i := range checkers {
		checkers[i] = &stubChecker{name: string(rune('a' + i)), delay: 100 * time.Millisecond}
	}
	p := NewProbes(checkers...)

	start := time.Now()
	w := httptest.NewRecorder()
	p.ReadinessHandler("inst", nil, nil)(
		w, httptest.NewRequest("GET", "/readyz", nil))
	elapsed := time.Since(start)

	if w.Code != 200 {
		t.Errorf("readyz = %d, want 200", w.Code)
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("checkers ran sequentially: elapsed=%v (want < 300ms)", elapsed)
	}
}

// ---- RedisChecker integration test (miniredis) ----

func TestRedisCheckerWithMiniredis(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Skipf("miniredis unavailable: %v", err)
	}
	defer mr.Close()

	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer cli.Close()

	c := RedisChecker(cli)
	if err := c.Check(context.Background()); err != nil {
		t.Errorf("healthy redis check failed: %v", err)
	}

	// Kill miniredis → check should fail
	mr.Close()
	if err := c.Check(context.Background()); err == nil {
		t.Error("dead redis check should fail")
	}
}

func TestRedisCheckerNilClient(t *testing.T) {
	c := RedisChecker(nil)
	if err := c.Check(context.Background()); err == nil {
		t.Error("nil redis client should fail")
	}
}

func TestDBCheckerNilDB(t *testing.T) {
	c := DBChecker(nil)
	if err := c.Check(context.Background()); err == nil {
		t.Error("nil db should fail")
	}
}
