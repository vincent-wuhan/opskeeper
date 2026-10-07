package pigagent

import (
	"encoding/json"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
)

// The conversion is the persistence path whole risk: a field dropped here
// replays as an orphaned tool result, which a strict provider rejects on a
// LATER turn with an HTTP 400 that names neither this function nor the turn
// that caused it. One case per role, plus the two drops.

func TestToPortsMessageUser(t *testing.T) {
	got := toPortsMessage(agent.AgentMessage{User: &agent.UserMessage{
		Role:    "user",
		Content: ai.UserText("is the db ok?"),
	}}, "gpt-5")
	if got.Role != "user" || got.Content != "is the db ok?" {
		t.Fatalf("got %+v", got)
	}
	if got.Model != "" {
		t.Error("a user turn must not be attributed to a model")
	}
}

func TestToPortsMessageUserBlockContent(t *testing.T) {
	// A user turn can arrive as a block list rather than a plain string;
	// reading only the string shape would silently persist an empty turn.
	got := toPortsMessage(agent.AgentMessage{User: &agent.UserMessage{
		Role:    "user",
		Content: ai.UserContentBlocks{ai.TextContent{Text: "first"}, ai.TextContent{Text: "second"}},
	}}, "")
	if got.Content != "first\nsecond" {
		t.Fatalf("content = %q", got.Content)
	}
}

func TestToPortsMessageSystem(t *testing.T) {
	got := toPortsMessage(agent.AgentMessage{System: &ai.SystemMessage{
		Content: ai.SystemText("you are an SRE"),
	}}, "")
	if got.Role != "system" || got.Content != "you are an SRE" {
		t.Fatalf("got %+v", got)
	}
}

func TestToPortsMessageAssistantCarriesToolCallsAndUsage(t *testing.T) {
	msg := agent.AgentMessage{Assistant: &agent.AssistantMessage{
		Role: "assistant",
		Content: []ai.AssistantContentBlock{
			ai.TextContent{Text: "checking"},
			ai.ToolCall{ID: "call_1", Name: "query_promql", Arguments: ai.JsonObject{"query": "up"}},
		},
		Usage: &ai.Usage{Input: 11, Output: 4, CacheRead: 2, CacheWrite: 1},
	}}
	got := toPortsMessage(msg, "glm-4.7")
	if got.Role != "assistant" || got.Content != "checking" || got.Model != "glm-4.7" {
		t.Fatalf("got %+v", got)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(got.ToolCalls))
	}
	tc := got.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "query_promql" {
		t.Fatalf("tool call = %+v", tc)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("arguments are not a JSON object: %v (%s)", err, tc.Arguments)
	}
	if args["query"] != "up" {
		t.Fatalf("arguments = %v", args)
	}
	if got.Usage == nil {
		t.Fatal("usage was dropped; the ledger would lose the row")
	}
	if got.Usage.InputTokens != 11 || got.Usage.OutputTokens != 4 ||
		got.Usage.CacheReadTokens != 2 || got.Usage.CacheWriteTokens != 1 {
		t.Fatalf("usage = %+v", *got.Usage)
	}
}

func TestToPortsMessageToolResultKeepsTheCallID(t *testing.T) {
	// The id is what pairs the result with its request. Dropping it is the
	// exact failure that makes a strict provider reject the next turn.
	got := toPortsMessage(agent.AgentMessage{ToolResult: &agent.ToolResultMessage{
		Role:       "toolResult",
		ToolCallID: "call_1",
		ToolName:   "query_promql",
		Content:    []ai.ToolResultMessageContent{ai.TextContent{Text: "1"}},
	}}, "")
	if got.Role != "tool" || got.ToolCallID != "call_1" || got.ToolName != "query_promql" {
		t.Fatalf("got %+v", got)
	}
	if got.Content != "1" {
		t.Fatalf("content = %q", got.Content)
	}
}

func TestToPortsMessageDropsAnEmptyAssistantTurn(t *testing.T) {
	// An assistant entry with neither text nor a call carries nothing, and
	// most providers reject an empty assistant turn on replay.
	got := toPortsMessage(agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant"}}, "")
	if got.Role != "" {
		t.Fatalf("expected a dropped message, got %+v", got)
	}
}

func TestToPortsMessageDropsAnUnrecognisedMessage(t *testing.T) {
	if got := toPortsMessage(agent.AgentMessage{}, ""); got.Role != "" {
		t.Fatalf("expected a dropped message, got %+v", got)
	}
}

func TestMarshalArgumentsEmptyBecomesEmptyObject(t *testing.T) {
	// A parameterless call is an empty object, not a null; a null argument
	// set replays as unparseable.
	if got := string(marshalArguments(nil)); got != "{}" {
		t.Fatalf("nil -> %q, want {}", got)
	}
	if got := string(marshalArguments(ai.JsonObject{})); got != "{}" {
		t.Fatalf("empty -> %q, want {}", got)
	}
}
