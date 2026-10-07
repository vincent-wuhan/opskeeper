package chatruntime

import (
	"testing"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
)

// historyScenarios are the replay shapes that decide whether a strict
// provider accepts a transcript. The parity test runs over all of them, so a
// rule that lands in one rendering and not the other fails here rather than
// in production as an HTTP 400 on one provider.
func historyScenarios() map[string][]*model.Message {
	callA, callB := "call_00_aaa", "call_01_bbb"
	budget := `{"status":"call_budget_exceeded","tool":"list_metric_catalog","calls":1,` +
		`"instruction":"You have already called it 1 times this turn. Do NOT call it again."}`
	return map[string][]*model.Message{
		"plain turns": {
			{ID: "u0", Role: model.RoleUser, Content: strPtr("why is web-1 slow?")},
			{ID: "a0", Role: model.RoleAssistant, Content: strPtr("checking")},
			{ID: "u1", Role: model.RoleUser, Content: strPtr("and now?")},
		},
		"system row": {
			{ID: "s0", Role: model.RoleSystem, Content: strPtr("you are an SRE")},
			{ID: "u0", Role: model.RoleUser, Content: strPtr("status?")},
		},
		"hoisted tool pair": {
			{ID: "u0", Role: model.RoleUser, Content: strPtr("check host + metrics")},
			{ID: "a1", Role: model.RoleAssistant, Content: strPtr("checking"),
				ToolCalls: []model.ToolCall{
					{ToolName: "get_host_processes", LLMCallID: strPtr(callA), ArgumentsJSON: "{}"},
					{ToolName: "query_promql", LLMCallID: strPtr(callB), ArgumentsJSON: `{"query":"up"}`},
				}},
			{ID: "t-a", Role: model.RoleTool, ToolCallID: strPtr(callA),
				ToolName: strPtr("get_host_processes"), Content: strPtr(`{"procs":[]}`)},
			{ID: "t-b", Role: model.RoleTool, ToolCallID: strPtr(callB),
				ToolName: strPtr("query_promql"), Content: strPtr(`{"resultType":"matrix"}`)},
			{ID: "u1", Role: model.RoleUser, Content: strPtr("thanks")},
		},
		"orphan tool row": {
			{ID: "u0", Role: model.RoleUser, Content: strPtr("load + cpu?")},
			{ID: "a1", Role: model.RoleAssistant, Content: strPtr("checking"),
				ToolCalls: []model.ToolCall{
					{ToolName: "get_host_processes", LLMCallID: strPtr(callA), ArgumentsJSON: "{}"},
					{ToolName: "query_promql", LLMCallID: strPtr(callB), ArgumentsJSON: "{}"},
				}},
			{ID: "t-a", Role: model.RoleTool, ToolCallID: strPtr(callA),
				ToolName: strPtr("get_host_processes"), Content: strPtr(`{"procs":[]}`)},
			{ID: "t-orphan", Role: model.RoleTool, ToolCallID: strPtr("query_promql|einoToolAdapter"),
				ToolName: strPtr("query_promql"), Content: strPtr(`{"resultType":"matrix"}`)},
			{ID: "t-b", Role: model.RoleTool, ToolCallID: strPtr(callB),
				ToolName: strPtr("query_promql"), Content: strPtr(`{"error":"autoheal"}`)},
			{ID: "u1", Role: model.RoleUser, Content: strPtr("1+2")},
		},
		"expired tool budget": {
			{ID: "u0", Role: model.RoleUser, Content: strPtr("create the alert")},
			{ID: "a1", Role: model.RoleAssistant, Content: strPtr("checking the catalog"),
				ToolCalls: []model.ToolCall{
					{ToolName: "list_metric_catalog", LLMCallID: strPtr("call_budget_1"), ArgumentsJSON: `{"query":"mysql"}`},
				}},
			{ID: "t-1", Role: model.RoleTool, ToolCallID: strPtr("call_budget_1"),
				ToolName: strPtr("list_metric_catalog"), Content: strPtr(budget)},
			{ID: "u1", Role: model.RoleUser, Content: strPtr("continue")},
		},
		"assistant with no signal": {
			{ID: "u0", Role: model.RoleUser, Content: strPtr("hi")},
			{ID: "a1", Role: model.RoleAssistant},
			{ID: "u1", Role: model.RoleUser, Content: strPtr("hello?")},
		},
		"incomplete tool pair": {
			{ID: "u0", Role: model.RoleUser, Content: strPtr("do both")},
			{ID: "a1", Role: model.RoleAssistant, Content: strPtr("working"),
				ToolCalls: []model.ToolCall{
					{ToolName: "get_host_processes", LLMCallID: strPtr(callA), ArgumentsJSON: "{}"},
					{ToolName: "query_promql", LLMCallID: strPtr(callB), ArgumentsJSON: "{}"},
				}},
			{ID: "t-a", Role: model.RoleTool, ToolCallID: strPtr(callA),
				ToolName: strPtr("get_host_processes"), Content: strPtr(`{"procs":[]}`)},
			{ID: "u1", Role: model.RoleUser, Content: strPtr("well?")},
		},
	}
}

// TestTheRendererAddsNoRuleOfItsOwn pins the invariant the two-rendering
// test used to check. With eino gone there is one rendering left, so the
// comparison is now between the plan and that rendering: a rule that decides
// which turn is replayed must live in planHistory, and the renderer must
// only format. A rule added to the renderer would change the conversation
// the model sees without appearing in the place the transcript policy is
// reviewed, which is exactly the drift the split exists to prevent.
func TestTheRendererAddsNoRuleOfItsOwn(t *testing.T) {
	for name, rows := range historyScenarios() {
		t.Run(name, func(t *testing.T) {
			plan := planHistory(rows)
			out := buildKernelHistory(rows)
			if len(plan) != len(out) {
				t.Fatalf("turns = %d (plan) vs %d (rendered)", len(plan), len(out))
			}
			for i := range plan {
				e, k := plan[i], out[i]
				if e.Role != k.Role || e.Content != k.Content {
					t.Fatalf("turn %d: plan %s/%q vs rendered %s/%q", i, e.Role, e.Content, k.Role, k.Content)
				}
				if e.ToolCallID != k.ToolCallID || e.ToolName != k.ToolName {
					t.Fatalf("turn %d: tool result plan %q/%q vs rendered %q/%q",
						i, e.ToolCallID, e.ToolName, k.ToolCallID, k.ToolName)
				}
				if len(e.ToolCalls) != len(k.ToolCalls) {
					t.Fatalf("turn %d: tool calls %d vs %d", i, len(e.ToolCalls), len(k.ToolCalls))
				}
				for j := range e.ToolCalls {
					ec, kc := e.ToolCalls[j], k.ToolCalls[j]
					if ec.ID != kc.ID || ec.Name != kc.Name || ec.ArgsJSON != string(kc.Arguments) {
						t.Fatalf("turn %d call %d: plan %+v vs rendered %+v", i, j, ec, kc)
					}
				}
			}
		})
	}
}

func TestBuildKernelHistoryDropsTheOrphanToolRow(t *testing.T) {
	// The same corruption the eino-side test reproduces, asserted in the
	// kernel's vocabulary: a tool row whose id no assistant ever emitted
	// must not reach the provider.
	rows := historyScenarios()["orphan tool row"]
	out := buildKernelHistory(rows)

	valid := map[string]bool{"call_00_aaa": true, "call_01_bbb": true}
	toolCount := 0
	for i, m := range out {
		if m.Role != model.RoleTool {
			continue
		}
		toolCount++
		if !valid[m.ToolCallID] {
			t.Fatalf("orphan tool message survived at %d: id=%q", i, m.ToolCallID)
		}
		if i == 0 || out[i-1].Role == model.RoleUser {
			t.Fatalf("tool message at %d is not adjacent to its request", i)
		}
	}
	if toolCount != 2 {
		t.Fatalf("tool messages = %d, want the two real slots", toolCount)
	}
}

func TestBuildKernelHistoryKeepsArgumentsByteForByte(t *testing.T) {
	// The stored bytes are what the provider produced. Re-marshalling them
	// would reorder keys and change float formatting, and a provider that
	// compares replayed arguments with its own output would reject the
	// transcript.
	raw := `{"query":"rate(x[5m])","limit":1.0,"nested":{"b":1,"a":2}}`
	rows := []*model.Message{
		{ID: "u0", Role: model.RoleUser, Content: strPtr("why?")},
		{ID: "a1", Role: model.RoleAssistant, Content: strPtr("checking"),
			ToolCalls: []model.ToolCall{
				{ToolName: "query_promql", LLMCallID: strPtr("c1"), ArgumentsJSON: raw},
			}},
		{ID: "t-1", Role: model.RoleTool, ToolCallID: strPtr("c1"),
			ToolName: strPtr("query_promql"), Content: strPtr("{}")},
		{ID: "u1", Role: model.RoleUser, Content: strPtr("ok")},
	}
	out := buildKernelHistory(rows)
	var got string
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			got = string(tc.Arguments)
		}
	}
	if got != raw {
		t.Fatalf("arguments = %q, want the stored bytes verbatim", got)
	}
}

func TestBuildKernelHistoryTrimsTheLiveUserTurn(t *testing.T) {
	// The kernel appends the live turn from the request; leaving the
	// persisted copy in the history would ask the model to answer the same
	// question twice.
	rows := []*model.Message{
		{ID: "u0", Role: model.RoleUser, Content: strPtr("first")},
		{ID: "a0", Role: model.RoleAssistant, Content: strPtr("answer")},
		{ID: "u1", Role: model.RoleUser, Content: strPtr("the live turn")},
	}
	out := buildKernelHistory(rows)
	for _, m := range out {
		if m.Content == "the live turn" {
			t.Fatal("the live turn was replayed from history")
		}
	}
	if len(out) != 2 {
		t.Fatalf("turns = %d, want 2", len(out))
	}
}
