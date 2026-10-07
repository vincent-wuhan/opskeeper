package tunnel

import (
	"time"

	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// The agent channel: manager → edge commands, edge → manager events.
//
// A node's AI agent is a `pig --mode rpc` subprocess (see
// core/edge/pigsupervisor), not a library in the edge agent. That makes
// these ordinary tunnel RPCs: the manager cannot reach into the process, so
// it speaks to it across the same multiplexed channel it already uses for
// host metrics and skill execution. The existing edge identity and
// authentication apply unchanged — an RPC here is exactly as trusted as
// `restart_service`, because it comes from the same authenticated edge
// session.
//
// Two directions, deliberately separate:
//
//   - Commands (below) are request/response. The manager asks the node to do
//     something and waits for the outcome.
//   - Events (MethodAgentEvent) are edge → manager pushes. A turn's output
//     cannot be a request/response: the manager has no way to know when a
//     turn ends, and holding an RPC open for minutes would pin a tunnel
//     stream per conversation.
//
// The methods are named `agent.*` rather than reusing `execute_skill`. The
// agent is not a skill: it is a long-lived process with a conversation, and
// routing it through the skill dispatcher would give it a request/response
// shape it cannot use and a registry entry that has to be re-resolved on
// every reconnect.
const (
	// MethodAgentPrompt sends a user turn to the node's agent. Returns as
	// soon as the turn is accepted, not when it finishes; output arrives
	// on the event channel.
	MethodAgentPrompt = "agent.prompt"
	// MethodAgentSteer injects a message into the turn already running.
	// The console's "actually, do X instead" affordance.
	MethodAgentSteer = "agent.steer"
	// MethodAgentAbort stops the current turn. Aborting an idle agent is
	// not an error: an operator clicking stop twice must not see a fault.
	MethodAgentAbort = "agent.abort"
	// MethodAgentState reports what the node's agent is doing. This is the
	// call that distinguishes "node is down" from "node is up but its
	// agent is wedged", which are different pages for an operator.
	MethodAgentState = "agent.state"
	// MethodAgentSetModel pins the node's agent to a provider and model.
	MethodAgentSetModel = "agent.set_model"
	// MethodAgentEvent is edge → manager: one frame from the agent's
	// stream. Body is an AgentEventFrame.
	MethodAgentEvent = "agent.event"
	// MethodAgentHealth is manager → edge: the supervisor's own view of
	// its process — restarts, degraded, last error. Distinct from
	// MethodAgentState, which asks the *process*; this asks the thing
	// that keeps it alive.
	MethodAgentHealth = "agent.health"
	// MethodAgentAuditReplay is edge → manager: the node hands over the
	// decisions it made on its own while the control plane was away, so
	// the center's tamper-evident chain can record them.
	//
	// It exists because of a sentence in the plan — "每次自治执行写本地审计
	// spool；隧道恢复后回传，补写中心审计链" — and the half that matters is
	// the clause after the semicolon. A row that lives only on the node
	// that made the decision is the node's own account of what it did; the
	// chain is what makes it evidence. Without this route the spool is a
	// local diary, and a node whose disk dies takes its self-heal history
	// with it.
	//
	// The node is the sender, so the direction is edge → manager. That is
	// the same direction as MethodPushHostMetrics, and for the same
	// reason: the manager cannot pull a batch out of a node it cannot
	// reach, and the whole point is that the node comes back.
	MethodAgentAuditReplay = "agent.audit.replay"
	// MethodAgentAuditEntries is a node handing the control plane its own
	// ledger rows — tool calls, plugin installs, the PiG agent's turns —
	// so they can be chained centrally. It is separate from
	// MethodAgentAuditReplay because a self-heal row and a tool-call row
	// share nothing but their producer; see audit.go for why widening the
	// older method would have made its name wrong.
	//
	// The direction and the reasoning are the same as above: the node
	// comes back, because the manager cannot pull a batch out of a node it
	// cannot reach.
	MethodAgentAuditEntries = "agent.audit.entries"
)

// AgentPromptRequest is the wire body for MethodAgentPrompt.
type AgentPromptRequest struct {
	// SessionID is the conversation this turn belongs to. The manager
	// mints it so two consoles watching one node do not interleave, and
	// so a reconnecting console can resume the same stream.
	SessionID string `json:"session_id"`
	// Text is the turn, after any mention rendering, exactly as the
	// operator sent it.
	Text string `json:"text"`
	// Role is the caller's system role (admin | user | viewer). The edge
	// filters the agent's tool set by it before the turn starts, so a
	// viewer's turn cannot reach a mutating tool no matter what the
	// console asked for.
	Role string `json:"role,omitempty"`
	// Locale is the console language the reply must use.
	Locale string `json:"locale,omitempty"`
}

// AgentPromptResponse acknowledges an accepted turn.
type AgentPromptResponse struct {
	// SessionID echoes the request's, so a multiplexed caller can match
	// the acknowledgement to the turn it sent.
	SessionID string `json:"session_id"`
	// Accepted is false when the node's agent could not take the turn —
	// process down, degraded, or refusing work. The reason is in Error.
	// A rejected turn produces no events, so the console would otherwise
	// sit on a spinner forever.
	Accepted bool `json:"accepted"`
	// Error explains a rejection. Human-facing; the console branches on
	// Code instead.
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
	// Restarts and Degraded are echoed from the supervisor so a rejection
	// can say "the agent is crash-looping" rather than "unavailable",
	// which is the difference between a page and a retry.
	Restarts int  `json:"restarts,omitempty"`
	Degraded bool `json:"degraded,omitempty"`
}

// AgentSteerRequest is the wire body for MethodAgentSteer.
type AgentSteerRequest struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

// AgentSteerResponse acknowledges a steering message.
type AgentSteerResponse struct {
	SessionID string `json:"session_id"`
	Accepted  bool   `json:"accepted"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
}

// AgentAbortRequest is the wire body for MethodAgentAbort.
type AgentAbortRequest struct {
	// SessionID is optional: aborting is safe without one, because the
	// agent has at most one turn in flight and aborting the wrong one is
	// still better than leaving it running.
	SessionID string `json:"session_id,omitempty"`
}

// AgentAbortResponse reports the outcome of an abort.
type AgentAbortResponse struct {
	// Aborted is true when a turn was actually stopped. False with a nil
	// Error means the agent was already idle, which is a success: the
	// operator's intent — "stop" — holds either way.
	Aborted bool   `json:"aborted"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
}

// AgentStateRequest is the wire body for MethodAgentState. It is empty; it
// exists so the method has a typed body like every other one on the
// channel, and so a later field can be added without a wire break.
type AgentStateRequest struct{}

// AgentStateResponse is the node agent's self-report.
//
// SessionID and Model are what the console shows in the header of an
// agent conversation: which conversation, and which model is answering.
// Reporting the model actually in use matters because a node can be pinned
// to something other than the cluster default, and a cost line that
// attributes every answer to the default model is wrong.
type AgentStateResponse struct {
	SessionID   string `json:"session_id,omitempty"`
	Running     bool   `json:"running"`
	Model       string `json:"model,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Version     string `json:"version,omitempty"`
	PendingTool int    `json:"pending_tool_calls,omitempty"`
	// Available is false when the node has no agent at all, as opposed to
	// having one that is idle. The console needs to tell "this node does
	// not run an agent" apart from "the agent is busy".
	Available bool `json:"available"`
}

// AgentSetModelRequest is the wire body for MethodAgentSetModel.
type AgentSetModelRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
}

// AgentSetModelResponse acknowledges a model pin.
type AgentSetModelResponse struct {
	SessionID string `json:"session_id,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Accepted  bool   `json:"accepted"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
}

// AgentHealthRequest is the wire body for MethodAgentHealth. It is empty.
type AgentHealthRequest struct{}

// AgentHealthResponse is the supervisor's view of its own process.
//
// This is deliberately not AgentStateResponse. State asks the process what
// it is doing; health asks the supervisor whether it has been crashing. A
// node whose agent is up and crash-looping three times a minute is "running"
// by state and "degraded" by health, and only the second one is worth
// waking somebody for.
type AgentHealthResponse struct {
	Running  bool   `json:"running"`
	Degraded bool   `json:"degraded"`
	Restarts int    `json:"restarts"`
	Version  string `json:"version,omitempty"`
	// LastError is the most recent start failure or unclean exit. It is
	// carried to the console verbatim: "plugin manifest not found" tells
	// an operator what to fix, and "agent unavailable" does not.
	LastError string `json:"last_error,omitempty"`
	// LastStartAt is when the current process came up, for an uptime
	// display on the node's page.
	LastStartAt time.Time `json:"last_start_at,omitzero"`
}

// AgentEventFrame is one relayed agent event, edge → manager.
//
// The payload is the agent's own record, forwarded verbatim. The control
// plane translates it to the console's frame vocabulary on arrival; it does
// not decode it here. Pinning the wire to a decoded copy of an agent schema
// that is still moving would break the channel the first time the agent grew
// a field — and the field most likely to be added is a tool result an
// operator is watching for.
type AgentEventFrame struct {
	// EdgeID scopes the frame on a shared manager connection.
	EdgeID uint64 `json:"edge_id"`
	// Type is the agent's event name, e.g. "assistant_delta" or
	// "tool_execution_end". Opaque to everything but the translator.
	Type string `json:"type"`
	// SessionID, Iteration and Seq are the agent's own envelope fields,
	// hoisted so the manager can demultiplex without parsing the payload.
	SessionID string `json:"session_id,omitempty"`
	Iteration int    `json:"iteration,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	// Terminal marks the end of a turn.
	Terminal bool `json:"terminal,omitempty"`
	// Frame is the event translated into the console's own vocabulary, or
	// nil when the event has no console counterpart.
	//
	// Translating on the node is what keeps the console unchanged: the
	// tunnel carries a frame the manager and the console already parse,
	// so neither of them learns that a node agent exists. Payload is kept
	// alongside it so an event this build cannot place is still delivered
	// rather than dropped.
	Frame *wire.StreamEvent `json:"frame,omitempty"`
	// Payload is the complete original record.
	Payload []byte `json:"payload,omitempty"`
	// At is the node's clock when the frame was emitted. It is the edge
	// clock, not the manager's, so a manager that lags or whose clock
	// drifted does not compress or stretch the agent's timeline.
	At time.Time `json:"at,omitzero"`
}

// MethodAgentDecide carries a human's answer to an approval request back to
// the node that asked.
//
// The direction is the point. Approval authority is the operator's and the
// queue lives in the control plane, but the request was raised by a call
// blocked inside the node's agent process — so the answer has to travel back
// down the same authenticated tunnel the question came up. It is a command
// like any other: it arrives on the same edge session and is trusted the
// same amount, and a node cannot originate one.
const MethodAgentDecide = "agent.decide"

// AgentDecideRequest is the wire body for MethodAgentDecide.
type AgentDecideRequest struct {
	// RequestID is the gate's handle for the pending call. The node
	// refuses a decision for an id it does not hold, rather than matching
	// on anything else — a decision that lands on the wrong call is worse
	// than one that is dropped.
	RequestID string `json:"request_id"`
	// Digest echoes the digest the node sent with the request. The node
	// recomputes it and refuses a mismatch, so a decision cannot be
	// replayed onto a call the operator was never shown.
	Digest string `json:"digest"`
	// Decision is "grant" or "deny". The node treats any other value as a
	// denial rather than as a malformed request, because the safe reading
	// of a value it does not recognise is "no".
	Decision string `json:"decision"`
	// DecidedBy is the operator's identity, for the audit ledger.
	DecidedBy string `json:"decided_by,omitempty"`
	// Note is optional free text, recorded with the decision and shown to
	// the next operator who sees the same call.
	Note string `json:"note,omitempty"`
}

// AgentDecideResponse reports how the node applied a decision.
type AgentDecideResponse struct {
	// RequestID echoes the request so a multiplexed caller can match the
	// answer to the question it asked.
	RequestID string `json:"request_id"`
	// Applied is true when the decision reached a live request. False with
	// a Code is the interesting case: the operator was told something, and
	// the console has to be able to say it did not land.
	Applied bool   `json:"applied"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------
// agent.audit.replay (edge -> manager)
// ---------------------------------------------------------------------

// AutonomyAuditRow is one self-heal decision as the node recorded it.
//
// It mirrors core/edge/autonomy.Row field-for-field rather than sharing the
// type, because core/floor/tunnel is the wire and the wire must not import
// the node's autonomy package any more than it imports the node's spool.
// The duplication is the same one every other message in this file already
// carries, and it is what lets the two sides change shape independently on
// either side of a version boundary.
type AutonomyAuditRow struct {
	// At is when the node wrote the row. The center stamps its own
	// OccurredAt and keeps this in the payload: the chain's timestamp
	// must be the time the record entered the chain, and the node's time
	// is the fact being recorded.
	At        time.Time `json:"at"`
	Action    string    `json:"action"`
	Package   string    `json:"package"`
	Tool      string    `json:"tool"`
	Target    string    `json:"target"`
	Argv      []string  `json:"argv"`
	Kind      string    `json:"trigger_kind"`
	Metric    string    `json:"trigger_metric,omitempty"`
	Threshold float64   `json:"trigger_threshold,omitempty"`
	Key       string    `json:"idempotency_key"`
	Verdict   string    `json:"verdict"`
	Reason    string    `json:"reason,omitempty"`
	Phase     string    `json:"phase"`
	Result    string    `json:"result,omitempty"`
	ExitCode  int       `json:"exit_code,omitempty"`
}

// AutonomyAuditReplayRequest is one batch of the node's self-heal rows.
//
// EdgeID scopes the batch on a shared manager connection, exactly as it does
// on the metrics and change-event pushes. The manager binds it to the
// authenticated edge identity rather than trusting it, so a compromised node
// cannot attribute its decisions to another.
type AutonomyAuditReplayRequest struct {
	EdgeID uint64             `json:"edge_id,omitempty"`
	Rows   []AutonomyAuditRow `json:"rows"`
}

// AutonomyAuditReplayResponse reports how the chain took the batch.
//
// The two counts are what the node acks its spool against, and the pair
// they form is deliberately not three states but two:
//
//   - accepted == len(rows): the chain has all of it. The node may forget
//     the batch.
//   - accepted == 0, rejected > 0: the batch was refused **whole** for
//     shape. Nothing was written. The refusal is not retried — the rows
//     will have the same shape next time — so the node counts them,
//     passes them, and logs it.
//
// There is no partial accept, and that is the design rather than an
// omission. The chain is an ordered append-only ledger with no dedupe key,
// and the node's pump retries a batch all-or-nothing. If the center wrote
// a prefix and then failed, the node would resend that prefix and the
// chain would record it twice — so a batch is written whole or not at
// all, and the center's answer only ever describes those two outcomes.
//
// The *third* answer a node has to distinguish is the one where the
// numbers (0, 0) arrive with no error, which means the manager could not
// place the rows yet — an unregistered edge, or a body it could not read.
// Decision 100's rule applies here unchanged: "took none" is not "refused
// all", and the node keeps the batch and asks again.
type AutonomyAuditReplayResponse struct {
	Accepted int `json:"accepted"`
	// Rejected counts rows the center refused for shape (a missing action,
	// an unknown phase). It is always either 0 or len(rows); see above.
	Rejected int `json:"rejected"`
	// Reason explains a non-zero Rejected count for the node's log.
	Reason string `json:"reason,omitempty"`
}
