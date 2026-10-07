package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Agent failure codes.
//
// The console branches on these, so they are a closed set and changing one
// is a wire break. The message beside them is for humans and may change;
// a console that renders the message instead of the code will break the
// first time a node reports a reason nobody anticipated.
const (
	// CodeAgentUnavailable means the node has no agent process running.
	CodeAgentUnavailable = "agent_unavailable"
	// CodeAgentDegraded means the supervisor gave up restarting: the
	// agent is failing too fast to be worth respawning. Distinct from
	// unavailable because the remedy is different — this is a node to go
	// and look at, not a node to wait for.
	CodeAgentDegraded = "agent_degraded"
	// CodeAgentNoSession means the request named a session the node is
	// not running.
	CodeAgentNoSession = "agent_no_session"
	// CodeAgentBadRequest means the request body was unusable.
	CodeAgentBadRequest = "agent_bad_request"
	// CodeAgentInternal means the bridge itself failed.
	CodeAgentInternal = "agent_internal"
	// CodeAgentNoGate means this node runs no approval gate, so it has no
	// pending requests and cannot accept a decision. It is a separate code
	// from NoSession because the remedies differ: a session that is gone
	// will be recreated, a node with no gate will not grow one because
	// somebody clicked "deny" in the console.
	CodeAgentNoGate = "agent_no_gate"
	// CodeAgentBadDecision means the request named a decision the node
	// does not accept, or one that names a request the node does not hold.
	// Either way nothing was applied, and the operator has to be told so
	// rather than left believing a call was released.
	CodeAgentBadDecision = "agent_bad_decision"
	// CodeAgentBusy means another conversation's turn is in flight.
	//
	// It exists because the node's agent is one process with one session.
	// Two consoles can both hold an open conversation on the same node, and
	// without this the second prompt is accepted and its output is stamped
	// with whichever conversation started last — which is not a degraded
	// answer, it is one operator's turn rendered in another's transcript.
	// A refusal costs the second operator one retry; the alternative costs
	// somebody a decision they never made.
	CodeAgentBusy = "agent_busy"
)

// AgentSource is what the bridge needs from the node's agent supervisor.
//
// It is the two methods that matter — reach the process, and report health —
// rather than the concrete supervisor type. That keeps the supervisor's
// lifecycle testable on its own and lets the bridge be tested against a
// stub with no process behind it.
type AgentSource interface {
	// Process returns the live agent process. An error means the node has
	// no agent to talk to right now.
	Process() (ports.AgentProcess, error)
	// Health reports what the supervisor sees, including whether it has
	// stopped restarting.
	Health() ports.ProcessHealth
}

// AgentDecider is the node's approval gate, as the bridge needs it.
//
// It is the same shape as ports.DecisionProvider with the context dropped,
// because there is no request to cancel: applying a decision is a map
// operation, and holding a tunnel stream open for it would only add a way
// for the answer to be lost after it was already given.
type AgentDecider interface {
	Decide(d ports.Decision) error
}

// AgentBridge serves the manager's agent.* RPCs on one node.
//
// It owns no agent state of its own. The process belongs to the supervisor
// and the supervisor owns its lifecycle; the bridge only translates between
// the tunnel's wire shapes and ports.AgentProcess, so a node's agent can be
// restarted underneath a conversation without the bridge being rebuilt.
type AgentBridge struct {
	source AgentSource
	// decider applies a human's answer to a pending gated call. Optional:
	// a bridge with no gate still serves commands, and refuses decisions
	// with a code that says why.
	decider AgentDecider
	// client is the tunnel, used to push events back. It is an interface
	// rather than the concrete tunnel.Client so the bridge can be tested
	// without a broker.
	client tunnelClient
	log    logger

	edgeID uint64
	now    func() time.Time
	// translate turns a raw agent event into the console's frame. It is
	// supplied by the caller so the bridge holds no PiG knowledge of its
	// own and the translation can be tested where PiG's wire format
	// lives.
	translate func(ports.ProcessEvent) []wire.StreamEvent
	dropped   atomic.Int64
	// unattributed counts agent records that arrived with no conversation
	// in flight, which the node cannot route and therefore does not send.
	unattributed atomic.Int64

	// roles remembers which system role each conversation is acting as.
	//
	// It exists because the role travels on the prompt and is needed on
	// every later tool call: the gate asks "who is asking" once a call is
	// already blocked, long after the turn began, and by then the only
	// evidence left is the session id the agent can supply. The node
	// resolves the role from its own record rather than asking the agent,
	// because an agent that could name its own privilege would name the
	// top of the ladder.
	roleMu sync.RWMutex
	roles  map[string]roleEntry
	// active is the conversation whose turn is currently in flight, or
	// empty when the agent is idle.
	active string
	// settled is the conversation whose turn most recently ended, and
	// settledAt when it did.
	//
	// It exists because an agent finishes *reporting* a turn after it
	// finishes *having* one. The event that ends the turn is not the last
	// event of it: the console's `done` frame — the one carrying the
	// turn's token usage, the frame the console's stream closes on — rides
	// on the event after it. Clearing the in-flight marker the moment the
	// turn ends therefore strands that frame on the floor, and an
	// operator watching a stream that never says why it stopped has no way
	// to tell a finished investigation from a broken node.
	settled   string
	settledAt time.Time
}

// settleGrace is how long a finished turn still owns the events that
// follow it.
//
// The tail is short in practice — the agent announces the end of a turn
// and then reports on it, milliseconds later — but the node has no way to
// know which event is last, so the window is a bound rather than a guess
// at a duration. It is deliberately far shorter than roleTTL: this is a
// relay concern, not a memory-retention one, and holding a conversation's
// claim on the node for hours would mean a second operator is told the
// node is busy long after it is free.
const settleGrace = 10 * time.Second

// roleEntry is one conversation's role and when it was last refreshed.
type roleEntry struct {
	role string
	seen time.Time
}

// roleTTL is how long a conversation's role survives without a turn.
//
// The record has to outlive the gap between turns, and a node is told
// nothing when the control plane closes a conversation — the close is a
// manager-side bookkeeping act that never reaches the node. Expiring
// instead of never expiring bounds the table on a long-lived node, and it
// fails safe: an expired session resolves to no role, which is read-only.
// A conversation that comes back sends a prompt first, and the role is
// written before the turn that could use it starts.
const roleTTL = 12 * time.Hour

// tunnelClient is the slice of tunnel.Client the bridge uses. Declaring it
// here rather than accepting tunnel.Client keeps the test double to two
// methods.
type tunnelClient interface {
	Call(ctx context.Context, method string, req, resp any) error
}

// logger is the slice of *slog.Logger the bridge uses.
type logger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// AgentBridgeOptions configures a bridge.
type AgentBridgeOptions struct {
	// Source is the node's agent supervisor. Required.
	Source AgentSource
	// Decider is the node's approval gate. Optional; without it the
	// bridge answers agent.decide with CodeAgentNoGate.
	Decider AgentDecider
	// Client is the edge tunnel, used to push events. Optional: a bridge
	// with no client serves commands but has nowhere to send output, and
	// says so rather than appearing to work.
	Client tunnelClient
	// Log is optional.
	Log logger
	// EdgeID scopes pushed frames.
	EdgeID uint64
	// Now defaults to time.Now.
	Now func() time.Time
	// Translate builds the console frame for a raw agent event. Optional;
	// without it the tunnel carries the raw record and the manager
	// translates.
	Translate func(ports.ProcessEvent) []wire.StreamEvent
	// EventQueueDepth bounds the outbound event buffer. It is
	// deliberately finite: a node whose control plane has gone away must
	// drop agent output rather than grow without bound or, worse, block
	// the agent's own stream waiting for a reader that is not there.
	// Default 256.
	EventQueueDepth int
}

// DefaultEventQueueDepth is the outbound event buffer depth when unset.
const DefaultEventQueueDepth = 256

// NewAgentBridge returns a bridge for one node.
func NewAgentBridge(opts AgentBridgeOptions) (*AgentBridge, error) {
	if opts.Source == nil {
		return nil, errors.New("agent bridge: Source is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.EventQueueDepth <= 0 {
		opts.EventQueueDepth = DefaultEventQueueDepth
	}
	return &AgentBridge{
		source:    opts.Source,
		decider:   opts.Decider,
		roles:     make(map[string]roleEntry),
		client:    opts.Client,
		log:       opts.Log,
		edgeID:    opts.EdgeID,
		now:       opts.Now,
		translate: opts.Translate,
	}, nil
}

// Register installs every agent handler on the edge's tunnel.
//
// A node with no agent still registers the handlers. An unregistered method
// surfaces to the manager as "method not found", which is indistinguishable
// from an edge agent that is too old to have this feature; a registered
// handler that answers "agent_unavailable" is a different and actionable
// message, and it is what lets a fleet view tell "no agent configured" from
// "agent broken".
func (b *AgentBridge) Register(c tunnel.Client) {
	c.RegisterHandler(tunnel.MethodAgentPrompt, b.handlePrompt)
	c.RegisterHandler(tunnel.MethodAgentSteer, b.handleSteer)
	c.RegisterHandler(tunnel.MethodAgentAbort, b.handleAbort)
	c.RegisterHandler(tunnel.MethodAgentState, b.handleState)
	c.RegisterHandler(tunnel.MethodAgentSetModel, b.handleSetModel)
	c.RegisterHandler(tunnel.MethodAgentHealth, b.handleHealth)
	c.RegisterHandler(tunnel.MethodAgentDecide, b.handleDecide)
}

// StartEvents subscribes to the agent's stream and relays frames to the
// manager until ctx ends.
//
// It returns immediately: the subscription and the wait live on their own
// goroutine. That is not a convenience, it is the contract callers are
// written against — Agent.Run calls this from the middle of its startup
// sequence, and a version that blocked here stranded everything below it
// (the changewatcher, the upgrade sentinel, eg.Wait and therefore graceful
// shutdown) while the node kept heartbeating and looked healthy. Nothing
// is reported as an error for an agent that is not up yet: that is
// agent.state's answer, not a failure to subscribe.
func (b *AgentBridge) StartEvents(ctx context.Context) {
	// Preferred path: a source that binds the relay itself, so the
	// subscription survives the process being replaced.
	//
	// This is the difference between a node that relays its agent's output
	// and a node that is permanently deaf to it. StartEvents runs once,
	// during startup, and on a cold node the agent process is not up yet —
	// so binding to "the process that exists right now" binds to nothing
	// and never retries. The node then heartbeats, reports a healthy agent,
	// answers agent.state, runs the turn, pays for the model call, and
	// shows the operator an empty conversation. No error anywhere, because
	// every individual fact is true.
	if src, ok := b.source.(eventSource); ok {
		release := src.OnEvent(b.relay)
		go func() {
			<-ctx.Done()
			release()
		}()
		return
	}

	proc, err := b.source.Process()
	if err != nil {
		// No process, and no source that can wait for one. The node still
		// answers agent.state and agent.health, so the console can render
		// the absence; but say it, because a node in this state is a node
		// whose conversations will never produce a frame, and the only
		// symptom an operator gets is silence.
		b.warn("node agent events are not relayed: no agent process yet and no source that survives a restart",
			"err", err)
		return
	}
	b.warn("node agent events are bound to one process and will stop at the next agent restart",
		"hint", "the agent source does not implement OnEvent, so the relay cannot be rebound")
	go func() {
		_ = proc.OnEvent(b.relay)
		<-ctx.Done()
	}()
}

// eventSource is an AgentSource that can bind a relay itself.
//
// It is declared here, beside the bridge that needs it, for the same reason
// AgentSource is a two-method interface rather than the concrete supervisor:
// the bridge is testable against a stub, and a stub that cannot promise a
// restart-durable subscription says so by not having this method — which is
// exactly the distinction the fallback above has to make.
type eventSource interface {
	OnEvent(fn func(ports.ProcessEvent)) func()
}

// warn logs through the bridge's logger when it has one.
func (b *AgentBridge) warn(msg string, args ...any) {
	if b.log == nil {
		return
	}
	b.log.Warn(msg, args...)
}

// SetEdgeID updates the edge id frames are stamped with. The manager
// assigns it during register_edge, which happens after the bridge is built.
func (b *AgentBridge) SetEdgeID(id uint64) { b.edgeID = id }

// DroppedEvents reports how many agent frames were discarded because the
// manager was not draining them fast enough.
//
// It is reported rather than silently absorbed because a run of drops means
// the console is showing a conversation with holes in it, and an operator
// debugging that needs to know the gaps are transport, not the agent
// forgetting to speak.
func (b *AgentBridge) DroppedEvents() int64 { return b.dropped.Load() }

// relay pushes one agent frame to the manager.
//
// The send is bounded and drops on overflow. A node's agent must keep
// running when the control plane is unreachable: the operator is most likely
// reading about that outage *through* this agent, and an agent that blocks
// on a dead socket cannot tell them anything.
func (b *AgentBridge) relay(ev ports.ProcessEvent) {
	if b.client == nil {
		return
	}

	// The frame belongs to the conversation whose prompt started this turn,
	// not to whatever the agent calls its own session.
	//
	// This is the one line where a node-agent turn either reaches a console
	// or does not, and it is worth being exact about why the two ids are
	// different. The agent is one process with one session, and it names
	// that session itself — the node never tells it which conversation is
	// talking, because the node is not supposed to hand the agent anything
	// it could assert. The control plane, meanwhile, routes every inbound
	// frame by the conversation id it minted for the console, and drops
	// anything it cannot attribute, because attributing a frame to the
	// wrong conversation puts one operator's turn in another's transcript.
	//
	// So the id that matters is the one the node recorded when the prompt
	// arrived, and stamping the agent's own id here means every frame of
	// every turn is dropped on arrival. That is a silent failure in the
	// worst direction: the turn runs, the model is paid for, the node's
	// ledger records it, and the console shows nothing at all.
	owner := b.turnOwner()

	// The turn is over the moment the agent says so. The role record
	// outlives it — a tool batch already in flight can still call the gate
	// — but there is no longer a turn whose role an unrecognised session
	// could be borrowing.
	if ev.Terminal {
		b.endTurn(owner)
	}

	// An event with no owner is a record the agent produced outside any
	// turn: a start-up line, a notice, something from a process the node
	// has just restarted. It is counted and dropped rather than sent with an
	// empty id, which the control plane would drop anyway — the difference
	// is that the count is on the node's health line, so "the agent is
	// talking and nobody hears it" becomes a number instead of a silence.
	if owner == "" {
		b.unattributed.Add(1)
		return
	}

	frame := tunnel.AgentEventFrame{
		EdgeID:    b.edgeID,
		Type:      ev.Type,
		SessionID: owner,
		Iteration: ev.Iteration,
		Seq:       ev.Seq,
		Terminal:  ev.Terminal,
		Payload:   ev.Payload,
		At:        b.now(),
	}
	// The console's frame is built on the node, where the agent's event
	// vocabulary is known, so the tunnel carries something the manager and
	// the console already parse. An event with no counterpart still goes
	// over the wire with Frame nil, because dropping it would leave a gap
	// in a conversation nobody could explain.
	if b.translate != nil {
		// The translator is handed an attributed copy, not the agent's own
		// record. It keys one counter chain per conversation and refuses an
		// event with no session id at all, on the grounds that guessing
		// would renumber a live conversation — a correct decision for the
		// control plane, which mints ids itself, and the wrong one here,
		// because a node agent's events arrive with an empty session: the
		// node never tells the agent which conversation is talking, by
		// design. Handing over the raw event therefore yields no frames at
		// all, and the turn runs, is paid for, and shows the operator
		// nothing.
		attributed := ev
		attributed.SessionID = owner
		translated := b.translate(attributed)
		if len(translated) == 1 {
			// The translator works from the agent's record, so the frame it
			// built carries the agent's session id. The console routes on
			// this id, so it has to be the conversation's — the same
			// correction as above, one level down, and skipping it is how a
			// correct outer frame ends up carrying an unroutable inner one.
			translated[0].SessionID = owner
			frame.Frame = &translated[0]
		}
	}
	b.push(frame)
}

// turnOwner reports which conversation the agent is speaking for, or ""
// when there is none.
//
// It is wider than inFlight on purpose: a turn that ended moments ago is
// still being described, and those last events belong to it. Narrowing
// this to the in-flight marker is what drops the `done` frame.
func (b *AgentBridge) turnOwner() string {
	b.roleMu.RLock()
	defer b.roleMu.RUnlock()
	if b.active != "" {
		return b.active
	}
	if b.settled != "" && b.now().Sub(b.settledAt) < settleGrace {
		return b.settled
	}
	return ""
}

// inFlight reports the conversation holding the node right now, or "".
//
// The busy check uses this and not turnOwner, because "this turn has
// ended" and "this node is free" are the same statement only for an
// instant, and refusing a second operator for the length of the settle
// window would be a node telling two people it cannot help.
func (b *AgentBridge) inFlight() string {
	b.roleMu.RLock()
	defer b.roleMu.RUnlock()
	return b.active
}

// UnattributedEvents reports agent records the node could not attribute to
// a conversation.
//
// It is a health number rather than a log line because the question it
// answers is asked at the worst possible moment: an operator staring at a
// console showing nothing, on a node whose agent is visibly working.
func (b *AgentBridge) UnattributedEvents() int64 { return b.unattributed.Load() }

// EmitApproval relays a gated call to the console.
//
// Approval frames travel the same channel as turn output rather than a
// second one: the console already parses a frame carrying an approval, and
// a second transport would mean a second thing to reconnect, buffer and
// lose. The frame is stamped with the conversation it came from, because
// the manager drops a frame it cannot attribute and a call blocked on a
// frame nobody received is a call that sits out its whole TTL.
func (b *AgentBridge) EmitApproval(sessionID string, f wire.ApprovalFrame) {
	if b.client == nil {
		return
	}
	kind := wire.StreamApprovalPending
	if f.Decision != "" {
		kind = wire.StreamApprovalResolved
	}
	// A copy, because the gate reuses nothing but the caller may, and a
	// shared frame would let a later write land in a frame already on the
	// wire.
	approval := f
	b.push(tunnel.AgentEventFrame{
		EdgeID:    b.edgeID,
		Type:      string(kind),
		SessionID: sessionID,
		Frame: &wire.StreamEvent{
			Type:      kind,
			SessionID: sessionID,
			Approval:  &approval,
		},
		At: b.now(),
	})
}

// SetDecider attaches the node's approval gate after construction.
//
// It exists because of the order the two are built in: the gate relays
// approval frames through the bridge, and the bridge applies decisions to
// the gate. Construction order has to break that cycle somewhere, and an
// explicit setter says so at the call site instead of leaving a closure
// over a variable that is written later.
func (b *AgentBridge) SetDecider(d AgentDecider) { b.decider = d }

// push sends one frame to the manager, dropping it rather than waiting.
//
// The tunnel client has no deadline-free send with a bounded queue, so the
// drop is decided here: a short deadline, and a counted failure. A node's
// agent must keep running when the control plane is unreachable - the
// operator is most likely reading about that outage *through* this agent.
func (b *AgentBridge) push(frame tunnel.AgentEventFrame) {
	ctx, cancel := context.WithTimeout(context.Background(), relayTimeout)
	defer cancel()
	if err := b.client.Call(ctx, tunnel.MethodAgentEvent, frame, nil); err != nil {
		b.dropped.Add(1)
		if b.log != nil {
			b.log.Warn("dropped an agent event; the manager is not draining",
				"type", frame.Type, "session", frame.SessionID, "dropped_total", b.dropped.Load())
		}
	}
}

// relayTimeout bounds one event push. Short on purpose: the agent's stream
// is not the place to wait out a control plane that has stopped reading.
const relayTimeout = 5 * time.Second

// resolve returns the live process, or the wire body explaining why there
// is none.
//
// The degraded case is checked before the generic unavailable one. A node
// whose agent is crash-looping and a node with no agent at all both have no
// process right now, but they are different incidents and the console has
// to be able to say which.
func (b *AgentBridge) resolve() (ports.AgentProcess, *tunnel.AgentPromptResponse) {
	if h := b.source.Health(); h.Degraded {
		return nil, &tunnel.AgentPromptResponse{
			Code:     CodeAgentDegraded,
			Error:    degradedMessage(h),
			Restarts: h.Restarts,
			Degraded: true,
		}
	}
	proc, err := b.source.Process()
	if err != nil {
		return nil, &tunnel.AgentPromptResponse{
			Code:     CodeAgentUnavailable,
			Error:    "this node's agent is not running",
			Restarts: b.source.Health().Restarts,
		}
	}
	return proc, nil
}

// degradedMessage explains a crash loop in the operator's terms.
//
// "agent crashed 5 times in 10m0s: plugin manifest not found" is a page
// somebody can act on. "agent unavailable" is a state to be stared at.
func degradedMessage(h ports.ProcessHealth) string {
	if h.LastError != "" {
		return h.LastError
	}
	return "the agent is restarting too often to be usable"
}

func (b *AgentBridge) handlePrompt(_ context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
	var req tunnel.AgentPromptRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return b.encodePrompt(tunnel.AgentPromptResponse{Code: CodeAgentBadRequest, Error: err.Error()})
	}
	if req.SessionID == "" || req.Text == "" {
		return b.encodePrompt(tunnel.AgentPromptResponse{
			SessionID: req.SessionID,
			Code:      CodeAgentBadRequest,
			Error:     "session_id and text are required",
		})
	}
	// One agent, one session, one turn at a time. The refusal names the
	// conversation holding the node rather than a generic busy, because the
	// operator who gets it is looking at a node and needs to know whether
	// somebody else is mid-investigation on it.
	if owner := b.inFlight(); owner != "" && owner != req.SessionID {
		return b.encodePrompt(tunnel.AgentPromptResponse{
			SessionID: req.SessionID,
			Code:      CodeAgentBusy,
			Error: fmt.Sprintf("conversation %q is mid-turn on this node; "+
				"a node runs one agent turn at a time", owner),
		})
	}

	// Recorded before the turn starts. The agent's first action can be a
	// tool call, and a tool call is asked about long after this returns —
	// a role written afterwards would be one turn too late.
	b.noteRole(req.SessionID, req.Role)

	proc, failure := b.resolve()
	if failure != nil {
		failure.SessionID = req.SessionID
		return b.encodePrompt(*failure)
	}

	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	if err := proc.Prompt(ctx, req.Text); err != nil {
		return b.encodePrompt(tunnel.AgentPromptResponse{
			SessionID: req.SessionID,
			Code:      CodeAgentInternal,
			Error:     err.Error(),
		})
	}
	return b.encodePrompt(tunnel.AgentPromptResponse{SessionID: req.SessionID, Accepted: true})
}

func (b *AgentBridge) handleSteer(_ context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
	var req tunnel.AgentSteerRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return b.encodeSteer(tunnel.AgentSteerResponse{Code: CodeAgentBadRequest, Error: err.Error()})
	}
	proc, failure := b.resolve()
	if failure != nil {
		return b.encodeSteer(tunnel.AgentSteerResponse{
			SessionID: req.SessionID, Code: failure.Code, Error: failure.Error,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	if err := proc.Steer(ctx, req.Text); err != nil {
		return b.encodeSteer(tunnel.AgentSteerResponse{
			SessionID: req.SessionID, Code: CodeAgentInternal, Error: err.Error(),
		})
	}
	return b.encodeSteer(tunnel.AgentSteerResponse{SessionID: req.SessionID, Accepted: true})
}

func (b *AgentBridge) handleAbort(_ context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
	var req tunnel.AgentAbortRequest
	// An unparseable body is not an error here: abort is idempotent by
	// intent, and refusing to stop a running turn because of a malformed
	// request would be the wrong way to fail.
	if len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}

	proc, failure := b.resolve()
	if failure != nil {
		// Aborting a node with no agent has achieved what the operator
		// asked for. Reporting an error would train them to ignore it.
		if failure.Code == CodeAgentUnavailable {
			return b.encodeAbort(tunnel.AgentAbortResponse{Aborted: true})
		}
		return b.encodeAbort(tunnel.AgentAbortResponse{Code: failure.Code, Error: failure.Error})
	}

	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	if err := proc.Abort(ctx); err != nil {
		return b.encodeAbort(tunnel.AgentAbortResponse{Code: CodeAgentInternal, Error: err.Error()})
	}
	return b.encodeAbort(tunnel.AgentAbortResponse{Aborted: true})
}

func (b *AgentBridge) handleState(_ context.Context, _ tunnel.Session, _ string, _ []byte) ([]byte, error) {
	resp := tunnel.AgentStateResponse{Available: true}
	proc, failure := b.resolve()
	if failure != nil {
		// State reports unavailability as data rather than as an error
		// code: "this node has no agent" is a state, and a fleet view
		// has to render it as one on every poll.
		resp.Available = false
		resp.Version = b.source.Health().Version
		return json.Marshal(resp)
	}
	state, err := proc.State(context.Background())
	if err != nil {
		resp.Available = false
		return json.Marshal(resp)
	}
	resp.SessionID = state.SessionID
	resp.Running = state.Running
	resp.Model = state.Model
	resp.Provider = state.Provider
	resp.Version = state.Version
	resp.PendingTool = state.PendingToolCalls
	return json.Marshal(resp)
}

func (b *AgentBridge) handleSetModel(_ context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
	var req tunnel.AgentSetModelRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return b.encodeSetModel(tunnel.AgentSetModelResponse{Code: CodeAgentBadRequest, Error: err.Error()})
	}
	proc, failure := b.resolve()
	if failure != nil {
		return b.encodeSetModel(tunnel.AgentSetModelResponse{
			SessionID: req.SessionID, Code: failure.Code, Error: failure.Error,
		})
	}
	if req.Provider == "" || req.Model == "" {
		return b.encodeSetModel(tunnel.AgentSetModelResponse{
			SessionID: req.SessionID, Code: CodeAgentBadRequest,
			Error: "provider and model are required",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	if err := proc.SetModel(ctx, req.Provider, req.Model); err != nil {
		return b.encodeSetModel(tunnel.AgentSetModelResponse{
			SessionID: req.SessionID, Code: CodeAgentInternal, Error: err.Error(),
		})
	}
	return b.encodeSetModel(tunnel.AgentSetModelResponse{
		SessionID: req.SessionID, Provider: req.Provider, Model: req.Model, Accepted: true,
	})
}

func (b *AgentBridge) handleHealth(_ context.Context, _ tunnel.Session, _ string, _ []byte) ([]byte, error) {
	h := b.source.Health()
	return json.Marshal(tunnel.AgentHealthResponse{
		Running:     h.Running,
		Degraded:    h.Degraded,
		Restarts:    h.Restarts,
		Version:     h.Version,
		LastError:   h.LastError,
		LastStartAt: h.LastStartAt,
	})
}

// encodePrompt and its siblings always return a body, never an error.
//
// A tunnel handler that returns a Go error turns into a transport failure
// the manager cannot read a code out of. Every agent outcome — accepted,
// refused, unavailable — is a body, because the console has to render all
// of them differently.
func (b *AgentBridge) encodePrompt(r tunnel.AgentPromptResponse) ([]byte, error) {
	return json.Marshal(r)
}

func (b *AgentBridge) encodeSteer(r tunnel.AgentSteerResponse) ([]byte, error) {
	return json.Marshal(r)
}

func (b *AgentBridge) encodeAbort(r tunnel.AgentAbortResponse) ([]byte, error) {
	return json.Marshal(r)
}

func (b *AgentBridge) encodeSetModel(r tunnel.AgentSetModelResponse) ([]byte, error) {
	return json.Marshal(r)
}

// handlerTimeout bounds one command against the local agent. It is short:
// the agent is a subprocess on this same host, and a prompt that has not
// been accepted in seconds is a prompt the node should report as stuck
// rather than hold a tunnel stream open for.
const handlerTimeout = 10 * time.Second

// handleDecide applies a human's answer to a pending gated call.
//
// It is the only path by which a blocked tool call on this node can be
// released, and the node cannot originate one: the method is registered by
// the edge and reached from the manager over the authenticated tunnel, so a
// compromised plugin has nothing to call it with.
func (b *AgentBridge) handleDecide(_ context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
	var req tunnel.AgentDecideRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return b.encodeDecide(tunnel.AgentDecideResponse{Code: CodeAgentBadRequest, Error: err.Error()})
	}
	resp := tunnel.AgentDecideResponse{RequestID: req.RequestID}
	if req.RequestID == "" {
		resp.Code = CodeAgentBadDecision
		resp.Error = "request_id is required"
		return b.encodeDecide(resp)
	}
	if b.decider == nil {
		// A node with no gate has nothing pending, so there is no
		// decision to lose. Saying so plainly beats reporting a bad
		// request, which would send an operator looking at a bug in their
		// console.
		resp.Code = CodeAgentNoGate
		resp.Error = "this node runs no approval gate, so it has no pending requests"
		return b.encodeDecide(resp)
	}

	// An unrecognised decision word is a denial, not a malformed request.
	// The gate refuses it, the call stays blocked, and the operator is told
	// their answer did not land - which is the correct outcome for a
	// console that has sent something nobody can honour.
	word := ports.ApprovalDecision(req.Decision)
	if word != ports.ApprovalGranted && word != ports.ApprovalDenied {
		resp.Code = CodeAgentBadDecision
		resp.Error = "decision must be \"grant\" or \"deny\""
		return b.encodeDecide(resp)
	}

	err := b.decider.Decide(ports.Decision{
		RequestID: req.RequestID,
		Digest:    req.Digest,
		Decision:  word,
		DecidedBy: req.DecidedBy,
		Note:      req.Note,
	})
	if err != nil {
		// A stale or unbound decision is the ordinary case here, not an
		// edge: the operator answered a request that had already lapsed.
		// The console needs the distinction to stop showing a pending
		// button that will never resolve.
		resp.Code = CodeAgentBadDecision
		resp.Error = err.Error()
		return b.encodeDecide(resp)
	}
	resp.Applied = true
	return b.encodeDecide(resp)
}

func (b *AgentBridge) encodeDecide(r tunnel.AgentDecideResponse) ([]byte, error) {
	return json.Marshal(r)
}

// ActorFor reports the system role a conversation is acting as.
//
// It is the node's answer to the one question the gate cannot answer for
// itself: which rung of the ladder is this caller standing on. The agent
// cannot be asked, because an extension that could assert its own privilege
// would assert the top of it.
//
// It resolves in two steps. A conversation the node has a record for is
// answered from that record. A name it does not recognise falls back to the
// turn currently in flight, because the agent on a node is one process
// serving one turn at a time and the name it has for that turn is the
// agent's, not ours. That fallback grants nothing the caller did not
// already have: the only role it can name is the one belonging to the turn
// it is itself running.
//
// Anything else resolves to the empty role, which the gate reads as
// read-only. A call that needed more is refused with a reason an operator
// can act on, which is the right failure for a node that cannot tell who is
// asking rather than an error worth failing a turn over.
func (b *AgentBridge) ActorFor(sessionID string) string {
	b.roleMu.RLock()
	entry, known := b.roles[sessionID]
	if !known && b.active != "" {
		entry, known = b.roles[b.active]
	}
	b.roleMu.RUnlock()
	if !known {
		return ""
	}
	// Expiry is checked on read rather than by a sweeper, so an idle node
	// does no work and a forgotten conversation costs one string.
	if b.now().Sub(entry.seen) > roleTTL {
		b.forget(sessionID, entry)
		return ""
	}
	return entry.role
}

// noteRole records the role a turn is running as, and marks the turn as in
// flight.
//
// It is called before the turn is handed to the agent, not after: a tool
// call can be the first thing a turn does, and a role recorded afterwards
// would be one tool call too late.
func (b *AgentBridge) noteRole(sessionID, role string) {
	if sessionID == "" {
		return
	}
	now := b.now()
	b.roleMu.Lock()
	defer b.roleMu.Unlock()
	if b.roles == nil {
		b.roles = make(map[string]roleEntry)
	}
	b.pruneLocked(now)
	b.roles[sessionID] = roleEntry{role: role, seen: now}
	b.active = sessionID
}

// endTurn clears the in-flight marker once the agent reports the turn
// finished.
//
// A tool call can still arrive after the terminal frame - a batch that was
// already in flight when the turn was reported done - so the role record
// itself outlives this. Only the fallback moves: once the turn is over
// there is no turn whose role an unrecognised name could be borrowing.
func (b *AgentBridge) endTurn(sessionID string) {
	// An empty owner is not an error: a turn that ended without ever being
	// attributed has nothing to clear, and refusing here would leave the
	// marker set for a turn that is already over.
	if sessionID == "" {
		return
	}
	b.roleMu.Lock()
	defer b.roleMu.Unlock()
	if b.active == sessionID {
		b.active = ""
		// The claim does not vanish with the turn: see settled.
		b.settled = sessionID
		b.settledAt = b.now()
	}
}

// forget drops one record, if it is still the one the reader saw.
//
// The caller re-checks the timestamp because a turn may have refreshed the
// record between the read and the expiry check, and pruning a live record
// would pull the floor out from under a turn that is already running.
func (b *AgentBridge) forget(sessionID string, seen roleEntry) {
	b.roleMu.Lock()
	defer b.roleMu.Unlock()
	if cur, ok := b.roles[sessionID]; ok && cur.seen.Equal(seen.seen) {
		delete(b.roles, sessionID)
		if b.settled == sessionID {
			b.settled = ""
		}
		if b.active == sessionID {
			b.active = ""
		}
	}
}

// pruneLocked drops records that have aged out. The caller holds the lock.
//
// Without this the table would grow by one entry per conversation the node
// has ever served, for ever, on a process expected to run for months.
// Expiry rather than a close notification because the node is never told a
// conversation closed - the close is a control-plane act that does not
// reach here.
func (b *AgentBridge) pruneLocked(now time.Time) {
	for id, entry := range b.roles {
		if now.Sub(entry.seen) > roleTTL {
			delete(b.roles, id)
			if b.active == id {
				b.active = ""
			}
		}
	}
}
