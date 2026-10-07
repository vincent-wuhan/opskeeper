package pigcoding

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
)

// TestTheBudgetIsSpentBeforeThePolicyGateSeesTheCall pins the hook order.
//
// The order is the argument, so it gets a test rather than a comment. The
// failure it guards against is specific and quiet: with the gate first, a
// deployment whose gate refuses everything never spends a round, the cap
// never trips, and the model retries forever against a wall — an unbounded
// turn produced by a defence, which is the worst place for one to come from.
func TestTheBudgetIsSpentBeforeThePolicyGateSeesTheCall(t *testing.T) {
	var order []string

	gate := func(_ context.Context, _, _ string, _ json.RawMessage) agent.ToolCallHookResult {
		order = append(order, "gate")
		return agent.ToolCallHookResult{}
	}
	second := func(_ context.Context, _, _ string, _ json.RawMessage) agent.ToolCallHookResult {
		order = append(order, "second")
		return agent.ToolCallHookResult{}
	}

	hooks := composeBeforeToolCall(NewTurnBudget(5), []agent.BeforeToolCallHook{gate, second})
	if len(hooks) != 3 {
		t.Fatalf("composed %d hooks, want 3 (budget + two caller hooks)", len(hooks))
	}

	for i, hook := range hooks {
		if hook(context.Background(), "id", "echo", json.RawMessage(`{}`)).Block {
			t.Fatalf("hook %d blocked a call inside the budget; nothing should be refused at round 1", i)
		}
	}

	want := []string{"gate", "second"}
	if len(order) != len(want) {
		t.Fatalf("recorded %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("recorded %v, want %v", order, want)
		}
	}
}

// TestAnUnboundedBudgetAddsNoHook keeps the zero case honest. A caller that
// asked for no cap must not inherit a hook that refuses nothing and appears in
// every chain, because "there is a gate here" and "the gate is a no-op" are
// indistinguishable at the call site that matters.
func TestAnUnboundedBudgetAddsNoHook(t *testing.T) {
	if hooks := composeBeforeToolCall(NewTurnBudget(0), nil); hooks != nil {
		t.Fatalf("an unbounded budget with no caller hooks composed %d hook(s), want none", len(hooks))
	}
	if got := NewTurnBudget(-3).MaxRounds(); got != 0 {
		t.Fatalf("a negative cap normalised to %d, want 0", got)
	}
}
