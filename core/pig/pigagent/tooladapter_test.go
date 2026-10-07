package pigagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// fakeTool is a ports.Tool whose behaviour each test dictates.
type fakeTool struct {
	schema ports.ToolSchema
	out    string
	err    error
	// gotArgs records the arguments the tool was actually invoked with.
	gotArgs json.RawMessage
	// gotToolCallID records the tool-call id the adapter stamped onto ctx.
	gotToolCallID string
	// cancelCtx makes Invoke return a context error, standing in for a
	// cancelled run.
	cancelCtx bool
	// calls counts invocations. gotArgs alone cannot answer "did this tool
	// run at all", because a tool called with empty arguments leaves it
	// indistinguishable from one that was never called — and "the gate
	// refused it, so it did not run" is the assertion that matters.
	calls int
}

// Calls reports how many times the tool was invoked.
func (f *fakeTool) Calls() int { return f.calls }

func (f *fakeTool) Schema() ports.ToolSchema { return f.schema }

func (f *fakeTool) Invoke(ctx context.Context, args json.RawMessage) (string, error) {
	f.calls++
	f.gotArgs = args
	f.gotToolCallID = ports.ToolCallIDFromContext(ctx)
	if f.cancelCtx && ctx.Err() == nil {
		return "", errors.New("should not be reached")
	}
	return f.out, f.err
}

func newTool(name string) *fakeTool {
	return &fakeTool{schema: ports.ToolSchema{
		Name:        name,
		Description: "does a thing",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		Class:       domain.ClassRead,
	}}
}

func TestAdapterExposesSchema(t *testing.T) {
	a, err := NewAdapter(newTool("get_topology"))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if a.Name() != "get_topology" {
		t.Errorf("Name() = %q", a.Name())
	}
	if a.Label() != "" {
		t.Errorf("Label() should be empty so PiG falls back to the name, got %q", a.Label())
	}
	if a.Schema().Description != "does a thing" {
		t.Errorf("Description = %q", a.Schema().Description)
	}
	if a.Schema().Parameters["type"] != "object" {
		t.Errorf("Parameters did not decode: %v", a.Schema().Parameters)
	}
}

func TestAdapterFoldsWhenToUseIntoDescription(t *testing.T) {
	tool := newTool("query_db")
	tool.schema.WhenToUse = "prefer over read_file for SQL questions"
	a, err := NewAdapter(tool)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	desc := a.Schema().Description
	if !strings.Contains(desc, "does a thing") || !strings.Contains(desc, "prefer over read_file") {
		t.Errorf("description should carry both halves, got %q", desc)
	}
}

func TestAdapterWhenToUseAloneBecomesDescription(t *testing.T) {
	tool := newTool("x")
	tool.schema.Description = ""
	tool.schema.WhenToUse = "only thing"
	a, _ := NewAdapter(tool)
	if a.Schema().Description != "only thing" {
		t.Errorf("Description = %q, want the hint alone", a.Schema().Description)
	}
}

func TestAdapterEmptyParamsBecomeEmptyObject(t *testing.T) {
	// A parameterless tool must still advertise an object schema, or a
	// model may send arguments the tool cannot accept.
	tool := newTool("list_nodes")
	tool.schema.Parameters = nil
	a, err := NewAdapter(tool)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	p := a.Schema().Parameters
	if p["type"] != "object" {
		t.Errorf("type = %v, want object", p["type"])
	}
	if _, ok := p["properties"]; !ok {
		t.Error("an absent schema must still declare properties")
	}
}

func TestAdapterRejectsUnusableTools(t *testing.T) {
	cases := []struct {
		name string
		tool ports.Tool
	}{
		{"nil tool", nil},
		{"unnamed", &fakeTool{schema: ports.ToolSchema{}}},
		{"malformed parameters", &fakeTool{schema: ports.ToolSchema{
			Name: "broken", Parameters: json.RawMessage(`{not json`),
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewAdapter(c.tool); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestNewAdaptersReportsFailingIndex(t *testing.T) {
	good := newTool("ok")
	bad := &fakeTool{schema: ports.ToolSchema{}}
	_, err := NewAdapters([]ports.Tool{good, good, bad})
	if err == nil {
		t.Fatal("expected an error")
	}
	// The index is what lets an operator find the registration.
	if !strings.Contains(err.Error(), "tool[2]") {
		t.Errorf("error should name the failing index, got %v", err)
	}
}

func TestExecuteReturnsTextResult(t *testing.T) {
	tool := newTool("get_topology")
	tool.out = `{"nodes":3}`
	a, _ := NewAdapter(tool)

	res, err := a.Execute(context.Background(), "call-1", json.RawMessage(`{"a":"b"}`), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Error("a successful call must not be marked as an error")
	}
	if res.Text() != `{"nodes":3}` {
		t.Errorf("Text() = %q", res.Text())
	}
	if string(tool.gotArgs) != `{"a":"b"}` {
		t.Errorf("tool received %q", tool.gotArgs)
	}
}

func TestExecuteStampsToolCallIDOnContext(t *testing.T) {
	// The id must reach the tool through ctx. Downstream consumers are
	// host-side and cannot be reached from the kernel: the approval proposer
	// pairs an approval card with the streaming card of THIS call, and the
	// persistence handler pairs a call OnStart with its OnEnd. Reconstructing
	// the id from completion order instead is what produced orphaned tool
	// results and provider 400s when parallel tools settled out of order.
	tool := newTool("get_topology")
	a, _ := NewAdapter(tool)
	if _, err := a.Execute(context.Background(), "call_abc123", json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if tool.gotToolCallID != "call_abc123" {
		t.Errorf("tool saw id %q, want call_abc123", tool.gotToolCallID)
	}
}

func TestExecuteWithoutCallIDLeavesContextBare(t *testing.T) {
	// A tool invoked outside a loop (a test, a scheduled job) has no
	// model-assigned call. It must see "" rather than a fabricated id: a
	// made-up id mis-pairs the result with an unrelated call and is worse
	// than no id, which consumers already treat as unknown.
	tool := newTool("ping")
	a, _ := NewAdapter(tool)
	if _, err := a.Execute(context.Background(), "", json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if tool.gotToolCallID != "" {
		t.Errorf("tool saw id %q, want empty", tool.gotToolCallID)
	}
}

func TestExecuteSubstitutesEmptyObjectForNilArgs(t *testing.T) {
	tool := newTool("ping")
	a, _ := NewAdapter(tool)
	if _, err := a.Execute(context.Background(), "c", nil, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(tool.gotArgs) != "{}" {
		t.Errorf("tool received %q, want {}", tool.gotArgs)
	}
}

func TestExecuteReportsToolErrorToModelNotAsLoopFailure(t *testing.T) {
	// A diagnostic that cannot reach a host must tell the agent what went
	// wrong so it can pick another path. Only cancellation propagates.
	tool := newTool("get_topology")
	tool.err = errors.New("dial tcp: connection refused")
	tool.out = "topology unavailable: dial tcp: connection refused"
	a, _ := NewAdapter(tool)

	res, err := a.Execute(context.Background(), "c", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("a tool error must not fail the turn, got %v", err)
	}
	if !res.IsError {
		t.Error("result should be marked as an error")
	}
	if !strings.Contains(res.Text(), "connection refused") {
		t.Errorf("the model must see why it failed, got %q", res.Text())
	}
}

func TestExecutePropagatesCancellation(t *testing.T) {
	tool := newTool("bash")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a, _ := NewAdapter(tool)
	_, err := a.Execute(ctx, "c", json.RawMessage(`{}`), nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context must propagate, got %v", err)
	}
}

func TestExecuteDeliversProgressUpdate(t *testing.T) {
	tool := newTool("scan")
	tool.out = "scanned 40 hosts"
	a, _ := NewAdapter(tool)

	var got []string
	updater := agent.ToolUpdateCallback(func(partial agent.AgentToolResult) { got = append(got, partial.Text()) })
	if _, err := a.Execute(context.Background(), "c", json.RawMessage(`{}`), updater); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got) != 1 || got[0] != "scanned 40 hosts" {
		t.Errorf("progress updates = %v", got)
	}
}

// prepTool normalises its input before the adapter hands it over.
type prepTool struct {
	fakeTool
	prepared json.RawMessage
	prepErr  error
}

func (p *prepTool) PrepareArguments(raw json.RawMessage) (json.RawMessage, error) {
	if p.prepErr != nil {
		return nil, p.prepErr
	}
	return p.prepared, nil
}

func TestExecuteNormalisesBeforeValidation(t *testing.T) {
	tool := &prepTool{
		fakeTool: fakeTool{schema: ports.ToolSchema{Name: "edit", Parameters: json.RawMessage(`{"type":"object"}`)}, out: "ok"},
		prepared: json.RawMessage(`{"edits":[{"old":"a","new":"b"}]}`),
	}
	a, err := NewAdapter(tool)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if _, err := a.Execute(context.Background(), "c", json.RawMessage(`{"old":"a","new":"b"}`), nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(tool.gotArgs) != `{"edits":[{"old":"a","new":"b"}]}` {
		t.Errorf("tool received %q, want the normalised shape", tool.gotArgs)
	}
}

func TestExecuteNormalisationFailureBecomesToolResult(t *testing.T) {
	tool := &prepTool{
		fakeTool: fakeTool{schema: ports.ToolSchema{Name: "edit", Parameters: json.RawMessage(`{"type":"object"}`)}},
		prepErr:  errors.New("oldText is required"),
	}
	a, _ := NewAdapter(tool)
	res, err := a.Execute(context.Background(), "c", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Error("a normalisation failure should surface as a tool error")
	}
	if !strings.Contains(res.Text(), "oldText is required") {
		t.Errorf("model should see the reason, got %q", res.Text())
	}
}

func TestAdapterSatisfiesAgentToolInterface(t *testing.T) {
	// Compile-time proof the adapter can be handed to PiG's loop.
	a, _ := NewAdapter(newTool("x"))
	var _ agent.AgentTool = a
}

func TestPreviewIsBoundedAndSingleLine(t *testing.T) {
	long := strings.Repeat("x", 5000) + "\n" + strings.Repeat("y", 100)
	got := previewOf(long)
	if len([]rune(got)) > previewLen+1 {
		t.Errorf("preview is %d runes, want <= %d", len([]rune(got)), previewLen+1)
	}
	if strings.Contains(got, "\n") {
		t.Error("preview must be a single line")
	}
}

func TestPreviewOfEmptyIsEmpty(t *testing.T) {
	if got := previewOf("   \n  "); got != "" {
		t.Errorf("previewOf(blank) = %q, want empty", got)
	}
}

func TestSchemaParametersDecodeRejectsScalarJSON(t *testing.T) {
	// A tool whose "schema" is a JSON array is not a schema; rejecting it
	// at construction beats failing every call at runtime.
	tool := &fakeTool{schema: ports.ToolSchema{Name: "x", Parameters: json.RawMessage(`[]`)}}
	if _, err := NewAdapter(tool); err == nil {
		t.Error("a non-object parameter schema should be rejected")
	}
}

var _ = ai.TextContent{}
