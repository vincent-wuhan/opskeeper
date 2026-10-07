package frontierbound

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/singchia/geminio"

	"github.com/vincent-wuhan/opskeeper/core/wire"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
)

// fakePromIngester captures the last Push call.
type fakePromIngester struct {
	mu      sync.Mutex
	gotEdge uint64
	gotSrc  string
	gotN    int
	wantErr error
	pushCnt int
}

func (f *fakePromIngester) Push(_ context.Context, edgeID uint64, source string, samples []tunnel.PromSample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushCnt++
	f.gotEdge = edgeID
	f.gotSrc = source
	f.gotN = len(samples)
	return f.wantErr
}

// fakeMetricIngester counts pushes and can be told to fail, which is what
// the push_host_metrics tests need in order to prove the handler forwards
// before it claims success.
type fakeMetricIngester struct {
	pushCnt int
	gotN    int
	wantErr error
}

func (f *fakeMetricIngester) Push(_ context.Context, _ uint64, points []tunnel.HostMetricPoint) error {
	f.pushCnt++
	f.gotN = len(points)
	return f.wantErr
}

// fakeDeviceResolver resolves edge_id -> device_id. By default it returns
// the edge_id itself (1:1, simulating a present host junction) so push
// tests reach the ingester. Set err (or id) to exercise the
// "junction missing -> drop" path (issue #96).
type fakeDeviceResolver struct {
	id  uint64 // when non-zero, always return this device_id
	err error  // when non-nil, simulate an unresolvable junction
}

func (f *fakeDeviceResolver) LookupHostDevice(_ context.Context, edgeID uint64) (uint64, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.id != 0 {
		return f.id, nil
	}
	return edgeID, nil
}

// installWith runs Install on a fakeService-backed Client and hands back
// both halves: the fake service, whose rpcs and lifecycle callbacks the
// tests drive, and the client, whose own surface (OnEdgeOffline) some tests
// need. The defaults below are what Install insists on; a test that cares
// about one of them sets it first.
func installWith(t *testing.T, w Wiring) (*fakeService, *Client) {
	t.Helper()
	fs := newFakeService()
	c := newWithService(fs, slog.Default())

	// Install requires non-nil EdgeAuthn / EdgeUC; supply zero-value
	// usecase + a tiny authn proxy. We don't dispatch register_edge etc
	// in these tests, so internal nil-deref is fine.
	if w.EdgeAuthn == nil {
		w.EdgeAuthn = (&edgebiz.AccessKeyAuthenticator{})
	}
	if w.EdgeUC == nil {
		w.EdgeUC = (&edgebiz.Usecase{})
	}
	if w.MetricIngester == nil {
		w.MetricIngester = &fakeMetricIngester{}
	}
	if w.DeviceResolver == nil {
		// Default: a present 1:1 junction so push tests reach the ingester.
		w.DeviceResolver = &fakeDeviceResolver{}
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	if err := Install(context.Background(), c, w); err != nil {
		t.Fatalf("Install: %v", err)
	}
	return fs, c
}

// installAndDispatch is installWith plus the handler most of these tests
// dispatch: push_prom_samples.
func installAndDispatch(t *testing.T, w Wiring) (*fakeService, geminio.RPC) {
	t.Helper()
	fs, _ := installWith(t, w)
	return fs, rpcFor(t, fs, tunnel.MethodPushPromSamples)
}

// rpcFor returns a registered reverse-call handler by method. The install
// helper returns the prom one because most tests need it; the metrics
// tests reach into the same service for theirs.
func rpcFor(t *testing.T, fs *fakeService, method string) geminio.RPC {
	t.Helper()
	rpc, ok := fs.rpcs[method]
	if !ok {
		t.Fatalf("%s not registered", method)
	}
	return rpc
}

func TestInstall_PushPromSamples_HappyPath(t *testing.T) {
	pi := &fakePromIngester{}
	_, rpc := installAndDispatch(t, Wiring{PromIngester: pi, Log: slog.Default()})

	// EdgeID in body establishes the canonical binding (mirrors the
	// real edge agent flow after register_edge succeeds). Without this,
	// canonicalizeEdgeID returns 0 and the handler correctly drops the
	// request — see TestInstall_PushPromSamples_DropsBeforeRegister.
	body, _ := json.Marshal(tunnel.PushPromSamplesRequest{
		EdgeID: 42,
		Source: "embedded:gopsutil",
		Samples: []tunnel.PromSample{
			{Name: "node_cpu_seconds_total", Value: 1, TsMs: 100},
			{Name: "node_cpu_seconds_total", Value: 2, TsMs: 200},
			{Name: "node_memory_MemAvailable_bytes", Value: 3, TsMs: 300},
		},
	})
	req := &fakeReq{data: body, clientID: 42}
	rsp := &fakeResp{}
	rpc(context.Background(), req, rsp)

	if rsp.err != nil {
		t.Fatalf("rpc returned error: %v", rsp.err)
	}
	if pi.pushCnt != 1 {
		t.Errorf("Push called %d times, want 1", pi.pushCnt)
	}
	if pi.gotEdge != 42 {
		t.Errorf("edgeID = %d, want 42", pi.gotEdge)
	}
	if pi.gotSrc != "embedded:gopsutil" {
		t.Errorf("source = %q", pi.gotSrc)
	}
	if pi.gotN != 3 {
		t.Errorf("n = %d, want 3", pi.gotN)
	}

	var out tunnel.PushPromSamplesResponse
	if err := json.Unmarshal(rsp.data, &out); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if out.Accepted != 3 {
		t.Errorf("Accepted = %d, want 3", out.Accepted)
	}
}

func TestInstall_PushPromSamples_BindsCanonicalEdgeIDFromBody(t *testing.T) {
	pi := &fakePromIngester{}
	_, rpc := installAndDispatch(t, Wiring{PromIngester: pi, Log: slog.Default()})

	body, _ := json.Marshal(tunnel.PushPromSamplesRequest{
		EdgeID: 2,
		Source: "embedded:gopsutil",
		Samples: []tunnel.PromSample{
			{Name: "node_cpu_seconds_total", Value: 1, TsMs: 100},
		},
	})
	req := &fakeReq{data: body, clientID: 7634846078675816708}
	rsp := &fakeResp{}
	rpc(context.Background(), req, rsp)

	if rsp.err != nil {
		t.Fatalf("rpc returned error: %v", rsp.err)
	}
	if pi.gotEdge != 2 {
		t.Fatalf("edgeID = %d, want 2", pi.gotEdge)
	}
}

func TestInstall_PushPromSamples_IngesterError(t *testing.T) {
	pi := &fakePromIngester{wantErr: errors.New("prom down")}
	_, rpc := installAndDispatch(t, Wiring{PromIngester: pi, Log: slog.Default()})

	// EdgeID in body so canonicalizeEdgeID resolves; otherwise the
	// pre-register drop path short-circuits before reaching the
	// ingester (see v0.7.39 fix for ghost edge_id label leak).
	body, _ := json.Marshal(tunnel.PushPromSamplesRequest{
		EdgeID:  1,
		Source:  "embedded",
		Samples: []tunnel.PromSample{{Name: "x", Value: 1, TsMs: 1}},
	})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 1}, rsp)
	if rsp.err == nil {
		t.Errorf("expected rsp.err on ingester failure")
	}
}

// Issue #96: when the host junction can't be resolved, resolveDeviceID
// returns 0 and the handler MUST drop the batch (never write edge_id as
// the device_id label). The ingester must not be called.
func TestInstall_PushPromSamples_DropsWhenDeviceUnresolved(t *testing.T) {
	pi := &fakePromIngester{}
	_, rpc := installAndDispatch(t, Wiring{
		PromIngester:   pi,
		DeviceResolver: &fakeDeviceResolver{err: errors.New("no host junction")},
		Log:            slog.Default(),
	})
	body, _ := json.Marshal(tunnel.PushPromSamplesRequest{
		EdgeID:  5,
		Source:  "embedded",
		Samples: []tunnel.PromSample{{Name: "x", Value: 1, TsMs: 1}},
	})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 1}, rsp)
	if rsp.err != nil {
		t.Fatalf("drop must not error: %v", rsp.err)
	}
	if pi.pushCnt != 0 {
		t.Fatalf("ingester called %d times, want 0 — must drop, never write edge_id as device_id", pi.pushCnt)
	}
}

func TestInstall_PushPromSamples_NoIngesterRefusesRatherThanPretends(t *testing.T) {
	// Wiring.PromIngester == nil => Prom disabled in this deployment. The
	// honest answer to the node is "accepted none": the old code returned
	// Accepted=n (a lie that made the node ack and discard the samples).
	// A node with a write-ahead log counts Accepted=0 as a refusal and
	// stops retrying — which is correct, because a store that is not wired
	// will not appear on a later attempt either.
	_, rpc := installAndDispatch(t, Wiring{PromIngester: nil, Log: slog.Default()})

	body, _ := json.Marshal(tunnel.PushPromSamplesRequest{
		EdgeID: 1,
		Source: "embedded",
		Samples: []tunnel.PromSample{
			{Name: "a", Value: 1, TsMs: 1},
			{Name: "b", Value: 2, TsMs: 2},
		},
	})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 1}, rsp)
	if rsp.err != nil {
		t.Errorf("expected a defined response, got err = %v", rsp.err)
	}
	var out tunnel.PushPromSamplesResponse
	if err := json.Unmarshal(rsp.data, &out); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if out.Accepted != 0 {
		t.Errorf("Accepted = %d, want 0 (refusal); a non-zero answer here is the silent loss", out.Accepted)
	}
}

// TestPushHostMetrics_ADeferredBatchIsReportedAsNothingAccepted is the
// regression for the one path where replayed telemetry was destroyed by
// the handshake rather than the transport.
//
// Before this, a node that had cached points during an outage would send
// its backlog, be told Accepted=0 because register_edge had not landed
// yet, and *discard the batch* — the log had been installed for exactly
// this outage and the retention it bought was one round trip. The
// center's zero is now read as "not yet", which keeps the rows; this test
// pins the zero so the reading cannot silently change back.
func TestPushHostMetrics_ADeferredBatchIsReportedAsNothingAccepted(t *testing.T) {
	mi := &fakeMetricIngester{}
	fs, _ := installAndDispatch(t, Wiring{
		MetricIngester: mi,
		DeviceResolver: &fakeDeviceResolver{err: errors.New("no host junction")},
		Log:            slog.Default(),
	})
	rpc := rpcFor(t, fs, tunnel.MethodPushHostMetrics)
	body, _ := json.Marshal(tunnel.PushHostMetricsRequest{
		EdgeID: 7,
		Points: []tunnel.HostMetricPoint{{Ts: 1, CPUPct: 1}},
	})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 1}, rsp)
	if rsp.err != nil {
		t.Fatalf("a deferral must not be an RPC error: %v", rsp.err)
	}
	if mi.pushCnt != 0 {
		t.Fatalf("the ingester was called for an unplaceable batch")
	}
	var out tunnel.PushHostMetricsResponse
	if err := json.Unmarshal(rsp.data, &out); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if out.Accepted != 0 {
		t.Errorf("Accepted = %d, want 0 — a non-zero answer makes the node discard its cached backlog", out.Accepted)
	}
}

// TestPushHostMetrics_TheAcceptedCountFollowsTheIngester is the other half:
// when the center *can* place a batch, the count must come from having
// written it, not from having received it.
func TestPushHostMetrics_TheAcceptedCountFollowsTheIngester(t *testing.T) {
	mi := &fakeMetricIngester{}
	fs, _ := installAndDispatch(t, Wiring{MetricIngester: mi, Log: slog.Default()})
	rpc := rpcFor(t, fs, tunnel.MethodPushHostMetrics)
	body, _ := json.Marshal(tunnel.PushHostMetricsRequest{
		EdgeID: 9,
		Points: []tunnel.HostMetricPoint{{Ts: 1, CPUPct: 1}, {Ts: 2, CPUPct: 2}},
	})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 9}, rsp)
	if rsp.err != nil {
		t.Fatalf("rpc returned error: %v", rsp.err)
	}
	if mi.pushCnt != 1 || mi.gotN != 2 {
		t.Fatalf("ingester pushCnt=%d gotN=%d, want 1/2", mi.pushCnt, mi.gotN)
	}
	var out tunnel.PushHostMetricsResponse
	if err := json.Unmarshal(rsp.data, &out); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if out.Accepted != 2 {
		t.Errorf("Accepted = %d, want 2", out.Accepted)
	}
}

// TestPushHostMetrics_AnIngesterErrorIsAnRPCError keeps the third sentence
// distinct: a store that failed is a transport-shaped failure, so the
// node retries rather than being told the data was refused.
func TestPushHostMetrics_AnIngesterErrorIsAnRPCError(t *testing.T) {
	mi := &fakeMetricIngester{wantErr: errors.New("db down")}
	fs, _ := installAndDispatch(t, Wiring{MetricIngester: mi, Log: slog.Default()})
	rpc := rpcFor(t, fs, tunnel.MethodPushHostMetrics)
	body, _ := json.Marshal(tunnel.PushHostMetricsRequest{
		EdgeID: 3,
		Points: []tunnel.HostMetricPoint{{Ts: 1, CPUPct: 1}},
	})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 3}, rsp)
	if rsp.err == nil {
		t.Fatalf("an ingester failure must reach the node as an error so it retries")
	}
}

func TestInstall_PushPromSamples_BadBody(t *testing.T) {
	pi := &fakePromIngester{}
	_, rpc := installAndDispatch(t, Wiring{PromIngester: pi, Log: slog.Default()})

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: []byte("not-json"), clientID: 1}, rsp)
	if rsp.err == nil {
		t.Errorf("expected decode error")
	}
	if pi.pushCnt != 0 {
		t.Errorf("ingester should not be called, got pushCnt=%d", pi.pushCnt)
	}
}

// --- agent.event --------------------------------------------------------

// fakeAgentEvents records the frames the agent.event push routed.
type fakeAgentEvents struct {
	mu     sync.Mutex
	frames []tunnel.AgentEventFrame
	// delivered is what DeliverInbound answers, so a test can script a
	// conversation that has since closed.
	delivered bool
}

func (f *fakeAgentEvents) DeliverInbound(frame tunnel.AgentEventFrame) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, frame)
	return f.delivered
}

func (f *fakeAgentEvents) last() (tunnel.AgentEventFrame, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.frames) == 0 {
		return tunnel.AgentEventFrame{}, false
	}
	return f.frames[len(f.frames)-1], true
}

func installAgentEvent(t *testing.T, router AgentEventRouter) *fakeService {
	t.Helper()
	fs := newFakeService()
	c := newWithService(fs, slog.Default())
	err := Install(context.Background(), c, Wiring{
		EdgeAuthn:      &edgebiz.AccessKeyAuthenticator{},
		EdgeUC:         &edgebiz.Usecase{},
		MetricIngester: &fakeMetricIngester{},
		AgentEvents:    router,
		Log:            slog.Default(),
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	return fs
}

func TestInstall_AgentEvent_ReachesTheRouter(t *testing.T) {
	router := &fakeAgentEvents{delivered: true}
	fs := installAgentEvent(t, router)
	rpc, ok := fs.rpcs[tunnel.MethodAgentEvent]
	if !ok {
		t.Fatal("agent.event was not registered")
	}

	frame := wire.StreamEvent{Type: wire.StreamAssistantDelta, SessionID: "s-1", Seq: 3}
	body, _ := json.Marshal(tunnel.AgentEventFrame{EdgeID: 42, SessionID: "s-1", Frame: &frame})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 42}, rsp)
	if rsp.err != nil {
		t.Fatalf("agent.event returned an error: %v", rsp.err)
	}

	got, ok := router.last()
	if !ok {
		t.Fatal("nothing was routed")
	}
	if got.SessionID != "s-1" {
		t.Errorf("routed session = %q, want s-1", got.SessionID)
	}
}

func TestInstall_AgentEvent_TrustsTheTransportEdgeID(t *testing.T) {
	// One node must not be able to write into another node's
	// conversation. The body carries the node's own stamp, and a node is
	// free to put any number in it; what authenticated is the connection,
	// so the transport's edge id is the one that counts.
	router := &fakeAgentEvents{delivered: true}
	fs := installAgentEvent(t, router)
	rpc := fs.rpcs[tunnel.MethodAgentEvent]

	frame := wire.StreamEvent{Type: wire.StreamAssistantDelta, SessionID: "s-1"}
	body, _ := json.Marshal(tunnel.AgentEventFrame{EdgeID: 7, SessionID: "s-1", Frame: &frame})
	rpc(context.Background(), &fakeReq{data: body, clientID: 42}, &fakeResp{})

	got, _ := router.last()
	if got.EdgeID != 42 {
		t.Errorf("routed to edge %d, want the authenticated 42", got.EdgeID)
	}
}

func TestInstall_AgentEvent_AnUnplaceableFrameIsStillSuccess(t *testing.T) {
	// A frame for a conversation that closed between the node sending it
	// and the manager receiving it is ordinary. Answering with an error
	// would make the edge treat a finished turn as a transport fault and
	// count it as a drop.
	router := &fakeAgentEvents{delivered: false}
	fs := installAgentEvent(t, router)
	rpc := fs.rpcs[tunnel.MethodAgentEvent]

	frame := wire.StreamEvent{Type: wire.StreamDone, SessionID: "s-1"}
	body, _ := json.Marshal(tunnel.AgentEventFrame{EdgeID: 42, SessionID: "s-1", Frame: &frame})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 42}, rsp)
	if rsp.err != nil {
		t.Errorf("an unplaceable frame was reported as an error: %v", rsp.err)
	}
}

func TestInstall_AgentEvent_AMalformedFrameDoesNotFailThePush(t *testing.T) {
	// A frame the manager cannot parse is the node's bug and there is
	// nothing for the edge to retry. Answering cleanly keeps a bad build
	// on one node from looking like a broken tunnel.
	router := &fakeAgentEvents{delivered: true}
	fs := installAgentEvent(t, router)
	rpc := fs.rpcs[tunnel.MethodAgentEvent]

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: []byte("not json"), clientID: 42}, rsp)
	if rsp.err != nil {
		t.Errorf("a malformed frame was reported as an error: %v", rsp.err)
	}
	if _, ok := router.last(); ok {
		t.Error("a malformed frame was routed as if it parsed")
	}
}

func TestInstall_WithoutAgentEvents_NoAgentEventHandler(t *testing.T) {
	// Nodes running an agent with no way to deliver a turn is a degraded
	// conversation, not a broken node. Installing a handler that answers
	// agent_unavailable would instead look like the node has no agent.
	fs := newFakeService()
	c := newWithService(fs, slog.Default())
	err := Install(context.Background(), c, Wiring{
		EdgeAuthn:      &edgebiz.AccessKeyAuthenticator{},
		EdgeUC:         &edgebiz.Usecase{},
		MetricIngester: &fakeMetricIngester{},
		Log:            slog.Default(),
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, ok := fs.rpcs[tunnel.MethodAgentEvent]; ok {
		t.Error("agent.event was registered with no router behind it")
	}
}

// --- agent.tool ---------------------------------------------------------

// fakeAgentTools records the upcalls a node's agent made and answers
// whatever the test scripted.
type fakeAgentTools struct {
	mu    sync.Mutex
	calls []agentToolCall
	// result and err are returned for every call.
	result json.RawMessage
	err    error
}

// agentToolCall is one recorded upcall.
type agentToolCall struct {
	edgeID    uint64
	sessionID string
	tool      string
	args      json.RawMessage
}

func (f *fakeAgentTools) RunAgentTool(_ context.Context, edgeID uint64, sessionID, tool string, args json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, agentToolCall{edgeID: edgeID, sessionID: sessionID, tool: tool, args: args})
	return f.result, f.err
}

func (f *fakeAgentTools) last() (agentToolCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return agentToolCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

func installAgentTools(t *testing.T, runner AgentToolRunner) *fakeService {
	t.Helper()
	fs := newFakeService()
	c := newWithService(fs, slog.Default())
	err := Install(context.Background(), c, Wiring{
		EdgeAuthn:      &edgebiz.AccessKeyAuthenticator{},
		EdgeUC:         &edgebiz.Usecase{},
		MetricIngester: &fakeMetricIngester{},
		AgentTools:     runner,
		Log:            slog.Default(),
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	return fs
}

func TestInstall_AgentTool_RunsTheToolAndReturnsItsOutput(t *testing.T) {
	tools := &fakeAgentTools{result: json.RawMessage(`{"edge_count":42}`)}
	fs := installAgentTools(t, tools)
	rpc, ok := fs.rpcs[tunnel.MethodAgentTool]
	if !ok {
		t.Fatal("agent.tool was not registered")
	}

	body, _ := json.Marshal(tunnel.AgentToolRequest{
		SessionID: "s-1", Tool: "get_topology", Arguments: json.RawMessage(`{}`),
	})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 7}, rsp)
	if rsp.err != nil {
		t.Fatalf("agent.tool returned an error: %v", rsp.err)
	}

	var out tunnel.AgentToolResponse
	if err := json.Unmarshal(rsp.data, &out); err != nil {
		t.Fatalf("decode response %q: %v", rsp.data, err)
	}
	if out.Error != "" {
		t.Errorf("error = %q, want the tool's output", out.Error)
	}
	if string(out.Result) != `{"edge_count":42}` {
		t.Errorf("result = %s, want the tool's bytes unchanged", out.Result)
	}

	got, ok := tools.last()
	if !ok {
		t.Fatal("nothing was run")
	}
	if got.tool != "get_topology" || got.sessionID != "s-1" {
		t.Errorf("ran %+v, want the tool and session the node named", got)
	}
}

func TestInstall_AgentTool_TrustsTheTransportEdgeID(t *testing.T) {
	// The body has no edge id, and the one the transport supplies is what
	// the connection authenticated as. A node authenticated as one edge
	// must not be able to spend another's authority on the control plane.
	tools := &fakeAgentTools{result: json.RawMessage(`{}`)}
	fs := installAgentTools(t, tools)
	rpc := fs.rpcs[tunnel.MethodAgentTool]

	body, _ := json.Marshal(tunnel.AgentToolRequest{Tool: "get_topology"})
	rpc(context.Background(), &fakeReq{data: body, clientID: 99}, &fakeResp{})

	got, _ := tools.last()
	if got.edgeID != 99 {
		t.Errorf("ran for edge %d, want the transport's 99", got.edgeID)
	}
}

// A refusal and a transport failure have to be distinguishable at the
// node: one is a decision the model should respect, the other is an
// incident an operator should look at.
func TestInstall_AgentTool_ReportsAToolFailureAsARefusalNotAnRPCError(t *testing.T) {
	tools := &fakeAgentTools{err: errors.New("query_alert_rules: alert usecase not configured")}
	fs := installAgentTools(t, tools)
	rpc := fs.rpcs[tunnel.MethodAgentTool]

	body, _ := json.Marshal(tunnel.AgentToolRequest{Tool: "query_alert_rules"})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 1}, rsp)
	if rsp.err != nil {
		t.Fatalf("a tool failure became an RPC error: %v", rsp.err)
	}

	var out tunnel.AgentToolResponse
	if err := json.Unmarshal(rsp.data, &out); err != nil {
		t.Fatalf("decode response %q: %v", rsp.data, err)
	}
	if out.Error == "" {
		t.Fatal("the node was told nothing about a failed tool")
	}
	if len(out.Result) != 0 {
		t.Errorf("result = %s, want none alongside the error", out.Result)
	}
}

func TestInstall_AgentTool_RefusesACallWithNoToolName(t *testing.T) {
	tools := &fakeAgentTools{result: json.RawMessage(`{}`)}
	fs := installAgentTools(t, tools)
	rpc := fs.rpcs[tunnel.MethodAgentTool]

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: []byte(`{"session_id":"s"}`), clientID: 1}, rsp)
	if rsp.err == nil {
		t.Fatal("a nameless tool call was accepted")
	}
	if _, ok := tools.last(); ok {
		t.Error("a nameless call reached the runner")
	}
}

func TestInstall_AgentTool_RefusesABodyItCannotRead(t *testing.T) {
	tools := &fakeAgentTools{result: json.RawMessage(`{}`)}
	fs := installAgentTools(t, tools)
	rpc := fs.rpcs[tunnel.MethodAgentTool]

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: []byte("{not json"), clientID: 1}, rsp)
	if rsp.err == nil {
		t.Fatal("an unreadable body was accepted")
	}
	if _, ok := tools.last(); ok {
		t.Error("an unreadable body reached the runner")
	}
}

func TestInstall_AgentTool_DoesNotInstallWithoutARunner(t *testing.T) {
	// A node whose control-plane tools are unavailable is degraded, not
	// broken: its host-local probes have to keep working, so the method
	// simply is not there rather than answering "no" to everything.
	fs := installAgentTools(t, nil)
	if _, ok := fs.rpcs[tunnel.MethodAgentTool]; ok {
		t.Error("agent.tool was registered with no runner behind it")
	}
}
