package biz_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/edge/biz"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// fakeClient is a tunnel.Client stub for agent loop tests. It counts the
// methods invoked on it; Dial always succeeds immediately.
type fakeClient struct {
	mu         sync.Mutex
	handlers   map[string]tunnel.Handler
	callCounts map[string]int32
	lastReqs   map[string]any

	onRegisterEdge func(req tunnel.RegisterEdgeRequest) tunnel.RegisterEdgeResponse

	// pushAccepted scripts what the center says about a telemetry push.
	// It exists because "Accepted is the number the center actually
	// stored" is the contract the drain is built on, and a fake that
	// always says zero cannot tell retry from discard apart.
	pushAccepted pushScript

	closed atomic.Bool
}

// pushScript is what a fake center answers to a push. Zero values mean
// "accepted nothing", which is the retry signal.
type pushScript struct {
	hostMetricAccepted uint32
	promAccepted       int
	err                error
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		handlers:   map[string]tunnel.Handler{},
		callCounts: map[string]int32{},
		lastReqs:   map[string]any{},
	}
}

func (f *fakeClient) Dial(ctx context.Context) error { return nil }

func (f *fakeClient) RegisterHandler(method string, h tunnel.Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = h
}

func (f *fakeClient) Call(ctx context.Context, method string, req, resp any) error {
	f.mu.Lock()
	f.callCounts[method]++
	f.lastReqs[method] = req
	f.mu.Unlock()

	if method == tunnel.MethodRegisterEdge && resp != nil {
		var rreq tunnel.RegisterEdgeRequest
		if b, err := json.Marshal(req); err == nil {
			_ = json.Unmarshal(b, &rreq)
		}
		fn := f.onRegisterEdge
		if fn == nil {
			fn = func(r tunnel.RegisterEdgeRequest) tunnel.RegisterEdgeResponse {
				return tunnel.RegisterEdgeResponse{EdgeID: 1, ServerTime: time.Now().Unix()}
			}
		}
		out := fn(rreq)
		b, _ := json.Marshal(out)
		return json.Unmarshal(b, resp)
	}
	if resp != nil {
		switch method {
		case tunnel.MethodPushHostMetrics:
			b, _ := json.Marshal(tunnel.PushHostMetricsResponse{Accepted: f.pushAccepted.hostMetricAccepted})
			_ = json.Unmarshal(b, resp)
		case tunnel.MethodPushPromSamples:
			b, _ := json.Marshal(tunnel.PushPromSamplesResponse{Accepted: f.pushAccepted.promAccepted})
			_ = json.Unmarshal(b, resp)
		}
	}
	return f.pushAccepted.err
}

// OnReconnect is a no-op in the fake — these tests never trigger a
// tunnel-level reconnect, only verify periodic Call invocations.
func (f *fakeClient) OnReconnect(_ func()) {}

// AcceptStream satisfies the Client interface. WebSSH stream dispatch
// is exercised elsewhere; agent-lifecycle tests don't use it.
func (f *fakeClient) AcceptStream() (tunnel.StreamConn, error) {
	return nil, fmt.Errorf("fakeClient: AcceptStream not implemented")
}

func (f *fakeClient) Close() error { f.closed.Store(true); return nil }

func (f *fakeClient) countOf(method string) int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCounts[method]
}

func (f *fakeClient) lastReq(method string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReqs[method]
}

// fakeCollector returns deterministic non-zero values.
type fakeCollector struct {
	collectCount atomic.Int32
	hostInfoHits atomic.Int32
}

func (c *fakeCollector) CollectAll(ctx context.Context) ([]biz.CollectorOutput, error) {
	c.collectCount.Add(1)
	return []biz.CollectorOutput{{
		Source: "embedded",
		HostPoint: tunnel.HostMetricPoint{
			Ts:     time.Now().Unix(),
			CPUPct: 1.0,
		},
		HostPointValid: true,
		Samples: []tunnel.PromSample{
			{Name: "node_load1", Value: 0.5, TsMs: time.Now().UnixMilli()},
		},
	}}, nil
}
func (c *fakeCollector) HostInfo(ctx context.Context) (tunnel.HostInfo, error) {
	c.hostInfoHits.Add(1)
	return tunnel.HostInfo{Hostname: "fake", OS: "linux", Arch: "amd64", CPUCount: 4}, nil
}
func (c *fakeCollector) GetHostLoad(ctx context.Context) (tunnel.GetHostLoadResponse, error) {
	return tunnel.GetHostLoadResponse{CPUPct: 3.3}, nil
}
func (c *fakeCollector) GetProcessList(ctx context.Context, topN int, sortBy string) (tunnel.GetProcessListResponse, error) {
	return tunnel.GetProcessListResponse{
		Processes: []tunnel.ProcessInfo{{PID: 1, Name: "init"}},
	}, nil
}

// TestAgent_RunBasics asserts register_edge + heartbeat + the three push
// methods all fire on a real run.
//
// The metrics tick is deliberately NOT shortened. It used to be 50ms, which
// is a sampling rate the wire cannot carry: host metric timestamps are whole
// seconds, so the test was asserting that a configuration the protocol
// cannot represent works. It also would have been actively misleading once
// host_metrics_raw deduplicated on (edge_id, ts) — a sub-second tick now
// drops every other sample by design. The test therefore runs a little over
// one honest tick instead of many impossible ones, which is slower by about a
// second and is the price of the assertion meaning something.
func TestAgent_RunBasics(t *testing.T) {
	fc := newFakeClient()
	coll := &fakeCollector{}

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := biz.NewAgent(fc, coll, biz.Config{
		HeartbeatInterval: 50 * time.Millisecond,
		MetricsInterval:   biz.MinMetricsInterval,
		MetricsBatchSize:  1,
		AgentVersion:      "test",
	}, discard)

	ctx, cancel := context.WithTimeout(context.Background(), biz.MinMetricsInterval+300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run didn't return after ctx cancel")
	}

	if got := fc.countOf(tunnel.MethodRegisterEdge); got != 1 {
		t.Errorf("register_edge called %d times, want 1", got)
	}
	if got := fc.countOf(tunnel.MethodHeartbeat); got < 1 {
		t.Errorf("heartbeat called %d times, want >=1", got)
	}
	if got := fc.countOf(tunnel.MethodPushHostMetrics); got < 1 {
		t.Errorf("push_host_metrics called %d times, want >=1", got)
	}
	if hb, ok := fc.lastReq(tunnel.MethodHeartbeat).(tunnel.HeartbeatRequest); !ok || hb.EdgeID != 1 {
		t.Errorf("heartbeat edge_id = %#v, want 1", fc.lastReq(tunnel.MethodHeartbeat))
	}
	if pm, ok := fc.lastReq(tunnel.MethodPushHostMetrics).(tunnel.PushHostMetricsRequest); !ok || pm.EdgeID != 1 {
		t.Errorf("push_host_metrics edge_id = %#v, want 1", fc.lastReq(tunnel.MethodPushHostMetrics))
	}
	if ps, ok := fc.lastReq(tunnel.MethodPushPromSamples).(tunnel.PushPromSamplesRequest); !ok || ps.EdgeID != 1 {
		t.Errorf("push_prom_samples edge_id = %#v, want 1", fc.lastReq(tunnel.MethodPushPromSamples))
	}
	if a.EdgeID() != 1 {
		t.Errorf("EdgeID()=%d want 1", a.EdgeID())
	}
	if coll.hostInfoHits.Load() != 1 {
		t.Errorf("HostInfo calls=%d want 1", coll.hostInfoHits.Load())
	}
	if coll.collectCount.Load() < 1 {
		t.Errorf("Collect calls=%d want >=1", coll.collectCount.Load())
	}
	if !fc.closed.Load() {
		t.Errorf("client.Close() was not called on shutdown")
	}
}

// TestAgent_HandlersRegistered asserts that handlers are available on
// the fakeClient after Run starts (before ctx cancel).
func TestAgent_HandlersRegistered(t *testing.T) {
	fc := newFakeClient()
	coll := &fakeCollector{}
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := biz.NewAgent(fc, coll, biz.Config{
		HeartbeatInterval: time.Second,
		MetricsInterval:   time.Second,
		MetricsBatchSize:  10,
	}, discard)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = a.Run(ctx) }()

	// Wait for registerHandlers to execute.
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		fc.mu.Lock()
		_, haveLoad := fc.handlers[tunnel.MethodGetHostLoad]
		_, haveProc := fc.handlers[tunnel.MethodGetProcessList]
		fc.mu.Unlock()
		if haveLoad && haveProc {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("handlers not registered within 500ms")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Invoke one via the handler table to confirm it actually calls
	// into the collector.
	fc.mu.Lock()
	h := fc.handlers[tunnel.MethodGetHostLoad]
	fc.mu.Unlock()
	if h == nil {
		t.Fatalf("get_host_load handler missing")
	}
	out, err := h(context.Background(), tunnel.Session{}, tunnel.MethodGetHostLoad, nil)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	var resp tunnel.GetHostLoadResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal handler out: %v", err)
	}
	if resp.CPUPct != 3.3 {
		t.Fatalf("response.CPUPct=%v want 3.3", resp.CPUPct)
	}

	cancel()
}

// TestNewAgent_ClampsAMetricsIntervalTheWireCannotCarry is the other half of
// the (edge_id, ts) dedup: once the center stores one point per edge per
// second, a sub-second tick is not "finer granularity", it is a sample that
// gets thrown away. NewAgent clamps rather than refuses — a node with a
// working link should not be stranded over a tunable — but the operator has
// to be able to find out that the number they configured is not the number in
// force, so the clamp is a WARN and this asserts it was written.
func TestNewAgent_ClampsAMetricsIntervalTheWireCannotCarry(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	fc := newFakeClient()
	coll := &fakeCollector{}
	a := biz.NewAgent(fc, coll, biz.Config{
		MetricsInterval: 250 * time.Millisecond,
	}, logger)

	// The clamp is observable through behaviour, not through a getter: run
	// for less than the *configured* interval and nothing must have been
	// sampled, which is the difference between "slowed to the floor" and
	// "still running at 250ms".
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	if n := coll.collectCount.Load(); n != 0 {
		t.Fatalf("the collector was called %d times in 150ms at a clamped 1s interval, want 0", n)
	}
	if !strings.Contains(logBuf.String(), "MetricsInterval") {
		t.Errorf("the clamp was not logged; the operator cannot see the number in force:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "in_force=1s") {
		t.Errorf("the log does not say what is in force:\n%s", logBuf.String())
	}
}

// TestNewAgent_LeavesAnHonestIntervalAlone is the other direction: the floor
// must not quietly rewrite a working configuration, or a node that had asked
// for 10s and got 10s would have no way to tell it from one that had asked
// for 250ms.
func TestNewAgent_LeavesAnHonestIntervalAlone(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	fc := newFakeClient()
	coll := &fakeCollector{}
	a := biz.NewAgent(fc, coll, biz.Config{MetricsInterval: 10 * time.Second}, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	if strings.Contains(logBuf.String(), "clamped") {
		t.Errorf("a 10s interval was rewritten:\n%s", logBuf.String())
	}
	if n := coll.collectCount.Load(); n != 0 {
		t.Errorf("Collect called %d times in 150ms at a 10s interval, want 0", n)
	}
}
