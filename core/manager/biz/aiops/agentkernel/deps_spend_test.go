package agentkernel

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/llm"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

var errDailyCap = errors.New("daily token cap reached")

// The console's daily cap used to be a checker with no recorder beside it.
// Every turn asked "may I spend?" against a ledger whose running total never
// moved, so the operator's ceiling was decorative on the one loop that spends
// the most. These tests are about the two halves agreeing.

// writingBudget is a ledger with both halves, which is what the cap needs.
type writingBudget struct {
	recordingBudget
	recorded int
	sessions []string
	err      error
}

func (b *writingBudget) Record(_ context.Context, userID uint64, tokens int) error {
	b.bucket, b.recorded = userID, tokens
	return b.err
}

// The money a call is charged to must be the money it was checked against.
// Two different buckets means the cap can be enforced against one person and
// spent by another, and neither half's test would show it.
func TestTheChargeLandsInTheSessionsOwnBucket(t *testing.T) {
	checker := &writingBudget{}
	b := NewBudget(checker, func(sessionID string) uint64 {
		if sessionID == "s-7" {
			return 42
		}
		return 0
	})
	if err := b.Record(context.Background(), "s-7", 900); err != nil {
		t.Fatalf("record: %v", err)
	}
	if checker.bucket != 42 || checker.recorded != 900 {
		t.Fatalf("charged %d tokens to bucket %d, want 900 to bucket 42",
			checker.recorded, checker.bucket)
	}
}

// A turn that spent money must change what the next check says. This is the
// whole defect in one assertion: the checker and the recorder are wired to the
// same ledger, so the total the cap reads is the total that was booked.
func TestChargingMovesTheNumberTheCapReads(t *testing.T) {
	var used int
	b := NewBudget(&thresholdBudget{limit: 1000, used: &used}, nil)

	if allowed, _ := b.Allow(context.Background(), "s-1"); !allowed {
		t.Fatal("a turn was stopped before anything was spent")
	}
	if err := b.Record(context.Background(), "s-1", 1000); err != nil {
		t.Fatalf("record: %v", err)
	}
	allowed, reason := b.Allow(context.Background(), "s-1")
	if allowed {
		t.Fatal("the cap still allowed a turn after the day's allowance was spent")
	}
	if reason == "" {
		t.Error("the refusal carries no reason for the operator's terminal frame")
	}
}

type thresholdBudget struct {
	limit int
	used  *int
}

func (b *thresholdBudget) Check(_ context.Context, _ uint64, _ int) error {
	if *b.used >= b.limit {
		return errDailyCap
	}
	return nil
}

func (b *thresholdBudget) Record(_ context.Context, _ uint64, tokens int) error {
	*b.used += tokens
	return nil
}

// A checker that cannot record is a deployment without a ledger to write to,
// not a failure: the turn still ran, the provider already billed it, and
// there is no second action that would make the bill smaller.
func TestACheckerThatCannotRecordIsNotAnError(t *testing.T) {
	b := NewBudget(&recordingBudget{}, nil)
	if err := b.Record(context.Background(), "s-1", 500); err != nil {
		t.Errorf("recording against a query-only checker failed the turn: %v", err)
	}
}

// Zero and negative counts are not spend. Recording them would let a caller
// credit itself budget it never saved, which is the arithmetic every ledger
// that clamps to zero refuses.
func TestNothingIsChargedForNoTokens(t *testing.T) {
	checker := &writingBudget{}
	b := NewBudget(checker, nil)
	for _, tokens := range []int{0, -5} {
		if err := b.Record(context.Background(), "s-1", tokens); err != nil {
			t.Fatalf("record(%d): %v", tokens, err)
		}
	}
	if checker.recorded != 0 {
		t.Errorf("a turn with no usage charged %d tokens", checker.recorded)
	}
}

// The host must forward both halves. A dropped Spender is not a visible
// failure: turns run normally, they just stop being counted.
func TestTheHostForwardsTheSpenderBesideTheChecker(t *testing.T) {
	budget := NewBudget(&writingBudget{}, nil)
	h := Host{
		ToolsFor: func(context.Context, ports.AgentRequest) (ports.ToolBag, error) {
			return &bag{name: "b"}, nil
		},
		Budget:  budget,
		Spender: budget,
	}
	provider, err := h.Provider()
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	deps, err := provider(context.Background(), ports.AgentRequest{SessionID: "s-1"})
	if err != nil {
		t.Fatalf("deps: %v", err)
	}
	if deps.Spender == nil {
		t.Fatal("the turn's deps carry no spender; usage will never be booked")
	}
	if deps.Budget == nil {
		t.Fatal("the turn's deps carry no budget checker")
	}
}

// The production ledger, not a double. The whole defect was that the real
// type was asked to check and nothing ever asked it to record, so the test
// that matters uses the ledger that actually ships and shows its running
// total moving.
func TestTheRealLedgerMovesWhenATurnIsCharged(t *testing.T) {
	ledger := llm.NewInMemoryBudget(1_000)
	budget := NewBudget(ledger, nil)

	if allowed, _ := budget.Allow(context.Background(), "s-1"); !allowed {
		t.Fatal("a within-budget turn was stopped")
	}
	if ledger.Used() != 0 {
		t.Fatalf("a fresh ledger already reports %d tokens", ledger.Used())
	}
	if err := budget.Record(context.Background(), "s-1", 900); err != nil {
		t.Fatalf("record: %v", err)
	}
	if ledger.Used() != 900 {
		t.Fatalf("ledger reports %d tokens, want the 900 that was charged", ledger.Used())
	}
	// The check now reads the number that was booked: this is the assertion
	// that would have failed before, when nothing was ever recorded.
	if err := budget.Record(context.Background(), "s-1", 200); err != nil {
		t.Fatalf("record: %v", err)
	}
	if allowed, _ := budget.Allow(context.Background(), "s-1"); allowed {
		t.Error("the cap allowed a turn after the day's allowance was spent")
	}
}
