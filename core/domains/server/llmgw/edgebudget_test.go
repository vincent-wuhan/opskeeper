package llmgw

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// The per-node token cap is the answer to the question the global cap cannot
// answer. With only a cluster ceiling, one node in a pathological loop spends
// the fleet's money and every other node is then refused for a reason that
// has nothing to do with it: the whole fleet goes quiet because one machine's
// agent loop is stuck. These tests are about attribution surviving that.

// A node that spent its own allowance is refused, and the refusal has to say
// so. "the cluster is out of budget" and "you, specifically, are out of
// budget" lead to different operator actions, and the second being reported
// as the first is how one bad node gets debugged as a cluster-wide incident.
func TestTheNodeThatSpentIsRefusedAndTheOthersAreNot(t *testing.T) {
	global := &stubBudget{}
	budget := NewAttributedBudget(global, 100, 0)
	ctx := context.Background()

	if err := budget.RecordEdge(ctx, 7, 101); err != nil {
		t.Fatalf("record: %v", err)
	}

	err := budget.CheckEdge(ctx, 7, 0)
	if !errors.Is(err, ErrNodeBudgetExceeded) {
		t.Fatalf("node 7 after spending its own allowance: %v, want ErrNodeBudgetExceeded", err)
	}
	if !strings.Contains(err.Error(), "node 7") {
		t.Errorf("the refusal does not name the node: %s", err)
	}
	// Not ErrBudgetExceeded: the cluster still has money here, and reporting
	// otherwise sends the operator to the wrong place.
	if errors.Is(err, errs.ErrBudgetExceeded) {
		t.Errorf("a per-node refusal was reported as a cluster-wide one: %s", err)
	}
	if err := budget.CheckEdge(ctx, 8, 0); err != nil {
		t.Errorf("a node that spent nothing was refused: %v", err)
	}
	// The per-node cap is not a way around the cluster ceiling.
	if err := budget.CheckEdge(ctx, 8, 0); err != nil {
		t.Errorf("second check for node 8: %v", err)
	}
}

// The cluster cap is asked on every node's call, including when the per-node
// cap is generous. A per-node limit configured by an operator must not
// become the ceiling the operator thought they still had.
func TestTheClusterCapStillRefusesANodeWithRoomOfItsOwn(t *testing.T) {
	global := &stubBudget{allowErr: errs.ErrBudgetExceeded}
	budget := NewAttributedBudget(global, 1_000_000, 0)

	err := budget.CheckEdge(context.Background(), 3, 0)
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("cluster exhaustion was not surfaced to the node: %v", err)
	}
	if len(global.checked) == 0 {
		t.Error("the global budget was never asked; a per-node cap replaced it instead of layering on it")
	}
}

// Charging one node must charge the cluster from the same call. Without that
// the two ledgers drift and the global cap starts passing calls the fleet has
// already paid for.
func TestChargingANodeAlsoChargesTheCluster(t *testing.T) {
	global := &stubBudget{}
	budget := NewAttributedBudget(global, 100, 0)

	if err := budget.RecordEdge(context.Background(), 4, 30); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(global.recorded) != 1 || global.recorded[0] != 30 {
		t.Fatalf("cluster was charged %v, want one call of 30", global.recorded)
	}
	// And the plain two-method seam stays usable, because that is what a
	// Budget-typed field is asked for.
	if err := budget.Record(context.Background(), 5); err != nil {
		t.Fatalf("plain record: %v", err)
	}
	if len(global.recorded) != 2 {
		t.Errorf("plain seam charged the cluster %v, want two calls", global.recorded)
	}
}

// End to end through the handler: two nodes, one per-node allowance. The
// sick node's spend must be refused while the healthy node keeps diagnosing,
// on both the buffered and the streaming path — a budget that counted only
// the buffered replies was one a node could bypass by asking for a stream.
func TestAStreamIsChargedToTheNodeThatAskedForIt(t *testing.T) {
	budget := NewAttributedBudget(&stubBudget{}, 90, 0)
	auth := &stubAuth{edges: map[string]uint64{"ak-sick:sk": 1, "ak-healthy:sk": 2}}
	handler := gatedHandler(t, auth,
		&stubCompleter{reply: meteredReply(10, 90, 100)}, budget, nil)

	streamBody := `{"model":"m","stream":true,"messages":[{"role":"user","content":"why is the disk full"}]}`
	if rec := post(t, handler, "ak-sick:sk", streamBody); rec.Code != http.StatusOK {
		t.Fatalf("first stream: status %d, body %s", rec.Code, rec.Body.String())
	}
	rec := post(t, handler, "ak-sick:sk", streamBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a streaming node past its own allowance was served: status %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "node 1") {
		t.Errorf("the refusal does not name the node: %s", body)
	}
	if rec := post(t, handler, "ak-healthy:sk", spendBody); rec.Code != http.StatusOK {
		t.Errorf("the healthy node was refused because of another node's spend: status %d, body %s",
			rec.Code, rec.Body.String())
	}
}

// A node with no per-node cap configured keeps exactly the previous
// behaviour: only the cluster ceiling applies. Conflating "no per-node limit"
// with "zero" would refuse the whole fleet over a missing env var.
func TestAnUnconfiguredPerNodeCapLeavesOnlyTheClusterCap(t *testing.T) {
	global := &stubBudget{}
	budget := NewAttributedBudget(global, 0, 0)
	ctx := context.Background()

	if err := budget.RecordEdge(ctx, 1, 10_000_000); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := budget.CheckEdge(ctx, 1, 0); err != nil {
		t.Errorf("a node was refused by a cap that was not configured: %v", err)
	}
	if len(budget.(*edgeBudget).used) != 0 {
		t.Error("tokens were tracked per node even though no per-node cap was configured")
	}
}

// The ledger must not grow a bucket per node forever. A node whose only
// spend is from a previous UTC day is dead weight in a long-running manager.
func TestPerNodeLedgersOfNodesThatStoppedSpendingAreSwept(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	budget := newEdgeBudget(&stubBudget{}, 100, 0, func() time.Time { return now })
	ctx := context.Background()

	if err := budget.RecordEdge(ctx, 5, 10); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(budget.used) != 1 {
		t.Fatalf("ledger is %v, want one node", budget.used)
	}
	// Two days on and a full sweep interval later, with nothing spent today.
	now = now.AddDate(0, 0, 2).Add(2 * time.Minute)
	if err := budget.RecordEdge(ctx, 6, 10); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, live := budget.used[5]; live {
		t.Errorf("an idle node's ledger survived the sweep: %v", budget.used)
	}
	if _, live := budget.used[6]; !live {
		t.Errorf("the sweep took the node that had just spent: %v", budget.used)
	}
}

// A per-node cap is configured without a global one — the deployment that
// wants to stop one node from spending while letting the rest of the fleet
// run. The nil global must not panic and must not refuse.
func TestAPerNodeCapWorksWithoutAClusterCap(t *testing.T) {
	budget := NewAttributedBudget(nil, 100, 0)
	ctx := context.Background()

	if err := budget.RecordEdge(ctx, 2, 101); err != nil {
		t.Fatalf("record against a nil global: %v", err)
	}
	if err := budget.CheckEdge(ctx, 2, 0); !errors.Is(err, ErrNodeBudgetExceeded) {
		t.Fatalf("node 2 past its own allowance with no cluster cap: %v", err)
	}
	if err := budget.CheckEdge(ctx, 3, 0); err != nil {
		t.Errorf("another node was refused with no cluster cap configured: %v", err)
	}
}

// The estimate the gateway passes is zero (there is no second tokenizer, see
// admission), so a node with exactly its remaining allowance left must still
// be admitted — otherwise the cap stops one call early and the operator sees
// a fleet that stops asking exactly when it is nearly out. The refusal is on
// "past the cap", not "at the cap": a deployment that budgets 100 tokens and
// has spent 100 has spent its budget, not exceeded it, and refusing here
// would make the effective cap 99.
func TestTheLastCallOfAnAllowanceIsServed(t *testing.T) {
	budget := NewAttributedBudget(&stubBudget{}, 100, 0)
	ctx := context.Background()

	if err := budget.RecordEdge(ctx, 9, 99); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := budget.CheckEdge(ctx, 9, 0); err != nil {
		t.Errorf("the final affordable call was refused: %v", err)
	}
	if err := budget.CheckEdge(ctx, 9, 1); err != nil {
		t.Errorf("a call landing exactly on the cap was refused: %v", err)
	}
	if err := budget.CheckEdge(ctx, 9, 2); !errors.Is(err, ErrNodeBudgetExceeded) {
		t.Errorf("an estimate past the remainder was admitted: %v", err)
	}
}
