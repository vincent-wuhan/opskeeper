package ports

import (
	"context"
	"time"
)

// AgentProcess is a long-lived agent process a node runs, reached over a
// request/response channel rather than in-process.
//
// Why a port and not the concrete client: a node's agent is a separate
// `pig --mode rpc` process, and the control plane drives the same protocol
// over a tunnel. Both sides want identical semantics, and neither should
// have to know that the transport underneath is PiG's RPC client. The port
// is what lets edge and manager share one supervisor implementation while
// the PiG-shaped type stays inside the pig module.
//
// The contract is deliberately narrower than a general RPC. Only the
// operations the control plane actually issues across a node boundary are
// here; anything session-local belongs in-process.
type AgentProcess interface {
	// Start spawns the process. It returns once the process is up or has
	// failed to come up; it does not wait for the run to finish.
	Start(ctx context.Context) error
	// Stop terminates the process, escalating from a graceful signal to a
	// kill. It must be safe to call on a process that already exited.
	Stop() error
	// Running reports whether the process is currently up. A supervisor
	// polls this rather than tracking exit itself, so there is one
	// authority on liveness.
	Running() bool
	// Exited is closed when the process is gone — whether it was asked to
	// stop or crashed. A supervisor restarts by waiting on this rather
	// than by polling, so a crash is noticed the moment it happens
	// instead of on the next tick, and a poll loop can never miss a
	// process that died and was replaced between two samples.
	//
	// Each Start opens a fresh channel. The channel for a dead process
	// stays closed, so a supervisor that has not yet restarted reads a
	// consistent "not running" rather than blocking forever on a channel
	// that belongs to a process it no longer has.
	Exited() <-chan struct{}
	// LastError reports the most recent start failure or non-clean exit.
	// It is a snapshot for health reporting, not a control signal: an
	// operator sees the cause, a retry loop does not branch on it.
	LastError() error

	// Prompt sends a user turn. It returns once the turn is accepted, not
	// once it is finished; results arrive on the event stream.
	Prompt(ctx context.Context, text string) error
	// Steer injects a message into the turn already running.
	Steer(ctx context.Context, text string) error
	// Abort stops the current turn. Aborting when nothing is running is
	// not an error: an operator clicking stop twice must not see a fault.
	Abort(ctx context.Context) error
	// SetModel pins the process to a provider and model.
	//
	// The pin is sticky across turns but not across restarts: a process
	// that crashes comes back on the configuration it was launched with,
	// because a supervisor that tried to restore live state would have to
	// persist it, and a persisted pin is a policy decision the control
	// plane has to make deliberately rather than a detail of a respawn.
	SetModel(ctx context.Context, provider, model string) error
	// State reports what the process is doing right now. It is the
	// `agent.state` reply, and a node that cannot answer it is a node the
	// console must be able to say so about.
	State(ctx context.Context) (*ProcessState, error)

	// OnEvent subscribes to the process's event stream and returns the
	// unsubscribe function. Events are the only way a turn's output
	// reaches the console, so a supervisor that starts a process without
	// subscribing has started a process nobody can hear.
	OnEvent(fn func(ProcessEvent)) (unsubscribe func())
}

// ProcessState is the node agent's self-report.
type ProcessState struct {
	// SessionID identifies the live session. Empty when idle.
	SessionID string `json:"session_id,omitempty"`
	// Running is true while a turn is in flight.
	Running bool `json:"running"`
	// Model is the model the process is currently pinned to, so the
	// console can show which model answered rather than which is default.
	Model string `json:"model,omitempty"`
	// Provider is that model's provider id.
	Provider string `json:"provider,omitempty"`
	// Version is the agent build, reported so the manager can refuse a
	// capability the node's binary does not have.
	Version string `json:"version,omitempty"`
	// PendingToolCalls is the number of tool calls the model has asked
	// for that have not settled.
	PendingToolCalls int `json:"pending_tool_calls,omitempty"`
}

// ProcessEvent is one frame from the node agent's event stream.
//
// The type is deliberately open rather than a closed enum. The agent's
// event vocabulary is PiG's, which moves, and a control plane pinned to an
// old enum would drop events it has never heard of instead of forwarding
// them. Unknown types are relayed; a frame the console cannot render is
// ignored, and a frame the console never received is a support ticket.
type ProcessEvent struct {
	// Type is the agent's own event name, e.g. "assistant_delta" or
	// "tool_execution_end". Opaque to every module except the console.
	Type string `json:"type"`
	// SessionID scopes the event to one session on a multiplexed process.
	SessionID string `json:"session_id,omitempty"`
	// Iteration is the turn number within the session, 1-based.
	Iteration int `json:"iteration,omitempty"`
	// Seq is the per-session monotonic counter.
	Seq int64 `json:"seq,omitempty"`
	// Payload is the event's body, forwarded verbatim. Forwarding raw is
	// what keeps this port from becoming a second, stale copy of the
	// agent's schema.
	Payload []byte `json:"payload,omitempty"`
	// Terminal marks an event that ends a turn. A relay uses it to close
	// out its bookkeeping without having to enumerate every end event
	// name the agent may grow.
	Terminal bool `json:"terminal,omitempty"`
}

// ProcessHealth is what a supervisor reports about its process.
//
// It is a snapshot rather than a status enum so that a degraded agent —
// alive but crash-looping, or alive but refusing prompts — is
// distinguishable from a healthy one. An operator seeing a node that is up
// but useless needs that difference, and a two-state flag erases it.
type ProcessHealth struct {
	// Running is true when the process is up right now.
	Running bool `json:"running"`
	// Version is the agent build last observed.
	Version string `json:"version,omitempty"`
	// Restarts counts process starts since the supervisor began, including
	// the first. A value that keeps climbing is a crash loop.
	Restarts int `json:"restarts"`
	// LastStartAt is when the current process came up.
	LastStartAt time.Time `json:"last_start_at,omitzero"`
	// LastExit is how the previous process ended, empty if none has.
	LastExit string `json:"last_exit,omitempty"`
	// Degraded is true when the supervisor has stopped restarting because
	// the process is failing too fast to be worth respawning. The
	// supervisor stays up and keeps reporting: a supervisor that exits
	// takes its health endpoint with it, and a node that vanishes from
	// the fleet is worse than one that is visibly broken.
	Degraded bool `json:"degraded"`
	// LastError is the most recent start or exit failure.
	LastError string `json:"last_error,omitempty"`
}
