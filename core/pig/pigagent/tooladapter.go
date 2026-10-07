// Package pigagent adapts OpsKeeper's tool and agent contracts onto PiG's
// agent loop.
//
// Everything in this package is a boundary. Callers inside OpsKeeper speak
// core/ports; PiG speaks its own types; the two meet here and nowhere else.
// When PiG's agent API moves, this is the only file that has to follow.
package pigagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Adapter presents one OpsKeeper tool to PiG's agent loop.
//
// The adapter is stateless beyond the cached schema, so a single Tool
// instance may back several adapters across concurrent turns. Schema
// resolution is done once: a tool's metadata is fixed for the process
// lifetime, and re-deriving it per call would put a JSON unmarshal on the
// hot path of every tool invocation.
type Adapter struct {
	tool   ports.Tool
	schema ai.ToolSchema
	// mode is the tool's parallelism preference. OpsKeeper's tool contract
	// does not yet express one, so every tool runs in parallel. This is the
	// conservative default for read-only tools and the wrong default for a
	// mutating one, which is why the loop's approval gate — not this
	// field — is what serialises destructive work.
	mode agent.ToolExecutionMode
}

// NewAdapter wraps a tool for PiG. It returns an error when the tool's
// schema cannot be converted, because a tool the model cannot call is
// worse than a tool that is absent: it advertises a capability that does
// not work.
func NewAdapter(tool ports.Tool) (*Adapter, error) {
	if tool == nil {
		return nil, fmt.Errorf("pigagent: nil tool")
	}
	src := tool.Schema()
	if src.Name == "" {
		return nil, fmt.Errorf("pigagent: tool has no name")
	}

	params, err := decodeParams(src.Parameters)
	if err != nil {
		return nil, fmt.Errorf("pigagent: tool %q: %w", src.Name, err)
	}

	// The routing hint is folded into the description. OpsKeeper keeps
	// WhenToUse separate so a skill manifest can override one without
	// rewriting the other, but the model sees a single blurb, so the
	// adapter joins them at the last moment rather than at authoring time.
	desc := src.Description
	if hint := strings.TrimSpace(src.WhenToUse); hint != "" {
		if desc == "" {
			desc = hint
		} else {
			desc = desc + " When to use: " + hint
		}
	}

	return &Adapter{
		tool: tool,
		schema: ai.ToolSchema{
			Name:        src.Name,
			Description: desc,
			Parameters:  params,
		},
		mode: agent.ToolModeParallel,
	}, nil
}

// NewAdapters wraps a whole bag, preserving order. A tool that fails to
// adapt is reported by name and index so an operator can find the offending
// registration; adapters are not silently dropped, because a silently
// missing tool shows up as a mysterious model refusal much later.
func NewAdapters(tools []ports.Tool) ([]agent.AgentTool, error) {
	out := make([]agent.AgentTool, 0, len(tools))
	for i, t := range tools {
		a, err := NewAdapter(t)
		if err != nil {
			return nil, fmt.Errorf("pigagent: tool[%d]: %w", i, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// Name implements agent.AgentTool.
func (a *Adapter) Name() string { return a.schema.Name }

// Label implements agent.AgentTool. An empty label falls back to the name
// in PiG's display path; OpsKeeper sets the name only, so the fallback is
// what renders.
func (a *Adapter) Label() string { return "" }

// Schema implements agent.AgentTool.
func (a *Adapter) Schema() ai.ToolSchema { return a.schema }

// ExecutionMode implements agent.AgentTool.
func (a *Adapter) ExecutionMode() agent.ToolExecutionMode { return a.mode }

// Execute implements agent.AgentTool.
//
// A tool that returns an error is reported to the model as a failed call
// rather than aborting the turn: a diagnostic that cannot reach a host must
// tell the agent what went wrong so it can try a different path. A
// cancellation is the exception and propagates, because retrying a call the
// operator just stopped is not the model's decision to make.
func (a *Adapter) Execute(
	ctx context.Context,
	toolCallID string,
	params json.RawMessage,
	onUpdate agent.ToolUpdateCallback,
) (agent.AgentToolResult, error) {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}

	// Stamp the provider-assigned call id onto ctx. The tool contract fixes
	// the Invoke signature, so the id cannot ride as a parameter; every kernel
	// threads a ctx down to the tool, so it rides there instead. Consumers
	// are host-side: the approval proposer that pairs the approval card with
	// the streaming card of this call, the persistence handler that pairs a
	// call OnStart with its OnEnd, and the transcript writer that must echo
	// the real id back or a strict provider rejects the turn. PiG can see
	// none of them, which is why the stamp belongs here and not in the loop.
	// An empty id is a no-op — see ports.WithToolCallID.
	ctx = ports.WithToolCallID(ctx, toolCallID)

	// Normalise before validation when the tool asks for it. Models emit
	// flat legacy shapes often enough that validating first turns a
	// recoverable call into a hard failure.
	if prep, ok := a.tool.(ports.ArgumentPreparer); ok {
		prepared, err := prep.PrepareArguments(params)
		if err != nil {
			return textResult(a.Name(), fmt.Sprintf("invalid arguments: %v", err)), nil
		}
		params = prepared
	}

	out, err := a.tool.Invoke(ctx, params)

	// A cancelled run must terminate the turn, not feed a result into the
	// transcript. This is checked after the call as well as on the error
	// path: a tool that ignores its context and returns normally must
	// still not have its output recorded, because the operator asked for
	// this turn to stop.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return agent.AgentToolResult{}, ctxErr
	}

	if err != nil {
		return agent.AgentToolResult{
			Content: []ai.ToolResultMessageContent{ai.TextContent{Text: out}},
			Details: nil,
			IsError: true,
			Preview: previewOf(out),
		}, nil
	}

	result := agent.AgentToolResult{
		Content: []ai.ToolResultMessageContent{ai.TextContent{Text: out}},
		IsError: false,
		Preview: previewOf(out),
	}
	if onUpdate != nil && out != "" {
		// Deliver the result as a progress frame too, so a long tool's
		// console tile fills before the call settles rather than only at
		// the end.
		//
		// v0.4.0 changed the callback from `func(content string, details any)`
		// to `func(partial AgentToolResult)`, and its contract is now that each
		// update is a complete-so-far snapshot rather than a delta. Sending
		// the same value that is about to be returned is therefore the
		// faithful translation -- under the old contract `out` was the whole
		// text as well, so the console tile shows the same bytes either way.
		onUpdate(result)
	}
	return result, nil
}

// textResult builds a one-block text result.
func textResult(name, text string) agent.AgentToolResult {
	return agent.AgentToolResult{
		Content: []ai.ToolResultMessageContent{ai.TextContent{Text: text}},
		IsError: true,
		Preview: previewOf(text),
	}
}

// previewLen bounds the collapsed-view summary. A tool returning a megabyte
// of JSON must not put that megabyte into a header the console renders in a
// fixed-height tile.
const previewLen = 160

func previewOf(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Collapse newlines: a preview is a one-line summary.
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= previewLen {
		return s
	}
	return s[:previewLen] + "…"
}

// decodeParams converts the tool's declared JSON Schema into the map form
// PiG's tool schema carries. An absent or empty schema becomes an
// object-with-no-properties, which is what a parameterless tool means;
// treating it as absent would let a model send arguments a tool cannot
// accept.
func decodeParams(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return emptyObjectSchema(), nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode parameters: %w", err)
	}
	if out == nil {
		return emptyObjectSchema(), nil
	}
	return out, nil
}

func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
