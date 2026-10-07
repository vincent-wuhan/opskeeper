package decorators

import (
	"context"
	"errors"
)

// This file is the seam the pause gate reads through. It is declared here
// rather than imported from biz/hitl so the aiops domain keeps no
// compile-time dependency on the hitl domain (decision 277) — the same
// shape decision 276 gave the topology tools.
//
// Unlike the topology port, *hitl.Coordinator cannot satisfy this interface
// at all, and the reason is worth writing down: ShouldPause takes
// *hitl.Action, a struct declared on the other side of the boundary, so
// there is nothing to satisfy. A conversion is required, and a conversion
// has to live wherever both sides are visible — the composition root. There
// is deliberately no such adapter in this knife, because the coordinator
// this port exists for is never constructed in production
// (hitl.NewCoordinator has no caller outside its own package's tests), so an
// adapter here would be unreferenced code. The conversion is written and
// pinned in pauseport_hitl_test.go instead, together with the two traps it
// has to survive; whoever wires the gate moves it across intact.

// PauseAction is what the decorator asks about: the tool, how risky it is,
// which resource it would touch, and the arguments it would run with.
type PauseAction struct {
	Tool        string
	RiskLevel   string
	Resource    string
	Sensitivity string
	Payload     map[string]interface{}
}

// PendingProposal is the answer: a proposal is waiting for a human, and
// these three fields are the part the error message is built from. The rest
// of the proposal row is the hitl domain's business — the decorator never
// reads it, and projecting it would be projecting a table.
type PendingProposal struct {
	ID          string
	Severity    string
	Sensitivity string
}

// ErrProposalPending is this package's own sentinel, not the hitl domain's.
// It says "a tool call is waiting on a human", which is a statement about
// the decorator's contract; the hitl spelling of the same fact is a
// statement about the coordinator's implementation. A conversion that
// forgets to translate produces a fail-fast error that names the cause but
// drops the proposal id, and the id is the one thing the caller needs.
var ErrProposalPending = errors.New("decorators: proposal pending human decision")

// PauseCoordinator decides whether a tool call has to wait for a human.
type PauseCoordinator interface {
	ShouldPause(ctx context.Context, action PauseAction) (*PendingProposal, error)
}
