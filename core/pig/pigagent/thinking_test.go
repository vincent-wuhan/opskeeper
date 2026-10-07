package pigagent

import (
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
)

// TestStripInlineThinking pins the three cases that decide whether an operator
// sees the model's private reasoning: a reasoning run followed by an answer, a
// message that is nothing but reasoning, and a partial stream that has opened
// a tag but not closed it.
func TestStripInlineThinking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "reasoning before the answer is removed",
			in:   "<think>The user wants a greeting.</think>\n\nHello.",
			want: "Hello.",
		},
		{
			name: "reasoning between answers is removed",
			in:   "first.<think>hm</think>second.",
			want: "first.second.",
		},
		{
			name: "an answer with no reasoning is untouched",
			in:   "Hello.",
			want: "Hello.",
		},
		{
			name: "an unterminated opening tag is left alone",
			in:   "<think>still thinking",
			want: "<think>still thinking",
		},
		{
			// This case used to want the tags back. Restoring them did not
			// "record that the model answered with no prose" — it recorded
			// the prose. MiniMax-M3 emits pure-reasoning turns between tool
			// calls, and each one landed in chat_messages and rendered as a
			// <think> bubble in the console.
			name: "reasoning-only comes back empty rather than with its tags",
			in:   "<think>all reasoning, no prose</think>",
			want: "",
		},
		{
			name: "reasoning-only between two tool calls comes back empty",
			in:   "<think>no such tool, try wider</think>",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stripInlineThinking(tc.in); got != tc.want {
				t.Errorf("stripInlineThinking(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestResultStripsInlineThinking covers the third copy of an assistant
// message — the one this package hands back to its caller.
//
// The Mapper and toPortsMessage already strip, so the console stream and the
// chat_messages row are clean. But a worker's turn returns its answer through
// result(), and that string becomes the AgentTool result the coordinator reads.
// Leaving it raw put a sub-agent's <think> block into chat_tool_calls and into
// the coordinator's prompt on every later turn — the exact reasoning this
// package withholds everywhere else, reintroduced through the one path that
// skipped the filter. Found by running a real MiniMax-M3 dispatch.
func TestResultStripsInlineThinking(t *testing.T) {
	t.Parallel()

	run, _ := harness(t, Deps{})
	res := run.result([]agent.AgentMessage{
		{Assistant: &agent.AssistantMessage{
			Content: []ai.AssistantContentBlock{
				ai.TextContent{Text: "<think>weighing the evidence</think>The edge is offline."},
			},
		}},
	})
	if strings.Contains(res.Content, "<think>") {
		t.Errorf("result() leaked inline reasoning: %q", res.Content)
	}
	if res.Content != "The edge is offline." {
		t.Errorf("result() = %q, want %q", res.Content, "The edge is offline.")
	}
}

// TestAPureReasoningTurnNeverReachesTheTranscript pins the consequence of the
// empty-string case above at the boundary that actually leaked.
//
// stripInlineThinking returning "" is only worth anything if the sink drops
// the row. toPortsMessage is that sink, and it has two distinct outcomes that
// matter in opposite directions:
//
//   - a reasoning-only turn with NO tool call carries nothing, so it is not
//     persisted at all — this is the leak being closed;
//   - a reasoning-only turn that DOES own tool calls must survive with empty
//     text, because dropping it would orphan its tool results on replay and
//     most providers reject an assistant-less tool message.
//
// The second case is why the fix cannot be "always skip empty assistants",
// and it is the one a reader of the first case would not think to check.
func TestAPureReasoningTurnNeverReachesTheTranscript(t *testing.T) {
	t.Parallel()

	reasoningOnly := agent.AgentMessage{Assistant: &agent.AssistantMessage{
		Content: []ai.AssistantContentBlock{
			ai.TextContent{Text: "<think>no such tool</think>"},
		},
	}}
	if got := toPortsMessage(reasoningOnly, "m"); got.Role != "" {
		t.Errorf("a reasoning-only turn with no tool call was persisted: %+v", got)
	}

	ownsCalls := agent.AgentMessage{Assistant: &agent.AssistantMessage{
		Content: []ai.AssistantContentBlock{
			ai.TextContent{Text: "<think>no such tool</think>"},
			ai.ToolCall{ID: "call-1", Name: "ToolSearch", Arguments: ai.JsonObject{"q": "x"}},
		},
	}}
	got := toPortsMessage(ownsCalls, "m")
	if got.Role != "assistant" {
		t.Fatalf("a turn that owns tool calls was dropped, orphaning them on replay: %+v", got)
	}
	if len(got.ToolCalls) != 1 {
		t.Errorf("tool calls = %d, want 1; the turn must survive to pair its results", len(got.ToolCalls))
	}
	if got.Content != "" {
		t.Errorf("Content = %q, want empty; the row stays, the reasoning does not", got.Content)
	}
}
