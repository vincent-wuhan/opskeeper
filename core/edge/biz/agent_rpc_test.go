package biz

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// recordingDecider stands in for the node's approval gate.
//
// It is a double rather than a real gate because what is under test here is
// the node's edge of the round trip: what it accepts, what it refuses, and
// what it says when it has nothing to apply the answer to. The gate's own
// behaviour is covered where the gate is.
type recordingDecider struct {
	got   ports.Decision
	err   error
	calls int
}

func (d *recordingDecider) Decide(dec ports.Decision) error {
	d.calls++
	d.got = dec
	return d.err
}

// newDecideBridge builds a bridge whose only interesting part is the gate.
func newDecideBridge(t *testing.T, d AgentDecider) *AgentBridge {
	t.Helper()
	b, err := NewAgentBridge(AgentBridgeOptions{
		Source:  &stubSource{},
		Decider: d,
	})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	return b
}

// decide invokes the handler the way the tunnel does.
func decide(t *testing.T, b *AgentBridge, body string) tunnel.AgentDecideResponse {
	t.Helper()
	raw, err := b.handleDecide(context.Background(), tunnel.Session{}, "", []byte(body))
	if err != nil {
		t.Fatalf("handleDecide returned a transport error: %v", err)
	}
	var resp tunnel.AgentDecideResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return resp
}

func TestAGrantIsAppliedAndCarriesTheOperatorAndNote(t *testing.T) {
	// The note and the operator are the two things the node's ledger cannot
	// reconstruct for itself. Dropping either on the way down would leave a
	// row saying who released a restart but not why.
	gate := &recordingDecider{}
	b := newDecideBridge(t, gate)

	resp := decide(t, b, `{"request_id":"ar-1","digest":"d-1","decision":"grant","decided_by":"alice","note":"canary first"}`)
	if !resp.Applied {
		t.Fatalf("applied = false, code %q: %s", resp.Code, resp.Error)
	}
	if gate.calls != 1 {
		t.Fatalf("the gate was called %d times, want 1", gate.calls)
	}
	if gate.got.Decision != ports.ApprovalGranted {
		t.Errorf("decision = %q, want a grant", gate.got.Decision)
	}
	if gate.got.Digest != "d-1" {
		t.Errorf("digest = %q, want it forwarded for the gate to check", gate.got.Digest)
	}
	if gate.got.DecidedBy != "alice" || gate.got.Note != "canary first" {
		t.Errorf("the gate saw %+v, want the operator and the note", gate.got)
	}
}

func TestADenialReachesTheGateAsADenial(t *testing.T) {
	gate := &recordingDecider{}
	b := newDecideBridge(t, gate)

	resp := decide(t, b, `{"request_id":"ar-1","digest":"d-1","decision":"deny"}`)
	if !resp.Applied {
		t.Fatalf("applied = false, code %q: %s", resp.Code, resp.Error)
	}
	if gate.got.Decision != ports.ApprovalDenied {
		t.Errorf("decision = %q, want a denial", gate.got.Decision)
	}
}

func TestANodeWithNoGateSaysSoRatherThanRefusingTheRequest(t *testing.T) {
	// A missing gate and a bad request are different problems with
	// different fixes. Reporting "bad decision" for a node that has no gate
	// would send an operator to look at their console.
	b := newDecideBridge(t, nil)
	resp := decide(t, b, `{"request_id":"ar-1","digest":"d-1","decision":"grant"}`)
	if resp.Applied || resp.Code != CodeAgentNoGate {
		t.Errorf("applied=%v code=%q, want %q", resp.Applied, resp.Code, CodeAgentNoGate)
	}
}

func TestAWordTheGateDoesNotAcceptIsRefusedWithoutReachingIt(t *testing.T) {
	// The safe reading of a value nobody can honour is "no". Forwarding it
	// would put an unparseable decision into the gate's map lookup, and the
	// gate would then be the only thing standing between a typo and a
	// released call.
	gate := &recordingDecider{}
	b := newDecideBridge(t, gate)

	resp := decide(t, b, `{"request_id":"ar-1","digest":"d-1","decision":"maybe"}`)
	if resp.Applied || resp.Code != CodeAgentBadDecision {
		t.Errorf("applied=%v code=%q, want %q", resp.Applied, resp.Code, CodeAgentBadDecision)
	}
	if gate.calls != 0 {
		t.Errorf("the gate was called %d times with a decision it cannot accept", gate.calls)
	}
}

func TestAStaleOrUnboundDecisionIsRefusedAndNotApplied(t *testing.T) {
	// An operator clicking a request that already lapsed is ordinary. The
	// answer has to be "that request is gone", not a transport error and not
	// a silent success that leaves the call blocked.
	gate := &recordingDecider{err: errors.New("policygate: no pending approval \"ar-1\"")}
	b := newDecideBridge(t, gate)

	resp := decide(t, b, `{"request_id":"ar-1","digest":"d-1","decision":"grant"}`)
	if resp.Applied || resp.Code != CodeAgentBadDecision {
		t.Errorf("applied=%v code=%q, want %q", resp.Applied, resp.Code, CodeAgentBadDecision)
	}
	if resp.RequestID != "ar-1" {
		t.Errorf("request_id = %q, want it echoed so a multiplexed caller can match the answer", resp.RequestID)
	}
}

func TestAMalformedBodyIsABadRequest(t *testing.T) {
	gate := &recordingDecider{}
	b := newDecideBridge(t, gate)
	resp := decide(t, b, `{"request_id":`)
	if resp.Applied || resp.Code != CodeAgentBadRequest {
		t.Errorf("applied=%v code=%q, want %q", resp.Applied, resp.Code, CodeAgentBadRequest)
	}
	if gate.calls != 0 {
		t.Error("an unparseable body reached the gate")
	}
}

func TestADecisionNamingNoRequestIsRefusedBeforeTheGate(t *testing.T) {
	gate := &recordingDecider{}
	b := newDecideBridge(t, gate)
	resp := decide(t, b, `{"digest":"d-1","decision":"grant"}`)
	if resp.Applied || resp.Code != CodeAgentBadDecision {
		t.Errorf("applied=%v code=%q, want %q", resp.Applied, resp.Code, CodeAgentBadDecision)
	}
	if gate.calls != 0 {
		t.Error("a decision with nothing to apply it to reached the gate")
	}
}

// stubSource is a node with an agent process this test never talks to.
//
// The decision path does not reach the process: the gate is on the node,
// not in the agent, which is the whole reason a blocked call can be
// released at all. A source that is present but unused is therefore the
// honest stub - using "no agent" would make the handler's refusals about
// unavailability rather than about the gate.
type stubSource struct{}

func (stubSource) Process() (ports.AgentProcess, error) {
	return nil, errors.New("not used by these tests")
}

func (stubSource) Health() ports.ProcessHealth { return ports.ProcessHealth{} }

// --- the node's own record of who is asking ------------------------------

// newRoleBridge builds a bridge with a clock the test controls.
func newRoleBridge(t *testing.T, now func() time.Time) *AgentBridge {
	t.Helper()
	b, err := NewAgentBridge(AgentBridgeOptions{Source: &stubSource{}, Now: now})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	return b
}

func TestAPromptRecordsTheRoleTheTurnIsRunningAs(t *testing.T) {
	// The role travels on the prompt and is needed on every later tool
	// call, so the node has to remember it. The gate asks "who is asking"
	// once a call is already blocked, and by then the turn is long
	// finished.
	now := time.Now()
	b := newRoleBridge(t, func() time.Time { return now })
	b.noteRole("s-1", "viewer")

	if got := b.ActorFor("s-1"); got != "viewer" {
		t.Errorf("actor = %q, want viewer", got)
	}
}

func TestAConversationTheNodeHasNeverSeenIsNobody(t *testing.T) {
	// Failing to the bottom of the ladder is the point: an agent that
	// invented a session id must not be able to reach a mutating tool by
	// getting it into the gate's actor.
	b := newRoleBridge(t, time.Now)
	if got := b.ActorFor("s-invented"); got != "" {
		t.Errorf("actor = %q, want empty for a session nobody opened", got)
	}
	if got := b.ActorFor(""); got != "" {
		t.Errorf("actor = %q, want empty for no session at all", got)
	}
}

func TestAnExpiredRoleResolvesToNobodyRatherThanSticking(t *testing.T) {
	// The node is never told a conversation closed — the close is a
	// manager-side act that does not reach here — so the record has to age
	// out on its own or the table grows for the life of the process.
	now := time.Now()
	b := newRoleBridge(t, func() time.Time { return now })
	b.noteRole("s-1", "admin")
	if got := b.ActorFor("s-1"); got != "admin" {
		t.Fatalf("actor = %q before expiry, want admin", got)
	}

	now = now.Add(roleTTL + time.Minute)
	if got := b.ActorFor("s-1"); got != "" {
		t.Errorf("actor = %q after the TTL, want empty: a stale admin on a closed conversation is the wrong way to fail", got)
	}
	// And the record is actually gone, not merely hidden.
	b.roleMu.RLock()
	held := len(b.roles)
	b.roleMu.RUnlock()
	if held != 0 {
		t.Errorf("%d role records survived their own expiry", held)
	}
}

func TestARefreshedConversationKeepsItsRoleAcrossTheExpiryBoundary(t *testing.T) {
	// The check-then-prune is racy by nature: a turn arriving as the clock
	// crosses the TTL must not have its own record pulled out from under
	// it. Dropping a live record would make a running admin turn read-only
	// halfway through — a failure that looks like a permissions bug and is
	// really a bookkeeping one.
	now := time.Now()
	b := newRoleBridge(t, func() time.Time { return now })
	b.noteRole("s-1", "admin")

	// A reader sees the record as expired...
	stale := now.Add(roleTTL + time.Minute)
	_ = stale
	now = now.Add(roleTTL + time.Minute)
	// ...but a turn refreshes it first, on another goroutine.
	b.noteRole("s-1", "admin")

	// The later read must still see it, and the pruner must not have taken
	// the refreshed record with it.
	now = now.Add(time.Second)
	if got := b.ActorFor("s-1"); got != "admin" {
		t.Errorf("actor = %q, want admin: a refreshed record was pruned by a read that had already decided it was stale", got)
	}
}

func TestTheRoleTableStaysBoundedOnALongLivedNode(t *testing.T) {
	// A node runs for months and serves an unbounded number of
	// conversations. Without pruning the table is a slow leak, and a slow
	// leak in the process that answers "who is asking" is not a good place
	// to have one.
	now := time.Now()
	b := newRoleBridge(t, func() time.Time { return now })
	for i := 0; i < 500; i++ {
		b.noteRole("s-"+strconv.Itoa(i), "viewer")
	}
	// A node serves conversations for months. Once the first batch is older
	// than anything a conversation can plausibly still be, the next turn
	// must sweep it.
	now = now.Add(roleTTL + time.Minute)
	for i := 0; i < 500; i++ {
		b.noteRole("s-live-"+strconv.Itoa(i), "viewer")
	}

	b.roleMu.RLock()
	held := len(b.roles)
	b.roleMu.RUnlock()
	if held != 500 {
		t.Errorf("%d role records held, want 500: the stale half should have been swept by the next turn", held)
	}
	if got := b.ActorFor("s-live-499"); got != "viewer" {
		t.Errorf("the newest live conversation lost its role: %q", got)
	}
}

func TestAnUnrecognisedNameBorrowsTheTurnInFlight(t *testing.T) {
	// The agent knows its own session id, and it is not the console's. If
	// the node only answered to names it issued, every gated call on every
	// node would resolve to nobody and every mutating call would be
	// refused — an approval queue that can never be used.
	//
	// The fallback is safe precisely because the agent on a node is one
	// process serving one turn: the only role it can borrow by inventing a
	// name is the role of the turn it is itself running.
	now := time.Now()
	b := newRoleBridge(t, func() time.Time { return now })
	b.noteRole("s-console", "admin")

	if got := b.ActorFor("pig-internal-9f3a"); got != "admin" {
		t.Errorf("actor = %q, want the in-flight turn's role", got)
	}
}

func TestAnIdleAgentGrantsNothingToAnUnknownName(t *testing.T) {
	// The fallback is scoped to a turn in flight. Once the turn is over
	// there is nothing to borrow, and a stale name must get nothing — or
	// the last role an operator ever used on this node would become the
	// node's standing privilege.
	now := time.Now()
	b := newRoleBridge(t, func() time.Time { return now })
	b.noteRole("s-console", "admin")
	b.endTurn("s-console")

	if got := b.ActorFor("pig-internal-9f3a"); got != "" {
		t.Errorf("actor = %q from an idle agent, want empty", got)
	}
	// The record itself survives, because a tool batch already in flight
	// can still reach the gate after the terminal frame.
	if got := b.ActorFor("s-console"); got != "admin" {
		t.Errorf("actor = %q for the conversation itself, want it to outlive the turn", got)
	}
}

func TestAnUnrecognisedNameResolvesToNobodyWhenNoTurnIsRunning(t *testing.T) {
	now := time.Now()
	b := newRoleBridge(t, func() time.Time { return now })
	if got := b.ActorFor("pig-internal-9f3a"); got != "" {
		t.Errorf("actor = %q before any turn, want empty", got)
	}
}

func TestAGateCallNamingTheRealSessionIsAnsweredByItsOwnRecord(t *testing.T) {
	// The fallback must not shadow an exact answer: two conversations on
	// one node have to be judged by their own roles, or a viewer's turn
	// would be approved at the operator standing on the other rung.
	now := time.Now()
	b := newRoleBridge(t, func() time.Time { return now })
	b.noteRole("s-viewer", "viewer")
	b.noteRole("s-admin", "admin")

	if got := b.ActorFor("s-viewer"); got != "viewer" {
		t.Errorf("actor = %q, want viewer: the newest turn must not override an older conversation's own role", got)
	}
}

// --- StartEvents must not block its caller -------------------------------

// liveSource is a source whose agent process is real enough to subscribe
// to, so the relay path is exercised rather than skipped.
type liveSource struct {
	proc  *liveProcess
	mu    sync.Mutex
	subs  []func(ports.ProcessEvent)
	ready chan struct{}
}

func (s *liveSource) Process() (ports.AgentProcess, error) { return s.proc, nil }
func (s *liveSource) Health() ports.ProcessHealth          { return ports.ProcessHealth{Running: true} }

func (s *liveSource) subscribe(fn func(ports.ProcessEvent)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

func (s *liveSource) emit(ev ports.ProcessEvent) {
	s.mu.Lock()
	var subs []func(ports.ProcessEvent)
	subs = append(subs, s.subs...)
	s.mu.Unlock()
	for _, fn := range subs {
		fn(ev)
	}
}

// liveProcess is the minimum AgentProcess the relay needs.
type liveProcess struct {
	src *liveSource
}

func (p *liveProcess) Start(context.Context) error                    { return nil }
func (p *liveProcess) Stop() error                                    { return nil }
func (p *liveProcess) Running() bool                                  { return true }
func (p *liveProcess) Exited() <-chan struct{}                        { return make(chan struct{}) }
func (p *liveProcess) LastError() error                               { return nil }
func (p *liveProcess) Prompt(context.Context, string) error           { return nil }
func (p *liveProcess) Steer(context.Context, string) error            { return nil }
func (p *liveProcess) Abort(context.Context) error                    { return nil }
func (p *liveProcess) SetModel(context.Context, string, string) error { return nil }
func (p *liveProcess) State(context.Context) (*ports.ProcessState, error) {
	return &ports.ProcessState{}, nil
}
func (p *liveProcess) OnEvent(fn func(ports.ProcessEvent)) func() {
	p.src.subscribe(fn)
	return func() {}
}

// capturingTunnel records what the bridge pushes.
type capturingTunnel struct {
	mu    sync.Mutex
	frame tunnel.AgentEventFrame
	got   chan struct{}
}

func newCapturingTunnel() *capturingTunnel { return &capturingTunnel{got: make(chan struct{}, 8)} }

func (c *capturingTunnel) Call(_ context.Context, method string, req, _ any) error {
	if method != tunnel.MethodAgentEvent {
		return nil
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var f tunnel.AgentEventFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	c.mu.Lock()
	c.frame = f
	c.mu.Unlock()
	select {
	case c.got <- struct{}{}:
	default:
	}
	return nil
}

// TestStartEventsDoesNotBlockItsCaller is a regression test for a wedge
// that shipped: Agent.Run calls StartEvents from the middle of its
// startup sequence, believing it returns immediately. A version that
// blocked there stranded the changewatcher, the upgrade sentinel, eg.Wait
// and therefore graceful shutdown — while the node kept heartbeating and
// looked perfectly healthy to an operator.
func TestStartEventsDoesNotBlockItsCaller(t *testing.T) {
	src := &liveSource{ready: make(chan struct{}, 1)}
	src.proc = &liveProcess{src: src}
	tunnelStub := newCapturingTunnel()
	b, err := NewAgentBridge(AgentBridgeOptions{Source: src, Client: tunnelStub, EdgeID: 7})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := make(chan struct{})
	go func() {
		b.StartEvents(ctx)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("StartEvents blocked its caller; everything sequenced after it in Agent.Run is unreachable")
	}

	// And the relay must still work — a non-blocking StartEvents that
	// quietly stopped relaying would be the same outage with a nicer
	// shape.
	select {
	case <-src.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("StartEvents returned without subscribing to the process")
	}
	// A turn has to belong to a conversation before its records can be
	// routed; the node learns which one from the prompt, not from the agent.
	b.noteRole("conv-1", "")
	src.emit(ports.ProcessEvent{
		Type: "turn_start", SessionID: "s-1", Seq: 1,
		Payload: []byte(`{}`),
	})
	select {
	case <-tunnelStub.got:
	case <-time.After(2 * time.Second):
		t.Fatal("no event reached the tunnel after StartEvents returned")
	}
}
