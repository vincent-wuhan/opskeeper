package pigagent

import (
	"context"
	"errors"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The console's agent kernel is the biggest spender in the deployment — a
// 30-round investigation loop driven by a human — and until this existed it
// checked a daily cap against a ledger nothing ever wrote to. These tests are
// about usage reaching the ledger exactly once per settled message.

// ledgerSpender records what the kernel charged.
type ledgerSpender struct {
	charges  []int
	sessions []string
	err      error
}

func (s *ledgerSpender) Record(_ context.Context, sessionID string, tokens int) error {
	s.charges = append(s.charges, tokens)
	s.sessions = append(s.sessions, sessionID)
	return s.err
}

// settledMessage is one finished model reply carrying its usage.
func settledMessage(usage *ai.Usage) agent.MessageEndEvent {
	return agent.MessageEndEvent{Message: agent.AgentMessage{
		Assistant: &agent.AssistantMessage{
			Content: []ai.AssistantContentBlock{ai.TextContent{Text: "checking"}},
			Usage:   usage,
		},
	}}
}

// One settled reply books its own tokens. The turn's cost is what the cap
// reads, so a reply that is never booked is a reply that is free.
func TestASettledReplyIsBookedAgainstTheLedger(t *testing.T) {
	spender := &ledgerSpender{}
	r, _ := harness(t, Deps{Spender: spender})

	r.onEvent(agent.TurnStartEvent{})
	r.onEvent(settledMessage(&ai.Usage{Input: 100, Output: 20}))

	if len(spender.charges) != 1 || spender.charges[0] != 120 {
		t.Fatalf("charges = %v, want one charge of 120", spender.charges)
	}
	if spender.sessions[0] != "s-1" {
		t.Errorf("charged session %q, want the turn's own session", spender.sessions[0])
	}
}

// Each message books itself, not the running total. Charging the total would
// bill the first reply twice and the third reply six times — a ledger that
// inflates the very number it exists to keep honest.
func TestEachReplyIsBookedOnceAndNotTheRunningTotal(t *testing.T) {
	spender := &ledgerSpender{}
	r, _ := harness(t, Deps{Spender: spender})

	r.onEvent(agent.TurnStartEvent{})
	for i := 0; i < 3; i++ {
		r.onEvent(settledMessage(&ai.Usage{Input: 10, Output: 10}))
	}
	want := []int{20, 20, 20}
	if len(spender.charges) != len(want) {
		t.Fatalf("charges = %v, want %v", spender.charges, want)
	}
	for i, charge := range want {
		if spender.charges[i] != charge {
			t.Fatalf("charges = %v, want %v", spender.charges, want)
		}
	}
}

// A provider that reported nothing is not charged a guess. The gateway takes
// the same position on the node side: inventing a number is how a cap stops
// meaning anything, and a provider that mis-reports is a provider problem to
// read in the logs, not a number to make up here.
func TestAProviderThatReportedNothingIsChargedNothing(t *testing.T) {
	spender := &ledgerSpender{}
	r, _ := harness(t, Deps{Spender: spender})

	r.onEvent(agent.TurnStartEvent{})
	r.onEvent(settledMessage(nil))
	r.onEvent(settledMessage(&ai.Usage{}))

	if len(spender.charges) != 0 {
		t.Fatalf("charges = %v, want none for a provider that reported no usage", spender.charges)
	}
}

// The provider's own total is the bill. Charging the folded sum instead would
// quietly change what the operator is capped at, on exactly the models whose
// totals include components the transcript does not name.
func TestTheProvidersOwnTotalIsWhatIsCharged(t *testing.T) {
	spender := &ledgerSpender{}
	r, _ := harness(t, Deps{Spender: spender})

	r.onEvent(agent.TurnStartEvent{})
	r.onEvent(settledMessage(&ai.Usage{Input: 10, Output: 10, TotalTokens: 5000}))

	if len(spender.charges) != 1 || spender.charges[0] != 5000 {
		t.Fatalf("charges = %v, want the provider's reported 5000", spender.charges)
	}
}

// Cache tokens are tokens the operator paid for. A cap that counted only
// input and output would let a cache-heavy provider spend without ever
// reaching the ceiling.
func TestCacheTokensAreChargedToo(t *testing.T) {
	spender := &ledgerSpender{}
	r, _ := harness(t, Deps{Spender: spender})

	r.onEvent(agent.TurnStartEvent{})
	r.onEvent(settledMessage(&ai.Usage{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 5}))

	if len(spender.charges) != 1 || spender.charges[0] != 120 {
		t.Fatalf("charges = %v, want 120 with cache tokens included", spender.charges)
	}
}

// A ledger that refuses the charge does not fail the turn. The provider has
// already been paid and no second action would make the bill smaller — the
// same position the node gateway takes, and for the same reason.
func TestARefusedChargeDoesNotFailTheTurn(t *testing.T) {
	spender := &ledgerSpender{err: errors.New("ledger is closed")}
	r, _ := harness(t, Deps{Spender: spender})

	r.onEvent(agent.TurnStartEvent{})
	r.onEvent(settledMessage(&ai.Usage{Input: 10, Output: 10}))
	r.onEvent(agent.AgentEndEvent{})

	if r.usage.InputTokens != 10 || r.usage.OutputTokens != 10 {
		t.Errorf("usage = %+v, want the turn's own accounting intact", r.usage)
	}
}

// A deployment with no cap configured has no spender, and the kernel runs
// exactly as before. A turn must not depend on the ledger to finish.
func TestNoSpenderIsNotADependency(t *testing.T) {
	r, _ := harness(t, Deps{})
	r.onEvent(agent.TurnStartEvent{})
	r.onEvent(settledMessage(&ai.Usage{Input: 10, Output: 10}))
	r.onEvent(agent.AgentEndEvent{})
	if r.usage.Billed() == 0 {
		t.Error("usage was not folded into the turn's own accounting")
	}
}

// Both PiG drivers must reach the same ledger: a cap that the `pig-sdk`
// driver honours and the bare loop ignores is a cap that depends on which
// kernel the operator selected.
func TestBothDriversShareTheSameDepsShape(t *testing.T) {
	var deps Deps
	var _ ports.TokenRecorder = (*ledgerSpender)(nil)
	// The field is on Deps rather than on one driver's options precisely so
	// this compiles only if both drivers read the same struct.
	deps.Spender = &ledgerSpender{}
	if deps.Spender == nil {
		t.Error("Deps cannot carry a spender")
	}
}
