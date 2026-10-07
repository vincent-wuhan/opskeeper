package ports

import (
	"context"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// ApprovalDecision is a human's answer to an approval request.
type ApprovalDecision string

const (
	// ApprovalGranted allows the call to proceed exactly as proposed.
	ApprovalGranted ApprovalDecision = "grant"
	// ApprovalDenied refuses it. The tool must not be retried with a
	// reformulated argument under the same proposal.
	ApprovalDenied ApprovalDecision = "deny"
)

// ApprovalRequest is a gated mutating call awaiting a human decision.
//
// The host binds the approval to a digest of the exact call: the tool name,
// the resolved arguments, and the command or payload. A grant applies to
// that digest only. If the agent re-plans and produces a different call, the
// stale grant does not carry over, and the request must be made again.
type ApprovalRequest struct {
	ID       string
	ToolName string
	Class    domain.ToolClass
	// SessionID is the conversation the call belongs to. It rides on the
	// request because the inbox is read back per session (ApprovalInbox.Open)
	// and a console that reconnected has no other way to ask which of the
	// outstanding rows belong to the session it is rendering. A request
	// without one can still be queued and decided; it simply cannot be
	// listed by session.
	SessionID string
	// Digest binds the eventual Decision back to this exact call. It is
	// carried on the request rather than looked up by the decision, because
	// a console that re-renders its queue after a reconnect has to echo it
	// verbatim — and a decision that arrives without one is refused rather
	// than matched on the request id alone.
	Digest string
	// Arguments is the exact JSON the agent proposed. It is what the
	// digest covers.
	Arguments []byte
	// Summary is the operator-facing one-line description.
	Summary string
	// BlastRadius is set by the host policy engine from the target the
	// agent resolved. The plugin does not set it.
	BlastRadius domain.BlastRadius
	// Target is the resolved resource, for display and for policy.
	Target string
	// ExpiresAt bounds how long a decision stays actionable. A request
	// past it is denied by default rather than blocking the queue.
	ExpiresAt time.Time
}

// Decision is the human's answer plus its binding digest.
type Decision struct {
	RequestID string
	Digest    string
	Decision  ApprovalDecision
	// DecidedBy is the operator's identity for the ledger.
	DecidedBy string
	// Note is optional free text recorded alongside the decision.
	Note string
}

// ApprovalGate is the host's sole path for a gated call to execute.
//
// Three properties are load-bearing and must hold in any implementation:
//
//  1. A plugin can request an approval but can never grant one. Granting
//     requires a Decision, and only a DecisionProvider mints those.
//  2. A Decision is bound to a digest of the exact proposed call. A grant
//     for one argument set does not authorise a different one.
//  3. A request past ExpiresAt is denied. Expiry fails closed.
type ApprovalGate interface {
	// Request blocks until a decision arrives, the context is cancelled, or
	// the request expires. A cancelled context must not be reported as a
	// grant.
	Request(ctx context.Context, req ApprovalRequest) (Decision, error)
	// Pending returns the outstanding requests for a session, so a
	// reconnecting console can re-render its approval queue.
	Pending(ctx context.Context, sessionID string) ([]ApprovalRequest, error)
}

// DecisionProvider is the operator-facing side of the gate. Only the
// control plane holds one, and only a control-plane handler calls Decide.
type DecisionProvider interface {
	Decide(ctx context.Context, d Decision) error
}

// GateError classifies an approval failure so callers can tell a refusal
// from a transport problem.
type GateError struct {
	// Reason is "denied", "expired", "cancelled", or "not_found".
	Reason string
	Err    error
}

func (e *GateError) Error() string {
	if e.Err != nil {
		return "approval: " + e.Reason + ": " + e.Err.Error()
	}
	return "approval: " + e.Reason
}

func (e *GateError) Unwrap() error { return e.Err }

// Approval failure reasons.
const (
	GateDenied    = "denied"
	GateExpired   = "expired"
	GateCancelled = "cancelled"
	GateNotFound  = "not_found"
)
