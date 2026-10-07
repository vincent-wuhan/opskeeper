package agent

import (
	"encoding/json"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"

	"github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

func sp(s string) *string { return &s }

// asstAt reads the assistant turn at index i.
//
// buildMessages returns PiG's own message union now, so a test that wants to
// know "what did the model ask for" type-asserts rather than reading a struct
// field. The assertion is the point: a replay that produced a tool result
// without its call has to fail here, not three provider round trips later as
// an HTTP 400 naming no message.
func asstAt(t *testing.T, out []pigai.Message, i int) pigai.AssistantMessage {
	t.Helper()
	msg, ok := out[i].(pigai.AssistantMessage)
	if !ok {
		t.Fatalf("out[%d] = %T, want pigai.AssistantMessage", i, out[i])
	}
	return msg
}

// ptrTo lets an assertion helper hand a value to a function that reads a
// reply. ReplyToolCalls does not need a pointer semantically — it only walks
// the content blocks — but its signature is the one the production call sites
// use, and a test passing a different shape would stop exercising the nil
// case that shape exists to handle.
func ptrTo(msg pigai.AssistantMessage) *pigai.AssistantMessage { return &msg }

// toolResultIDAt reads the tool-result turn at index i.
func toolResultIDAt(t *testing.T, out []pigai.Message, i int) (string, string) {
	t.Helper()
	msg, ok := out[i].(pigai.ToolResultMessage)
	if !ok {
		t.Fatalf("out[%d] = %T, want pigai.ToolResultMessage", i, out[i])
	}
	return msg.ToolCallID, msg.ToolName
}

// TestBuildMessages_ToolCallReplay covers the v0.7.170 fix:
// assistant turns with content=NULL but populated ToolCalls must
// replay as {role:assistant, content:"", tool_calls:[...]} so the
// following role=tool messages remain paired. DeepSeek v4+ rejected
// orphan tool messages with HTTP 400; OpenAI silently tolerated them
// pre-fix.
func TestBuildMessages_ToolCallReplay(t *testing.T) {
	a := &Agent{cfg: Config{SystemPrompt: "S"}}
	history := []*aiops.Message{
		{ID: "u1", Role: aiops.RoleUser, Content: sp("你好")},
		{ID: "a1", Role: aiops.RoleAssistant, Content: sp("你好,有什么可以帮你")},
		{ID: "u2", Role: aiops.RoleUser, Content: sp("查一下机器")},
		{
			ID: "a2", Role: aiops.RoleAssistant, Content: nil,
			ToolCalls: []aiops.ToolCall{
				{ToolName: "query_devices", ArgumentsJSON: `{}`, LLMCallID: sp("call_abc")},
			},
		},
		{ID: "t1", Role: aiops.RoleTool, Content: sp(`{"devices":[]}`), ToolCallID: sp("call_abc"), ToolName: sp("query_devices")},
		{ID: "a3", Role: aiops.RoleAssistant, Content: sp("你这边没有注册的设备")},
		{ID: "u3", Role: aiops.RoleUser, Content: sp("继续")},
	}
	out := a.buildMessages(history)
	// system + all 7 history rows (a2 now emits with tool_calls)
	if len(out) != 8 {
		t.Fatalf("len(out) = %d, want 8", len(out))
	}
	if _, ok := out[0].(pigai.SystemMessage); !ok {
		t.Errorf("out[0] = %T, want pigai.SystemMessage", out[0])
	}
	asst := asstAt(t, out, 4)
	if text := pigmodel.ReplyText(&asst); text != "" {
		t.Errorf("assistant tool-call turn content = %q, want empty", text)
	}
	calls := pigmodel.ReplyToolCalls(&asst)
	if len(calls) != 1 || calls[0].ID != "call_abc" {
		t.Errorf("assistant tool calls = %+v, want one with id=call_abc", calls)
	}
	if got := string(pigmodel.ArgumentsJSON(calls[0].Arguments)); got != `{}` {
		t.Errorf("tool call arguments = %q, want {}", got)
	}
	if id, name := toolResultIDAt(t, out, 5); id != "call_abc" || name != "query_devices" {
		t.Errorf("tool message id/name = %q/%q, want call_abc/query_devices", id, name)
	}
}

// TestBuildMessages_ToolCallReplay_BackcompatPairByOrder covers
// rows written before chat_tool_calls.llm_call_id existed: the call
// id is recoverable from the following role=tool message's
// tool_call_id column.
func TestBuildMessages_ToolCallReplay_BackcompatPairByOrder(t *testing.T) {
	a := &Agent{cfg: Config{}}
	history := []*aiops.Message{
		{ID: "u1", Role: aiops.RoleUser, Content: sp("查一下")},
		{
			ID: "a1", Role: aiops.RoleAssistant, Content: nil,
			ToolCalls: []aiops.ToolCall{
				{ToolName: "tool_a", ArgumentsJSON: `{"x":1}`}, // LLMCallID nil — legacy
				{ToolName: "tool_b", ArgumentsJSON: `{"y":2}`},
			},
		},
		{ID: "t1", Role: aiops.RoleTool, Content: sp(`r1`), ToolCallID: sp("legacy_call_1"), ToolName: sp("tool_a")},
		{ID: "t2", Role: aiops.RoleTool, Content: sp(`r2`), ToolCallID: sp("legacy_call_2"), ToolName: sp("tool_b")},
		{ID: "a2", Role: aiops.RoleAssistant, Content: sp("好的")},
	}
	out := a.buildMessages(history)
	if len(out) != 5 {
		t.Fatalf("len(out) = %d, want 5", len(out))
	}
	calls := pigmodel.ReplyToolCalls(ptrTo(asstAt(t, out, 1)))
	if len(calls) != 2 {
		t.Fatalf("assistant tool calls = %d entries, want 2", len(calls))
	}
	if calls[0].ID != "legacy_call_1" || calls[1].ID != "legacy_call_2" {
		t.Errorf("pairing failed: got ids %q,%q", calls[0].ID, calls[1].ID)
	}
	if calls[0].Name != "tool_a" || calls[1].Name != "tool_b" {
		t.Errorf("names = %q,%q", calls[0].Name, calls[1].Name)
	}
	// tool messages also kept
	if id, _ := toolResultIDAt(t, out, 2); id != "legacy_call_1" {
		t.Errorf("out[2] tool call id = %q, want legacy_call_1", id)
	}
	if id, _ := toolResultIDAt(t, out, 3); id != "legacy_call_2" {
		t.Errorf("out[3] tool call id = %q, want legacy_call_2", id)
	}
}

// TestBuildMessages_ToolCallReplay_UnresolvableDropsAssistantAndTools
// — if neither LLMCallID nor pairing can recover ids, drop the
// assistant + dependent tool rows so the LLM never sees an orphan
// tool. This is the pre-fix MVP behavior, kept as a safety net.
func TestBuildMessages_ToolCallReplay_UnresolvableDropsAssistantAndTools(t *testing.T) {
	a := &Agent{cfg: Config{}}
	history := []*aiops.Message{
		{ID: "u1", Role: aiops.RoleUser, Content: sp("hi")},
		{
			ID: "a1", Role: aiops.RoleAssistant, Content: nil,
			ToolCalls: []aiops.ToolCall{{ToolName: "x", ArgumentsJSON: `{}`}},
		},
		// no following role=tool — corrupt session
		{ID: "u2", Role: aiops.RoleUser, Content: sp("are you there?")},
	}
	out := a.buildMessages(history)
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (u1 + u2)", len(out))
	}
	for i, m := range out {
		if _, ok := m.(pigai.UserMessage); !ok {
			t.Errorf("out[%d] = %T, want pigai.UserMessage", i, m)
		}
	}
}

// Smoke that ToolCalls Args JSON round-trips through json.RawMessage
// (a regression would be invalid JSON in the outgoing request body).
func TestBuildMessages_ToolCallArgsRoundtrip(t *testing.T) {
	a := &Agent{cfg: Config{}}
	args := `{"x":1,"nested":{"k":"v"}}`
	history := []*aiops.Message{
		{ID: "u1", Role: aiops.RoleUser, Content: sp("go")},
		{
			ID: "a1", Role: aiops.RoleAssistant, Content: nil,
			ToolCalls: []aiops.ToolCall{
				{ToolName: "n", ArgumentsJSON: args, LLMCallID: sp("c1")},
			},
		},
		{ID: "t1", Role: aiops.RoleTool, Content: sp(`{}`), ToolCallID: sp("c1"), ToolName: sp("n")},
	}
	out := a.buildMessages(history)
	calls := pigmodel.ReplyToolCalls(ptrTo(asstAt(t, out, 1)))
	if len(calls) != 1 {
		t.Fatalf("missing tool_call")
	}
	var probe any
	raw := pigmodel.ArgumentsJSON(calls[0].Arguments)
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("args not valid JSON: %v (raw=%q)", err, string(raw))
	}
}
