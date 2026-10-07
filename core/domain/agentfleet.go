package domain

import (
	"errors"
	"fmt"
)

// This file is the seventh answer to the same measurement, and its subject is
// the reason decisions 276 through 281 kept producing one domain per cut: a
// port between two bounded contexts has to be able to name its arguments, and
// when the arguments live in the producer's package the port cannot exist.
//
// nodeagent drove the fleet through nine methods. Five of the nine named a
// nodefleet type — a prompt, a decision, a session's counters, a process
// handle, and a refusal — so the seam was a package boundary wearing an
// interface's clothes: `Fleet` was declared in nodeagent, and the only way to
// satisfy it was to import nodefleet.
//
// The five types are here. The refusal vocabulary is here too, and for a
// different reason worth stating: a node refusing a call is a fact about the
// protocol between a console and an agent, not an implementation detail of
// whichever component happened to route the call. The HTTP layer branches on it
// to answer 409 instead of 500, and a reader of that handler should not have
// to know which package holds the router.

// AgentPrompt is one turn, as the console states it and the node will run it.
type AgentPrompt struct {
	// EdgeID is the node whose agent should answer.
	EdgeID uint64
	// SessionID scopes the conversation. The fleet mints it when a console
	// opens a conversation and reuses it for every turn after.
	SessionID string
	// UserText is the turn, after any mention rendering.
	UserText string
	// Role is the caller's system role. It is recorded on the request and
	// is the node's to enforce: the edge filters the agent's tool set by
	// it before the turn starts, so a viewer's turn cannot reach a
	// mutating tool no matter what the console asked for.
	Role string
	// Locale is the console language the reply must use.
	Locale string
	// Selection optionally pins the model for this conversation.
	Selection ModelSelection
}

// AgentDecision is the operator's answer to a node's request for permission.
//
// Grant is a bool rather than a string because the only two answers the gate
// accepts are these two, and a third one should not be expressible here.
type AgentDecision struct {
	// RequestID is the handle the node minted for the pending call.
	RequestID string
	// Digest is echoed from the frame the console rendered. The node
	// refuses a decision whose digest does not match the request it names.
	Digest string
	Grant  bool
	// DecidedBy is the operator's identity, for the node's audit ledger.
	DecidedBy string
	// Note is free text recorded with the decision.
	Note string
}

// AgentSessionStats is what one conversation has cost so far.
type AgentSessionStats struct {
	EdgeID    uint64
	SessionID string
	// Frames counts what has reached the console.
	Frames int64
	// Dropped counts frames the node could not translate and the fleet
	// therefore did not emit. A non-zero value means the console is
	// showing a conversation with holes, and somebody debugging that
	// needs to know the gaps are translation, not silence from the agent.
	Dropped int64
	// Terminal is true once the agent reported the turn finished.
	Terminal bool
}

// AgentRefusal is a node declining a call, as distinct from the manager failing
// to reach it. The distinction is the whole point: a refusal is an answer, and
// the console has to render it as one.
type AgentRefusal struct {
	// Code is the node's machine-readable reason.
	Code string
	// Message is the node's human-facing explanation, carried verbatim:
	// "plugin manifest not found" tells an operator what to fix.
	Message string
	// EdgeID is the node that refused.
	EdgeID uint64
	// Restarts and Degraded are echoed from the node's supervisor when it
	// has them, so a refusal can say "crash-looping" rather than
	// "unavailable" without a second round trip.
	Restarts int
	Degraded bool
}

// Error makes AgentRefusal an error, which is what lets it travel up a call
// stack and be recovered by errors.As rather than by a side channel. The text
// is carried over verbatim from where it lived, because an operator sees it in
// a log and a 500 body, and changing it to "tidy it up" would be a silent
// behaviour change.
func (e *AgentRefusal) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	return fmt.Sprintf("node %d refused: %s", e.EdgeID, msg)
}

// Retryable reports whether retrying could plausibly succeed.
//
// Every refusal the node sends is a statement about the node's own state —
// it has no agent, its agent is crash-looping, its request was malformed —
// and none of them is fixed by trying again in a second. A transport failure
// is the retryable case, and is reported through the wrapped error instead of
// here.
//
// This method has no caller in the tree today (decision 282 measured it). It
// is kept because the alternative is losing the sentence that explains why
// every refusal is terminal, and a dead method that documents a decision is
// cheaper than the same method deleted with the reasoning deleted too.
func (e *AgentRefusal) Retryable() bool { return false }

// IsAgentRefusal reports whether err is a node's deliberate refusal rather
// than a failure to reach it.
func IsAgentRefusal(err error) bool {
	var remote *AgentRefusal
	return errors.As(err, &remote)
}

// AsAgentRefusal extracts a node's refusal from err, if there is one.
func AsAgentRefusal(err error) (*AgentRefusal, bool) {
	var remote *AgentRefusal
	if errors.As(err, &remote) {
		return remote, true
	}
	return nil, false
}
