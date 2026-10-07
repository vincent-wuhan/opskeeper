package main

import (
	"context"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/llm"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/agentkernel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The console's daily cap exists in exactly one place: this host binding. A
// mistake here is invisible everywhere else — turns run, turns stop when the
// cap says so, and nothing anywhere reports that the cap is not being fed.
// That is not hypothetical: the binding shipped with a checker and no
// recorder, and the ceiling never moved.

// The ledger is fed from the same adapter that gates it. Two adapters would
// be two buckets, and a turn could be checked against one and charged to
// another — unenforceable, and invisible to a test of either half alone.
func TestTheConsoleKernelFeedsTheLedgerItChecks(t *testing.T) {
	ledger := llm.NewInMemoryBudget(1_000)
	host := kernelHost(agentKernelInput{Budget: ledger, Gate: noopGate{}}, nil)
	host.ToolsFor = staticTools

	if host.Budget == nil {
		t.Fatal("the host has no budget checker")
	}
	if host.Spender == nil {
		t.Fatal("the host has no spender; every console turn runs for free")
	}
	provider, err := host.Provider()
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	deps, err := provider(context.Background(), ports.AgentRequest{SessionID: "s-1"})
	if err != nil {
		t.Fatalf("deps: %v", err)
	}
	if err := deps.Spender.Record(context.Background(), "s-1", 1_001); err != nil {
		t.Fatalf("record: %v", err)
	}
	if ledger.Used() != 1_001 {
		t.Fatalf("the production ledger reports %d tokens, want 1001", ledger.Used())
	}
	allowed, reason := deps.Budget.Allow(context.Background(), "s-1")
	if allowed {
		t.Fatal("the cap allowed a turn after the day's allowance was spent")
	}
	if reason == "" {
		t.Error("the refusal carries no reason for the turn's terminal frame")
	}
}

// staticTools resolves an empty bag: the budget wiring is what this test is
// about, and the tool source is a precondition of the provider.
func staticTools(context.Context, ports.AgentRequest) (ports.ToolBag, error) {
	return emptyBag{}, nil
}

type emptyBag struct{}

func (emptyBag) Tools() []ports.Tool { return nil }
func (emptyBag) Names() []string     { return nil }

// A deployment with no cap configured wires no budget at all, and must not
// grow a half-wired one: an adapter over nothing looks like a ceiling.
func TestAnUncappedDeploymentWiresNoLedger(t *testing.T) {
	host := kernelHost(agentKernelInput{Gate: noopGate{}}, nil)
	if host.Budget != nil {
		t.Errorf("budget = %v, want nil with no cap configured", host.Budget)
	}
	if host.Spender != nil {
		t.Errorf("spender = %v, want nil with no cap configured", host.Spender)
	}
}

// noopGate is the smallest possible approval gate: the host binding requires
// one, and this test is about the budget wiring.
type noopGate struct{}

func (noopGate) Request(context.Context, ports.ApprovalRequest) (ports.Decision, error) {
	return ports.Decision{Decision: ports.ApprovalGranted}, nil
}

func (noopGate) Pending(context.Context, string) ([]ports.ApprovalRequest, error) {
	return nil, nil
}

var _ agentkernel.Host
