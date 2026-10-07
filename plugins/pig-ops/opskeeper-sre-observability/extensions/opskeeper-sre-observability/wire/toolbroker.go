package wire

import "encoding/json"

// The node tool-broker protocol: how a tool inside the agent's process asks
// the host to actually run it.
//
// It is the sibling of the gate protocol, and the pairing is the point.
// The gate answers "may this run?" and the broker answers "run it" — two
// separate questions, asked over two separate sockets, to two separate
// pieces of host state. A package that replaced the courier extension would
// silence the first question; it cannot answer the second, because the
// broker re-checks the same allow-list against the same registry before it
// dispatches anything. Defence in depth here is not a slogan: it is the
// only reason the two checks are not the same check.
//
// The split also decides where privileged code lives. The agent process runs
// with the node's privileges, and the operations OpsKeeper needs on a host —
// reading the kernel ring buffer, tracing a syscall, tailing a log — are
// exactly the operations that should not be reimplemented inside it. They
// already exist in the edge, already have permission classes, already have
// tests. So the extension registers tool *definitions* and routes calls;
// the edge owns every implementation. The agent process holds no capability
// the host has not handed it, which is what makes "isolated subprocess" a
// containment boundary rather than a naming convention.
//
// One JSON object per line in each direction over a unix socket, for the
// same reason the gate is a line protocol: this is the last thing between a
// model and a shell, and framing that can be confused about is
// authorisation that can be confused about.
const (
	// ToolSocketEnv names the environment variable carrying the broker
	// socket path, the same way GateSocketEnv carries the gate's.
	//
	// It is the only thing the agent is told. No role, no session, no
	// registry: the agent asserts which tool it wants to run and what
	// arguments it proposes, and the host resolves everything else.
	ToolSocketEnv = "OPSKEEPER_TOOL_SOCKET"
)

// ToolRequest is one line the agent sends to run a tool.
//
// Arguments is forwarded as the model proposed it, and the host re-encodes
// it before execution, so what runs is what the host parsed rather than
// what the agent claimed to send.
type ToolRequest struct {
	// SessionID is the conversation the call came from. The host uses it
	// to resolve the actor, and it is the only identity the agent asserts.
	SessionID string `json:"session_id"`
	// ToolName is the tool to run. The host looks it up in its own
	// registry; a name it does not know does not exist.
	ToolName string `json:"tool"`
	// Arguments is the model's proposed input.
	Arguments map[string]any `json:"arguments,omitempty"`
}

// ToolReply is the host's answer.
//
// Error is a tool that failed or a call the host refused, and the two are
// deliberately the same field: the agent turns either into text the model
// reads, and a model that cannot tell "the tool broke" from "the host said
// no" would retry a refusal. Result is the tool's own JSON, passed through
// unchanged.
type ToolReply struct {
	// Result is the tool's output, verbatim. A tool that returns nothing
	// omits it rather than sending null.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is why the call did not produce a result.
	Error string `json:"error,omitempty"`
}

// OK reports whether the call produced a result.
func (r ToolReply) OK() bool { return r.Error == "" }
