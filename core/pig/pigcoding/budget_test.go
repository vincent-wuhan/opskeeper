package pigcoding_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigcoding"
)

// call spends one round of the budget the way the agent loop would.
func call(t *testing.T, hook agent.BeforeToolCallHook) agent.ToolCallHookResult {
	t.Helper()
	if hook == nil {
		t.Fatal("budget produced no hook; an unbounded budget and a broken one look the same from here")
	}
	return hook(context.Background(), "call-1", "echo", json.RawMessage(`{}`))
}

// TestTheTurnBudgetRefusesTheRoundPastTheCap is the arithmetic the control
// plane's turn cap rests on.
//
// Three distinct outcomes are asserted because the cap needs all three. A
// budget that only blocked would let a model that ignores the refusal loop
// until the provider runs out of credit. A budget that terminated on the cap
// round would throw away the last round's findings, which is the round that
// usually contains the answer. Refusing softly first and terminating only on
// a second attempt is the difference between an investigation that concludes
// and one that is cut off mid-sentence.
func TestTheTurnBudgetRefusesTheRoundPastTheCap(t *testing.T) {
	const maxRounds = 3
	budget := pigcoding.NewTurnBudget(maxRounds)
	hook := budget.Hook()

	for round := 1; round <= maxRounds; round++ {
		if res := call(t, hook); res.Block {
			t.Fatalf("round %d of %d was refused; the cap fired early", round, maxRounds)
		}
		if budget.Spent() {
			t.Fatalf("budget reported spent after %d of %d rounds", round, maxRounds)
		}
	}

	soft := call(t, hook)
	if !soft.Block {
		t.Fatal("the round past the cap was allowed; the cap does not cap")
	}
	if soft.Terminate {
		t.Error("the first refusal terminated the turn; the model never got to land an answer with what it had")
	}
	if !strings.Contains(soft.Reason, "3") {
		t.Errorf("refusal reason %q does not name the cap, so an operator reading the transcript cannot tell a budget stop from a policy refusal", soft.Reason)
	}
	if !budget.Spent() {
		t.Error("budget reports unspent after the cap was exceeded")
	}

	hard := call(t, hook)
	if !hard.Block || !hard.Terminate {
		t.Errorf("a call after the soft refusal returned {Block:%v Terminate:%v}, want both true; the loop is still unbounded", hard.Block, hard.Terminate)
	}

	// Every call spends a round, including the refused ones. A cap that only
	// counted executed calls would be disarmed by a model that keeps asking.
	if got := budget.Rounds(); got != maxRounds+2 {
		t.Errorf("budget counted %d rounds, want %d", got, maxRounds+2)
	}
	if got := budget.MaxRounds(); got != maxRounds {
		t.Errorf("MaxRounds reported %d, want %d", got, maxRounds)
	}
}
