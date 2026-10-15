// Package opskeepersreautonomy is the self-heal toolset: the one tool the
// agent offers for an action a node may take with nobody there to approve
// it, declared and routed but never implemented here.
//
// Every tool in this package declares a name, a description and a schema,
// and when the model calls it, the call is carried across a unix socket to
// the OpsKeeper host on this node. The implementation — the arbiter, the
// signed manifest, the sandbox, the audit spool — is in the host, where it
// already existed, already carries permission classes, and is already
// wrapped in the review that produced the signature.
//
// What makes this package different from the mutating one is *who answers*.
//
// The mutating toolset is answered by the control plane, always, because
// the reviewer that must see a proposal lives on the other side of the
// tunnel. This one is answered by the node — but only while the control
// plane is unreachable, and only for an argument vector a person read and
// signed when the package was admitted. So:
//
//   - The agent process still runs with the node's privileges and gains
//     none. The argv is not in this package, not in this process, and not
//     reachable from anything the model can write. The model names an
//     action; the host resolves that name against the manifest and runs
//     the manifest's own argv.
//   - The tool is registered unconditionally, and the host refuses the
//     call on a node that declared no autonomy. An agent that can discuss
//     self-healing and cannot perform it is the correct failure, and the
//     refusal says exactly that rather than the tool silently not
//     existing.
//   - The host re-checks everything on arrival: that this package
//     declared the tool, that the node's role may run it, that the arbiter
//     is the authority, and — while the control plane is away — that the
//     action was declared, its trigger actually holds, the argv matches
//     byte for byte, the reach is inside the node's ceiling, the window
//     has not expired, and the key has not been spent.
//
// The last list is the reason this is a tool rather than a shell. Seven
// checks a model cannot talk its way past, in front of a command the model
// never gets to write.
package opskeepersreautonomy

import (
	"encoding/json"
	"fmt"
	"os"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/vincent-wuhan/opskeeper/plugins/pig-ops/opskeeper-sre-autonomy/extensions/opskeeper-sre-autonomy/wire"
)

// extensionName is how the agent names this extension in diagnostics.
//
// It is distinct from the other two toolsets' names on purpose. All three
// are loaded into one agent process, and a diagnostic that cannot say which
// toolset a call came from is a diagnostic that cannot be acted on.
const extensionName = "opskeeper-sre-autonomy"

// Extension builds the toolset.
//
// It reads only the broker socket path from the environment, and it does so
// at load rather than per call: a tool definition has to exist before the
// model can call it, so there is no "report it when it is used" option for
// a missing socket. The tools are registered regardless.
//
// Registration cannot fail per tool, so a schema this package ships that
// the agent cannot parse would be a load-time panic in PiG. That is
// deliberate: these schemas are literals in this file, so a broken one is
// an author-time mistake, and an author-time mistake should stop the build
// rather than quietly removing a self-heal capability from production.
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
		// The instruction not to retry is load-bearing here in a way it is
		// not for the read-only toolset, and for a sharper reason than the
		// mutating one. A read that is sent twice costs a turn. This is a
		// command on a live host, and the host spends an action's
		// idempotency key the moment it adjudicates it — so a retry
		// arriving after a lost reply is not "the same action again", it
		// is a claim about an occurrence that has already been spent.
		return nil, fmt.Errorf(
			"%s was not run: the OpsKeeper host on this node could not be reached (%v). "+
				"Do not retry this call — the action is spent by its idempotency key the moment "+
				"the host adjudicates it, so a retry is a second action rather than a repeat of "+
				"the first. Report it so an operator can check the node's autonomy audit row.",
			name, err)
	}
	if !reply.OK() {
		// A refusal from the host is not a failure of this extension. It
		// is a decision the model needs to read and respect, so it is
		// returned in the same shape with the host's own words. A deferral
		// ("the control plane is reachable, so this goes to the approval
		// gate") arrives here too, and it is the most valuable answer this
		// tool gives: it is how the model learns that asking does not
		// bypass a human while a human is present.
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
// A schema that does not decode is a broken literal in this file, and the
// panic it produces is the point: the alternative is a tool that appears to
// the model with no parameters, which is a tool that fails in production
// and passes every build.
func schemaOf(spec toolSpec) sdk.Schema {
	var schema sdk.Schema
	if err := json.Unmarshal([]byte(spec.Parameters), &schema); err != nil {
		panic(fmt.Sprintf("opskeeper-sre-autonomy: tool %q has an unparseable schema: %v", spec.Name, err))
	}
	return schema
}

// sessionOf reports the session the agent is serving.
//
// The host does not trust this — it resolves the caller from its own record
// and falls back to the turn in flight — but sending it costs nothing and
// makes the exact lookup succeed whenever the identifiers happen to line
// up.
func sessionOf(ctx sdk.Context) string {
	id, err := ctx.GetSessionID()
	if err != nil {
		return ""
	}
	return id
}
