// Package nodefleet routes operator actions to the right node's agent.
//
// A control plane that talked to a node's agent directly would need to know
// that the agent is a subprocess speaking PiG's RPC dialect. It does not,
// and must not: the tunnel already carries a frame the manager and the
// console both understand. So the manager holds a ports.AgentProcess per
// edge — the same interface the node's own supervisor implements — and the
// fleet's routing, session bookkeeping and fan-out are written once against
// that contract rather than twice against two transports.
package nodefleet

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Dialer opens a command channel to one node.
//
// It is the slice of the manager's frontierbound client the fleet needs.
// Declaring it here rather than accepting the concrete client keeps the test
// double to one method and keeps this package from depending on the broker.
type Dialer interface {
	Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error)
}

// Refusal codes the node returns in a well-formed reply body.
//
// The node's codes are defined in core/edge/biz; they are repeated
// here rather than imported because the edge and the control plane will
// eventually be separate binaries, and a shared string constant is a weaker
// contract than a compiled one. A code the manager does not recognise is
// still surfaced verbatim — an unknown reason beats a wrong one.
const (
	CodeAgentUnavailable = "agent_unavailable"
	CodeAgentDegraded    = "agent_degraded"
	CodeAgentNoSession   = "agent_no_session"
	CodeAgentBadRequest  = "agent_bad_request"
	CodeAgentInternal    = "agent_internal"
)

// RemoteError is a refusal the node returned in a well-formed reply.
//
// It is deliberately distinguishable from a transport failure. A refusal is
// the node answering — "this node runs no agent", "its agent is
// crash-looping" — while a transport failure is the two not talking. The
// console says something different for each, and an operator debugging a
// fleet needs the difference: the first is a configuration or node problem,
// the second is a network one.
// RemoteError is a node declining a call. The declaration moved to core/domain
// (decision 282) with the rest of the fleet's port vocabulary: the console's
// HTTP layer branches on a refusal to answer 409 rather than 500, and a reader
// of that handler should not have to know which package holds the router.
// Alias, not copy — one declaration, so IsRefusal and AsRefusal below recover
// the same type the node produced.
type RemoteError = domain.AgentRefusal

// IsRefusal reports whether err is a node's deliberate refusal rather than a
// failure to reach it.
func IsRefusal(err error) bool { return domain.IsAgentRefusal(err) }

// AsRefusal extracts a node's refusal from err, if there is one.
func AsRefusal(err error) (*RemoteError, bool) { return domain.AsAgentRefusal(err) }

// TunelledProcess is a node's agent, reached over the tunnel.
//
// It implements the same port the node's own supervisor does, so the fleet
// cannot tell — and must not try to tell — whether it is talking to a local
// process or to one four network hops away.
type TunelledProcess struct {
	edgeID uint64
	dial   Dialer

	mu        sync.Mutex
	subs      map[int]func(tunnel.AgentEventFrame)
	nextSub   int
	lastState *ports.ProcessState
	lastErr   error
	// sessionID scopes the commands this handle sends. The fleet mints one
	// per conversation so two consoles watching one node do not interleave
	// their turns into a single agent session.
	sessionID string
}

var _ ports.AgentProcess = (*TunelledProcess)(nil)

// NewTunelledProcess returns a handle to one node's agent.
func NewTunelledProcess(edgeID uint64, sessionID string, dial Dialer) *TunelledProcess {
	return &TunelledProcess{
		edgeID:    edgeID,
		dial:      dial,
		sessionID: sessionID,
		subs:      make(map[int]func(tunnel.AgentEventFrame)),
	}
}

// EdgeID reports which node this handle addresses.
func (p *TunelledProcess) EdgeID() uint64 { return p.edgeID }

// SessionID reports the conversation this handle's commands belong to.
func (p *TunelledProcess) SessionID() string { return p.sessionID }

// Start is a no-op on the manager side.
//
// There is no process to spawn: the node owns the agent's lifecycle, and the
// supervisor there is the only thing entitled to start, stop or restart it.
// The port requires the method because the node's own implementation needs
// it, and a control plane that appeared able to start a node's agent would
// eventually be asked to.
func (p *TunelledProcess) Start(context.Context) error { return nil }

// Stop is a no-op for the same reason. The manager can abort a turn; it
// cannot stop a node's agent. A control plane that could would hold a
// remote-kill button on every host in the fleet, keyed off a single RPC.
func (p *TunelledProcess) Stop() error { return nil }

// Running reports the last known answer from the node.
func (p *TunelledProcess) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastState != nil && p.lastState.Running
}

// Exited reports a node's agent as never-endingly present.
//
// The node restarts its own agent under its own supervisor. The fleet
// restarts nothing: a manager-side respawn loop would race the node's and
// turn a crash loop into a fork bomb with two parents. A never-closed
// channel is the honest encoding of "the manager is not in a position to
// observe this process ending".
func (p *TunelledProcess) Exited() <-chan struct{} {
	never := make(chan struct{})
	return never
}

// LastError reports the most recent failure reaching this node's agent.
func (p *TunelledProcess) LastError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

// Prompt sends a turn to the node's agent.
func (p *TunelledProcess) Prompt(ctx context.Context, text string) error {
	_, err := p.call(ctx, tunnel.MethodAgentPrompt, tunnel.AgentPromptRequest{
		SessionID: p.sessionID,
		Text:      text,
	}, decodePromptReply)
	return err
}

// Steer injects a message into the turn already running.
func (p *TunelledProcess) Steer(ctx context.Context, text string) error {
	_, err := p.call(ctx, tunnel.MethodAgentSteer, tunnel.AgentSteerRequest{
		SessionID: p.sessionID,
		Text:      text,
	}, decodeSteerReply)
	return err
}

// Abort stops the node's current turn.
func (p *TunelledProcess) Abort(ctx context.Context) error {
	_, err := p.call(ctx, tunnel.MethodAgentAbort, tunnel.AgentAbortRequest{
		SessionID: p.sessionID,
	}, decodeAbortReply)
	return err
}

// SetModel pins the node's agent to a provider and model.
func (p *TunelledProcess) SetModel(ctx context.Context, provider, model string) error {
	_, err := p.call(ctx, tunnel.MethodAgentSetModel, tunnel.AgentSetModelRequest{
		SessionID: p.sessionID,
		Provider:  provider,
		Model:     model,
	}, decodeSetModelReply)
	return err
}

// replyDecoder turns a well-formed node reply into either nothing or the
// refusal it carries. Every command supplies one, because a node reports a
// refusal in the body rather than as a transport error — the console has to
// be able to tell "this node has no agent" from "the tunnel dropped", and a
// handler that returned a Go error would collapse the two.
type replyDecoder func(raw []byte) (code, message string, restarts int, degraded bool, refused bool, err error)

// call runs one request/response RPC and folds a refusal in the reply into
// an error.
func (p *TunelledProcess) call(ctx context.Context, method string, req any, decode replyDecoder) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("nodefleet: encode %s: %w", method, err)
	}
	raw, err := p.dial.Call(ctx, p.edgeID, method, body)
	if err != nil {
		// A transport failure is the retryable case, and the one the
		// operator needs distinguished from a refusal.
		wrapped := fmt.Errorf("nodefleet: %s on edge %d: %w", method, p.edgeID, err)
		p.recordFailure(wrapped)
		return nil, wrapped
	}
	code, message, restarts, degraded, refused, err := decode(raw)
	if err != nil {
		wrapped := fmt.Errorf("nodefleet: decode %s reply from edge %d: %w", method, p.edgeID, err)
		p.recordFailure(wrapped)
		return nil, wrapped
	}
	if refused {
		refusal := &RemoteError{Code: code, Message: message, EdgeID: p.edgeID, Restarts: restarts, Degraded: degraded}
		p.recordFailure(refusal)
		return raw, refusal
	}
	p.recordSuccess()
	return raw, nil
}

// State asks the node what its agent is doing.
//
// Unlike the other operations this one updates the cached availability, so
// a caller can learn a node has no agent at all from a state poll rather
// than only from a failed command.
func (p *TunelledProcess) State(ctx context.Context) (*ports.ProcessState, error) {
	raw, err := p.dial.Call(ctx, p.edgeID, tunnel.MethodAgentState, nil)
	if err != nil {
		wrapped := fmt.Errorf("nodefleet: agent state on edge %d: %w", p.edgeID, err)
		p.recordFailure(wrapped)
		return nil, wrapped
	}
	var resp tunnel.AgentStateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		wrapped := fmt.Errorf("nodefleet: decode agent state from edge %d: %w", p.edgeID, err)
		p.recordFailure(wrapped)
		return nil, wrapped
	}
	state := &ports.ProcessState{
		SessionID:        resp.SessionID,
		Running:          resp.Running,
		Model:            resp.Model,
		Provider:         resp.Provider,
		Version:          resp.Version,
		PendingToolCalls: resp.PendingTool,
	}
	p.mu.Lock()
	p.lastState = state
	if resp.Available {
		p.lastErr = nil
	} else {
		// A node with no agent is a fact about the node, not a transport
		// fault, so it is not recorded as the last error. Recording it
		// would make a fleet health view report an error for a node that
		// is behaving exactly as configured.
		p.lastErr = nil
	}
	p.mu.Unlock()
	return state, nil
}

// Health asks the node's supervisor how its process is doing.
//
// It is separate from State because they answer different questions: State
// asks the process what it is doing, Health asks the supervisor whether it
// has been crashing. A node whose agent is up and crash-looping is
// "running" by State and "degraded" by Health.
func (p *TunelledProcess) Health(ctx context.Context) (*tunnel.AgentHealthResponse, error) {
	raw, err := p.dial.Call(ctx, p.edgeID, tunnel.MethodAgentHealth, nil)
	if err != nil {
		return nil, fmt.Errorf("nodefleet: agent health on edge %d: %w", p.edgeID, err)
	}
	var resp tunnel.AgentHealthResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("nodefleet: decode agent health from edge %d: %w", p.edgeID, err)
	}
	return &resp, nil
}

// OnFrame subscribes to the frames the node pushes.
//
// The node translates PiG's event vocabulary into the console's before the
// frame crosses the tunnel, so a subscriber here is handed a
// wire.StreamEvent and does no translation of its own. That is the whole
// reason the edge translates rather than the control plane: the console's
// contract stays fixed and neither side has to learn the other's.
//
// OnEvent below is the port's event hook, for callers that want the raw
// record. The fleet uses this one.
func (p *TunelledProcess) OnFrame(fn func(tunnel.AgentEventFrame)) func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := p.nextSub
	p.nextSub++
	p.subs[id] = fn
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.subs, id)
	}
}

// OnEvent implements the port's event hook over the same frames.
//
// The payload is the agent's own record, not a re-encoding of the
// translated frame: a caller that wants the console frame uses OnFrame, and
// one that wants the raw record uses this, and neither pays for the other.
func (p *TunelledProcess) OnEvent(fn func(ports.ProcessEvent)) func() {
	return p.OnFrame(func(frame tunnel.AgentEventFrame) { fn(ProjectEvent(frame)) })
}

// ProjectEvent converts a pushed frame into the port's raw event shape.
func ProjectEvent(frame tunnel.AgentEventFrame) ports.ProcessEvent {
	return ports.ProcessEvent{
		Type:      frame.Type,
		SessionID: frame.SessionID,
		Iteration: frame.Iteration,
		Seq:       frame.Seq,
		Payload:   frame.Payload,
		Terminal:  frame.Terminal,
	}
}

// Deliver fans one pushed frame in to this handle's subscribers.
//
// It is exported because the inbound path is not the caller: a frontierbound
// handler receives the push and hands it here, which keeps the tunnel's
// transport out of the port.
func (p *TunelledProcess) Deliver(frame tunnel.AgentEventFrame) {
	p.mu.Lock()
	subs := make([]func(tunnel.AgentEventFrame), 0, len(p.subs))
	for _, sub := range p.subs {
		subs = append(subs, sub)
	}
	p.mu.Unlock()
	for _, sub := range subs {
		sub(frame)
	}
}

// SubscriberCount reports how many live subscriptions this handle has.
//
// A frame that reaches zero subscribers is a conversation nobody is
// watching any more, which is how the fleet knows to stop routing to a node
// rather than accumulating handles for conversations that ended days ago.
func (p *TunelledProcess) SubscriberCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subs)
}

func (p *TunelledProcess) recordFailure(err error) {
	p.mu.Lock()
	p.lastErr = err
	p.mu.Unlock()
}

func (p *TunelledProcess) recordSuccess() {
	p.mu.Lock()
	p.lastErr = nil
	p.mu.Unlock()
}

// decodePromptReply reads a prompt acknowledgement.
func decodePromptReply(raw []byte) (code, message string, restarts int, degraded, refused bool, err error) {
	var resp tunnel.AgentPromptResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", 0, false, false, err
	}
	if resp.Accepted {
		return "", "", 0, false, false, nil
	}
	return resp.Code, resp.Error, resp.Restarts, resp.Degraded, true, nil
}

// decodeSteerReply reads a steer acknowledgement.
func decodeSteerReply(raw []byte) (code, message string, restarts int, degraded, refused bool, err error) {
	var resp tunnel.AgentSteerResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", 0, false, false, err
	}
	if resp.Accepted {
		return "", "", 0, false, false, nil
	}
	return resp.Code, resp.Error, 0, false, true, nil
}

// decodeAbortReply reads an abort acknowledgement.
//
// The edge reports aborting an idle agent as a success, because the
// operator's intent — stop — holds either way and an error would train them
// to ignore the message. Only a node that could not be reached, or that
// refused for a stated reason, is a refusal here.
func decodeAbortReply(raw []byte) (code, message string, restarts int, degraded, refused bool, err error) {
	var resp tunnel.AgentAbortResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", 0, false, false, err
	}
	if resp.Aborted {
		return "", "", 0, false, false, nil
	}
	return resp.Code, resp.Error, 0, false, true, nil
}

// decodeSetModelReply reads a model-pin acknowledgement.
func decodeSetModelReply(raw []byte) (code, message string, restarts int, degraded, refused bool, err error) {
	var resp tunnel.AgentSetModelResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", 0, false, false, err
	}
	if resp.Accepted {
		return "", "", 0, false, false, nil
	}
	return resp.Code, resp.Error, 0, false, true, nil
}

// Decide carries an operator's answer to a gated call on the node.
//
// It is a command like any other, which is the point: the approval queue
// lives in the control plane but the blocked call lives inside a process on
// a host the control plane cannot reach directly, so the answer has to go
// back the same way the question came. The node applies it or refuses it,
// and a refusal is a refusal rather than a transport failure — the operator
// clicking an already-lapsed request is a normal thing to do.
func (p *TunelledProcess) Decide(ctx context.Context, req tunnel.AgentDecideRequest) error {
	_, err := p.call(ctx, tunnel.MethodAgentDecide, req, decodeDecideReply)
	return err
}

// decodeDecideReply reads a decision acknowledgement.
func decodeDecideReply(raw []byte) (code, message string, restarts int, degraded, refused bool, err error) {
	var resp tunnel.AgentDecideResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", 0, false, false, err
	}
	if resp.Applied {
		return "", "", 0, false, false, nil
	}
	return resp.Code, resp.Error, 0, false, true, nil
}
