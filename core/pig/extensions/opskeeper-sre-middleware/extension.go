// Package opskeepermiddleware is the live-middleware read toolset: the
// tools a node's agent offers for questions about the databases, caches,
// clusters and brokers the control plane is connected to.
//
// Every tool here does the same thing. It declares a name, a description
// and a schema, and when the model calls it, it carries the call across a
// unix socket to the OpsKeeper host on this node and returns what the host
// said. The implementations — listing PostgreSQL sessions, censusing Redis
// keys, reading a Kubernetes pod list — are not here. They are in the
// control plane, where the DSNs and the credentials are configured, where
// every tool already has a permission class, and where the answer is
// auditable.
//
// That split is the whole design and not a shortcut:
//
//   - The tools could not be implemented here. A subprocess on a node has
//     no path to the manager's PostgreSQL DSN, its Redis endpoint, its
//     kubeconfig or its broker credentials. A node that held them would be
//     a node that could be talked into using them, and the reach would be
//     the node's rather than the operator's.
//   - The agent process runs with the node's privileges. Routing means the
//     agent holds no capability the host did not hand it, and no credential
//     it was not configured with.
//   - The host re-checks the allow-list before it dispatches. The gate
//     already refused the call on the way in, but the gate is reached
//     through an extension in this process — a package that replaced it
//     would silence that check. This one cannot.
//
// Every tool in this package is classified L0 or L1 by the adapter that
// implements it, and the package ships only those. The way this package
// could become dangerous is not a malicious edit to the manifest — it is a
// routine change to an adapter that nobody thinks to re-check — so the
// class is asserted where it is decided, not here.
//
// **Where the check actually lives**, because this paragraph used to get it
// wrong in a way that sends the next reader looking in the wrong tree: the
// generated tools.go carries no class field, and the test that enforces
// this is not in this package. It is
// core/manager/middleware/toolset:
//
//   - TestTheMiddlewareToolsetIsReadOnly asserts, per tool and against the
//     adapters' own registration, that no write-classified tool reached the
//     read toolset — and that the registry is non-empty and does contain
//     writes, so it cannot pass for the wrong reason;
//   - TestToolsetMatchesTheAdapters compares this package's file with what
//     the generator produces, byte for byte, so a reclassification shows up
//     as a failing diff rather than as a line nobody notices in a 52-entry
//     diff.
//
// Two of those live in a different module from this one, so a reader who
// greps only the extension packages concludes there is no guard. There is.
//
// The writes those adapters also offer (pg.kill_session, k8s.drain,
// redis.flushdb) deliberately do not appear here at any version. They are
// reachable through the control plane's approval path, where a human sees
// the blast radius before anything runs, and offering them on the node's
// upcall channel would be a second door into the same room with no queue
// behind it.
//
// The manifest's tool list is therefore the review surface, and it is
// generated from the same registration this file is: an adapter that gains
// a read tool shows up as a diff somebody has to accept.
package opskeepermiddleware

import (
	"encoding/json"
	"fmt"
	"os"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// extensionName is how the agent names this extension in diagnostics.
//
// It is deliberately not the read-only or repair toolset's name. All three
// are loaded into one agent process, and a diagnostic that cannot say
// which toolset a call came from is a diagnostic that cannot be acted on.
const extensionName = "opskeeper-sre-middleware"

// Extension builds the toolset.
//
// It reads only the broker socket path from the environment, and it does
// so at load rather than per call: a tool definition has to exist before
// the model can call it, so there is no "report it when it is used" option
// for a missing socket. The tools are registered regardless. An agent with
// no broker is an agent that can hold a conversation and cannot reach the
// control plane, which is the correct failure — and each call says exactly
// why, rather than the tool silently not existing.
//
// Registration cannot fail per tool, so a schema this package ships that
// the agent cannot parse would be a load-time panic in PiG. That is
// deliberate: the schemas in tools.go are generated from the live registry
// and copied in by scripts/sync-pig-ops.sh, so a broken one is an
// author-time mistake, and an author-time mistake should stop the build
// rather than quietly removing `query_promql` from production.
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
		//
		// Every tool in this package is a read, and a read that gets sent
		// twice costs a turn rather than a system — but the instruction not
		// to retry is kept anyway, because it is the same message shape
		// the repair toolset sends and a model that learns "a lost reply
		// means try again" on a read will apply that lesson to a restart.
		return nil, fmt.Errorf(
			"%s was not run: the OpsKeeper host on this node could not be reached (%v). "+
				"Do not retry this call — report it so an operator can check the node's agent.",
			name, err)
	}
	if !reply.OK() {
		// A refusal from the host is not a failure of this extension. It
		// is a decision the model needs to read and respect, so it is
		// returned in the same shape with the host's own words.
		return nil, fmt.Errorf("%s was not run: %s", name, reply.Error)
	}
	if len(reply.Result) == 0 {
		// An empty payload is still an answer, and it is still an answer
		// the runtime has to be able to read. Returning an empty Go map
		// would be read as a result object with no content -- the same
		// shape bug as above, one step earlier.
		return sdk.ToolResult{Content: "{}"}, nil
	}

	// The host's result is decoded, and then handed back in the SHAPE the
	// agent runtime reads a tool result in -- not in the shape the tool
	// happens to produce.
	//
	// That distinction is the whole of this function's last step, and
	// getting it wrong is invisible: the agent unmarshals a tool result
	// into {content, details, is_error} and IGNORES every field it does
	// not know. A tool that answers with its own JSON object is therefore
	// not "rendered oddly" -- it is decoded into a result with no content
	// at all, and the model is told "(no tool output)". No error, no
	// warning, a tool card that looks like it ran.
	//
	// So the payload goes in as text, and the same bytes go in as details
	// for anything that wants the structure without re-parsing text.
	var out any
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		return nil, fmt.Errorf(
			"%s ran, but its result was not JSON this agent could read: %w", name, err)
	}
	return sdk.ToolResult{
		Content: string(reply.Result),
		Details: out,
	}, nil
}

// schemaOf decodes one spec's raw schema literal.
//
// A schema that does not decode is a broken literal in the generated file,
// and the panic it produces is the point: the alternative is a tool that
// appears to the model with no parameters, which is a tool that fails in
// production and passes every build.
func schemaOf(spec toolSpec) sdk.Schema {
	var schema sdk.Schema
	if err := json.Unmarshal([]byte(spec.Parameters), &schema); err != nil {
		panic(fmt.Sprintf("opskeeper-sre-middleware: tool %q has an unparseable schema: %v", spec.Name, err))
	}
	return schema
}

// sessionOf reports the session the agent is serving.
//
// The host does not trust this — it resolves the caller from its own record
// and falls back to the turn in flight — but sending it costs nothing and
// makes the exact lookup succeed whenever the identifiers happen to line
// up. It matters here because the control plane attributes every query to
// a conversation, and an unattributed fleet-wide query is one nobody can
// ask about afterwards.
func sessionOf(ctx sdk.Context) string {
	id, err := ctx.GetSessionID()
	if err != nil {
		return ""
	}
	return id
}
