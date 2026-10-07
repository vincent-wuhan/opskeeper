package wire

// The node gate protocol: how a tool call inside the agent's process asks
// the host whether it may run.
//
// It exists because the agent is a separate process. The tool allow-list,
// the approval queue, and the audit ledger all live in the edge, and none
// of the three can be delegated to the agent: an agent that could grant its
// own request would make the queue decorative. So every call crosses this
// boundary and gets an answer from the only party that is allowed to give
// one.
//
// The protocol is deliberately small and deliberately boring — one JSON
// object per line in each direction over a unix socket — because it is the
// last thing between an agent and a shell. Anything that could be confused
// about framing is a way to be confused about authorisation.
//
// The session is the only thing the agent asserts. It does not get to say
// who it is acting for: the host resolves the actor from the session,
// because an agent that could name its own privilege would do exactly that.
// A session the host does not know resolves to read-only, which is the
// correct answer for "we do not know who this is".
const (
	// GateSocketEnv names the environment variable carrying the socket
	// path. The agent inherits the edge's environment, so this is how a
	// plugin-internal extension finds the host without being configured
	// with a path a package could have written.
	//
	// It is the only thing the agent is told. There is deliberately no
	// variable carrying the role or the session: an agent handed those at
	// start-up would be handed the ability to assert them, and an assertion
	// is not a lookup.
	GateSocketEnv = "OPSKEEPER_GATE_SOCKET"
)

// GateRequest is one line the agent sends to ask about a call.
//
// Arguments is the exact JSON the model proposed. It is what the approval
// digest covers, so it is relayed verbatim rather than re-encoded: a
// re-encoding that reordered or dropped a key would produce a digest over a
// call the operator was never shown.
type GateRequest struct {
	// SessionID is the conversation the call came from. The host uses it
	// to resolve who is asking.
	SessionID string `json:"session_id"`
	// ToolName is the tool the agent is about to run.
	ToolName string `json:"tool"`
	// Arguments is the model's proposed input, forwarded unchanged.
	Arguments map[string]any `json:"arguments,omitempty"`
	// Target is a best-effort description of what the call reaches, shown
	// to the operator. It is a hint for display, never for policy: the
	// host assesses the blast radius itself.
	Target string `json:"target,omitempty"`
	// Summary is the agent's one-line description of what it is about to
	// do, shown alongside the operator's own assessment. Also a hint.
	Summary string `json:"summary,omitempty"`
}

// GateVerdict is the host's answer.
//
// It is a closed pair. Anything the host cannot affirm is an error, and the
// caller treats an error as a refusal — the same answer, arrived at by a
// different route, which is what makes the failure path safe to write once.
type GateVerdict struct {
	// Outcome is "allowed", "blocked" or "denied".
	Outcome GateOutcome `json:"outcome"`
	// Reason explains a refusal. It is written for the model to read back
	// into its transcript, so it says what was wrong rather than what was
	// refused.
	Reason string `json:"reason,omitempty"`
}

// GateOutcome is the host's verdict on one call.
type GateOutcome string

const (
	// GateAllow means the call may run.
	GateAllow GateOutcome = "allowed"
	// GateBlock means policy refused it outright. No human can grant this
	// one; the tool is not permitted for this actor.
	GateBlock GateOutcome = "blocked"
	// GateDeny means a human refused it, or the approval lapsed. It is
	// distinct from a block because it was a decision rather than a
	// configuration problem, and the model should say so differently.
	GateDeny GateOutcome = "denied"
)

// Permitted reports whether the verdict authorises the call.
func (v GateVerdict) Permitted() bool { return v.Outcome == GateAllow }
