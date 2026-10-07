package tunnel

import "encoding/json"

// The upcall: a node's agent asking the control plane to run a tool.
//
// This is the one direction of the agent channel that runs the other way,
// and it exists because half the node's read-only toolset is not on the
// node. get_topology walks the manager's graph; query_alert_rules reads
// the manager's rule table. Neither has any local implementation to call,
// and inventing a second route to that data from inside the agent process
// would be an unaudited way into the control plane — so the node asks, and
// the manager answers on the same authenticated edge session every other
// RPC uses.
//
// The direction is the whole reason this is its own method rather than
// another entry in the agent command list. Every other `agent.*` method is
// manager → node: the manager drives a conversation it owns. This one is
// raised by a call the node's own agent made, so it is an effect of the
// agent rather than a command to it. Conflating the two would make
// "something told this node to do X" and "this node decided to do X" the
// same event in the audit ledger, which is the one distinction the ledger
// exists to keep.
const MethodAgentTool = "agent.tool"

// AgentToolRequest is the wire body for MethodAgentTool.
type AgentToolRequest struct {
	// SessionID is the conversation the call came from. The manager uses
	// it to attribute the call to the operator who caused it, and to
	// apply that operator's permissions rather than the node's.
	//
	// It is a claim, not an authority. The manager resolves the session
	// against its own record; a node that invents one gets the answer for
	// a session that does not exist, which is nobody's.
	SessionID string `json:"session_id"`
	// Tool is the control-plane tool to run. The node cannot name a
	// host-side tool here; those are dispatched locally and never travel.
	Tool string `json:"tool"`
	// Arguments is the model's proposed input, re-encoded by the broker
	// before it is sent so the manager parses what the host validated.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// AgentToolResponse is the manager's answer.
type AgentToolResponse struct {
	// Result is the tool's output, verbatim.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is why the call produced no result. A refusal and a failure
	// share this field deliberately: the agent turns either into text the
	// model reads, and a model that cannot tell them apart retries the
	// refusal.
	Error string `json:"error,omitempty"`
}
