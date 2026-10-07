// Package opskeepergate is the courier: the only thing inside the agent
// process that talks to the host's approval gate.
//
// Every tool call the agent makes passes through the host, and that is the
// whole design. The agent is a separate process running with the node's
// privileges, its tools run with the agent's privileges, and nothing inside
// it can be trusted to decide whether one of its own tools may run — not the
// prompt it was given, not the model, and certainly not the tool. So the
// decision is made outside, in the edge, and this extension is the courier
// that carries each call out and brings the answer back.
//
// Three properties are load-bearing, and each of them is the reason this is
// a separate program rather than a prompt suffix:
//
//   - It fails closed. No socket, an unreachable socket, a truncated
//     response, a verdict it does not recognise — every one of those is a
//     block, not a pass. A gate that can be talked into not answering is
//     not a gate.
//   - It is not on the answer path for identity. It sends the tool name and
//     the arguments, and nothing else it could have invented. The host
//     resolves who is asking from its own record; an extension that could
//     name its own privilege would name the top of the ladder.
//   - It holds no state worth stealing. No allow-list, no approval queue, no
//     ledger, no credentials. It has a socket path and a line protocol.
//     Anything it cannot be asked to leak, it has not got.
package opskeepergate

import (
	"fmt"
	"os"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/vincent-wuhan/opskeeper/plugins/pig-ops/opskeeper-sre-autonomy/extensions/opskeeper-gate/wire"
)

// extensionName is how the agent names this extension in diagnostics.
const extensionName = "opskeeper-gate"

// toolCallEvent is the event name PiG emits before every tool runs.
const toolCallEvent = "tool_call"

// Extension builds the courier.
//
// It reads nothing from the environment here. The socket path is resolved
// when the first call arrives rather than at load, so an agent that is
// started without a gate is not an agent that cannot answer — it is an agent
// that refuses every call with a reason naming the missing socket, which is
// the difference between a legible failure and a mysterious one.
func Extension() *sdk.Extension {
	ext := sdk.New(extensionName)

	courier := &courier{
		socketPath: os.Getenv(wire.GateSocketEnv),
		session:    sessionOf,
	}
	ext.OnEvent(toolCallEvent, courier.onToolCall)

	return ext
}

// courier holds the connection to the host and speaks the gate protocol.
//
// The connection is per-process and reused: a tool call is frequent, and a
// fresh dial per call would make the gate's latency a round trip on a unix
// socket plus a handshake, on a path that is asked about while a model is
// waiting.
type courier struct {
	// socketPath is the host's gate socket. Empty means no host, which is
	// a refusal rather than a pass.
	socketPath string
	// client is opened lazily and reopened on the first failure after a
	// break. A dead edge must not permanently deafen the agent, and a live
	// one must not be reconnected per call.
	client *client
	// session names the conversation for the host to look up. It is a
	// field so the lookup can be exercised without an agent runtime: the
	// real one reaches into the agent's own session, which only exists
	// inside one.
	session func(sdk.Context) string
}

// ask asks the host about one call, opening the connection on first use.
//
// The client is created here rather than in Extension so that an agent
// started with no socket reports that on the call that needed it, rather
// than failing to load and taking the whole package set with it.
func (c *courier) ask(req wire.GateRequest) (wire.GateVerdict, error) {
	if c.client == nil {
		c.client = &client{path: c.socketPath}
	}
	return c.client.ask(req)
}

// onToolCall asks the host about one call and returns the block decision.
//
// The returned shape is the agent's own event vocabulary — {"block": true,
// "reason": "..."} — rather than a Go error. An error here is a transport
// failure of the extension, which the host renders as an extension error
// and the model never sees; a block is a decision the model reads back into
// its transcript and can reason about. A blocked call has to be legible to
// the agent, or the agent will simply try something else at random.
func (c *courier) onToolCall(ctx sdk.Context, data map[string]any) (any, error) {
	tool, _ := data["toolName"].(string)
	if tool == "" {
		// A call with no name cannot be adjudicated, and guessing at one
		// would be adjudicating something the operator was never shown.
		return block("this tool call has no name, so the host cannot check it"), nil
	}

	session := ""
	if c.session != nil {
		session = c.session(ctx)
	}
	args, _ := data["input"].(map[string]any)

	verdict, err := c.ask(wire.GateRequest{
		SessionID: session,
		ToolName:  tool,
		Arguments: args,
		Target:    wire.ToolTargetMap(args),
		Summary:   wire.ToolSummaryMap(tool, args),
	})
	if err != nil {
		// Fail closed, and say why in terms the model can act on. "The
		// host could not be reached" is not actionable; "ask an operator"
		// is.
		return block(fmt.Sprintf(
			"%s was not run: the OpsKeeper host on this node could not be asked for permission (%v). "+
				"Do not retry the same call; report it so an operator can check the node's agent.",
			tool, err)), nil
	}
	if verdict.Permitted() {
		return nil, nil
	}
	return block(verdict.Reason), nil
}

// sessionOf reports the session the agent is serving.
//
// The host does not trust this — it resolves the caller from its own record
// and falls back to the turn in flight — but sending it costs nothing and
// makes the exact lookup succeed whenever the identifiers happen to line
// up. Absent, the call is still adjudicated; it is just adjudicated against
// the turn rather than against a name.
func sessionOf(ctx sdk.Context) string {
	id, err := ctx.GetSessionID()
	if err != nil {
		return ""
	}
	return id
}

// The target and the summary this file sends are display hints for the
// operator deciding an approval, never inputs to policy: the host assesses
// the blast radius itself, from the tool's class and what the call site can
// see. Naming the resource here only saves the operator from being shown
// "restart_service(service=orders-api)".
//
// They are derived by core/domain rather than here, because the control
// plane derives the same two strings for the same call and the two copies
// used to disagree about which argument names the resource.

// block is the refusal the agent sees.
func block(reason string) map[string]any {
	if reason == "" {
		// A refusal with no reason is one the model cannot learn from, and
		// a model that cannot learn will try the same call again.
		reason = "the OpsKeeper host refused this call without giving a reason"
	}
	return map[string]any{"block": true, "reason": reason}
}
