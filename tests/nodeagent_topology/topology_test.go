// Package e2e runs the plan's three operational scenarios against the
// node-agent topology of Phase C.
//
// What is real here and what is not, stated plainly because a green test
// that overstates its own coverage is worse than no test:
//
// Real: the control plane's Fleet, the tunnel wire types, the node's
// AgentBridge, the node's policy gate, the PiG event translator the node
// uses in production, and the console's frame contract. Every frame a
// scenario asserts on was produced by translating a PiG event and pushing
// it across the tunnel, not by a test writing a frame directly.
//
// Substituted: the transport (an in-process loopback rather than geminio)
// and the agent process (a scripted stand-in for `pig --mode rpc`).
// Everything above the stdio JSONL boundary is production code.
//
// The scenarios are the same three the teamharness eval runs
// (alert_storm, rca_loop, recovery_verify), re-expressed so they exercise
// the topology rather than the plugin's own dispatch table. A scenario
// that passed only against the legacy in-process agent would not have
// caught a broken tunnel frame, a gate whose approval frame never reached
// the console, or a decision that could not travel back down to the node.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigwire"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"

	"github.com/vincent-wuhan/opskeeper/core/domains/biz/nodefleet"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/edge/biz"
)

// ── loopback transport ────────────────────────────────────────────────

// loopback is a tunnel whose two ends live in one process. Manager calls
// land in the node's handler registry; frames the node pushes come back
// through inbound, which is the manager's real DeliverInbound.
//
// The seam it replaces is the geminio session. Everything above it —
// method routing, JSON bodies, the frame envelope, the edge's decision
// about which frames are translatable — is the code that runs in
// production.
type loopback struct {
	mu       sync.Mutex
	handlers map[string]tunnel.Handler
	inbound  func(tunnel.AgentEventFrame)
	calls    []string
}

func newLoopback() *loopback {
	return &loopback{handlers: map[string]tunnel.Handler{}}
}

func (l *loopback) register(method string, h tunnel.Handler) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.handlers[method] = h
}

// dispatch is manager → node.
func (l *loopback) dispatch(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error) {
	l.mu.Lock()
	l.calls = append(l.calls, method)
	h, ok := l.handlers[method]
	l.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("loopback: no handler for %q on edge %d", method, edgeID)
	}
	return h(ctx, tunnel.Session{}, method, body)
}

// methodsCalled reports the manager→node calls that crossed the tunnel, in
// order. Scenarios assert on it so a flow that "passed" without ever
// leaving the control plane is caught.
func (l *loopback) methodsCalled() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func (l *loopback) countMethod(method string) int {
	n := 0
	for _, m := range l.methodsCalled() {
		if m == method {
			n++
		}
	}
	return n
}

// push is node → manager, for the one method that travels that way.
func (l *loopback) push(frame tunnel.AgentEventFrame) {
	l.mu.Lock()
	inbound := l.inbound
	l.mu.Unlock()
	if inbound != nil {
		inbound(frame)
	}
}

// managerDial is the control plane's Dialer.
type managerDial struct{ lb *loopback }

func (d *managerDial) Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error) {
	return d.lb.dispatch(ctx, edgeID, method, body)
}

// edgeClient is the node's tunnel.Client. Only Call is exercised beyond
// handler registration; the rest exist because the interface demands them
// and a node in these scenarios neither dials nor streams.
type edgeClient struct {
	lb     *loopback
	spy    *auditSpy
	edgeID uint64
}

func (c *edgeClient) Dial(context.Context) error { return nil }

func (c *edgeClient) RegisterHandler(method string, h tunnel.Handler) { c.lb.register(method, h) }

// Call is node → manager. Every method the bridge pushes goes out this
// way; anything else is a bug in the bridge, and saying so beats
// silently dropping it.
func (c *edgeClient) Call(ctx context.Context, method string, req, resp any) error {
	if method != tunnel.MethodAgentEvent {
		return fmt.Errorf("edgeClient: unexpected node→manager call %q", method)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var frame tunnel.AgentEventFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return err
	}
	c.lb.push(frame)
	if resp == nil {
		return nil
	}
	out, err := json.Marshal(map[string]any{"delivered": true})
	if err != nil {
		return err
	}
	return json.Unmarshal(out, resp)
}

func (c *edgeClient) AcceptStream() (tunnel.StreamConn, error) {
	return nil, errors.New("edgeClient: streams are not part of these scenarios")
}
func (c *edgeClient) OnReconnect(func()) {}
func (c *edgeClient) Close() error       { return nil }

var _ tunnel.Client = (*edgeClient)(nil)
var _ nodefleet.Dialer = (*managerDial)(nil)

// ── the node's agent process ───────────────────────────────────────────

// step is one thing the scripted agent does in a turn.
type step struct {
	// say emits assistant text.
	say string
	// tool, when set, is a call the agent proposes. It is adjudicated by
	// the node's real gate before the agent is allowed to run it.
	tool *policygate.Call
	// result is what the tool returns if the gate lets it run.
	result string
}

// scriptedAgent stands in for `pig --mode rpc`.
//
// It is scripted rather than model-driven so a scenario asserts on a
// specific sequence of tool calls. What is NOT scripted is the part the
// scenarios exist to test: the agent proposes a call, the node's real
// gate adjudicates it, and a human decision arrives over the tunnel from
// the control plane before the tool is allowed to run.
type scriptedAgent struct {
	mu     sync.Mutex
	subs   map[int]func(ports.ProcessEvent)
	nextID int
	seq    int64

	turns map[string][]step
	// runs counts completed turns per session.
	runs map[string]int
	// model is what SetModel pinned.
	model string
	// aborted ends the current turn early.
	aborted map[string]bool
	// gate is the node's real policy gate.
	gate *policygate.Gate
	// policy is the same policy the gate judges with. The agent needs it
	// too, because whether a call carries an approval receipt is a
	// question about the call's class, and the gate does not export the
	// answer.
	policy policygate.Policy
	edgeID uint64
	// subscribed closes once the bridge has attached to this process's
	// event stream. StartEvents is asynchronous by contract, so a turn
	// that begins in the same instant would otherwise emit its first
	// frames to nobody — a race the scenarios must not inherit.
	subscribed chan struct{}
}

func newScriptedAgent(edgeID uint64, gate *policygate.Gate, pol policygate.Policy) *scriptedAgent {
	return &scriptedAgent{
		subscribed: make(chan struct{}),
		subs:       map[int]func(ports.ProcessEvent){},
		turns:      map[string][]step{},
		runs:       map[string]int{},
		aborted:    map[string]bool{},
		gate:       gate,
		policy:     pol,
		edgeID:     edgeID,
	}
}

func (a *scriptedAgent) script(sessionID string, steps []step) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turns[sessionID] = steps
}

func (a *scriptedAgent) Start(context.Context) error { return nil }
func (a *scriptedAgent) Stop() error                 { return nil }
func (a *scriptedAgent) Running() bool               { return true }
func (a *scriptedAgent) Exited() <-chan struct{}     { ch := make(chan struct{}); return ch }
func (a *scriptedAgent) LastError() error            { return nil }

func (a *scriptedAgent) SetModel(_ context.Context, provider, model string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = provider + "/" + model
	return nil
}

func (a *scriptedAgent) State(context.Context) (*ports.ProcessState, error) {
	return &ports.ProcessState{Running: false, Version: "scripted"}, nil
}

func (a *scriptedAgent) OnEvent(fn func(ports.ProcessEvent)) func() {
	a.mu.Lock()
	id := a.nextID
	a.nextID++
	a.subs[id] = fn
	a.mu.Unlock()
	select {
	case <-a.subscribed:
	default:
		close(a.subscribed)
	}
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.subs, id)
	}
}

// emit translates the event on the node and pushes what the console can
// render — exactly the path a real PiG event takes.
func (a *scriptedAgent) emit(evType, sessionID string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	a.mu.Lock()
	a.seq++
	ev := ports.ProcessEvent{
		Type:      evType,
		SessionID: sessionID,
		Seq:       a.seq,
		Payload:   raw,
		// The last event of a turn is terminal. A real agent marks it, and
		// the console uses it to close the bubble: a conversation that
		// never reports one looks like an agent that stopped mid-sentence.
		Terminal: evType == "agent_end",
	}
	subs := make([]func(ports.ProcessEvent), 0, len(a.subs))
	for _, fn := range a.subs {
		subs = append(subs, fn)
	}
	a.mu.Unlock()

	// Only the subscribers are notified. Translating the event and
	// pushing it at the manager is the bridge's job — that is the
	// production path — and doing it here as well would deliver every
	// frame twice, which is exactly the kind of defect a scenario that
	// only checks "some frames arrived" would sail past.
	for _, fn := range subs {
		fn(ev)
	}
}

var _ ports.AgentProcess = (*scriptedAgent)(nil)

// Prompt accepts the turn and runs it on its own goroutine, the way a real
// agent process does. Returning before the turn finishes is the whole
// reason the tunnel has an event stream at all.
//
// The RPC context is deliberately not the turn's context. The bridge
// cancels it the moment the prompt command returns — that is how a node
// bounds a call against a local subprocess — but a turn lives inside the
// agent process and outlives the command that started it. pigrpc.Client
// behaves the same way: Prompt sends the prompt down the pipe and ignores
// ctx entirely. A fake that bound its turn to ctx would cancel every
// gated tool call the instant the prompt was acknowledged, which is not
// a bug in the topology but a bug in the double — and it would make
// every approval scenario here fail for the wrong reason.
func (a *scriptedAgent) Prompt(_ context.Context, text string) error {
	a.mu.Lock()
	steps, ok := a.turns[text]
	a.aborted[text] = false
	a.mu.Unlock()
	if !ok {
		// An unscripted turn still runs: the agent answers and stops. A
		// scenario that forgot to script a turn then fails on its
		// assertions rather than hanging on a terminal frame that never
		// comes.
		steps = []step{{say: "unscripted turn"}}
	}
	go a.run(context.Background(), text, steps)
	return nil
}

func (a *scriptedAgent) run(ctx context.Context, sessionID string, steps []step) {
	a.emit("turn_start", sessionID, map[string]any{})
	for i, st := range steps {
		if a.isAborted(sessionID) {
			break
		}
		if st.say != "" {
			a.emit("message_update", sessionID, map[string]any{
				"assistantMessageEvent": map[string]any{
					"type": "text_delta", "delta": st.say, "contentIndex": 0,
				},
			})
			a.emit("message_end", sessionID, map[string]any{
				"message": map[string]any{
					"content": []map[string]any{{"type": "text", "text": st.say}},
					"model":   "scripted",
				},
			})
			continue
		}
		if st.tool == nil {
			continue
		}
		a.runTool(ctx, sessionID, i, st)
	}
	a.emit("agent_end", sessionID, map[string]any{"isError": false})
	a.mu.Lock()
	a.runs[sessionID]++
	a.mu.Unlock()
}

// runTool proposes one tool call to the node's gate and runs it only if
// the gate says so.
//
// The gate blocks while a human decides, which is what makes the HITL
// loop real: the decision that unblocks this call arrives from the
// control plane over agent.decide, not from anything in this process.
func (a *scriptedAgent) runTool(ctx context.Context, sessionID string, idx int, st step) {
	call := *st.tool
	call.SessionID = sessionID
	callID := fmt.Sprintf("call-%s-%d", sessionID, idx)

	a.emit("tool_execution_start", sessionID, map[string]any{
		"toolCallId": callID, "toolName": call.ToolName, "args": call.Arguments,
	})

	outcome, reason, err := a.gate.Admit(ctx, call)
	if err != nil || outcome != policygate.Allowed {
		// The tool does not run, and the console is told why. A scenario
		// asserting on the absence of a result is asserting that the
		// refusal was visible, not merely that it happened.
		a.emit("tool_execution_end", sessionID, map[string]any{
			"toolCallId": callID, "toolName": call.ToolName, "isError": true,
			"result": map[string]any{"content": []map[string]any{
				{"type": "text", "text": "refused: " + reason},
			}},
		})
		return
	}
	// Host code re-checks the grant at the point of execution. A package
	// that swapped the tool out from under the approval must not get a run
	// it was never shown.
	//
	// Only a call that needed a human has a receipt to claim. The gate
	// mints none for a read — there is nobody to prove agreed — so asking
	// for one unconditionally would refuse every read on the node. This
	// mirrors the production authoriser in cmd/opskeeper-edge/policy.go,
	// which asks the same question in the same order.
	if a.policy.NeedsApproval(call) && !a.gate.ClaimReceipt(call) {
		a.emit("tool_execution_end", sessionID, map[string]any{
			"toolCallId": callID, "toolName": call.ToolName, "isError": true,
			"result": map[string]any{"content": []map[string]any{
				{"type": "text", "text": "refused: no approval receipt"},
			}},
		})
		return
	}
	a.emit("tool_execution_end", sessionID, map[string]any{
		"toolCallId": callID, "toolName": call.ToolName, "isError": false,
		"result": map[string]any{"content": []map[string]any{
			{"type": "text", "text": st.result},
		}},
	})
}

func (a *scriptedAgent) isAborted(sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.aborted[sessionID]
}

func (a *scriptedAgent) Steer(context.Context, string) error { return nil }

func (a *scriptedAgent) Abort(_ context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.aborted {
		a.aborted[k] = true
	}
	return nil
}

// ── the topology ──────────────────────────────────────────────────────

// policy is the node's effective policy: reads run, writes need a human,
// and a call assessed worse than its declared class is refused outright.
type policy struct{ declared map[string]domain.ToolClass }

func (p policy) Permitted(c policygate.Call) (bool, string) {
	want, ok := p.declared[c.ToolName]
	if !ok {
		return false, "tool is not installed on this node"
	}
	// The host's independent assessment may only raise the class. A call
	// that turns out to be worse than the manifest declared is refused
	// rather than quietly reclassified: reclassifying would run it under
	// an approval the operator was shown for something else.
	if c.Class != "" && c.Class.Rank() > want.Rank() {
		return false, "call assessed more dangerous than the tool's declared class"
	}
	return true, ""
}

func (p policy) NeedsApproval(c policygate.Call) bool {
	cls, ok := p.declared[c.ToolName]
	if !ok {
		cls = c.Class
	}
	return cls.Rank() >= domain.ClassWrite.Rank()
}

func (p policy) EffectiveClass(c policygate.Call) domain.ToolClass {
	cls, ok := p.declared[c.ToolName]
	if !ok {
		cls = c.Class
	}
	if c.Class != "" && c.Class.Rank() > cls.Rank() {
		return c.Class
	}
	return cls
}

func (p policy) MaxRadius(policygate.Call) domain.BlastRadius { return domain.RadiusCluster }

// topology is one node plus the control plane that talks to it.
type topology struct {
	fleet   *nodefleet.Fleet
	bridge  *edgebiz.AgentBridge
	gate    *policygate.Gate
	agent   *scriptedAgent
	lb      *loopback
	spy     *auditSpy
	edgeID  uint64
	console *consoleSink
}

// topologyConfig is what a scenario varies about the wiring. The default
// wiring is production's; the knobs exist so a scenario can reach a
// branch of production code that production reaches on a clock.
type topologyConfig struct {
	// approvalTTL bounds how long a gated call waits for a human.
	approvalTTL time.Duration
}

type topologyOpt func(*topologyConfig)

// withApprovalTTL shortens the gate's wait for a human.
//
// Production uses policygate.DefaultApprovalTTL — a quarter of an hour,
// because a woken on-call operator needs time to reach a keyboard. No
// test can wait out a quarter of an hour, and a scenario whose point is
// that an unanswered request fails closed would rather see the deadline
// arrive in milliseconds than assert the default. The branch exercised is
// the same one; only the clock differs.
func withApprovalTTL(d time.Duration) topologyOpt {
	return func(c *topologyConfig) { c.approvalTTL = d }
}

// newTopology wires a node and a control plane together.
//
// toolClasses is the node's installed tool set as the manifest declared
// it — the gate's input, and the thing a scenario varies to change what
// needs a human.
func newTopology(t *testing.T, edgeID uint64, toolClasses map[string]domain.ToolClass, opts ...topologyOpt) *topology {
	t.Helper()

	var cfg topologyConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	lb := newLoopback()
	translators := pigwire.NewSet(nil, 0)

	// Approval frames go to the console the same way turn output does:
	// through the node's push path, so a control plane asking a human for
	// a decision and a node reporting tool output travel identically.
	console := newConsoleSink()
	spy := &auditSpy{}

	// The gate and the bridge refer to each other: the gate is the
	// bridge's decider, and the bridge is how an approval frame reaches
	// the console. The sink is therefore filled in once both exist.
	// Nothing can arrive before then — no conversation is open yet.
	var approvalSink policygate.FrameSink
	nodePolicy := policy{declared: toolClasses}
	gateOpts := policygate.Options{
		Policy: nodePolicy,
		Audit:  spy,
		// EmitApproval is the production path for a gated call reaching
		// the console, including its pending/resolved split. Pushing the
		// frame straight at the tunnel here would have exercised a
		// transport the node does not use in production.
		Emit: func(sessionID string, f wire.ApprovalFrame) {
			if approvalSink != nil {
				approvalSink(sessionID, f)
			}
		},
		Now: time.Now,
	}
	if cfg.approvalTTL > 0 {
		gateOpts.TTL = cfg.approvalTTL
	}
	gate, err := policygate.New(gateOpts)
	if err != nil {
		t.Fatalf("policygate.New: %v", err)
	}

	agent := newScriptedAgent(edgeID, gate, nodePolicy)
	client := &edgeClient{lb: lb, edgeID: edgeID}

	source := &agentSource{agent: agent, health: ports.ProcessHealth{Running: true, Version: "scripted"}}
	bridge, err := edgebiz.NewAgentBridge(edgebiz.AgentBridgeOptions{
		Source:    source,
		Decider:   gate,
		Client:    client,
		EdgeID:    edgeID,
		Translate: translators.Translate,
	})
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	bridge.Register(client)
	approvalSink = bridge.EmitApproval

	fleet, err := nodefleet.New(nodefleet.Options{Dial: &managerDial{lb: lb}})
	if err != nil {
		t.Fatalf("nodefleet.New: %v", err)
	}
	t.Cleanup(fleet.CloseAll)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bridge.StartEvents(ctx)
	select {
	case <-agent.subscribed:
	case <-time.After(2 * time.Second):
		t.Fatal("the node's agent bridge never subscribed to the agent's event stream")
	}

	lb.inbound = func(f tunnel.AgentEventFrame) {
		// DeliverInbound reports whether a conversation was open to
		// receive the frame. A false here means the frame arrived for a
		// conversation the control plane had already closed, which in
		// production is a dropped frame and in these scenarios is a
		// scenario bug — the topology wires inbound only after Open.
		fleet.DeliverInbound(f)
	}

	return &topology{
		fleet: fleet, bridge: bridge, gate: gate, agent: agent,
		lb: lb, spy: spy, edgeID: edgeID, console: console,
	}
}

// agentSource adapts the scripted agent to what the bridge needs from a
// supervisor.
type agentSource struct {
	agent  *scriptedAgent
	health ports.ProcessHealth
}

func (s *agentSource) Process() (ports.AgentProcess, error) { return s.agent, nil }
func (s *agentSource) Health() ports.ProcessHealth          { return s.health }

// auditSpy records what the gate decided, so a scenario can assert that a
// refusal left a trace even when the console frames were dropped.
type auditSpy struct {
	mu      sync.Mutex
	entries []ports.AuditEntry
}

func (a *auditSpy) Record(_ context.Context, e ports.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}
func (a *auditSpy) Verify(context.Context) error { return nil }

func (a *auditSpy) actions() []ports.AuditAction {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ports.AuditAction, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, e.Action)
	}
	return out
}

func (a *auditSpy) saw(action ports.AuditAction) bool {
	for _, got := range a.actions() {
		if got == action {
			return true
		}
	}
	return false
}

var _ ports.AuditSink = (*auditSpy)(nil)

// consoleSink is the console's SSE stream: the frames that actually
// arrived, in order.
type consoleSink struct {
	mu     sync.Mutex
	frames []wire.StreamEvent
	// delivered fires on every frame so a test can wait for the turn to
	// end instead of sleeping.
	delivered chan struct{}
}

func newConsoleSink() *consoleSink { return &consoleSink{delivered: make(chan struct{}, 64)} }

func (c *consoleSink) Emit(_ context.Context, ev wire.StreamEvent) error {
	c.mu.Lock()
	c.frames = append(c.frames, ev)
	c.mu.Unlock()
	select {
	case c.delivered <- struct{}{}:
	default:
	}
	return nil
}

func (c *consoleSink) snapshot() []wire.StreamEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]wire.StreamEvent(nil), c.frames...)
}

func (c *consoleSink) types() []wire.StreamEventType {
	out := []wire.StreamEventType{}
	for _, f := range c.snapshot() {
		out = append(out, f.Type)
	}
	return out
}

func (c *consoleSink) count(t wire.StreamEventType) int {
	n := 0
	for _, got := range c.types() {
		if got == t {
			n++
		}
	}
	return n
}

// toolResults returns the text of every tool_end frame, which is how a
// scenario tells "the tool ran" from "the tool was refused".
func (c *consoleSink) toolResults() []string {
	out := []string{}
	for _, f := range c.snapshot() {
		if f.Type != wire.StreamToolEnd || f.Tool == nil {
			continue
		}
		out = append(out, f.Tool.ResultJSON)
	}
	return out
}

// assistantText is everything the agent said, joined. Scenarios that care
// about the closing summary of a turn read it from here rather than from
// a frame they kept, so a summary the translator dropped fails the test
// instead of passing against a value the test wrote itself.
func (c *consoleSink) assistantText() string {
	var out strings.Builder
	for _, f := range c.snapshot() {
		if f.Type == wire.StreamAssistantEnd && f.Assistant != nil {
			out.WriteString(f.Assistant.Content)
		}
	}
	return out.String()
}

// pendingApproval is the request the console was actually shown, as it
// arrived. Approval-resolved frames deliberately carry only the decision
// and the cause — the console already has the rest of the request, and a
// second copy of it is a second thing that can drift — so anything an
// operator judged has to be read off the pending frame.
func (n *node) pendingApproval(t *testing.T) wire.ApprovalFrame {
	t.Helper()
	for _, f := range n.top.console.snapshot() {
		if f.Type == wire.StreamApprovalPending && f.Approval != nil {
			return *f.Approval
		}
	}
	t.Fatal("no approval request reached the console")
	return wire.ApprovalFrame{}
}

func (c *consoleSink) toolNames() []string {
	out := []string{}
	for _, f := range c.snapshot() {
		if f.Type == wire.StreamToolStart && f.Tool != nil {
			out = append(out, f.Tool.Name)
		}
	}
	return out
}

func (c *consoleSink) waitFor(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if pred() {
			return
		}
		select {
		case <-c.delivered:
		case <-deadline:
			t.Fatalf("timed out waiting for %s; console saw %v", what, c.types())
		}
	}
}

var _ ports.EventSink = (*consoleSink)(nil)
