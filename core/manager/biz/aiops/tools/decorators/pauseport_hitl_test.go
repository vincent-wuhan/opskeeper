package decorators

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/hitl"
	hitlmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/hitl"
)

// The conversion below is the one that will be needed the day the pause
// gate is actually wired. It lives in a test on purpose: *hitl.Coordinator
// has no caller in production (NewCoordinator is referenced only from its
// own package's tests), so an adapter in the composition root would be
// unreferenced code today. The two traps it has to survive are pinned here
// rather than left to whoever wires it.

// hitlPauser is the shape of the far side. Narrowing it to one method keeps
// the test off the real Coordinator, which needs a proposal writer, a pause
// policy and a severity provider to construct.
//
// The assertion below is the part that matters for production: it says the
// real coordinator is something this conversion can be pointed at, so the
// conversion tested here is the conversion that will be used.
type hitlPauser interface {
	ShouldPause(ctx context.Context, action *hitl.Action) (*hitlmodel.Proposal, error)
}

var _ hitlPauser = (*hitl.Coordinator)(nil)

type hitlPauseAdapter struct{ inner hitlPauser }

var _ PauseCoordinator = hitlPauseAdapter{}

func (a hitlPauseAdapter) ShouldPause(ctx context.Context, action PauseAction) (*PendingProposal, error) {
	proposal, err := a.inner.ShouldPause(ctx, &hitl.Action{
		Tool:        action.Tool,
		RiskLevel:   action.RiskLevel,
		Resource:    action.Resource,
		Sensitivity: action.Sensitivity,
		Payload:     action.Payload,
	})
	if errors.Is(err, hitl.ErrProposalPending) {
		// The proposal comes back WITH the error, and it is the only place
		// the caller's proposal id exists. Returning (nil, err) here — the
		// shape every other error takes below — compiles, reads as an
		// ordinary early return, and silently drops the id.
		return pendingFrom(proposal), ErrProposalPending
	}
	if err != nil {
		return nil, err
	}
	return pendingFrom(proposal), nil
}

func pendingFrom(p *hitlmodel.Proposal) *PendingProposal {
	if p == nil {
		return nil
	}
	return &PendingProposal{ID: p.ID, Severity: p.Severity, Sensitivity: p.Sensitivity}
}

type stubPauser struct {
	gotAction *hitl.Action
	proposal  *hitlmodel.Proposal
	err       error
}

func (s *stubPauser) ShouldPause(_ context.Context, action *hitl.Action) (*hitlmodel.Proposal, error) {
	s.gotAction = action
	return s.proposal, s.err
}

// Trap 1: the pending case loses the proposal id if the conversion returns
// early on error. The assertion is on both halves — the translated sentinel
// AND a non-nil proposal — because either alone passes in the other world.
func TestTheHitlConversionCarriesTheProposalAlongWithThePendingError(t *testing.T) {
	stub := &stubPauser{
		proposal: &hitlmodel.Proposal{ID: "prop-xyz", Severity: "dangerous", Sensitivity: "top_secret"},
		err:      hitl.ErrProposalPending,
	}
	got, err := hitlPauseAdapter{inner: stub}.ShouldPause(context.Background(), PauseAction{Tool: "cloud_bash"})
	if !errors.Is(err, ErrProposalPending) {
		t.Fatalf("the hitl sentinel was not translated: got %v", err)
	}
	if got == nil {
		t.Fatal("proposal dropped: the caller loses the id it has to show the human")
	}
	if got.ID != "prop-xyz" {
		t.Errorf("ID: got %q, want %q", got.ID, "prop-xyz")
	}
	if got.Severity != "dangerous" {
		t.Errorf("Severity: got %q, want %q", got.Severity, "dangerous")
	}
	if got.Sensitivity != "top_secret" {
		t.Errorf("Sensitivity: got %q, want %q", got.Sensitivity, "top_secret")
	}
}

// Trap 2: a system error must not be laundered into a pending error, or the
// caller shows a human an approval request for a database that is simply
// unreachable.
func TestTheHitlConversionLeavesEveryOtherErrorAlone(t *testing.T) {
	boom := errors.New("db connection lost")
	stub := &stubPauser{err: boom}
	_, err := hitlPauseAdapter{inner: stub}.ShouldPause(context.Background(), PauseAction{})
	if !errors.Is(err, boom) {
		t.Errorf("got %v, want the original error", err)
	}
	if errors.Is(err, ErrProposalPending) {
		t.Error("a system error must not read as a pending proposal")
	}
}

// Trap 3: nil in, nil out, in both directions. A nil proposal behind a nil
// error is the pass-through case, and returning a non-nil empty struct for
// it would make the decorator build an error message with three empty
// fields.
func TestTheHitlConversionKeepsThePassThroughCaseEmpty(t *testing.T) {
	got, err := hitlPauseAdapter{inner: &stubPauser{}}.ShouldPause(context.Background(), PauseAction{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("pass-through should stay nil, got %+v", got)
	}
}

// Every field of the action crosses, with values that are distinct so a
// crossed assignment is visible. Payload is the one that matters most: it
// is the whole argument object, and a dropped copy is a proposal that asks
// a human to approve nothing.
func TestTheHitlConversionCarriesEveryActionField(t *testing.T) {
	stub := &stubPauser{}
	payload := map[string]interface{}{"host": "host-9", "cmd": "reboot"}
	_, err := hitlPauseAdapter{inner: stub}.ShouldPause(context.Background(), PauseAction{
		Tool:        "cloud_bash",
		RiskLevel:   "manage",
		Resource:    "host-9",
		Sensitivity: "top_secret",
		Payload:     payload,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := stub.gotAction
	if got == nil {
		t.Fatal("the far side was never called")
	}
	if got.Tool != "cloud_bash" {
		t.Errorf("Tool: got %q, want %q", got.Tool, "cloud_bash")
	}
	if got.RiskLevel != "manage" {
		t.Errorf("RiskLevel: got %q, want %q", got.RiskLevel, "manage")
	}
	if got.Resource != "host-9" {
		t.Errorf("Resource: got %q, want %q", got.Resource, "host-9")
	}
	if got.Sensitivity != "top_secret" {
		t.Errorf("Sensitivity: got %q, want %q", got.Sensitivity, "top_secret")
	}
	if len(got.Payload) != 2 || got.Payload["cmd"] != "reboot" {
		t.Errorf("Payload: got %+v, want the full argument object", got.Payload)
	}
}
