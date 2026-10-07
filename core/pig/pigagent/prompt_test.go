package pigagent

import (
	"encoding/json"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// TestBuildPromptReplaysHistoryInOrder pins that persisted history reaches the
// provider oldest-first. Order is load-bearing twice over: a provider's prompt
// cache keys on a stable prefix, and a transcript whose tool result precedes
// its request is rejected outright.
func TestBuildPromptReplaysHistoryInOrder(t *testing.T) {
	t.Parallel()
	req := ports.AgentRequest{
		History: []ports.AgentMessage{
			{Role: "user", Content: "why is node-01 slow?"},
			{Role: "assistant", Content: "checking", ToolCalls: []ports.AgentToolCall{
				{ID: "call_1", Name: "get_host_load", Arguments: []byte(`{"host":"node-01"}`)},
			}},
			{Role: "tool", ToolCallID: "call_1", ToolName: "get_host_load", Content: `{"load":9.1}`},
		},
		UserText: "and now?",
	}
	got := buildPrompt(req)

	// history(3) + current user turn(1)
	if len(got) != 4 {
		t.Fatalf("messages = %d, want 4", len(got))
	}
	want := []string{"user", "assistant", "toolResult", "user"}
	for i, w := range want {
		if r := got[i].Role(); r != w {
			t.Errorf("message[%d] role = %q, want %q (full order: %v)", i, r, w, rolesOf(got))
		}
	}
	if got[0].User == nil || string(got[0].User.Content.(ai.UserText)) != "why is node-01 slow?" {
		t.Errorf("history user text not preserved: %#v", got[0].User)
	}
	if got[3].User == nil || string(got[3].User.Content.(ai.UserText)) != "and now?" {
		t.Errorf("current turn must come LAST: %#v", got[3].User)
	}
}

// TestBuildPromptKeepsAssistantToolCalls pins that a prior assistant turn's
// tool call survives replay with its id and arguments intact. A dropped call
// leaves the following tool result orphaned, which providers reject.
func TestBuildPromptKeepsAssistantToolCalls(t *testing.T) {
	t.Parallel()
	req := ports.AgentRequest{
		History: []ports.AgentMessage{
			{Role: "assistant", ToolCalls: []ports.AgentToolCall{
				{ID: "call_9", Name: "query_promql", Arguments: []byte(`{"q":"up"}`)},
			}},
			{Role: "tool", ToolCallID: "call_9", ToolName: "query_promql", Content: "1"},
		},
		UserText: "next",
	}
	got := buildPrompt(req)
	if len(got) < 2 || got[0].Assistant == nil {
		t.Fatalf("first message must be the replayed assistant turn: %#v", got)
	}
	var call *ai.ToolCall
	for _, block := range got[0].Assistant.Content {
		if tc, ok := block.(ai.ToolCall); ok {
			c := tc
			call = &c
		}
	}
	if call == nil {
		t.Fatalf("assistant turn lost its tool call: %#v", got[0].Assistant.Content)
	}
	if call.ID != "call_9" || call.Name != "query_promql" {
		t.Errorf("tool call = %+v, want call_9/query_promql", call)
	}
	if call.Arguments["q"] != "up" {
		t.Errorf("arguments = %v, want {q: up}", call.Arguments)
	}
	if got[1].ToolResult == nil || got[1].ToolResult.ToolCallID != "call_9" {
		t.Errorf("tool result did not keep its call id: %#v", got[1].ToolResult)
	}
}

// TestBuildPromptDropsUnusableHistoryInsteadOfFailing pins the fail-soft
// direction. History is persisted data the operator cannot edit; an entry this
// build cannot express must cost the model some context, not make the session
// permanently unusable.
func TestBuildPromptDropsUnusableHistoryInsteadOfFailing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  ports.AgentMessage
	}{
		{"unknown role", ports.AgentMessage{Role: "narrator", Content: "hi"}},
		{"empty role", ports.AgentMessage{Content: "hi"}},
		{"tool result without id", ports.AgentMessage{Role: "tool", Content: "x"}},
		{"assistant with no content at all", ports.AgentMessage{Role: "assistant"}},
		{"assistant with malformed arguments", ports.AgentMessage{Role: "assistant",
			ToolCalls: []ports.AgentToolCall{{ID: "c", Name: "t", Arguments: []byte("{not json")}}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := ports.AgentRequest{History: []ports.AgentMessage{tc.msg}, UserText: "next"}
			got := buildPrompt(req)
			// The unusable entry is gone; the user turn is still there, so the
			// turn is runnable rather than empty.
			if len(got) != 1 {
				t.Fatalf("messages = %d, want 1 (the bad entry dropped, the user turn kept)", len(got))
			}
			if got[0].User == nil {
				t.Fatalf("surviving message = %#v, want the user turn", got[0])
			}
		})
	}
}

// TestBuildPromptReminderPrecedesUserTurn pins the anti-drift reminder's
// position. It must be the message immediately before the user turn: the
// system prompt is the provider's cached prefix, so a reminder injected there
// is cached once and stops being read.
func TestBuildPromptReminderPrecedesUserTurn(t *testing.T) {
	t.Parallel()
	got := buildPrompt(ports.AgentRequest{
		History:          []ports.AgentMessage{{Role: "user", Content: "older"}},
		CriticalReminder: "never invent data",
		UserText:         "current",
	})
	if len(got) != 3 {
		t.Fatalf("messages = %d, want 3 (history + reminder + turn)", len(got))
	}
	if got[1].User == nil || string(got[1].User.Content.(ai.UserText)) != "never invent data" {
		t.Errorf("reminder is not the second-to-last message: %#v", got[1].User)
	}
	if got[2].User == nil || string(got[2].User.Content.(ai.UserText)) != "current" {
		t.Errorf("user turn is not last: %#v", got[2].User)
	}
}

// TestBuildPromptAlwaysProducesARunnableTurn pins the empty-request case. A
// provider charged for a turn with nothing to answer is a real (if small)
// cost, and an empty prompt list can make a provider reject the request.
func TestBuildPromptAlwaysProducesARunnableTurn(t *testing.T) {
	t.Parallel()
	got := buildPrompt(ports.AgentRequest{})
	if len(got) != 1 || got[0].User == nil {
		t.Fatalf("empty request produced %#v, want one placeholder user turn", got)
	}
	if s := string(got[0].User.Content.(ai.UserText)); s == "" {
		t.Fatalf("placeholder content is empty; the provider would have nothing to answer")
	}
}

// TestBuildPromptAcceptsHistoryOnlyWithNoNewText pins that a turn carrying a
// complete transcript and no new user text still runs. A worker resuming from
// history is exactly this shape, and requiring a fresh user line would make it
// impossible to express.
func TestBuildPromptAcceptsHistoryOnlyWithNoNewText(t *testing.T) {
	t.Parallel()
	got := buildPrompt(ports.AgentRequest{
		History: []ports.AgentMessage{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "ok"}},
	})
	if len(got) != 2 {
		t.Fatalf("messages = %d, want 2 (no placeholder added when history is present)", len(got))
	}
	if got[0].User == nil || got[1].Assistant == nil {
		t.Fatalf("history not replayed: %#v", got)
	}
}

// TestDecodeArgumentsTreatsAbsentAsEmptyObject pins the parameterless-call
// case. A tool called with no arguments must reach the provider as `{}`, not
// as a missing/JSON-null value, or the arg decoders on the far side see a
// different shape for the same request.
func TestDecodeArgumentsTreatsAbsentAsEmptyObject(t *testing.T) {
	t.Parallel()
	for _, raw := range [][]byte{nil, {}, []byte("null")} {
		got, ok := decodeArguments(raw)
		if !ok {
			t.Fatalf("decodeArguments(%q) reported unusable", raw)
		}
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(b) != "{}" {
			t.Fatalf("decodeArguments(%q) = %s, want {}", raw, b)
		}
	}
}

func rolesOf(msgs []agent.AgentMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role())
	}
	return out
}
