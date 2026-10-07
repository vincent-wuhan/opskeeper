package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/mcp"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/mcpclient"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The agent-facing catalogue: which registered MCP servers' tools this
// deployment offers, and how one of them is run.
//
// Everything the registration side already does — CRUD validation, credential
// resolution, header templating, the initialize/tools/list probe — happens
// before this file. What was missing is the step after it: the probe result
// was cached onto the row and nothing ever read it. Usecase.ListEnabled says
// so in its own comment ("boot connects to these to pull tools and register
// them into the agent toolbag") and no boot has ever done that, which is why
// an operator could register a working MCP server, see its probe go green,
// and still find an agent that has never heard of it.
//
// # Discovery reads the cache, not the servers
//
// Tools does not call ListTools. The probe already ran — an operator ran it,
// or TestConnection did — and its answer is on the row.
//
// The reason is availability, and it is worth being concrete about the
// failure being avoided. A catalogue that re-probed on every call would put
// one network round trip per server in the path of starting a conversation,
// and it would make one unreachable server remove the tools of every healthy
// one, because a partial catalogue is indistinguishable from an empty one to
// the model. Reading the cache means a server that is down right now still
// offers its tools, and the failure surfaces where it belongs: on the call,
// as a transport error the model can be told about, with the other servers'
// tools still working.
//
// The cost is staleness, and it is bounded by something an operator already
// does: re-probing. That is the same action that refreshes the cache, so
// "the tool list is stale" has a fix that is already a documented button.

// The catalogue is the deployment's ports.MCPCatalogue. The assertion is
// here rather than at the composition root so that a signature drift breaks
// this package's build instead of surfacing as a wiring failure in whichever
// binary happens to construct the usecase.
var _ ports.MCPCatalogue = (*Usecase)(nil)

// mcpToolFilter narrows the catalogue to what one caller may see.
//
// It is a field rather than a parameter because the caller is not an argument
// to any method here — it arrives in the context, and threading it through
// explicitly would invite a call site to pass the wrong one. A nil filter
// offers everything, which is correct for a single-tenant deployment and
// wrong for a shared one; SetToolFilter exists so the composition root states
// which it is rather than inheriting the answer from a nil.
type mcpToolFilter func(ctx context.Context, tools []ports.MCPTool) []ports.MCPTool

// SetToolFilter installs the per-caller narrowing applied by Tools.
func (u *Usecase) SetToolFilter(f func(ctx context.Context, tools []ports.MCPTool) []ports.MCPTool) {
	u.toolFilter = f
}

// Tools lists the MCP tools this deployment offers.
//
// The order is sorted by composed name so that two callers in one process, or
// one caller on two nodes, see the same list in the same order. Discovery
// order follows the repository, and a tool bag whose order changes between
// calls makes a prompt cache miss for no reason and makes a diff of two
// snapshots unreadable.
func (u *Usecase) Tools(ctx context.Context) ([]ports.MCPTool, error) {
	servers, err := u.ListEnabled(ctx)
	if err != nil {
		return nil, err
	}

	// The map is what makes a collision a collision, and sanitisation is
	// what makes one possible. "My-Server" and "my_server" are different
	// strings and the database's unique index admits both, yet both
	// compose to mcp__my_server__*; within one server, "list datasources"
	// and "list-datasources" do the same. Neither is exotic — they are
	// what happens the first time somebody registers a server with a
	// hyphen in it and then a copy of it. One of the two would silently
	// answer for the other.
	seen := make(map[string]string)
	out := make([]ports.MCPTool, 0, len(servers))

	for _, s := range servers {
		if err := validServerName(s.Name); err != nil {
			return nil, err
		}
		cached, err := decodeToolCache(s)
		if err != nil {
			return nil, err
		}
		for _, t := range cached {
			name := ports.ComposeMCPToolName(s.Name, t.Name)
			if strings.TrimSpace(t.Name) == "" {
				return nil, fmt.Errorf("mcp server %q: published a tool with no name", s.Name)
			}
			if other, dup := seen[name]; dup {
				return nil, fmt.Errorf(
					"mcp tool %q is offered by both %q and server %q; one of the two names has to change",
					name, other, s.Name)
			}
			seen[name] = s.Name
			// The schema is checked here rather than at the point of use
			// because it is the host's relationship with a third party, and
			// the consumer of it is a module that must not be deciding what
			// a third party is allowed to publish. A tool whose schema does
			// not parse as a JSON object is one the model cannot be given a
			// usable signature for.
			if err := validToolSchema(t.InputSchema); err != nil {
				return nil, fmt.Errorf("mcp server %q: tool %q: %w", s.Name, t.Name, err)
			}
			out = append(out, ports.MCPTool{
				Name:        name,
				Server:      s.Name,
				Tool:        t.Name,
				Description: t.Description,
				Schema:      t.InputSchema,
			})
		}
	}

	if u.toolFilter != nil {
		out = u.toolFilter(ctx, out)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Call runs one tool by its composed name.
//
// The server is resolved by matching the name against the registered set
// rather than by splitting the name at the separator, and the difference is
// the security property of this function. Splitting would let a caller that
// knows the separator reach any server by writing "other__server__tool",
// and the ambiguity is not hypothetical: server names are admin-chosen and
// only the uniqueness of the whole string is enforced, so a name that splits
// one way for the model and another way for the lookup is a name that means
// two things. Matching against the registered set cannot be steered that
// way, because the set of prefixes is the set of servers this deployment
// actually has.
//
// A server whose own name contains the separator is refused here too, and by
// the same rule rather than by a second one. That case is not a refinement:
// server "a" offering "b__c" and server "a__b" offering "c" both compose to
// a__b__c, and "a__" is a prefix of it exactly as "a__b__" is, so a walk that
// took the first match would pick between two different servers by map
// iteration order. The same call would then reach a different server from one
// request to the next — a defect that reproduces on about half its runs and is
// invisible on the other half.
//
// Refusing in both places is deliberate even though Tools has already failed
// by the time a call arrives. The alternative — skip the bad server here and
// answer from the other one — produces a confident wrong answer, and a caller
// that cannot tell "this went to the wrong server" from "this worked" is the
// worst position to be in. One rule, reported once, from both directions.
//
// A name whose server is unknown, disabled, or was never probed is reported
// as ErrNoSuchMCPTool rather than as a connection failure. From the caller's
// side those are one fact — the tool is not on offer — and a caller that
// could not tell them apart would retry a refusal for ever.
func (u *Usecase) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	servers, err := u.ListEnabled(ctx)
	if err != nil {
		return "", err
	}
	// Every enabled server is checked before any of them is dispatched to,
	// so a deployment that cannot be addressed unambiguously is refused
	// even when the name that was called would have happened to resolve.
	// A caller that gets an answer has then got it from a deployment whose
	// names mean one thing each.
	for _, s := range servers {
		if err := validServerName(s.Name); err != nil {
			return "", err
		}
	}

	server, tool, ok := resolveMCPName(servers, name)
	if !ok {
		return "", fmt.Errorf("%w: %s", ports.ErrNoSuchMCPTool, name)
	}
	// The tool half came off a SANITISED name, and a server does not know
	// its tools by the sanitised spelling: it published "list datasources"
	// and will answer "no such tool" to "list_datasources". The probe
	// snapshot is the only place both spellings exist, so the round trip
	// goes through it.
	//
	// Falling back to the wire spelling is deliberate for a server with no
	// snapshot. It cannot have published the name, so the caller reached it
	// some other way, and refusing would break a direct tools/call for a
	// name that happens to need no sanitising — which is most of them.
	return u.CallTool(ctx, server, ownToolName(servers, server, tool, name), args)
}

// ownToolName maps a sanitised tool segment back to the spelling the server
// published, using the probe snapshot.
func ownToolName(servers []*model.Server, server, sanitized, wireName string) string {
	for _, s := range servers {
		if s.Name != server {
			continue
		}
		cached, err := decodeToolCache(s)
		if err != nil {
			return sanitized
		}
		for _, t := range cached {
			if ports.ComposeMCPToolName(s.Name, t.Name) == wireName {
				return t.Name
			}
		}
	}
	return sanitized
}

// resolveMCPName maps a wire name back to the registered server and the
// server's own tool name.
//
// It matches on the SANITISED server name including the mcp__ prefix, because
// that is what the wire name was composed from. A server registered as
// "My-Server" appears to the model as mcp__my_server__*, and a resolver that
// looked for the raw registration would answer "no such tool" for every call
// to a tool it had itself just published.
//
// The longest matching prefix wins. The caller has already refused a
// deployment whose server names overlap, so this cannot be ambiguous here;
// the comparison exists so that the choice is a property of the names rather
// than of the order the repository happened to return them in.
func resolveMCPName(servers []*model.Server, name string) (server, tool string, ok bool) {
	best := -1
	for _, s := range servers {
		p := ports.MCPToolNamePrefix + ports.SanitizeMCPSegment(s.Name) + ports.MCPToolSeparator
		if strings.HasPrefix(name, p) && len(p) > best {
			best = len(p)
			server = s.Name
		}
	}
	if best < 0 {
		return "", "", false
	}
	tool = name[best:]
	if tool == "" {
		return "", "", false
	}
	return server, tool, true
}

// decodeToolCache reads a server's probe snapshot.
//
// A server with no snapshot contributes nothing and is not an error: it was
// never probed, which is a state an operator creates by registering a server
// and not testing it yet. A snapshot that will not parse is an error, because
// that row was written by a successful probe and something has since changed
// its shape — silently treating it as "no tools" would turn a migration
// problem into an agent that has quietly lost a capability.
func decodeToolCache(s *model.Server) ([]mcpclient.Tool, error) {
	if strings.TrimSpace(s.ToolsCacheJSON) == "" {
		return nil, nil
	}
	var tools []mcpclient.Tool
	if err := json.Unmarshal([]byte(s.ToolsCacheJSON), &tools); err != nil {
		return nil, fmt.Errorf("mcp server %q: cached tool list is unreadable (%v); re-probe the server", s.Name, err)
	}
	return tools, nil
}

// validServerName rejects a server whose own name would make its tools'
// composed names ambiguous.
//
// The separator is legal in a server name today — nothing in the
// registration path forbids it — and a server called "a__b" produces
// "a__b__c", which is indistinguishable from server "a" tool "b__c". The
// registration side is not changed here, because an existing deployment may
// already have such a row and refusing to start is worse than refusing to
// offer its tools. The name is rejected at the point where it would cause a
// misrouted call, and the message says which server to rename.
//
// This rule is also what makes a duplicate composed name impossible, which is
// why Tools carries no collision scan. Two servers with different valid names
// have different prefixes, so "<a><sep><t>" and "<b><sep><u>" differ however
// the tool halves are spelled — including when one tool half contains the
// separator itself. That is worth stating rather than leaving as a reader's
// exercise, because the obvious defence against a future change here is to
// add a scan, and a scan that can never fire is worse than none: it reads as
// the thing standing between a deployment and two servers answering for each
// other, while the name rule is.
func validServerName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("mcp server has no name")
	}
	// The check is on the sanitised name because that is the one the wire
	// name is built from. A registration of "a-b" is unambiguous and
	// becomes "a_b"; a registration of "a__b" stays "a__b" and is not.
	if strings.Contains(ports.SanitizeMCPSegment(name), ports.MCPToolSeparator) {
		return fmt.Errorf("server name %q contains %q, which would make its tools indistinguishable from another server's", name, ports.MCPToolSeparator)
	}
	return nil
}

// validToolSchema rejects a tool whose input schema is not a JSON object.
//
// The type is what the model needs, not a nicety: a schema that is an array
// or a bare string has no properties to generate a call from, and handing it
// on produces a tool the model fills in wrongly every time rather than one
// that visibly fails.
//
// An absent schema is accepted. MCP permits a tool with no declared
// parameters, and a no-argument tool is a real thing; refusing it would make
// this deployment reject servers that are behaving correctly.
func validToolSchema(schema json.RawMessage) error {
	if len(strings.TrimSpace(string(schema))) == 0 {
		return nil
	}
	var probe map[string]any
	if err := json.Unmarshal(schema, &probe); err != nil {
		return fmt.Errorf("input schema is not a JSON object: %v", err)
	}
	if probe == nil {
		return fmt.Errorf("input schema is JSON null, which declares no parameters")
	}
	return nil
}

// There is deliberately no character check on a composed name here. The
// executing registry has always sanitised rather than refused — a tool named
// "list datasources" becomes mcp__grafana__list_datasources and works — and
// this catalogue composes names the same way for that reason. Refusing a name
// the registry is willing to serve would leave the two surfaces disagreeing
// about what exists, which is the exact failure the shared rule in core/ports
// was introduced to prevent.
//
// What sanitisation does not do is stay injective, and the collision map in
// Tools is where that is caught.
