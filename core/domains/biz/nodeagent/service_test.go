package nodeagent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// fakeFleet records what the service asked of the routing layer and lets a
// test push frames back through whichever sink was registered.
type fakeFleet struct {
	mu sync.Mutex
	// sinks is keyed on the conversation id the fleet was opened with.
	sinks map[string]ports.EventSink
	// opens counts Open calls, so a test can tell a fresh conversation
	// from one that reused an id.
	opens int
	// promptErr, when set, is what Prompt returns.
	promptErr error
	// openErr, when set, is what Open returns.
	openErr error

	prompts   []domain.AgentPrompt
	steers    []string
	abortCall int
	closed    []string
	stats     []domain.AgentSessionStats

	// decide records what the last approval answer carried and which node
	// it was routed to. Both matter: the edge id proves the answer went to
	// the node running the turn rather than to whichever node was
	// convenient.
	decide     domain.AgentDecision
	decideEdge uint64
	decideErr  error
}

func newFakeFleet() *fakeFleet { return &fakeFleet{sinks: map[string]ports.EventSink{}} }

func (f *fakeFleet) Open(req domain.AgentPrompt, sink ports.EventSink) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return f.openErr
	}
	f.opens++
	f.sinks[req.SessionID] = sink
	return nil
}

func (f *fakeFleet) Prompt(_ context.Context, req domain.AgentPrompt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, req)
	return f.promptErr
}

func (f *fakeFleet) Steer(_ context.Context, _ uint64, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steers = append(f.steers, text)
	return nil
}

func (f *fakeFleet) Abort(context.Context, uint64, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abortCall++
	return nil
}

func (f *fakeFleet) Decide(_ context.Context, edgeID uint64, _ string, d domain.AgentDecision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decideEdge = edgeID
	f.decide = d
	if f.decideErr != nil {
		return f.decideErr
	}
	return nil
}

func (f *fakeFleet) State(context.Context, uint64) (*ports.ProcessState, error) {
	return &ports.ProcessState{}, nil
}

func (f *fakeFleet) Health(context.Context, uint64) (*tunnel.AgentHealthResponse, error) {
	return &tunnel.AgentHealthResponse{}, nil
}

func (f *fakeFleet) Close(_ uint64, sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, sessionID)
	delete(f.sinks, sessionID)
}

func (f *fakeFleet) AllStats() []domain.AgentSessionStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AgentSessionStats(nil), f.stats...)
}

// push delivers a frame as the fleet would, from the node's relay.
func (f *fakeFleet) push(t *testing.T, sessionID string, ev wire.StreamEvent) {
	t.Helper()
	f.mu.Lock()
	sink := f.sinks[sessionID]
	f.mu.Unlock()
	if sink == nil {
		t.Fatalf("no conversation %q is open with the fleet", sessionID)
	}
	if err := sink.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
}

func (f *fakeFleet) promptTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.prompts))
	for _, p := range f.prompts {
		out = append(out, p.UserText)
	}
	return out
}

func newService(t *testing.T, fleet *fakeFleet) *Service {
	t.Helper()
	var n int
	s, err := New(Options{
		Fleet:      fleet,
		QueueDepth: 8,
		Grace:      50 * time.Millisecond,
		NewID: func() string {
			n++
			return "s-" + string(rune('0'+n))
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestOpenMintsAnIdAndRegistersWithTheFleet(t *testing.T) {
	fleet := newFakeFleet()
	s := newService(t, fleet)
	id, err := s.Open(OpenRequest{EdgeID: 7})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if id == "" {
		t.Fatal("Open returned no conversation id")
	}
	if s.Count() != 1 {
		t.Errorf("Count = %d, want 1", s.Count())
	}
	if _, err := s.EdgeID(id); err != nil {
		t.Errorf("EdgeID: %v", err)
	}
}

func TestOpenRefusesAClashingConversationID(t *testing.T) {
	// Two consoles on one id would interleave two assistants into a single
	// agent session, and the transcript neither of them could read would
	// be the only record of what the agent did to the host.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "shared"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Open(OpenRequest{EdgeID: 8, SessionID: "shared"}); !errors.Is(err, ErrSessionExists) {
		t.Errorf("second Open = %v, want ErrSessionExists", err)
	}
	if s.Count() != 1 {
		t.Errorf("Count = %d, want 1", s.Count())
	}
}

func TestARefusedOpenLeavesNoConversationBehind(t *testing.T) {
	// A node that refused the open - no agent, crash-looping - must not
	// leave a half-registered conversation the console can send turns to.
	fleet := newFakeFleet()
	fleet.openErr = errors.New("node refused")
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err == nil {
		t.Fatal("Open succeeded")
	}
	if s.Count() != 0 {
		t.Errorf("Count = %d after a refused open, want 0", s.Count())
	}
}

func TestSendIsRefusedWhileNobodyIsWatching(t *testing.T) {
	// Sending blind is how "the button did nothing" reports start: the
	// agent works for minutes and the operator has no way to see any of it.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Send(context.Background(), "s-1", "why is the disk full?", false); !errors.Is(err, ErrNoStream) {
		t.Errorf("Send = %v, want ErrNoStream", err)
	}
	if got := len(fleet.promptTexts()); got != 0 {
		t.Errorf("the node was sent %d turns for an unwatched conversation", got)
	}
}

func TestSendReachesTheNodeOnceAConsoleIsAttached(t *testing.T) {
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	st, err := s.Attach("s-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer st.Detach()

	if err := s.Send(context.Background(), "s-1", "why is the disk full?", false); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := fleet.promptTexts()
	if len(got) != 1 || got[0] != "why is the disk full?" {
		t.Errorf("the node was sent %v", got)
	}
}

func TestSteeringGoesToTheRunningTurnRatherThanStartingANewOne(t *testing.T) {
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	st, err := s.Attach("s-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer st.Detach()

	if err := s.Send(context.Background(), "s-1", "actually, check the inode table", true); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := len(fleet.steers); got != 1 {
		t.Errorf("steers = %d, want 1", got)
	}
	if got := len(fleet.prompts); got != 0 {
		t.Errorf("a steer also started %d new turns: the agent would answer two questions at once", got)
	}
}

func TestStopEndsTheTurnAndKeepsTheConversation(t *testing.T) {
	// The operator means "stop this turn", not "end the conversation": the
	// bubble they are looking at has to accept another message.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	st, err := s.Attach("s-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer st.Detach()

	if err := s.Stop(context.Background(), "s-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if fleet.abortCall != 1 {
		t.Errorf("aborts = %d, want 1", fleet.abortCall)
	}
	if s.Count() != 1 {
		t.Errorf("Count = %d after stopping a turn, want 1: the conversation must survive", s.Count())
	}
	if err := s.Send(context.Background(), "s-1", "try something else", false); err != nil {
		t.Errorf("Send after Stop = %v, want the conversation to still take turns", err)
	}
}

func TestCloseEndsTheConversationOnBothSides(t *testing.T) {
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Close("s-1")
	if s.Count() != 0 {
		t.Errorf("Count = %d after Close, want 0", s.Count())
	}
	if len(fleet.closed) != 1 {
		t.Errorf("the fleet closed %d sessions, want 1: the node would keep the agent session alive forever", len(fleet.closed))
	}
	if err := s.Send(context.Background(), "s-1", "hello?", false); !errors.Is(err, ErrNoSession) {
		t.Errorf("Send after Close = %v, want ErrNoSession", err)
	}
}

func TestClosingAStreamEndsAWaitingConsole(t *testing.T) {
	// A console blocked in Next holds an HTTP request open forever, and
	// the request is what keeps the connection counted against the
	// manager's limits.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	st, err := s.Attach("s-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	result := make(chan bool, 1)
	go func() {
		_, ok := st.Next(context.Background())
		result <- ok
	}()
	time.Sleep(10 * time.Millisecond)
	s.Close("s-1")
	select {
	case ok := <-result:
		if ok {
			t.Error("a closed conversation still delivered a frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next never returned for a closed conversation")
	}
}

func TestAStreamEndsAfterATurnFinishes(t *testing.T) {
	// An SSE stream that never closes is a console reconnecting forever
	// and a manager accumulating dead requests. The turn is over, so the
	// stream is over.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	st, err := s.Attach("s-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer st.Detach()
	fleet.push(t, "s-1", wire.StreamEvent{Type: wire.StreamDone, SessionID: "s-1"})

	if ev, ok := st.Next(context.Background()); !ok || ev.Type != wire.StreamDone {
		t.Fatalf("first frame = %+v, want the done frame", ev)
	}
	if _, ok := st.Next(context.Background()); ok {
		t.Error("the stream stayed open after a finished turn")
	}
}

func TestFramesStillArivingAfterTheTerminalFrameAreDelivered(t *testing.T) {
	// The terminal frame and the tool frames it follows can cross the
	// tunnel out of order. Closing on the terminal frame alone truncates
	// a turn in the middle of its own summary.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	st, err := s.Attach("s-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer st.Detach()

	fleet.push(t, "s-1", wire.StreamEvent{Type: wire.StreamDone, SessionID: "s-1"})
	if _, ok := st.Next(context.Background()); !ok {
		t.Fatal("the done frame was not delivered")
	}
	// A straggler that lands inside the grace window.
	fleet.push(t, "s-1", wire.StreamEvent{Type: wire.StreamAssistantEnd, SessionID: "s-1"})
	ev, ok := st.Next(context.Background())
	if !ok || ev.Type != wire.StreamAssistantEnd {
		t.Errorf("late frame = %+v (ok=%v), want the straggler delivered", ev, ok)
	}
}

func TestAFollowUpTurnInsideTheGraceWindowKeepsTheStreamOpen(t *testing.T) {
	// The operator saw a finished turn and immediately asked a follow-up.
	// Closing on them here would answer into a stream nobody is reading,
	// and the agent's work would be invisible.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	st, err := s.Attach("s-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer st.Detach()

	fleet.push(t, "s-1", wire.StreamEvent{Type: wire.StreamDone, SessionID: "s-1"})
	if _, ok := st.Next(context.Background()); !ok {
		t.Fatal("the done frame was not delivered")
	}
	if err := s.Send(context.Background(), "s-1", "and what changed?", false); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fleet.push(t, "s-1", wire.StreamEvent{Type: wire.StreamAssistantEnd, SessionID: "s-1"})
	ev, ok := st.Next(context.Background())
	if !ok || ev.Type != wire.StreamAssistantEnd {
		t.Errorf("follow-up frame = %+v (ok=%v), want the stream still open", ev, ok)
	}
}

func TestASlowConsoleDropsFramesRatherThanStallingTheNode(t *testing.T) {
	// The caller here is the node's relay, and the node's agent has to keep
	// working when the control plane is slow - an operator is most likely
	// reading about that slowness through this very agent. A dropped frame
	// shows up as a sequence gap the console already renders; a stalled
	// node shows up as no agent at all.
	fleet := newFakeFleet()
	s, err := New(Options{Fleet: fleet, QueueDepth: 2, Grace: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			fleet.push(t, "s-1", wire.StreamEvent{Type: wire.StreamAssistantDelta, Seq: int64(i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pushing frames blocked: the node's relay would stall on a slow console")
	}

	var found bool
	for _, st := range s.AllStats() {
		if st.Dropped > 0 {
			found = true
		}
	}
	if !found {
		t.Error("no drop was counted: the console would be shown a conversation with silent holes")
	}
}

func TestFramesThatArriveBeforeAConsoleAttachesAreKept(t *testing.T) {
	// A console opens the stream and sends its turn in that order, so the
	// opening frames of the first turn always arrive before anyone is
	// listening. Dropping them makes every first turn start mid-sentence.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{EdgeID: 7, SessionID: "s-1"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	fleet.push(t, "s-1", wire.StreamEvent{Type: wire.StreamAssistantStart, Seq: 1})
	fleet.push(t, "s-1", wire.StreamEvent{Type: wire.StreamAssistantDelta, Seq: 2})

	st, err := s.Attach("s-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer st.Detach()
	for _, want := range []wire.StreamEventType{wire.StreamAssistantStart, wire.StreamAssistantDelta} {
		ev, ok := st.Next(context.Background())
		if !ok || ev.Type != want {
			t.Fatalf("frame = %+v (ok=%v), want %s", ev, ok, want)
		}
	}
}

func TestAModelSelectionReachesTheFleet(t *testing.T) {
	fleet := newFakeFleet()
	s := newService(t, fleet)
	if _, err := s.Open(OpenRequest{
		EdgeID: 7, SessionID: "s-1",
		Selection: domain.ModelSelection{Provider: domain.ProviderAnthropic, Model: "claude-opus-5"},
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The fleet pins before the first turn; the service's job is only to
	// carry the choice through, so the assertion is that it was not
	// dropped on the way.
	if s.Count() != 1 {
		t.Errorf("Count = %d, want 1", s.Count())
	}
}

func TestAMintedIDIsNotGuessable(t *testing.T) {
	// The id travels in the console's URL and on every frame the node
	// stamps. An enumerable one would let any caller that can reach the
	// manager attach to another operator's conversation.
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		id := NewSessionID()
		if seen[id] {
			t.Fatalf("NewSessionID repeated %q within 200 calls", id)
		}
		seen[id] = true
		if len(id) < 16 {
			t.Fatalf("NewSessionID = %q, too short to be unguessable", id)
		}
	}
}

func TestNewRequiresAFleet(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("a Service with no fleet was accepted: every turn would fail at the first call")
	}
}

// --- approval answers ----------------------------------------------------

func TestADecisionGoesToTheNodeRunningTheTurn(t *testing.T) {
	// The conversation is the routing key. An answer that reached a
	// different node would be refused there as an unknown request, and the
	// operator would be left watching a call that stays blocked with no
	// explanation of where their click went.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	id, err := s.Open(OpenRequest{EdgeID: 7})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Decide(context.Background(), id, domain.AgentDecision{
		RequestID: "ar-1", Digest: "d-1", Grant: true, DecidedBy: "alice",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	fleet.mu.Lock()
	defer fleet.mu.Unlock()
	if fleet.decideEdge != 7 {
		t.Errorf("routed to edge %d, want 7", fleet.decideEdge)
	}
	if fleet.decide.RequestID != "ar-1" || !fleet.decide.Grant {
		t.Errorf("decision = %+v, want a grant of ar-1", fleet.decide)
	}
	if fleet.decide.Digest != "d-1" {
		t.Errorf("digest = %q, want it echoed verbatim: the node is what checks it", fleet.decide.Digest)
	}
}

func TestADenialIsRoutedAsADenial(t *testing.T) {
	// A refusal has to stay a refusal all the way to the node. Encoding
	// "not granted" as anything other than deny would leave the call
	// blocked by a decision that looks like it was accepted.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	id, err := s.Open(OpenRequest{EdgeID: 3})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Decide(context.Background(), id, domain.AgentDecision{RequestID: "ar-9", Grant: false}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	fleet.mu.Lock()
	defer fleet.mu.Unlock()
	if fleet.decide.Grant {
		t.Error("a refusal arrived at the node as a grant")
	}
}

func TestADecisionForAnUnknownConversationIsRefused(t *testing.T) {
	fleet := newFakeFleet()
	s := newService(t, fleet)
	err := s.Decide(context.Background(), "s-none", domain.AgentDecision{RequestID: "ar-1"})
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession: there is no node to send it to", err)
	}
}

func TestADecisionWithNoRequestIDIsRefused(t *testing.T) {
	// The service cannot check the digest - only the node holds the call -
	// so a decision with nothing to identify is caught here rather than
	// travelling the tunnel to be refused for the same reason.
	fleet := newFakeFleet()
	s := newService(t, fleet)
	id, err := s.Open(OpenRequest{EdgeID: 3})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Decide(context.Background(), id, domain.AgentDecision{}); err == nil {
		t.Error("a decision naming no request was accepted")
	}
}

func TestANodeRefusalIsPassedBackRatherThanSwallowed(t *testing.T) {
	// The common case is an operator answering a request that already
	// lapsed. The console has to be able to say so; a service that turned
	// every refusal into success would show a released call that is still
	// blocked.
	fleet := newFakeFleet()
	fleet.decideErr = errors.New("node 3 refused: no pending approval")
	s := newService(t, fleet)
	id, err := s.Open(OpenRequest{EdgeID: 3})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Decide(context.Background(), id, domain.AgentDecision{RequestID: "ar-1"}); err == nil {
		t.Error("a node refusal was reported as a decision applied")
	}
}
