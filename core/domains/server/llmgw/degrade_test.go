package llmgw

import (
	"context"
	"net/http"
	"testing"
)

// A hard cut is the worst way to run out of budget. A node refused at 100%
// answers nothing for the rest of the UTC day, which is exactly when a long
// diagnosis still needs it. These tests are about the node still finishing
// what it was halfway through — by answering briefly, not by being silenced.

// tunedBudgetHandler wires a gateway with both ceilings, which is the shape a
// deployment that configured them actually gets.
func tunedBudgetHandler(t *testing.T, budget Budget, bounds CallBounds) (*Handler, *tunedCompleter) {
	t.Helper()
	completer := &tunedCompleter{}
	handler, err := NewHandler(Options{
		Auth:      &stubAuth{edges: map[string]uint64{"ak:sk": 3}},
		Completer: completer,
		Bounds:    bounds,
		Budget:    budget,
	})
	if err != nil {
		t.Fatalf("build the handler: %v", err)
	}
	return handler, completer
}

func askFor(t *testing.T, handler *Handler, tokens string) *tunedCompleter {
	t.Helper()
	completer := &tunedCompleter{}
	handler.opts.Completer = completer
	body := `{"model":"m","max_completion_tokens":` + tokens +
		`,"messages":[{"role":"user","content":"hi"}]}`
	if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	return completer
}

// A node with most of its allowance left gets exactly what it asked for.
// Degradation that starts early is worse than none: short answers during a
// healthy investigation waste the operator's money on terseness nobody needed.
func TestANodeBelowTheLineStillGetsItsOwnCeiling(t *testing.T) {
	budget := NewAttributedBudget(&stubBudget{}, 1000, 80)
	ctx := context.Background()
	if err := budget.RecordEdge(ctx, 3, 799); err != nil {
		t.Fatalf("record: %v", err)
	}
	handler, _ := tunedBudgetHandler(t, budget, CallBounds{MaxOutputTokens: 4096})

	completer := askFor(t, handler, "512")
	if completer.maxTokens != 512 {
		t.Errorf("a node at 79.9%% of its allowance was answered with %d tokens, want its own 512",
			completer.maxTokens)
	}
}

// The line itself is included. A node sitting exactly on 80% has told us it is
// heading for the cut, and "not quite" is not a state a percentage can be in.
func TestTheLineItselfStartsShorterAnswers(t *testing.T) {
	budget := NewAttributedBudget(&stubBudget{}, 1000, 80)
	ctx := context.Background()
	if err := budget.RecordEdge(ctx, 3, 800); err != nil {
		t.Fatalf("record: %v", err)
	}
	handler, _ := tunedBudgetHandler(t, budget, CallBounds{MaxOutputTokens: 4096})

	completer := askFor(t, handler, "4096")
	if completer.maxTokens != 250 {
		t.Errorf("a node on the degradation line got %d tokens, want a quarter of its allowance (250)",
			completer.maxTokens)
	}
}

// Degradation narrows; it never widens. A node that asked for less than the
// degraded ceiling keeps its own number, because the gateway enforces ceilings
// and does not get a vote on how big answers are.
func TestDegradationNeverMakesAnAnswerBiggerThanAsked(t *testing.T) {
	budget := NewAttributedBudget(&stubBudget{}, 1000, 80)
	ctx := context.Background()
	if err := budget.RecordEdge(ctx, 3, 900); err != nil {
		t.Fatalf("record: %v", err)
	}
	handler, _ := tunedBudgetHandler(t, budget, CallBounds{})

	completer := askFor(t, handler, "100")
	if completer.maxTokens != 100 {
		t.Errorf("degradation widened a 100-token request to %d", completer.maxTokens)
	}
}

// The operator's clamp outranks the ledger's degradation. A deployment that
// says "no reply may exceed 100 tokens" means it even for a node that is
// running out of money.
func TestTheOperatorsClampOutranksDegradation(t *testing.T) {
	budget := NewAttributedBudget(&stubBudget{}, 100_000, 50)
	ctx := context.Background()
	if err := budget.RecordEdge(ctx, 3, 60_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	handler, _ := tunedBudgetHandler(t, budget, CallBounds{MaxOutputTokens: 100})

	completer := askFor(t, handler, "50000")
	if completer.maxTokens != 100 {
		t.Errorf("the operator's 100-token clamp became %d once degradation applied", completer.maxTokens)
	}
}

// A node that never opted into degradation gets nothing but the previous
// behaviour, even after spending its entire allowance. Conflating "not
// configured" with "on" would silently shorten every answer on every fleet.
func TestAnUnconfiguredDegradePercentChangesNothing(t *testing.T) {
	budget := NewAttributedBudget(&stubBudget{}, 1000, 0)
	ctx := context.Background()
	if err := budget.RecordEdge(ctx, 3, 1000); err != nil {
		t.Fatalf("record: %v", err)
	}
	handler, _ := tunedBudgetHandler(t, budget, CallBounds{})

	completer := askFor(t, handler, "4096")
	if completer.maxTokens != 4096 {
		t.Errorf("answers were shortened to %d with degradation not configured", completer.maxTokens)
	}
}

// Degradation is per node, not global. One node approaching its ceiling must
// not shorten the investigations of every other node — that is the fleet-wide
// silence this whole area of the gateway exists to prevent, wearing a
// friendlier name.
func TestOneNodesSpendingDoesNotShortenAnotherNodesAnswers(t *testing.T) {
	budget := NewAttributedBudget(&stubBudget{}, 1000, 80)
	ctx := context.Background()
	if err := budget.RecordEdge(ctx, 3, 1000); err != nil {
		t.Fatalf("record: %v", err)
	}
	completer := &tunedCompleter{}
	handler, err := NewHandler(Options{
		Auth:      &stubAuth{edges: map[string]uint64{"ak:sick": 3, "ak:healthy": 4}},
		Completer: completer,
		Budget:    budget,
	})
	if err != nil {
		t.Fatalf("build the handler: %v", err)
	}
	body := `{"model":"m","max_completion_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`
	if rec := post(t, handler, "ak:healthy", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if completer.maxTokens != 2048 {
		t.Errorf("a second node's answer was shortened to %d by another node's spend", completer.maxTokens)
	}
}

// The degraded size is a share of the node's own allowance, not a constant. A
// node with a 10k allowance and one with a 10M allowance are in the same
// situation at different numbers, and a fixed answer size would treat them as
// different problems.
func TestTheDegradedSizeFollowsTheNodesOwnAllowance(t *testing.T) {
	for _, tc := range []struct {
		daily, degradedWant int
	}{{1_000, 250}, {8_000, 2_000}, {40_000, 10_000}} {
		budget := NewAttributedBudget(&stubBudget{}, tc.daily, 50)
		ctx := context.Background()
		if err := budget.RecordEdge(ctx, 3, tc.daily); err != nil {
			t.Fatalf("record: %v", err)
		}
		handler, _ := tunedBudgetHandler(t, budget, CallBounds{})
		completer := askFor(t, handler, "999999")
		if completer.maxTokens != tc.degradedWant {
			t.Errorf("a %d-token allowance degraded to %d, want %d", tc.daily, completer.maxTokens, tc.degradedWant)
		}
	}
}

// The streaming path degrades exactly like the buffered one. A node that
// switches to streaming to avoid the shorter answer has found a bypass, and
// a bypass here is a node that spends what it was refused.
func TestAStreamIsDegradedTheSameWay(t *testing.T) {
	budget := NewAttributedBudget(&stubBudget{}, 1000, 80)
	ctx := context.Background()
	if err := budget.RecordEdge(ctx, 3, 900); err != nil {
		t.Fatalf("record: %v", err)
	}
	completer := &tunedCompleter{}
	handler, err := NewHandler(Options{
		Auth:      &stubAuth{edges: map[string]uint64{"ak:sk": 3}},
		Completer: completer,
		Budget:    budget,
	})
	if err != nil {
		t.Fatalf("build the handler: %v", err)
	}
	body := `{"model":"m","stream":true,"max_completion_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`
	if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if completer.maxTokens != 250 {
		t.Errorf("the streaming path answered with %d tokens, want the degraded 250", completer.maxTokens)
	}
}

// The seam itself, because a gateway with no attributed budget must not be
// asked to degrade: an absent ledger has no opinion.
func TestALedgerThatIsNotAttributedHasNoDegradationOpinion(t *testing.T) {
	var plain Budget = &stubBudget{}
	if _, ok := plain.(Degrader); ok {
		t.Error("a cluster-only budget claimed to know per-node room")
	}
	// And the real one does, because it is the ledger that knows the spend.
	var attributed Budget = NewAttributedBudget(&stubBudget{}, 1000, 80)
	degrader, ok := attributed.(Degrader)
	if !ok {
		t.Fatal("the attributed ledger does not expose its degradation seam")
	}
	if _, degraded := degrader.DegradedTokens(context.Background(), 9); degraded {
		t.Error("a node that has spent nothing was degraded")
	}
}
