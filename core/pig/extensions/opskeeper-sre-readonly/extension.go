// Package opskeepersre is the read-only SRE toolset: eighteen tools the
// node's agent offers, and not one line of operational code.
//
// Every tool here does the same thing. It declares a name, a description
// and a schema, and when the model calls it, it carries the call across a
// unix socket to the OpsKeeper host on this node and returns what the host
// said. The implementations — reading the kernel ring buffer, tracing a
// syscall, walking the topology graph — are not here. They are in the
// edge, where they already existed, already have permission classes, and
// are already covered by tests that were written before this package was.
//
// That split is the design, not a shortcut:
//
//   - The agent process runs with the node's privileges. Every tool that
//     touches the node is exactly the kind of code that should not be
//     reimplemented inside it. Routing means the agent process holds no
//     operational capability the host did not hand it.
//   - The control-plane tools could not be here at all. get_topology and
//     query_alert_rules read the manager's graph and its rule table; a
//     subprocess on the node has no path to either, and inventing one
//     would be a second, unaudited route into the control plane.
//   - The host re-checks the allow-list before it dispatches. The gate
//     already refused the call on the way in, but the gate is reached
//     through an extension in this process — a package that replaced it
//     would silence that check. This one cannot.
//
// What this package contributes is therefore declarative: a name the model
// can call, a schema that tells it how, and a description that tells it
// when. Everything past the socket belongs to the host.
package opskeepersre

import (
	"encoding/json"
	"fmt"
	"os"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// extensionName is how the agent names this extension in diagnostics.
const extensionName = "opskeeper-sre-readonly"

// Extension builds the toolset.
//
// It reads only the broker socket path from the environment, and it does
// so at load rather than per call: a tool definition has to exist before
// the model can call it, so there is no "report it when it is used" option
// for a missing socket. The tools are registered regardless. An agent with
// no broker is an agent that can hold a conversation and cannot touch the
// machine, which is the correct failure — and each call says exactly why,
// rather than the tool silently not existing.
//
// Registration cannot fail per tool, so a schema this package ships that
// the agent cannot parse would be a load-time panic in PiG. That is
// deliberate: these schemas are literals in this file, so a broken one is
// an author-time mistake, and an author-time mistake should stop the build
// rather than quietly removing a capability from production.
func Extension() *sdk.Extension {
	ext := sdk.New(extensionName)

	r := &router{
		socketPath: os.Getenv(wire.ToolSocketEnv),
		session:    sessionOf,
	}
	for _, spec := range tools {
		ext.RegisterTool(sdk.ToolDefinition{
			Name:        spec.Name,
			Label:       spec.Label,
			Description: spec.Description,
			Parameters:  schemaOf(spec),
			Execute:     r.tool(spec.Name),
		})
	}
	return ext
}

// router holds the connection to the host and speaks the broker protocol.
type router struct {
	// socketPath is the host's tool socket. Empty means no host, which
	// every call reports rather than silently succeeding.
	socketPath string
	// client is opened lazily and reopened on the first failure after a
	// break, so a dead edge does not permanently deafen the agent and a
	// live one is not reconnected per call.
	client *client
	// session names the conversation for the host to look up. It is a
	// field so the lookup can be exercised without an agent runtime.
	session func(sdk.Context) string
}

// tool returns the Execute for one named tool.
//
// The name is bound here rather than read from the call so a tool cannot
// be talked into running a different one: the host looks up the name this
// package registered, and the model's idea of which tool it called has no
// path to the dispatch.
func (r *router) tool(name string) sdk.ToolFunc {
	return func(ctx sdk.Context, params map[string]any) (any, error) {
		session := ""
		if r.session != nil {
			session = r.session(ctx)
		}
		return r.run(session, name, params)
	}
}

// run carries one call to the host and shapes what comes back.
func (r *router) run(session, name string, params map[string]any) (any, error) {
	if r.client == nil {
		r.client = &client{path: r.socketPath}
	}

	reply, err := r.client.run(wire.ToolRequest{
		SessionID: session,
		ToolName:  name,
		Arguments: params,
	})
	if err != nil {
		// The message is written for the model. "connection refused" is
		// not something it can act on; "the OpsKeeper host on this node
		// could not be reached, report it to an operator" is.
		return nil, fmt.Errorf(
			"%s was not run: the OpsKeeper host on this node could not be reached (%v). "+
				"Do not retry the same call — report it so an operator can check the node's agent.",
			name, err)
	}
	if !reply.OK() {
		// A refusal from the host is not a failure of this extension. It
		// is a decision the model needs to read and respect, so it is
		// returned in the same shape with the host's own words.
		return nil, fmt.Errorf("%s was not run: %s", name, reply.Error)
	}
	if len(reply.Result) == 0 {
		return map[string]any{}, nil
	}

	// The host's result is decoded rather than passed through as bytes.
	// The agent renders whatever it gets, and a decoded value renders the
	// same way in the transcript, in a tool card, and in a packed run.
	var out any
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		return nil, fmt.Errorf(
			"%s ran, but its result was not JSON this agent could read: %w", name, err)
	}
	return out, nil
}

// schemaOf decodes one spec's raw schema literal.
//
// A schema that does not decode is a broken literal in this file, and the
// panic it produces is the point: the alternative is a tool that appears to
// the model with no parameters, which is a tool that fails in production
// and passes every build.
func schemaOf(spec toolSpec) sdk.Schema {
	var schema sdk.Schema
	if err := json.Unmarshal([]byte(spec.Parameters), &schema); err != nil {
		panic(fmt.Sprintf("opskeeper-sre-readonly: tool %q has an unparseable schema: %v", spec.Name, err))
	}
	return schema
}

// sessionOf reports the session the agent is serving.
//
// The host does not trust this — it resolves the caller from its own record
// and falls back to the turn in flight — but sending it costs nothing and
// makes the exact lookup succeed whenever the identifiers happen to line
// up. Absent, the call is still attributed; it is just attributed to the
// turn rather than to a name.
func sessionOf(ctx sdk.Context) string {
	id, err := ctx.GetSessionID()
	if err != nil {
		return ""
	}
	return id
}
