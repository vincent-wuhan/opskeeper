package agentkernel

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The interfaces this binding exists to satisfy.
var (
	_ ports.BudgetChecker = (*Budget)(nil)
)

// bag is a minimal ports.ToolBag. The binding never inspects a bag, so a
// bag that only reports its own name is enough to prove the bag that arrived
// is the bag the kernel got.
type bag struct {
	name  string
	tools []ports.Tool
}

func (b *bag) Tools() []ports.Tool { return b.tools }
func (b *bag) Names() []string     { return []string{b.name} }

func TestHostProviderRefusesToRunWithoutAToolSource(t *testing.T) {
	// A host with no tool source would run every turn with an empty bag,
	// which looks like a model that simply chose not to call anything. The
	// wiring bug has to be visible at assembly time instead.
	if _, err := (Host{}).Provider(); !errors.Is(err, ErrNoToolSource) {
		t.Fatalf("err = %v, want ErrNoToolSource", err)
	}
}

func TestHostProviderResolvesTheBagPerTurn(t *testing.T) {
	// The bag depends on the request (role, session, live write gate), so
	// resolving it once at assembly would hand every later turn the first
	// caller's view of what it may reach.
	var seen []string
	h := Host{ToolsFor: func(_ context.Context, req ports.AgentRequest) (ports.ToolBag, error) {
		seen = append(seen, req.Role)
		return &bag{name: req.Role}, nil
	}}
	provider, err := h.Provider()
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	for _, role := range []string{"admin", "viewer"} {
		deps, err := provider(context.Background(), ports.AgentRequest{SessionID: "s-1", Role: role})
		if err != nil {
			t.Fatalf("deps(%s): %v", role, err)
		}
		if got := deps.Tools.Names()[0]; got != role {
			t.Fatalf("bag(%s) = %q, want the bag resolved for that turn", role, got)
		}
	}
	if len(seen) != 2 || seen[0] != "admin" || seen[1] != "viewer" {
		t.Fatalf("resolver saw %v, want one call per turn", seen)
	}
}

func TestHostProviderForwardsEveryHostService(t *testing.T) {
	// A dropped service is not a visible failure: the turn still runs, it
	// just runs without a ledger or without a gate. Forwarding is asserted
	// field by field so a future edit that forgets one fails here.
	ledger := NewAuditLedger(&recordingLedger{}, nil)
	budget := NewBudget(&recordingBudget{}, nil)
	recorder := &recordingRecorder{}
	gate := &countingGate{}
	h := Host{
		ToolsFor: func(context.Context, ports.AgentRequest) (ports.ToolBag, error) {
			return &bag{name: "b"}, nil
		},
		Audit:    ledger,
		Gate:     gate,
		Budget:   budget,
		Recorder: recorder,
	}
	provider, err := h.Provider()
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	deps, err := provider(context.Background(), ports.AgentRequest{SessionID: "s-1"})
	if err != nil {
		t.Fatalf("deps: %v", err)
	}
	if deps.Audit != ledger || deps.Gate != gate || deps.Budget != budget || deps.Recorder != recorder {
		t.Fatalf("a host service was dropped: %+v", deps)
	}
	if deps.Tools == nil {
		t.Fatal("the bag was dropped")
	}
}

func TestHostProviderPropagatesAToolSourceFailure(t *testing.T) {
	// Swallowing this would start a turn with no tools at all, which is
	// indistinguishable from a model declining to use them.
	want := errors.New("session not found")
	h := Host{ToolsFor: func(context.Context, ports.AgentRequest) (ports.ToolBag, error) {
		return nil, want
	}}
	provider, _ := h.Provider()
	if _, err := provider(context.Background(), ports.AgentRequest{}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the resolver's own error", err)
	}
}

// recordingBudget refuses by default and records the bucket it was asked
// about, so a test can tell which session mapped to which bucket.
type recordingBudget struct {
	refuse bool
	bucket uint64
	est    int
}

func (b *recordingBudget) Check(_ context.Context, userID uint64, est int) error {
	b.bucket, b.est = userID, est
	if b.refuse {
		return errors.New("daily token cap reached")
	}
	return nil
}

// countingGate records how often it was consulted.
type countingGate struct{ calls int }

func (g *countingGate) Request(context.Context, ports.ApprovalRequest) (ports.Decision, error) {
	g.calls++
	return ports.Decision{Decision: ports.ApprovalGranted}, nil
}

func (g *countingGate) Pending(context.Context, string) ([]ports.ApprovalRequest, error) {
	return nil, nil
}

// recordingRecorder is here to prove the field is forwarded; the persister
// tests cover the writes.
type recordingRecorder struct{ starts int }

func (r *recordingRecorder) Started(context.Context, ports.ToolCallRecord) error {
	r.starts++
	return nil
}
func (r *recordingRecorder) Settled(context.Context, ports.ToolCallRecord) error { return nil }

func TestBudgetStopsTheTurnAndKeepsTheCheckersReason(t *testing.T) {
	// The reason reaches the operator in the turn's terminal frame. Inventing
	// one here would replace the text that names the cap.
	b := NewBudget(&recordingBudget{refuse: true}, nil)
	allowed, reason := b.Allow(context.Background(), "s-1")
	if allowed {
		t.Fatal("a refused turn was allowed")
	}
	if reason != "daily token cap reached" {
		t.Fatalf("reason = %q, want the checker's own error", reason)
	}
}

func TestBudgetAsksTheSessionsBucket(t *testing.T) {
	// One tenant's exhausted cap must not stop another tenant's turn.
	checker := &recordingBudget{}
	b := NewBudget(checker, func(sessionID string) uint64 {
		if sessionID == "s-7" {
			return 42
		}
		return 0
	})
	if allowed, _ := b.Allow(context.Background(), "s-7"); !allowed {
		t.Fatal("a within-budget turn was stopped")
	}
	if checker.bucket != 42 {
		t.Fatalf("bucket = %d, want the session's own", checker.bucket)
	}
	if allowed, _ := b.Allow(context.Background(), "other"); !allowed {
		t.Fatal("a within-budget turn was stopped")
	}
	if checker.bucket != 0 {
		t.Fatalf("bucket = %d, want the global bucket without a resolver", checker.bucket)
	}
}

func TestNewBudgetRefusesToWrapNothing(t *testing.T) {
	// A non-nil adapter over a nil checker would look wired and check
	// nothing. nil means "this deployment does not budget", and the kernel
	// treats a nil budget as unconstrained.
	if b := NewBudget(nil, nil); b != nil {
		t.Fatalf("budget = %v, want nil", b)
	}
	var b *Budget
	if allowed, reason := b.Allow(context.Background(), "s-1"); !allowed || reason != "" {
		t.Fatalf("a nil budget must allow: %v/%q", allowed, reason)
	}
}

func TestTurnToolsFromContextRefusesAnUnstampedTurn(t *testing.T) {
	// The stamp is what makes a viewer's filtered bag real. A forgotten
	// stamp must fail the turn, not run it with everything the process
	// happens to have loaded.
	if _, err := TurnToolsFromContext(context.Background(), ports.AgentRequest{}); !errors.Is(err, ErrNoToolSource) {
		t.Fatalf("err = %v, want ErrNoToolSource", err)
	}
	// A stamped empty bag is a legitimate "this turn reaches nothing" — a
	// summariser, a classifier — and must not be confused with the missing
	// stamp above.
	stamped := ports.WithTurnTools(context.Background(), &bag{name: "read-only"})
	got, err := TurnToolsFromContext(stamped, ports.AgentRequest{})
	if err != nil {
		t.Fatalf("stamped bag: %v", err)
	}
	if got.Names()[0] != "read-only" {
		t.Fatalf("bag = %v, want the stamped one", got.Names())
	}
}

func TestWithTurnToolsRefusesToStampNothing(t *testing.T) {
	// Storing nil would turn "nobody said what the tools are" into "the
	// caller said there are none", which is the confusion the stamp exists
	// to remove.
	ctx := ports.WithTurnTools(context.Background(), nil)
	if _, ok := ports.TurnToolsFromContext(ctx); ok {
		t.Fatal("a nil bag was stamped as a deliberate one")
	}
}
