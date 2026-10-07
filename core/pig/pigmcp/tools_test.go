package pigmcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigmcp"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// fakeCatalogue records what it was asked for and answers from a script. It
// stands in for the manager's implementation, which has its own tests; what
// this package has to prove is that the tool it produces is a real, gated,
// callable agent tool.
type fakeCatalogue struct {
	mu    sync.Mutex
	tools []ports.MCPTool

	// calls records every Call, in order, as name plus arguments.
	calls []string
	// reply is what Call returns on success.
	reply string
	// err is what Call returns instead, when set.
	err error

	// toolsErr is what Tools returns instead, when set.
	toolsErr error
}

func (f *fakeCatalogue) Tools(context.Context) ([]ports.MCPTool, error) {
	if f.toolsErr != nil {
		return nil, f.toolsErr
	}
	return f.tools, nil
}

func (f *fakeCatalogue) Call(_ context.Context, name string, args map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(args)
	f.calls = append(f.calls, name+" "+string(raw))
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

func (f *fakeCatalogue) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func oneTool() ports.MCPTool {
	return ports.MCPTool{
		Name:        "mcp__grafana__query_dashboard",
		Server:      "grafana",
		Tool:        "query_dashboard",
		Description: "Run a dashboard query.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
	}
}

// TestTheCatalogueDecidesTheToolSet is the pass-through contract. A name, a
// description or a schema that the bridge rewrites is a menu the executor
// cannot satisfy, and a third party's vocabulary is not this repository's to
// edit.
func TestTheCatalogueDecidesTheToolSet(t *testing.T) {
	cat := &fakeCatalogue{tools: []ports.MCPTool{oneTool()}}

	tools, err := pigmcp.New(cat).Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	tool := tools[0]
	if tool.Name() != "mcp__grafana__query_dashboard" {
		t.Errorf("name = %q, want the composed name the catalogue published", tool.Name())
	}
	if tool.Schema().Description != "Run a dashboard query." {
		t.Errorf("description = %q, want the server's own", tool.Schema().Description)
	}
	params, ok := tool.Schema().Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("parameters did not survive as a JSON object: %#v", tool.Schema().Parameters)
	}
	if _, ok := params["q"]; !ok {
		t.Errorf("schema properties = %#v, want the server's q", params)
	}
	// Parallel is argued for in the package comment — one slow third-party
	// tool must not stretch every other tool in the same turn — so it gets
	// an assertion rather than a comment nobody checks.
	if mode := tool.ExecutionMode(); mode != agent.ToolModeParallel {
		t.Errorf("execution mode = %q, want parallel", mode)
	}
}

// TestExecuteAddressesTheToolByItsComposedName pins the one thing the bridge
// must not improvise. The catalogue is what authorises the call, so the
// adapter has to address it the way the catalogue publishes — otherwise the
// check that decides whether this may run is not the check that ran.
func TestExecuteAddressesTheToolByItsComposedName(t *testing.T) {
	cat := &fakeCatalogue{tools: []ports.MCPTool{oneTool()}, reply: "42 dashboards"}

	tools, err := pigmcp.New(cat).Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	res, err := tools[0].Execute(context.Background(), "call-1", json.RawMessage(`{"q":"p99"}`), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := cat.called()
	if len(got) != 1 {
		t.Fatalf("catalogue called %v, want exactly once", got)
	}
	if !strings.HasPrefix(got[0], "mcp__grafana__query_dashboard ") {
		t.Errorf("catalogue received %q, want the composed name", got[0])
	}
	if !strings.Contains(got[0], `"q":"p99"`) {
		t.Errorf("arguments were not passed through: %q", got[0])
	}
	if res.Text() != "42 dashboards" {
		t.Errorf("result text = %q, want the server's own", res.Text())
	}
}

// TestAFailedCallIsAnErrorRatherThanAnApology keeps "the server refused" and
// "this returned nothing" from looking alike to a model deciding what to do
// next. A successful result carrying an error is content the model may quote
// as though the operation worked.
func TestAFailedCallIsAnErrorRatherThanAnApology(t *testing.T) {
	cat := &fakeCatalogue{tools: []ports.MCPTool{oneTool()}, err: errors.New("grafana is down")}

	tools, err := pigmcp.New(cat).Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	res, err := tools[0].Execute(context.Background(), "call-1", json.RawMessage(`{}`), nil)
	if err == nil {
		t.Fatalf("a failed call returned a result: %q", res.Text())
	}
	// The name has to be in the message: the model is the one reading it,
	// and "grafana is down" without the tool is ambiguous when the turn
	// called three of them.
	if !strings.Contains(err.Error(), "mcp__grafana__query_dashboard") {
		t.Errorf("error %q does not name the tool", err)
	}
}

// TestAnAbsentCatalogueIsAnErrorNotAnEmptyList keeps "MCP is switched off"
// distinguishable from "MCP is on and offers nothing", because only one of
// them is a configuration mistake.
func TestAnAbsentCatalogueIsAnErrorNotAnEmptyList(t *testing.T) {
	if _, err := pigmcp.New(nil).Tools(context.Background()); err == nil {
		t.Fatal("a bridge with no catalogue reported an empty tool set")
	}
}

// newFauxModel builds a scripted model, so a turn's tool call is a fact
// rather than a hope. A test that waits for a live model to decide to call a
// tool proves nothing on a run where it does not.
func newFauxModel(t *testing.T, steps ...ai.FauxResponseStep) *ai.Model {
	t.Helper()
	provider := ai.NewFauxProvider(ai.FauxConfig{
		Model:           "faux-1",
		Models:          []ai.FauxModelDefinition{{ID: "faux-1", Name: "Faux"}},
		TokensPerSecond: 1_000_000,
	})
	provider.SetResponses(steps)
	model := provider.GetModel("faux-1")
	if model == nil {
		t.Fatal("faux provider did not register faux-1")
	}
	return model
}

func scriptedModel(t *testing.T) *ai.Model {
	t.Helper()
	return newFauxModel(t,
		ai.FauxStaticStep(ai.FauxResponse{
			Content:    []ai.FauxContentBlock{ai.FauxToolCall("mcp__grafana__query_dashboard", map[string]any{"q": "p99"}, "tc-1")},
			StopReason: "toolUse",
		}),
		ai.FauxStaticStep(ai.FauxResponse{
			Content:    []ai.FauxContentBlock{ai.FauxText("the p99 panel is flat")},
			StopReason: "stop",
		}),
	)
}

// TestAnUngatedMCPToolCallReachesTheCatalogueAndTheTranscript is the positive
// half: the tool is a real agent tool, callable by the loop, and its answer
// comes back into the conversation.
func TestAnUngatedMCPToolCallReachesTheCatalogueAndTheTranscript(t *testing.T) {
	cat := &fakeCatalogue{tools: []ports.MCPTool{oneTool()}, reply: "42 dashboards"}
	tools, err := pigmcp.New(cat).Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}

	ag := agent.NewAgent(agent.AgentOptions{
		Model:        scriptedModel(t),
		Tools:        tools,
		SystemPrompt: "You are a concise assistant.",
		MaxTurns:     4,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := ag.Send(ctx, "how is the p99 panel doing"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(cat.called()) != 1 {
		t.Fatalf("catalogue called %v, want exactly one call", cat.called())
	}
	// The agent's own history, not Send's return value: a prompt run
	// returns the messages it produced, and a tool result is appended to
	// the conversation rather than returned by the run that caused it.
	if !strings.Contains(transcript(ag.Messages()), "42 dashboards") {
		t.Errorf("the tool's answer is not in the conversation:\n%s", transcript(ag.Messages()))
	}
}

// TestTheHostGateBlocksAnMCPToolCall is the claim this package exists to make
// safe: an MCP server is not a way around the host's policy.
//
// It runs the same real agent loop with a BeforeToolCall hook, and asserts
// the catalogue was never reached. A bridge that reached it anyway would leave
// every other guarantee in this repository intact and still be a hole, which
// is why this test exists rather than a comment claiming it does not happen.
func TestTheHostGateBlocksAnMCPToolCall(t *testing.T) {
	cat := &fakeCatalogue{tools: []ports.MCPTool{oneTool()}, reply: "42 dashboards"}
	tools, err := pigmcp.New(cat).Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}

	var blocked string
	ag := agent.NewAgent(agent.AgentOptions{
		Model:        scriptedModel(t),
		Tools:        tools,
		SystemPrompt: "You are a concise assistant.",
		MaxTurns:     4,
		BeforeToolCall: []agent.BeforeToolCallHook{
			func(_ context.Context, _, name string, _ json.RawMessage) agent.ToolCallHookResult {
				blocked = name
				return agent.ToolCallHookResult{
					Block:     true,
					Terminate: true,
					Reason:    "this profile may not query Grafana",
				}
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := ag.Send(ctx, "how is the p99 panel doing"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if blocked != "mcp__grafana__query_dashboard" {
		t.Errorf("the gate saw %q; an MCP tool that the gate cannot see is a second door", blocked)
	}
	if calls := cat.called(); len(calls) != 0 {
		t.Errorf("a blocked MCP call still reached the catalogue: %v", calls)
	}
}

// transcript renders both halves of the conversation. The tool result is the
// half worth seeing here: it is where a routed call's answer actually lands,
// and reading only the assistant's text would let a bridge that dropped every
// result still pass.
func transcript(msgs []agent.AgentMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		// Tool results first, and before the assistant nil check: a tool
		// result message is precisely the one with no assistant half, so a
		// helper that skipped those would skip exactly what this test is
		// here to see.
		if m.ToolResult != nil {
			b.WriteString("[tool] ")
			b.WriteString(m.ToolResult.Text())
			b.WriteString("\n")
		}
		if m.Assistant == nil {
			continue
		}
		for _, block := range m.Assistant.Content {
			if text, ok := block.(ai.TextContent); ok {
				b.WriteString(text.Text)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}
