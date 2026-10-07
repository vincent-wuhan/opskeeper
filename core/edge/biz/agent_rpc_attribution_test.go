package biz

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The node's agent and the control plane do not agree on what a
// conversation is called, and the tests in this file are about the seam
// where that disagreement used to end the turn.
//
// The agent is one process with one session and names that session itself.
// The control plane mints a conversation id per console, routes every
// inbound frame by it, and drops anything it cannot attribute. So the node
// has to carry the conversation across: it learns the id from the prompt,
// and stamps the frames that come back with it.
//
// The version of this code that stamped the agent's own session id passed
// every test in the package, because the in-process e2e writes its frames
// with the control plane's id and never goes near a real agent. The only
// thing that catches it is a node and an agent that are actually running,
// which is what the delivery acceptance is for; the tests here are what
// stops it coming back.

// fakeClock is a clock the test moves by hand, so that a window measured in
// seconds can be checked without a test that sleeps for them.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// TestAFrameIsStampedWithTheConversationThatPrompted is the regression this
// whole file exists for.
func TestAFrameIsStampedWithTheConversationThatPrompted(t *testing.T) {
	src := &liveSource{ready: make(chan struct{}, 1)}
	src.proc = &liveProcess{src: src}
	stub := newCapturingTunnel()
	b, err := NewAgentBridge(AgentBridgeOptions{Source: src, Client: stub, EdgeID: 7})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.StartEvents(ctx)
	<-src.ready

	// The agent calls its own session "s-1". The console is looking at
	// "conv-1". The node is the only thing that knows both.
	b.noteRole("conv-1", "investigator")
	src.emit(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1", Seq: 1, Payload: []byte(`{}`)})

	frame := waitForFrame(t, stub)
	if frame.SessionID != "conv-1" {
		t.Fatalf("frame is stamped %q, want the conversation %q; "+
			"the control plane routes by this id and drops what it cannot attribute",
			frame.SessionID, "conv-1")
	}
}

// TestTheConsoleFrameCarriesTheSameConversation covers the inner frame. The
// translator builds a console frame from the agent's record, so it inherits
// the agent's session id — and a correct outer envelope carrying an
// unroutable inner one is the same outage with better manners.
func TestTheConsoleFrameCarriesTheSameConversation(t *testing.T) {
	src := &liveSource{ready: make(chan struct{}, 1)}
	src.proc = &liveProcess{src: src}
	stub := newCapturingTunnel()
	b, err := NewAgentBridge(AgentBridgeOptions{
		Source: src, Client: stub, EdgeID: 7,
		// A translator that does what the node's does: it reads the agent's
		// record and names the session the agent used.
		Translate: func(ev ports.ProcessEvent) []wire.StreamEvent {
			return []wire.StreamEvent{{
				Type:      wire.StreamEventType(ev.Type),
				SessionID: ev.SessionID,
				Assistant: &wire.AssistantFrame{Content: "hi"},
			}}
		},
	})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.StartEvents(ctx)
	<-src.ready

	b.noteRole("conv-1", "")
	src.emit(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1", Seq: 1, Payload: []byte(`{}`)})

	frame := waitForFrame(t, stub)
	if frame.Frame == nil {
		t.Fatal("the node sent an envelope with no console frame in it")
	}
	if frame.Frame.SessionID != "conv-1" {
		t.Fatalf("the console frame names session %q, want %q", frame.Frame.SessionID, "conv-1")
	}
}

// TestTheTranslatorIsHandedTheConversation pins the difference between
// stamping the frame the translator returns and handing the translator an
// event it will accept.
//
// The node's real translator keys one counter chain per conversation and
// refuses an event with no session id, because guessing would renumber a
// live conversation. A node agent's events arrive with an empty session —
// the node never tells the agent which console is talking — so a translator
// fed the agent's raw record produces nothing at all, and the turn runs,
// is paid for, and reaches the operator as silence. The test uses that
// refusing translator rather than an accommodating one, so that stamping
// the returned frame cannot be mistaken for the fix.
func TestTheTranslatorIsHandedTheConversation(t *testing.T) {
	src := &liveSource{ready: make(chan struct{}, 1)}
	src.proc = &liveProcess{src: src}
	stub := newCapturingTunnel()
	b, err := NewAgentBridge(AgentBridgeOptions{
		Source: src, Client: stub, EdgeID: 7,
		// The node's contract, verbatim: no session id, no conversation to
		// number the event against, no frame.
		Translate: func(ev ports.ProcessEvent) []wire.StreamEvent {
			if ev.SessionID == "" {
				return nil
			}
			return []wire.StreamEvent{{
				Type:      wire.StreamEventType(ev.Type),
				SessionID: ev.SessionID,
				Assistant: &wire.AssistantFrame{Content: "hi"},
			}}
		},
	})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.StartEvents(ctx)
	<-src.ready

	b.noteRole("conv-1", "")
	src.emit(ports.ProcessEvent{Type: "turn_start", SessionID: "", Seq: 1, Payload: []byte(`{}`)})

	frame := waitForFrame(t, stub)
	if frame.Frame == nil {
		t.Fatal("the translator was handed the agent's empty session, so it built nothing")
	}
	if frame.Frame.SessionID != "conv-1" {
		t.Fatalf("the console frame names session %q, want %q", frame.Frame.SessionID, "conv-1")
	}
}

// TestARecordWithNoTurnInFlightIsCountedRatherThanSent covers the agent
// talking when no console asked it to.
func TestARecordWithNoTurnInFlightIsCountedRatherThanSent(t *testing.T) {
	src := &liveSource{ready: make(chan struct{}, 1)}
	src.proc = &liveProcess{src: src}
	stub := newCapturingTunnel()
	b, err := NewAgentBridge(AgentBridgeOptions{Source: src, Client: stub, EdgeID: 7})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.StartEvents(ctx)
	<-src.ready

	src.emit(ports.ProcessEvent{Type: "notice", SessionID: "s-1", Seq: 1, Payload: []byte(`{}`)})

	select {
	case <-stub.got:
		t.Fatal("an unattributable record reached the control plane; " +
			"it would be routed to whichever conversation looked closest")
	case <-time.After(200 * time.Millisecond):
	}
	if got := b.UnattributedEvents(); got != 1 {
		t.Fatalf("unattributed records = %d, want 1; this number is the node's "+
			"only evidence that its agent is talking and nobody is listening", got)
	}
}

// TestATurnEndsWhenTheAgentSaysSo is the other half of the ownership: the
// marker is the conversation's turn, so a terminal record clears it and the
// next conversation is not told the node is busy.
//
// It also pins the two claims apart, because they are two claims. "This
// turn is over" releases the node; "this turn is still being described"
// keeps attributing its last events. Collapsing them is how a node ends up
// either refusing a second operator for ten seconds, or dropping the `done`
// frame that closes the console's stream.
func TestATurnEndsWhenTheAgentSaysSo(t *testing.T) {
	src := &liveSource{ready: make(chan struct{}, 1)}
	src.proc = &liveProcess{src: src}
	stub := newCapturingTunnel()
	b, err := NewAgentBridge(AgentBridgeOptions{Source: src, Client: stub, EdgeID: 7})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.StartEvents(ctx)
	<-src.ready

	b.noteRole("conv-1", "")
	src.emit(ports.ProcessEvent{Type: "turn_end", SessionID: "s-1", Terminal: true, Payload: []byte(`{}`)})
	waitForFrame(t, stub)

	if owner := b.inFlight(); owner != "" {
		t.Fatalf("the node still believes %q is mid-turn, so a second operator is "+
			"told the node is busy after the agent ended the turn", owner)
	}
	// The turn is over but not yet reported on: the `done` frame the console
	// closes its stream on arrives on the next event.
	if owner := b.turnOwner(); owner != "conv-1" {
		t.Fatalf("events after the turn ended are attributed to %q, want the "+
			"conversation that ran the turn, %q", owner, "conv-1")
	}
}

// TestTheClaimOnAConversationOutlivesItsTurnOnly briefly checks the other
// end: the window is a bound, and a node that held yesterday's turn would
// be unusable.
func TestTheClaimOnAConversationOutlivesItsTurnOnlyBriefly(t *testing.T) {
	clock := newFakeClock()
	src := &liveSource{ready: make(chan struct{}, 1)}
	src.proc = &liveProcess{src: src}
	stub := newCapturingTunnel()
	b, err := NewAgentBridge(AgentBridgeOptions{
		Source: src, Client: stub, EdgeID: 7, Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.StartEvents(ctx)
	<-src.ready

	b.noteRole("conv-1", "")
	src.emit(ports.ProcessEvent{Type: "turn_end", SessionID: "s-1", Terminal: true, Payload: []byte(`{}`)})
	waitForFrame(t, stub)

	clock.advance(settleGrace + time.Second)
	if owner := b.turnOwner(); owner != "" {
		t.Fatalf("the node still attributes events to %q long after the turn ended; "+
			"the claim has to expire so a stale record cannot follow an operator around", owner)
	}
}

// TestASecondConversationIsRefusedWhileATurnIsInFlight covers two consoles
// on one node.
//
// The refusal is the point: a single agent session cannot interleave two
// investigations, and accepting the second prompt would stamp its output
// with whichever conversation started last — which renders one operator's
// turn inside another's transcript.
func TestASecondConversationIsRefusedWhileATurnIsInFlight(t *testing.T) {
	src := &liveSource{ready: make(chan struct{}, 1)}
	src.proc = &liveProcess{src: src}
	stub := newCapturingTunnel()
	b, err := NewAgentBridge(AgentBridgeOptions{Source: src, Client: stub, EdgeID: 7})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}

	raw, err := b.handlePrompt(context.Background(), tunnel.Session{}, "agent.prompt",
		mustJSON(t, tunnel.AgentPromptRequest{SessionID: "conv-1", Text: "look"}))
	if err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	if resp := decodePrompt(t, raw); !resp.Accepted {
		t.Fatalf("the first turn was refused: %+v", resp)
	}

	raw, err = b.handlePrompt(context.Background(), tunnel.Session{}, "agent.prompt",
		mustJSON(t, tunnel.AgentPromptRequest{SessionID: "conv-2", Text: "also look"}))
	if err != nil {
		t.Fatalf("second prompt: %v", err)
	}
	resp := decodePrompt(t, raw)
	if resp.Accepted {
		t.Fatal("a second conversation was accepted mid-turn; its output would be " +
			"stamped with the first conversation's id")
	}
	if resp.Code != CodeAgentBusy {
		t.Fatalf("refusal code = %q, want %q", resp.Code, CodeAgentBusy)
	}
	if resp.SessionID != "conv-2" {
		t.Fatalf("the refusal names session %q, want the one that asked (conv-2)", resp.SessionID)
	}
}

// TestAConversationMayContinueItsOwnTurn makes sure the busy rule is about
// concurrency and not about being busy at all: the same conversation
// steering or re-prompting itself is the normal shape of a turn.
func TestAConversationMayContinueItsOwnTurn(t *testing.T) {
	src := &liveSource{ready: make(chan struct{}, 1)}
	src.proc = &liveProcess{src: src}
	stub := newCapturingTunnel()
	b, err := NewAgentBridge(AgentBridgeOptions{Source: src, Client: stub, EdgeID: 7})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}

	for i := 0; i < 2; i++ {
		raw, err := b.handlePrompt(context.Background(), tunnel.Session{}, "agent.prompt",
			mustJSON(t, tunnel.AgentPromptRequest{SessionID: "conv-1", Text: "again"}))
		if err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
		if resp := decodePrompt(t, raw); !resp.Accepted {
			t.Fatalf("a conversation was locked out of its own turn: %+v", resp)
		}
	}
}

func waitForFrame(t *testing.T, stub *capturingTunnel) tunnel.AgentEventFrame {
	t.Helper()
	select {
	case <-stub.got:
	case <-time.After(2 * time.Second):
		t.Fatal("no frame reached the tunnel")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.frame
}

// mustJSON encodes a request body for a direct handlePrompt call.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return raw
}

// decodePrompt reads the node's reply to a prompt.
func decodePrompt(t *testing.T, raw []byte) tunnel.AgentPromptResponse {
	t.Helper()
	var resp tunnel.AgentPromptResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode reply %s: %v", raw, err)
	}
	return resp
}

// durableSource is a supervisor-shaped source: it can bind a relay whether or
// not a process exists yet, and keeps the binding across replacements. That
// is the shape pigsupervisor.Supervisor has and the shape a node's bridge
// depends on.
type durableSource struct {
	mu    sync.Mutex
	subs  []func(ports.ProcessEvent)
	ready bool
}

func (d *durableSource) Process() (ports.AgentProcess, error) {
	if !d.up() {
		return nil, errors.New("pigsupervisor: agent process is not running")
	}
	return &liveProcess{}, nil
}

func (d *durableSource) Health() ports.ProcessHealth { return ports.ProcessHealth{Running: d.up()} }

func (d *durableSource) OnEvent(fn func(ports.ProcessEvent)) func() {
	d.mu.Lock()
	d.subs = append(d.subs, fn)
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		for i, sub := range d.subs {
			if sub == nil {
				continue
			}
			d.subs[i] = nil
		}
		d.mu.Unlock()
	}
}

func (d *durableSource) up() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ready
}

func (d *durableSource) comeUp() {
	d.mu.Lock()
	d.ready = true
	d.mu.Unlock()
}

func (d *durableSource) emit(ev ports.ProcessEvent) {
	d.mu.Lock()
	subs := append([]func(ports.ProcessEvent){}, d.subs...)
	d.mu.Unlock()
	for _, fn := range subs {
		if fn != nil {
			fn(ev)
		}
	}
}

// TestARelayBoundBeforeTheAgentExistsStillDelivers is the regression for the
// node that was deaf.
//
// StartEvents runs once during startup, and on a cold node there is no agent
// process to bind to. A bridge that treats "no process yet" as "nothing to
// do" is deaf for the rest of its life: the node heartbeats, reports a
// healthy agent, runs the turn, pays for the model — and the operator sees
// an empty conversation, with no error anywhere to explain it.
func TestARelayBoundBeforeTheAgentExistsStillDelivers(t *testing.T) {
	src := &durableSource{}
	stub := newCapturingTunnel()
	b, err := NewAgentBridge(AgentBridgeOptions{Source: src, Client: stub, EdgeID: 7})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The agent is not up yet. This is the ordinary state of a node that has
	// just started.
	b.StartEvents(ctx)
	src.comeUp()

	b.noteRole("conv-1", "investigator")
	src.emit(ports.ProcessEvent{Type: "turn_start", Seq: 1, Payload: []byte(`{}`)})

	frame := waitForFrame(t, stub)
	if frame.SessionID != "conv-1" {
		t.Fatalf("frame is stamped %q, want conv-1", frame.SessionID)
	}
}
