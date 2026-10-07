package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/domains/biz/nodeagent"
	"github.com/vincent-wuhan/opskeeper/core/domains/biz/nodefleet"
)

// This is the only place in the repository that knows nodeagent and nodefleet
// are two bounded contexts rather than one package, so it is the only place
// the translation between their vocabularies can be tested. Getting it wrong
// is silent in both directions: the console's HTTP layer matches
// nodeagent.ErrConversationLimit, so a mistranslation there shows up as a 500
// with a correct-looking body, and an operator debugging a fleet needs the
// LimitError that a Replace-style translation would have thrown away.

// TestTheFleetFullSentinelArrivesAsTheConsoleSurfaceNamesIt is the mapping
// itself. The fleet answers 429-worthy refusals with an error that unwraps to
// ErrFleetFull; the console surface answers them with ErrConversationLimit.
func TestTheFleetFullSentinelArrivesAsTheConsoleSurfaceNamesIt(t *testing.T) {
	cases := []struct {
		name string
		in   error
	}{
		{"per-node cap", &nodefleet.LimitError{Scope: "edge", EdgeID: 7, Open: 32, Limit: 32}},
		{"whole-fleet cap", &nodefleet.LimitError{Scope: "fleet", Open: 512, Limit: 512}},
		// Wrapped on the way out of the biz layer, as a real handler carries
		// it. A translation that used == instead of errors.Is would pass the
		// two cases above and fail this one, which is why it is here.
		{"wrapped by context", errors.Join(errors.New("open conversation:"), &nodefleet.LimitError{Scope: "edge", EdgeID: 9, Open: 3, Limit: 3})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateFleetError(tc.in)
			if !errors.Is(got, nodeagent.ErrConversationLimit) {
				t.Errorf("errors.Is(%v, ErrConversationLimit) is false; the console layer would "+
					"answer 500 instead of 429", got)
			}
		})
	}
}

// TestTheNumbersThatSayWhichBudgetRanOutSurviveTheTranslation is the reason
// the translation joins instead of replacing. The LimitError underneath says
// "node 7 already has 32 of 32 conversations open", which is the difference
// between an instruction to the operator and a ticket for someone else.
func TestTheNumbersThatSayWhichBudgetRanOutSurviveTheTranslation(t *testing.T) {
	in := &nodefleet.LimitError{Scope: "edge", EdgeID: 7, Open: 32, Limit: 32}
	got := translateFleetError(in)

	var limit *nodefleet.LimitError
	if !errors.As(got, &limit) {
		t.Fatalf("the LimitError is gone: %v. An operator hitting this cap is told which node "+
			"and how many, and a Join that dropped it would leave the console's answer right and "+
			"the diagnosis impossible", got)
	}
	if limit.EdgeID != 7 || limit.Open != 32 || limit.Limit != 32 {
		t.Errorf("the numbers changed in translation: %+v", limit)
	}
	// errors.Is has to answer true for BOTH sentinels, and that is the whole
	// trick: the console matches one, the log's readers can still match the
	// other. A translation that replaced the error would satisfy the first
	// assertion and silently break every diagnostic that used the second.
	if !errors.Is(got, nodefleet.ErrFleetFull) {
		t.Error("errors.Is(got, ErrFleetFull) is false; the fleet's own sentinel stopped working " +
			"on the way past, so anything downstream of the adapter that still asks loses")
	}
	if !strings.Contains(got.Error(), "32 of 32") {
		t.Errorf("the message lost the numbers: %q", got.Error())
	}
}

// TestEverythingElseTravelsThroughUnchanged: a node's own refusal and a
// transport failure are answers the console already renders correctly, and
// re-wrapping them here would only cost a reader the type.
func TestEverythingElseTravelsThroughUnchanged(t *testing.T) {
	refusal := &domain.AgentRefusal{Code: "agent_unavailable", Message: "no agent", EdgeID: 7}
	if got := translateFleetError(refusal); !errors.Is(got, refusal) {
		t.Errorf("a node's refusal came back as %v, want the same error so the HTTP layer can "+
			"read its code", got)
	}
	plain := errors.New("tunnel: dial timeout")
	if got := translateFleetError(plain); !errors.Is(got, plain) {
		t.Errorf("a transport failure came back as %v, want it untouched", got)
	}
	if got := translateFleetError(nil); got != nil {
		t.Errorf("translateFleetError(nil) = %v, want nil: a nil in must not become a non-nil out "+
			"that the caller then has to special-case", got)
	}
}
