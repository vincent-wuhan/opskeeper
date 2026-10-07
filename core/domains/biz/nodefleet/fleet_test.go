package nodefleet

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// fakeDial records what was sent and replies with whatever the test scripted.
type fakeDial struct {
	mu       sync.Mutex
	calls    []recordedCall
	reply    func(method string, edgeID uint64, body []byte) ([]byte, error)
	sessions map[uint64]bool
}

type recordedCall struct {
	EdgeID uint64
	Method string
	Body   []byte
}

func (d *fakeDial) Call(_ context.Context, edgeID uint64, method string, body []byte) ([]byte, error) {
	d.mu.Lock()
	d.calls = append(d.calls, recordedCall{EdgeID: edgeID, Method: method, Body: append([]byte(nil), body...)})
	fn := d.reply
	d.mu.Unlock()
	if fn == nil {
		return []byte(`{}`), nil
	}
	return fn(method, edgeID, body)
}

func (d *fakeDial) sent(method string) []recordedCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []recordedCall
	for _, c := range d.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (d *fakeDial) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// acceptAll is a dial that reports every command as accepted.
func acceptAll() func(string, uint64, []byte) ([]byte, error) {
	return func(method string, _ uint64, _ []byte) ([]byte, error) {
		switch method {
		case tunnel.MethodAgentPrompt:
			return json.Marshal(tunnel.AgentPromptResponse{Accepted: true})
		case tunnel.MethodAgentSteer:
			return json.Marshal(tunnel.AgentSteerResponse{Accepted: true})
		case tunnel.MethodAgentAbort:
			return json.Marshal(tunnel.AgentAbortResponse{Aborted: true})
		case tunnel.MethodAgentSetModel:
			return json.Marshal(tunnel.AgentSetModelResponse{Accepted: true})
		case tunnel.MethodAgentState:
			return json.Marshal(tunnel.AgentStateResponse{Available: true, SessionID: "s-1"})
		case tunnel.MethodAgentHealth:
			return json.Marshal(tunnel.AgentHealthResponse{Running: true})
		}
		return []byte(`{}`), nil
	}
}

// recordingSink keeps the console frames a session produced.
type recordingSink struct {
	mu     sync.Mutex
	frames []wire.StreamEvent
	err    error
}

func (s *recordingSink) Emit(_ context.Context, ev wire.StreamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.frames = append(s.frames, ev)
	return nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

func (s *recordingSink) types() []wire.StreamEventType {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]wire.StreamEventType, 0, len(s.frames))
	for _, f := range s.frames {
		out = append(out, f.Type)
	}
	return out
}

func newFleet(t *testing.T, dial Dialer) *Fleet {
	t.Helper()
	f, err := New(Options{Dial: dial})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(f.CloseAll)
	return f
}

// --- refusals are not transport failures -------------------------------

func TestANodeRefusalIsDistinguishableFromALostTunnel(t *testing.T) {
	// The difference an operator needs: a refusal is a statement about the
	// node (no agent, crash-looping, bad request), while a transport
	// failure is the two not talking. One is a node to go and look at; the
	// other is a network.
	dial := &fakeDial{reply: func(method string, _ uint64, _ []byte) ([]byte, error) {
		return json.Marshal(tunnel.AgentPromptResponse{
			SessionID: "s-1", Code: CodeAgentDegraded,
			Error: "the agent crashed 5 times in 10m: plugin manifest not found",
		})
	}}
	f := newFleet(t, dial)
	sink := &recordingSink{}
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, sink); err != nil {
		t.Fatalf("Open: %v", err)
	}

	err := f.Prompt(context.Background(), PromptRequest{EdgeID: 7, SessionID: "s-1", UserText: "restart web"})
	if err == nil {
		t.Fatal("a refusal was reported as success: the console would show the turn as accepted and then nothing")
	}
	refusal, ok := AsRefusal(err)
	if !ok {
		t.Fatalf("err = %v, want a RemoteError", err)
	}
	if refusal.Code != CodeAgentDegraded {
		t.Errorf("code = %q, want %q", refusal.Code, CodeAgentDegraded)
	}
	// The node's own words survive: "plugin manifest not found" tells an
	// operator what to fix.
	if !contains(refusal.Message, "plugin manifest not found") {
		t.Errorf("message = %q, want the node's explanation verbatim", refusal.Message)
	}
	if refusal.EdgeID != 7 {
		t.Errorf("edge = %d, want 7", refusal.EdgeID)
	}
	if refusal.Retryable() {
		t.Error("a crash-looping node reported as retryable: retrying does not fix a broken binary")
	}
}

func TestATransportFailureIsNotARefusal(t *testing.T) {
	dial := &fakeDial{reply: func(string, uint64, []byte) ([]byte, error) {
		return nil, errors.New("frontier: node 7 is not connected")
	}}
	f := newFleet(t, dial)
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, &recordingSink{}); err != nil {
		t.Fatalf("Open: %v", err)
	}

	err := f.Prompt(context.Background(), PromptRequest{EdgeID: 7, SessionID: "s-1", UserText: "hi"})
	if err == nil {
		t.Fatal("a transport failure was reported as success")
	}
	if IsRefusal(err) {
		t.Errorf("err = %v, want a transport failure, not a node refusal", err)
	}
}

func TestEveryCommandDecodesItsReply(t *testing.T) {
	// The dangerous bug this guards: a handler that returns a refusal in
	// the body rather than as a transport error, and a client that treats
	// a 200 with a refusal body as success. Each command is checked here so
	// none can be added later without the check.
	cases := []struct {
		method string
		body   string
		call   func(*Fleet) error
	}{
		{
			method: tunnel.MethodAgentPrompt,
			body:   `{"accepted":false,"code":"agent_unavailable","error":"no agent"}`,
			call: func(f *Fleet) error {
				return f.Prompt(context.Background(), PromptRequest{EdgeID: 7, SessionID: "s-1", UserText: "hi"})
			},
		},
		{
			method: tunnel.MethodAgentSteer,
			body:   `{"accepted":false,"code":"agent_internal","error":"pipe closed"}`,
			call: func(f *Fleet) error {
				return f.Steer(context.Background(), 7, "s-1", "actually, do X")
			},
		},
		{
			method: tunnel.MethodAgentAbort,
			body:   `{"aborted":false,"code":"agent_degraded","error":"crash-looping"}`,
			call:   func(f *Fleet) error { return f.Abort(context.Background(), 7, "s-1") },
		},
		{
			method: tunnel.MethodAgentSetModel,
			body:   `{"accepted":false,"code":"agent_bad_request","error":"unknown model"}`,
			call:   func(f *Fleet) error { return f.handleFor(t, 7, "s-1").SetModel(context.Background(), "openai", "nope") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			reply := tc.body
			dial := &fakeDial{reply: func(string, uint64, []byte) ([]byte, error) { return []byte(reply), nil }}
			f := newFleet(t, dial)
			if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, &recordingSink{}); err != nil {
				t.Fatalf("Open: %v", err)
			}
			err := tc.call(f)
			if err == nil {
				t.Fatalf("%s reported a refusal body as success", tc.method)
			}
			if !IsRefusal(err) {
				t.Errorf("err = %v, want a RemoteError carrying the node's code", err)
			}
		})
	}
}

// handleFor reaches a session's handle for a test that needs the port
// directly rather than the fleet's convenience wrappers.
func (f *Fleet) handleFor(t *testing.T, edgeID uint64, sessionID string) *TunelledProcess {
	t.Helper()
	s, err := f.lookup(edgeID, sessionID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	return s.handle
}

func TestAbortOfAnIdleNodeIsASuccess(t *testing.T) {
	// The edge reports aborting a node with no agent as success, because
	// the operator's intent — stop — holds either way. The manager must
	// pass that through rather than manufacture a failure, or operators
	// learn to ignore the abort button.
	dial := &fakeDial{reply: acceptAll()}
	f := newFleet(t, dial)
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, &recordingSink{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.Abort(context.Background(), 7, "s-1"); err != nil {
		t.Errorf("Abort = %v, want nil", err)
	}
}

func TestStateReportsANodeWithNoAgent(t *testing.T) {
	// A node that is not running an agent is a state, not a fault. The
	// fleet view has to render it as one on every poll, so State answers
	// with the state and no error rather than failing.
	dial := &fakeDial{reply: func(method string, _ uint64, _ []byte) ([]byte, error) {
		if method == tunnel.MethodAgentState {
			return json.Marshal(tunnel.AgentStateResponse{Available: false, Version: "0.3.0"})
		}
		return acceptAll()(method, 0, nil)
	}}
	f := newFleet(t, dial)
	state, err := f.State(context.Background(), 7)
	if err != nil {
		t.Fatalf("State = %v, want the absence reported as data", err)
	}
	if state.Version != "0.3.0" {
		t.Errorf("version = %q, want the node's agent build even when it is not running", state.Version)
	}
	if state.Running {
		t.Error("Running is true for a node with no agent")
	}
}

func TestHealthIsSeparateFromState(t *testing.T) {
	// A node whose agent is up and crash-looping is "running" by state and
	// "degraded" by health. Only the second is worth waking somebody for.
	dial := &fakeDial{reply: func(method string, _ uint64, _ []byte) ([]byte, error) {
		switch method {
		case tunnel.MethodAgentState:
			return json.Marshal(tunnel.AgentStateResponse{Available: true, Running: true})
		case tunnel.MethodAgentHealth:
			return json.Marshal(tunnel.AgentHealthResponse{Running: true, Degraded: true, Restarts: 5,
				LastError: "agent crashed 5 times in 10m0s: plugin manifest not found"})
		}
		return acceptAll()(method, 0, nil)
	}}
	f := newFleet(t, dial)

	state, err := f.State(context.Background(), 7)
	if err != nil || !state.Running {
		t.Fatalf("State = (%+v, %v), want running", state, err)
	}
	health, err := f.Health(context.Background(), 7)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.Degraded || health.Restarts != 5 {
		t.Errorf("health = %+v, want degraded after 5 restarts", health)
	}
	if !contains(health.LastError, "plugin manifest not found") {
		t.Errorf("last error = %q, want the node's own words", health.LastError)
	}
}

// --- session lifecycle -------------------------------------------------

func TestNewRequiresADial(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("a fleet with no Dial was accepted")
	}
}

func TestOpenRequiresAnEdgeAndASession(t *testing.T) {
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	if _, err := f.Open(PromptRequest{SessionID: "s-1"}, &recordingSink{}); err == nil {
		t.Error("a conversation with no edge was accepted")
	}
	if _, err := f.Open(PromptRequest{EdgeID: 7}, &recordingSink{}); err == nil {
		t.Error("a conversation with no session id was accepted: its frames could not be demultiplexed")
	}
}

func TestOpenRefusesASessionIDClash(t *testing.T) {
	// Two consoles claiming one conversation id would interleave two
	// assistants into a single agent session, producing a transcript
	// neither operator can read.
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, &recordingSink{}); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, &recordingSink{}); err == nil {
		t.Fatal("a second console opened the same conversation id")
	}
}

func TestTheSameSessionIDOnDifferentNodesIsFine(t *testing.T) {
	// Session ids are scoped per node: the edge is the outer key, so two
	// nodes can each have a conversation called s-1 without colliding.
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, &recordingSink{}); err != nil {
		t.Fatalf("Open on 7: %v", err)
	}
	if _, err := f.Open(PromptRequest{EdgeID: 8, SessionID: "s-1"}, &recordingSink{}); err != nil {
		t.Errorf("Open on 8 = %v, want a separate conversation", err)
	}
	if got := f.SessionCount(); got != 2 {
		t.Errorf("SessionCount = %d, want 2", got)
	}
}

func TestOperationsOnAnUnknownSessionAreRefused(t *testing.T) {
	// Nothing was asked of the node, so this must not read as the node
	// being broken.
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	ctx := context.Background()
	if err := f.Prompt(ctx, PromptRequest{EdgeID: 7, SessionID: "nope", UserText: "hi"}); !errors.Is(err, ErrNoSession) {
		t.Errorf("Prompt = %v, want %v", err, ErrNoSession)
	}
	if err := f.Abort(ctx, 7, "nope"); !errors.Is(err, ErrNoSession) {
		t.Errorf("Abort = %v, want %v", err, ErrNoSession)
	}
	if err := f.Steer(ctx, 7, "nope", "x"); !errors.Is(err, ErrNoSession) {
		t.Errorf("Steer = %v, want %v", err, ErrNoSession)
	}
	if IsRefusal(f.Prompt(ctx, PromptRequest{EdgeID: 7, SessionID: "nope", UserText: "hi"})) {
		t.Error("an unroutable request was reported as a node refusal")
	}
}

func TestPromptCarriesTheTurnToTheRightNode(t *testing.T) {
	dial := &fakeDial{reply: acceptAll()}
	f := newFleet(t, dial)
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1", Role: "admin", Locale: "zh-CN"}, &recordingSink{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.Prompt(context.Background(), PromptRequest{EdgeID: 7, SessionID: "s-1", UserText: "why is web-1 down?"}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	sent := dial.sent(tunnel.MethodAgentPrompt)
	if len(sent) != 1 {
		t.Fatalf("prompt sent %d times, want 1", len(sent))
	}
	if sent[0].EdgeID != 7 {
		t.Errorf("edge = %d, want 7", sent[0].EdgeID)
	}
	var req tunnel.AgentPromptRequest
	if err := json.Unmarshal(sent[0].Body, &req); err != nil {
		t.Fatalf("decode prompt body: %v", err)
	}
	if req.Text != "why is web-1 down?" {
		t.Errorf("text = %q", req.Text)
	}
	if req.SessionID != "s-1" {
		t.Errorf("session = %q, want it carried so the node can demultiplex", req.SessionID)
	}
}

func TestCloseReleasesTheSession(t *testing.T) {
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, &recordingSink{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.Close(7, "s-1")
	if got := f.SessionCount(); got != 0 {
		t.Errorf("SessionCount = %d, want 0", got)
	}
	if err := f.Prompt(context.Background(), PromptRequest{EdgeID: 7, SessionID: "s-1", UserText: "hi"}); !errors.Is(err, ErrNoSession) {
		t.Errorf("Prompt after Close = %v, want %v", err, ErrNoSession)
	}
	// Closing twice is ordinary — a console's defer and its explicit
	// unsubscribe both run it.
	f.Close(7, "s-1")
}

func TestCloseAllEmptiesTheFleet(t *testing.T) {
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	for _, edge := range []uint64{7, 8, 9} {
		if _, err := f.Open(PromptRequest{EdgeID: edge, SessionID: "s-1"}, &recordingSink{}); err != nil {
			t.Fatalf("Open on %d: %v", edge, err)
		}
	}
	f.CloseAll()
	if got := f.SessionCount(); got != 0 {
		t.Errorf("SessionCount = %d, want 0", got)
	}
}

// --- the event path ----------------------------------------------------

func TestPushedFramesReachTheConsole(t *testing.T) {
	// The whole point of translating on the node: the manager hands the
	// console a frame it already parses.
	dial := &fakeDial{reply: acceptAll()}
	f := newFleet(t, dial)
	sink := &recordingSink{}
	handle, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, sink)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	frames := []wire.StreamEvent{
		{Type: wire.StreamAssistantStart, SessionID: "s-1", Seq: 1},
		{Type: wire.StreamAssistantDelta, SessionID: "s-1", Seq: 2, Assistant: &wire.AssistantFrame{Content: "checking"}},
		{Type: wire.StreamToolStart, SessionID: "s-1", Seq: 3, Tool: &wire.ToolFrame{ToolCallID: "tc-1", Name: "get_topology"}},
		{Type: wire.StreamToolEnd, SessionID: "s-1", Seq: 4, Tool: &wire.ToolFrame{ToolCallID: "tc-1", Status: wire.ToolSuccess}},
		{Type: wire.StreamDone, SessionID: "s-1", Seq: 5, Done: &wire.DoneFrame{Iterations: 1}},
	}
	for _, frame := range frames {
		handle.Deliver(tunnel.AgentEventFrame{
			EdgeID: 7, SessionID: "s-1", Type: "agent_end",
			Frame: &frame, Terminal: frame.Type == wire.StreamDone,
		})
	}

	got := sink.types()
	want := []wire.StreamEventType{
		wire.StreamAssistantStart, wire.StreamAssistantDelta,
		wire.StreamToolStart, wire.StreamToolEnd, wire.StreamDone,
	}
	if len(got) != len(want) {
		t.Fatalf("sink saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestUntranslatedFramesAreDroppedAndCounted(t *testing.T) {
	// The node could not place this event, so there is no console frame
	// for it. It is counted rather than guessed at: a wrong frame is worse
	// than a missing one, because the console would render it as fact.
	dial := &fakeDial{reply: acceptAll()}
	f := newFleet(t, dial)
	sink := &recordingSink{}
	handle, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, sink)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	handle.Deliver(tunnel.AgentEventFrame{EdgeID: 7, Type: "agent_settled", Payload: []byte(`{}`)})
	handle.Deliver(tunnel.AgentEventFrame{EdgeID: 7, Type: "summarization_retry_scheduled", Payload: []byte(`{}`)})

	if sink.count() != 0 {
		t.Errorf("sink received %d frames, want none: an untranslatable event must not reach the console", sink.count())
	}
	stats, ok := f.Stats(7, "s-1")
	if !ok {
		t.Fatal("Stats found no session")
	}
	if stats.Dropped != 2 {
		t.Errorf("Dropped = %d, want 2: the gaps have to be explainable", stats.Dropped)
	}
	if stats.Frames != 0 {
		t.Errorf("Frames = %d, want 0", stats.Frames)
	}
}

func TestStatsReportDeliveredFrames(t *testing.T) {
	dial := &fakeDial{reply: acceptAll()}
	f := newFleet(t, dial)
	handle, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, &recordingSink{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	frame := wire.StreamEvent{Type: wire.StreamAssistantDelta, SessionID: "s-1", Seq: 1}
	handle.Deliver(tunnel.AgentEventFrame{EdgeID: 7, SessionID: "s-1", Frame: &frame})

	stats, _ := f.Stats(7, "s-1")
	if stats.Frames != 1 {
		t.Errorf("Frames = %d, want 1", stats.Frames)
	}
	all := f.AllStats()
	if len(all) != 1 || all[0].EdgeID != 7 || all[0].SessionID != "s-1" {
		t.Errorf("AllStats = %+v", all)
	}
}

func TestATerminalFrameStopsLaterFrames(t *testing.T) {
	// A turn's done frame ends the conversation's output. A frame that
	// arrives afterwards belongs to a turn nobody is watching, and
	// delivering it would reopen a bubble the operator dismissed.
	dial := &fakeDial{reply: acceptAll()}
	f := newFleet(t, dial)
	sink := &recordingSink{}
	handle, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, sink)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	done := wire.StreamEvent{Type: wire.StreamDone, SessionID: "s-1", Seq: 1}
	handle.Deliver(tunnel.AgentEventFrame{EdgeID: 7, SessionID: "s-1", Frame: &done, Terminal: true})
	late := wire.StreamEvent{Type: wire.StreamAssistantDelta, SessionID: "s-1", Seq: 2}
	handle.Deliver(tunnel.AgentEventFrame{EdgeID: 7, SessionID: "s-1", Frame: &late})

	if sink.count() != 1 {
		t.Errorf("sink received %d frames, want 1: a late frame must not reopen a closed turn", sink.count())
	}
	stats, _ := f.Stats(7, "s-1")
	if !stats.Terminal {
		t.Error("stats do not report the conversation as terminal")
	}
}

func TestAFollowUpTurnReopensTheConversation(t *testing.T) {
	// An operator asking a follow-up in the same session must not be
	// silently ignored because the previous turn finished.
	dial := &fakeDial{reply: acceptAll()}
	f := newFleet(t, dial)
	sink := &recordingSink{}
	handle, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, sink)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	done := wire.StreamEvent{Type: wire.StreamDone, SessionID: "s-1", Seq: 1}
	handle.Deliver(tunnel.AgentEventFrame{EdgeID: 7, SessionID: "s-1", Frame: &done, Terminal: true})

	if err := f.Prompt(context.Background(), PromptRequest{EdgeID: 7, SessionID: "s-1", UserText: "and the logs?"}); err != nil {
		t.Fatalf("follow-up Prompt: %v", err)
	}
	next := wire.StreamEvent{Type: wire.StreamAssistantStart, SessionID: "s-1", Seq: 2}
	handle.Deliver(tunnel.AgentEventFrame{EdgeID: 7, SessionID: "s-1", Frame: &next})

	if sink.count() != 2 {
		t.Errorf("sink received %d frames, want 2: a follow-up turn must be delivered", sink.count())
	}
}

func TestADeadConsoleClosesTheSession(t *testing.T) {
	// Continuing to relay into a stream nobody reads would buffer frames
	// on the node until somebody restarted it.
	sink := &recordingSink{err: errors.New("stream closed")}
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	handle, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, sink)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	frame := wire.StreamEvent{Type: wire.StreamAssistantDelta, SessionID: "s-1", Seq: 1}
	handle.Deliver(tunnel.AgentEventFrame{EdgeID: 7, SessionID: "s-1", Frame: &frame})

	if got := f.SessionCount(); got != 0 {
		t.Errorf("SessionCount = %d, want 0: a console that has gone away must not hold the session open", got)
	}
	if got := handle.SubscriberCount(); got != 0 {
		t.Errorf("SubscriberCount = %d, want 0", got)
	}
}

func TestTwoConsolesOnDifferentNodesDoNotCrossTalk(t *testing.T) {
	// The demultiplexing that makes one node's agent usable by several
	// conversations at once.
	dial := &fakeDial{reply: acceptAll()}
	f := newFleet(t, dial)
	sinkA, sinkB := &recordingSink{}, &recordingSink{}
	handleA, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, sinkA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	handleB, err := f.Open(PromptRequest{EdgeID: 8, SessionID: "s-1"}, sinkB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}

	frame := wire.StreamEvent{Type: wire.StreamAssistantDelta, SessionID: "s-1", Seq: 1, Assistant: &wire.AssistantFrame{Content: "for A"}}
	handleA.Deliver(tunnel.AgentEventFrame{EdgeID: 7, SessionID: "s-1", Frame: &frame})

	if sinkA.count() != 1 {
		t.Errorf("sink A received %d frames, want 1", sinkA.count())
	}
	if sinkB.count() != 0 {
		t.Errorf("sink B received %d frames, want 0: one node's output must never reach another's console", sinkB.count())
	}

	// And the reverse, on the same session id: identical identifiers on
	// different nodes must not merge either.
	handleB.Deliver(tunnel.AgentEventFrame{EdgeID: 8, SessionID: "s-1", Frame: &frame})
	if sinkB.count() != 1 {
		t.Errorf("sink B received %d frames, want 1", sinkB.count())
	}
	if sinkA.count() != 1 {
		t.Errorf("sink A received %d frames, want 1: the other node's turn leaked in", sinkA.count())
	}
}

// --- model pinning -----------------------------------------------------

func TestAModelPinIsAppliedWhenTheConversationOpens(t *testing.T) {
	// Pinned before the first turn, so the conversation does not open on
	// one model and answer on another — which would make the cost line
	// wrong with no explanation.
	dial := &fakeDial{reply: acceptAll()}
	f := newFleet(t, dial)
	if _, err := f.Open(PromptRequest{
		EdgeID: 7, SessionID: "s-1",
		Selection: domain.ModelSelection{Provider: domain.ProviderAnthropic, Model: "claude-opus-5"},
	}, &recordingSink{}); err != nil {
		t.Fatalf("Open: %v", err)
	}

	pins := dial.sent(tunnel.MethodAgentSetModel)
	if len(pins) != 1 {
		t.Fatalf("model pinned %d times, want 1", len(pins))
	}
	var req tunnel.AgentSetModelRequest
	if err := json.Unmarshal(pins[0].Body, &req); err != nil {
		t.Fatalf("decode pin body: %v", err)
	}
	if req.Provider != string(domain.ProviderAnthropic) || req.Model != "claude-opus-5" {
		t.Errorf("pin = %s/%s", req.Provider, req.Model)
	}
	// The pin has to land before the first prompt, not after it.
	dial.mu.Lock()
	defer dial.mu.Unlock()
	sawPin := false
	for _, c := range dial.calls {
		if c.Method == tunnel.MethodAgentSetModel {
			sawPin = true
		}
		if c.Method == tunnel.MethodAgentPrompt {
			if sawPin {
				t.Error("the model was pinned after the first prompt was sent")
			}
			break
		}
	}
}

func TestARefusedModelPinFailsTheOpen(t *testing.T) {
	// Silently answering on a different model than the one the operator
	// chose would make the cost line wrong with nothing to explain it.
	dial := &fakeDial{reply: func(method string, _ uint64, _ []byte) ([]byte, error) {
		if method == tunnel.MethodAgentSetModel {
			return json.Marshal(tunnel.AgentSetModelResponse{
				Code: CodeAgentBadRequest, Error: "model claude-opus-5 is not configured on this node",
			})
		}
		return acceptAll()(method, 0, nil)
	}}
	f := newFleet(t, dial)
	_, err := f.Open(PromptRequest{
		EdgeID: 7, SessionID: "s-1",
		Selection: domain.ModelSelection{Provider: domain.ProviderAnthropic, Model: "claude-opus-5"},
	}, &recordingSink{})
	if err == nil {
		t.Fatal("a refused model pin was reported as success")
	}
	if !IsRefusal(err) {
		t.Errorf("err = %v, want the node's refusal surfaced", err)
	}
	// The half-open conversation must not be left behind.
	if got := f.SessionCount(); got != 0 {
		t.Errorf("SessionCount = %d, want 0: a failed open must not leak a session", got)
	}
}

func TestNoPinIsSentWhenNoModelWasChosen(t *testing.T) {
	dial := &fakeDial{reply: acceptAll()}
	f := newFleet(t, dial)
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, &recordingSink{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if pins := dial.sent(tunnel.MethodAgentSetModel); len(pins) != 0 {
		t.Errorf("model pinned %d times with no selection, want 0", len(pins))
	}
}

// --- the port ----------------------------------------------------------

func TestTunelledProcessSatisfiesTheAgentPort(t *testing.T) {
	// The manager and the node hold the same interface, so the fleet's
	// routing cannot depend on which side it is running.
	var _ ports.AgentProcess = (*TunelledProcess)(nil)
}

func TestStartAndStopAreNoOpsOnTheManagerSide(t *testing.T) {
	// A control plane that appeared able to start or stop a node's agent
	// would hold a remote-kill button on every host in the fleet, keyed
	// off a single RPC.
	dial := &fakeDial{reply: acceptAll()}
	p := NewTunelledProcess(7, "s-1", dial)
	if err := p.Start(context.Background()); err != nil {
		t.Errorf("Start = %v, want nil: the node owns the lifecycle", err)
	}
	if err := p.Stop(); err != nil {
		t.Errorf("Stop = %v, want nil: the node owns the lifecycle", err)
	}
	if n := dial.count(); n != 0 {
		t.Errorf("Start/Stop sent %d RPCs, want 0: the node owns the lifecycle", n)
	}
}

func TestExitedNeverFires(t *testing.T) {
	// The fleet must not wait on an exit it cannot observe: the node
	// restarts its own agent, and a manager-side respawn loop would race
	// the node's own.
	p := NewTunelledProcess(7, "s-1", &fakeDial{reply: acceptAll()})
	select {
	case <-p.Exited():
		t.Fatal("Exited fired for a process the manager does not own")
	default:
	}
}

func TestLastErrorTracksTheMostRecentFailure(t *testing.T) {
	dial := &fakeDial{reply: func(string, uint64, []byte) ([]byte, error) {
		return json.Marshal(tunnel.AgentPromptResponse{Code: CodeAgentUnavailable, Error: "no agent"})
	}}
	p := NewTunelledProcess(7, "s-1", dial)
	if err := p.Prompt(context.Background(), "hi"); err == nil {
		t.Fatal("Prompt succeeded")
	}
	if p.LastError() == nil {
		t.Error("LastError is nil after a refusal")
	}
	// A node that has no agent is a fact about the node, not a transport
	// fault, and must not be left showing as a fleet-wide error.
	if _, err := p.State(context.Background()); err != nil {
		t.Fatalf("State: %v", err)
	}
	if p.LastError() != nil {
		t.Errorf("LastError = %v after a state poll succeeded", p.LastError())
	}
}

func TestRunningReflectsTheLastStatePoll(t *testing.T) {
	busy := false
	dial := &fakeDial{reply: func(method string, _ uint64, _ []byte) ([]byte, error) {
		if method == tunnel.MethodAgentState {
			return json.Marshal(tunnel.AgentStateResponse{Available: true, Running: busy})
		}
		return acceptAll()(method, 0, nil)
	}}
	p := NewTunelledProcess(7, "s-1", dial)
	if p.Running() {
		t.Error("Running is true before anything was asked of the node")
	}
	if _, err := p.State(context.Background()); err != nil {
		t.Fatalf("State: %v", err)
	}
	if p.Running() {
		t.Error("Running is true for an idle agent")
	}
	busy = true
	if _, err := p.State(context.Background()); err != nil {
		t.Fatalf("State: %v", err)
	}
	if !p.Running() {
		t.Error("Running is false for a streaming agent")
	}
}

func TestProjectEventCarriesTheAgentsOwnRecord(t *testing.T) {
	// The port's raw view is the agent's record, not a re-encoding of the
	// translated frame: a caller that wants the console frame uses
	// OnFrame, and one that wants the raw record uses this.
	ev := ProjectEvent(tunnel.AgentEventFrame{
		Type: "tool_execution_end", SessionID: "s-1", Iteration: 2, Seq: 9,
		Terminal: true, Payload: []byte(`{"toolName":"get_topology"}`),
	})
	if ev.Type != "tool_execution_end" || ev.SessionID != "s-1" || ev.Iteration != 2 || ev.Seq != 9 || !ev.Terminal {
		t.Errorf("event = %+v", ev)
	}
	if string(ev.Payload) != `{"toolName":"get_topology"}` {
		t.Errorf("payload = %s", ev.Payload)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// --- inbound routing ----------------------------------------------------

func TestAPushedFrameReachesTheConversationItNames(t *testing.T) {
	// The node multiplexes several conversations over one agent process,
	// so the manager can only put a frame back where it came from by
	// matching both ids it stamped on it.
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	sink := &recordingSink{}
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, sink); err != nil {
		t.Fatalf("Open: %v", err)
	}

	frame := wire.StreamEvent{Type: wire.StreamAssistantDelta, SessionID: "s-1", Seq: 1}
	if !f.DeliverInbound(tunnel.AgentEventFrame{EdgeID: 7, SessionID: "s-1", Frame: &frame}) {
		t.Fatal("DeliverInbound reported no conversation for an open session")
	}
	if sink.count() != 1 {
		t.Errorf("the console received %d frames, want 1", sink.count())
	}
}

func TestAFrameForAClosedConversationIsDroppedQuietly(t *testing.T) {
	// The edge must never be made to retry a frame the manager cannot
	// place: a turn that ended before the last frames crossed the tunnel
	// is normal, and a retry storm against a closed conversation would
	// take down the node's agent for everybody.
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	frame := wire.StreamEvent{Type: wire.StreamDone, SessionID: "s-1"}
	if f.DeliverInbound(tunnel.AgentEventFrame{EdgeID: 7, SessionID: "s-1", Frame: &frame}) {
		t.Error("DeliverInbound claimed a conversation that was never opened")
	}
}

func TestAFrameWithNoConversationIDIsRefused(t *testing.T) {
	// There is no honest place to put it. Emitting to whichever console is
	// currently reading would write one operator's turn into another's
	// transcript, and the transcript is the audit record.
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	sink := &recordingSink{}
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, sink); err != nil {
		t.Fatalf("Open: %v", err)
	}
	frame := wire.StreamEvent{Type: wire.StreamAssistantDelta}
	if f.DeliverInbound(tunnel.AgentEventFrame{EdgeID: 7, Frame: &frame}) {
		t.Error("DeliverInbound accepted a frame with no session id")
	}
	if sink.count() != 0 {
		t.Errorf("the console received %d frames, want 0", sink.count())
	}
}

func TestAFrameForAnotherNodeIsNotDeliveredHere(t *testing.T) {
	f := newFleet(t, &fakeDial{reply: acceptAll()})
	sink := &recordingSink{}
	if _, err := f.Open(PromptRequest{EdgeID: 7, SessionID: "s-1"}, sink); err != nil {
		t.Fatalf("Open: %v", err)
	}
	frame := wire.StreamEvent{Type: wire.StreamAssistantDelta, SessionID: "s-1"}
	if f.DeliverInbound(tunnel.AgentEventFrame{EdgeID: 9, SessionID: "s-1", Frame: &frame}) {
		t.Error("a frame for node 9 was routed to node 7's conversation")
	}
	if sink.count() != 0 {
		t.Errorf("the console received %d frames, want 0", sink.count())
	}
}
