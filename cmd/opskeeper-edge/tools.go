package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/toolbroker"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The node's tool invoker: what the broker dispatches to.
//
// A node's toolset is two kinds of tool wearing one name. The host_*
// probes are the node's own business — it can read its own kernel ring
// buffer without asking anyone. The topology and alert queries are the
// control plane's business: the graph and the rule table live in the
// manager, and the only honest way to reach them is to ask. So resolution
// is local first, then upcall, and the order is not a preference — a tool
// the node can answer itself should not spend a tunnel round trip, and a
// node that shadowed a control-plane tool with a local one would be a node
// answering questions about the fleet from its own machine.
//
// "Local" is narrowed to reads, and that narrowing is the whole reason
// this file has a class check in it. A mutating skill is registered in the
// node's registry too — that is how the catalog knows to draw it — but its
// approval authority is not the node's. The control plane's BaseTool
// wrapper carries the reviewer that asks a human, and the dispatcher that
// finally reaches the edge is a different tunnel method from the broker's.
// So a mutating call goes up the tunnel, where the approval lives, and
// comes back down the path a human already agreed to. Dispatching it from
// here would be running the change with the one gate in the system that
// cannot produce consent.
//
// This is what makes the read-only profile real. The PiG extension inside
// the agent process holds no implementation at all; it routes. Every
// privileged operation on a node therefore happens here, in the edge,
// where it is permissioned by the skill registry, covered by tests written
// before any of this existed, and recorded.
//
// It lives in the composition root rather than in the edge module because
// it is the one place that knows both the skill framework and the broker
// protocol exist: core/edge holds the tools and the broker, and cmd wires
// them to the tunnel. A component that knew both ends would be the whole
// node plane in one package.

// agentToolInvoker runs a tool the broker has already permitted.
type agentToolInvoker struct {
	// client is the tunnel up to the control plane. Required for the
	// control-plane half; without it those tools report an error rather
	// than being silently absent.
	client tunnel.Client
	// log records what ran. The gate's ledger records what was permitted;
	// this records what actually happened, which is the half a gate alone
	// cannot supply.
	log *slog.Logger
	// obs is how the router knows whether the control plane is answering.
	// It is only consulted for the autonomy tool; every other tool routes
	// the same way it always did.
	obs autonomyObservations
}

// Invoke runs one permitted call.
//
// Reads run here, where the evidence is. Everything else is the control
// plane's, either because the node has no executor for it or because a
// human has to be asked somewhere the human can see the queue. A name that
// is neither is not a tool this node has, and saying so is more useful than
// an empty result the model would then have to interpret.
func (t *agentToolInvoker) Invoke(ctx context.Context, c toolbroker.Call) (json.RawMessage, error) {
	exec, registered := skill.Get(c.ToolName)
	if !registered || !runsOnThisNode(exec) {
		// The autonomy tool is the one call a node answers for itself, and
		// only while the control plane is not answering. Everything else
		// — including this tool while the centre is up — goes up the
		// tunnel, where the approval for a mutating call lives.
		if registered && c.ToolName == builtin.ToolKey && autonomyIsLocal(t.obs) {
			return t.runLocal(ctx, exec, c)
		}
		return t.upcall(ctx, c)
	}
	return t.runLocal(ctx, exec, c)
}

// runsOnThisNode reports whether the node may dispatch a skill by itself.
//
// The test is the skill's own class rather than its name or its scope,
// because class is the property the rest of the policy path is already
// built on: it is what the allow-list admits, what the role ceiling is
// compared against, and what decides whether a receipt is demanded. A
// second, independent notion of "where this runs" would be free to disagree
// with all three.
//
// Read-only is the positive case, so the failure mode is the safe one. A
// skill whose class this function has not heard of is sent to the control
// plane, where a reviewer exists, rather than run by a node that would not
// ask. Sending an unknown tool uphill costs a round trip and is answered;
// running an unknown tool here is not.
func runsOnThisNode(exec skill.Executor) bool {
	return classOfSkill(exec.Metadata().EffectiveClass()) == domain.ClassRead
}

// runLocal executes a skill that lives on this node.
func (t *agentToolInvoker) runLocal(ctx context.Context, exec skill.Executor, c toolbroker.Call) (json.RawMessage, error) {
	// The parameters are the JSON the broker re-encoded from the model's
	// proposal, so the executor parses exactly what the host validated
	// rather than whatever the agent process claimed to send.
	out, err := exec.Execute(ctx, c.Arguments)
	if t.log != nil {
		t.log.Info("agent tool ran on the node",
			"tool", c.ToolName, "session", c.SessionID, "err", errString(err))
	}
	if err != nil {
		// A skill that fails is reported as a failure, not as an
		// exception the agent renders as an extension fault the model
		// never sees. A container that cannot read /proc/kmsg is a normal
		// answer, and the investigator persona needs to be able to read
		// it rather than be told the tool does not exist.
		return nil, fmt.Errorf("%s failed: %w", c.ToolName, err)
	}
	return out, nil
}

// upcall asks the control plane to run a tool it owns.
func (t *agentToolInvoker) upcall(ctx context.Context, c toolbroker.Call) (json.RawMessage, error) {
	if t.client == nil {
		return nil, fmt.Errorf(
			"%s is a control-plane tool and this node has no tunnel to the control plane", c.ToolName)
	}
	var resp tunnel.AgentToolResponse
	err := t.client.Call(ctx, tunnel.MethodAgentTool, tunnel.AgentToolRequest{
		SessionID: c.SessionID,
		Tool:      c.ToolName,
		Arguments: c.Arguments,
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("%s: the control plane could not be reached: %w", c.ToolName, err)
	}
	if resp.Error != "" {
		// The manager's refusal arrives as an error string rather than a
		// transport failure, so the model reads it as a decision rather
		// than as something to retry.
		return nil, fmt.Errorf("%s: %s", c.ToolName, resp.Error)
	}
	return resp.Result, nil
}

// errString renders an error for a log line without the ceremony.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
