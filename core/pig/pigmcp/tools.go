// Package pigmcp turns a deployment's registered MCP servers into tools a PiG
// session can call.
//
// # What this package is, and what it is careful not to become
//
// It is a shape converter. A ports.MCPCatalogue hands back names, descriptions
// and JSON Schemas; this turns them into agent.AgentTool values whose Execute
// calls back into that same catalogue. There is no endpoint in this package, no
// header, no token and no connection pool, and that is the whole design.
//
// The alternative is the one this repository refused everywhere else. A node's
// agent process holds no operational code and no credential: it routes, and
// the host executes. An MCP bridge that connected to Grafana from inside the
// agent process would break that in one line — the URL is one field, and a
// bearer token is one header — and it would break it *invisibly*, because the
// tool would keep working while ceasing to be auditable, permissioned or
// revocable. Routing costs a hop. The hop is the feature.
//
// # The gate still applies
//
// The tools produced here are ordinary session tools. They are handed to a
// session the same way every other tool is, which means the host's
// BeforeToolCall hooks run on an MCP call exactly as they run on a
// read-only probe or a repair. There is no path by which registering an MCP
// server widens what an agent may do; it widens what it may ask about, and the
// answer still goes through the policy the operator wrote.
//
// That is the reason this lives in core/pig rather than being handed to the
// model layer as a list of names. A caller that had to invoke these by string
// would be one call away from building the same tool twice, once gated and
// once not, and the ungated copy would be the one somebody reaches for.
package pigmcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Bridge adapts one catalogue.
type Bridge struct {
	cat ports.MCPCatalogue
}

// New wraps a catalogue. A nil catalogue yields a bridge whose Tools is an
// error rather than an empty list, because "MCP is switched off" and "MCP is
// switched on and offers nothing" are different answers and only one of them
// is a configuration mistake.
func New(cat ports.MCPCatalogue) *Bridge {
	return &Bridge{cat: cat}
}

// Tools lists what the deployment offers, as agent tools.
//
// The error is returned rather than logged-and-dropped because the catalogue
// is the component that knows which third-party server is misbehaving, and it
// has already decided that a server it cannot represent is worth refusing
// over. Swallowing that here would put the decision in two places and the
// visible one in the wrong one.
func (b *Bridge) Tools(ctx context.Context) ([]agent.AgentTool, error) {
	if b == nil || b.cat == nil {
		return nil, fmt.Errorf("pigmcp: no MCP catalogue is configured")
	}
	found, err := b.cat.Tools(ctx)
	if err != nil {
		return nil, fmt.Errorf("pigmcp: list MCP tools: %w", err)
	}

	out := make([]agent.AgentTool, 0, len(found))
	for _, t := range found {
		tool, err := newTool(b.cat, t)
		if err != nil {
			return nil, err
		}
		out = append(out, tool)
	}
	return out, nil
}

// mcpTool is one catalogue entry, bound to the catalogue rather than to a
// server.
type mcpTool struct {
	cat    ports.MCPCatalogue
	ref    ports.MCPTool
	schema ai.ToolSchema
}

func newTool(cat ports.MCPCatalogue, ref ports.MCPTool) (agent.AgentTool, error) {
	// The schema is the server's, byte for byte. Re-typing it here would
	// mean a third party's vocabulary is validated against whatever this
	// struct happens to allow, and a tool the model cannot address because
	// OpsKeeper disagreed with a vendor about JSON Schema is a support
	// ticket about somebody else's product.
	//
	// The catalogue has already established that this parses as a JSON
	// object, so a failure here would be a bug rather than a third party's,
	// and it is reported as one instead of being papered over with an empty
	// schema that would let the model call the tool with anything.
	params := map[string]any{}
	if raw := ref.Schema; len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, fmt.Errorf("pigmcp: tool %s: input schema became unreadable after validation: %w", ref.Name, err)
		}
	}
	return &mcpTool{
		cat: cat,
		ref: ref,
		schema: ai.ToolSchema{
			Name:        ref.Name,
			Description: ref.Description,
			Parameters:  params,
		},
	}, nil
}

func (t *mcpTool) Name() string  { return t.ref.Name }
func (t *mcpTool) Label() string { return t.ref.Name }
func (t *mcpTool) Schema() ai.ToolSchema {
	return t.schema
}

// ExecutionMode is parallel.
//
// The host broker already dispatches the sibling tool calls of one assistant
// turn in parallel, and this tool is one hop closer to the same server rather
// than further from it. Serialising MCP calls would make one slow third-party
// tool stretch every other tool in the same turn, which is the difference
// between a fleet question taking two seconds and taking twenty.
func (t *mcpTool) ExecutionMode() agent.ToolExecutionMode { return agent.ToolModeParallel }

// Execute runs the tool through the catalogue.
//
// Every failure is returned as an error rather than as a result carrying an
// apology, because the agent turns the two into different things: an error
// becomes a tool result the model reads and can reason about, while a
// successful result with an error inside it is content the model may quote as
// though the operation had worked. "Grafana refused" and "this returned
// nothing" must not look alike to a model deciding what to do next.
//
// The composed name is sent back rather than the server's own tool name,
// because the catalogue is what resolves it — and it is the catalogue that
// applies the caller's authorisation, so addressing the tool any other way
// would be a way to skip the check that decides whether it may run at all.
func (t *mcpTool) Execute(ctx context.Context, _ string, params json.RawMessage, _ agent.ToolUpdateCallback) (agent.AgentToolResult, error) {
	args := map[string]any{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("%s: arguments are not a JSON object: %w", t.ref.Name, err)
		}
	}
	out, err := t.cat.Call(ctx, t.ref.Name, args)
	if err != nil {
		return agent.AgentToolResult{}, fmt.Errorf("%s: %w", t.ref.Name, err)
	}
	return agent.AgentToolResult{
		Content: []ai.ToolResultMessageContent{ai.TextContent{Text: out}},
	}, nil
}
