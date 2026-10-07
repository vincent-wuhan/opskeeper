package ports

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// MCP is the Model Context Protocol, and this file is the seam where a
// deployment's registered MCP servers become tools the agent can see.
//
// # Why a port at all, when the manager already has an MCP usecase
//
// Because of where the two consumers sit. The control plane owns the servers,
// their credentials and their call path; the agent runtime lives in core/pig,
// which is the one module permitted to name PiG, and which by construction
// cannot import the manager — the dependency arrow points inward at the
// contracts. An interface that both sides can hold is the only shape that
// survives that rule without either side reaching around it.
//
// The alternative, and the one this repository has taken twice before, is to
// let the manager hand the agent a list of strings and let the agent build
// the tools. That shape has no place to put a name-mangling rule, so the
// mangling ends up in the adapter and the call ends up as a second lookup
// path beside the first. Names cross this boundary as values, not strings to
// be re-parsed, and that is the whole reason this type exists.
//
// # The naming rule lives here because two modules have to agree on it
//
// A tool's wire name is "mcp__<server>__<tool>", with both segments
// sanitised. That rule was already implemented — in the BaseTool that
// actually executes the call — and it is stated here so there is one of it.
//
// The reason it could not simply stay there is the dependency direction.
// core/pig is the only module permitted to name PiG and cannot import the
// manager, so the SDK-shaped surface has to reach the same names from the
// other side. Two implementations of one naming rule is the failure this
// file exists to prevent: the same Grafana query would appear to the model as
// mcp__grafana__query and grafana__query, and the console would show a
// capability the agent cannot call.
//
// Sanitisation is part of the rule, not a detail of it. A server registered
// as "My-Server" is reachable as mcp__my_server__*, and a caller that
// composed the name from the raw registration would address a tool that does
// not exist. Composition therefore goes through here, and the resolver undoes
// it by matching sanitised prefixes.
const (
	// MCPToolNamePrefix marks a name as an MCP tool and keeps the MCP
	// namespace disjoint from the built-in tool names. It is exported
	// because the executing registry already uses it to route by name.
	MCPToolNamePrefix = "mcp__"
	// MCPToolSeparator divides the server half from the tool half.
	MCPToolSeparator = "__"
)

// ErrNoSuchMCPTool is what Call reports for a name this deployment does not
// offer.
//
// It is named because "the model asked for a tool that is not here" and "the
// server that should have served it is down" send an operator to different
// places, and a caller that cannot tell them apart retries the second forever.
var ErrNoSuchMCPTool = errors.New("ports: no such MCP tool")

// MCPTool is one tool offered by a registered MCP server.
//
// Schema is raw JSON Schema rather than a typed struct for the same reason
// ports.AgentMessage carries opaque JSON: the schema belongs to the server,
// and OpsKeeper has no business re-typing a vocabulary a third party
// controls. The agent runtime hands it to the model unchanged.
type MCPTool struct {
	// Name is the composed "<server>__<tool>" identifier. It is the only
	// field a caller needs, and it round-trips: passing it back to
	// MCPCatalogue.Call addresses exactly this tool.
	Name string
	// Server is the registered server the tool came from. It is carried so
	// a refusal can name the server an operator has to go and look at,
	// which a bare tool name does not tell them.
	Server string
	// Tool is the server's own name for it, which is what Call must send.
	Tool string
	// Description is what the model reads to decide whether to call it.
	Description string
	// Schema is the tool's JSON Schema, passed through verbatim.
	Schema json.RawMessage
}

// MCPCatalogue is the deployment's MCP surface: what it offers, and how to
// run one of the things it offers.
//
// Tools and Call are deliberately not derived from each other. Tools is a
// discovery answer — a snapshot of what the servers said the last time
// anybody asked — and Call is an action against the live server. A catalogue
// that computed Tools by calling every server on every turn would put a
// network round trip in the path of starting a conversation, and a
// deployment with one unreachable server would then fail to offer the tools of
// the four that are healthy.
type MCPCatalogue interface {
	// Tools lists the MCP tools this deployment offers to this caller.
	//
	// Implementations apply the caller's own authorisation here. An
	// implementation that returned every tool and left filtering to the
	// agent would put the only copy of the policy somewhere the agent
	// process can reach.
	Tools(ctx context.Context) ([]MCPTool, error)

	// Call runs one tool by its composed name and returns the server's
	// text content.
	//
	// It reports ErrNoSuchMCPTool for a name Tools would not have returned.
	// It must not report that for a server that is registered and enabled
	// but unreachable: that is a transport failure and the caller may
	// retry it, while a refusal is a decision.
	Call(ctx context.Context, name string, args map[string]any) (string, error)
}

// ComposeMCPToolName builds the wire name for one MCP tool.
//
// It is the one place the name is built. Both halves are sanitised to
// lower-case alphanumerics with every other rune becoming an underscore,
// which is what the executing registry has always done and what the
// system prompt tells the model to expect.
func ComposeMCPToolName(server, tool string) string {
	return MCPToolNamePrefix + SanitizeMCPSegment(server) + MCPToolSeparator + SanitizeMCPSegment(tool)
}

// SanitizeMCPSegment reduces one name segment to the character set a model
// tool name may use.
//
// A separator inside a segment survives as two underscores, which is why a
// server name that already contains one stays ambiguous after sanitisation.
// That is not fixed here: the registry is what executes, and a tool whose
// name cannot be addressed is refused where the names are composed, not
// silently mangled into something that addresses the wrong server.
func SanitizeMCPSegment(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// MCPServerOf splits a composed name back into its server and tool.
//
// It returns ok=false for a name with no separator, or with an empty half on
// either side. The two are reported together because a name like "__x" is
// not a tool with a missing server; it is not a name this deployment ever
// mints, and a caller that treated it as "server is empty" would then try to
// look up the empty server.
func MCPServerOf(name string) (server, tool string, ok bool) {
	idx := strings.Index(name, MCPToolSeparator)
	if idx <= 0 || idx+len(MCPToolSeparator) >= len(name) {
		return "", "", false
	}
	return name[:idx], name[idx+len(MCPToolSeparator):], true
}
