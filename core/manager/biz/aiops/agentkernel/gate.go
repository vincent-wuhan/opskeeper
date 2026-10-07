package agentkernel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Errors this binding distinguishes. They are named because a caller that
// sees a refusal needs to tell "the inbox is not wired" from "the operator
// said no", and a bare string makes the two the same thing.
var (
	// ErrGateNotWired reports that no inbox was configured. A gate that
	// cannot produce a decision must refuse rather than pass the call
	// through: failing open here turns a misconfiguration into an
	// unauthorised change to a production system.
	ErrGateNotWired = errors.New("agentkernel: approval gate is not wired")
	// ErrUnbindableRequest reports a request that cannot be bound to a
	// decision — no digest, or no row to decide on.
	ErrUnbindableRequest = errors.New("agentkernel: the approval request cannot be bound to a decision")
)

// ApprovalInbox is the host's pending-approval store, narrowed to the three
// operations a gate needs.
//
// It is deliberately not the approval usecase: the gate must not be able to
// list, edit or execute approvals, only to queue one, wait for its verdict,
// and answer a reconnecting console's question about what is still open.
type ApprovalInbox interface {
	// Propose queues one call and returns the row id a decision will name.
	Propose(ctx context.Context, req ports.ApprovalRequest) (string, error)
	// Await blocks until the row reaches a terminal state, the context is
	// cancelled, or the row expires.
	Await(ctx context.Context, id string) (ports.Decision, error)
	// Open lists the outstanding rows for a session so a console that
	// reconnected can re-render its queue.
	Open(ctx context.Context, sessionID string) ([]ports.ApprovalRequest, error)
}

// InboxGate is the control plane's approval gate: it queues each mutating
// call in the human inbox and blocks until an operator answers.
//
// It implements the three load-bearing properties of the port, and each one
// is a refusal rather than a shortcut:
//
//  1. Only a Decision mints a grant, and a Decision only ever comes from
//     Await — the model and the tools have no path to one.
//  2. The grant is bound to the request's digest. The row is created from
//     this exact request, so the verdict that arrives for it is a verdict
//     about exactly this call; the digest is echoed back for the kernel to
//     check against the call it is holding.
//  3. An expired request is refused before it is queued, and one that
//     cannot be bound at all is refused rather than queued unbound.
type InboxGate struct {
	inbox ApprovalInbox
	// now defaults to time.Now. Injected so a test can place a request on
	// either side of its own expiry without sleeping.
	now func() time.Time
}

// NewInboxGate wraps an inbox. A nil inbox yields nil: see ErrGateNotWired
// for what the kernel does with the missing gate.
func NewInboxGate(inbox ApprovalInbox) *InboxGate {
	if inbox == nil {
		return nil
	}
	return &InboxGate{inbox: inbox, now: time.Now}
}

// Request queues req and blocks for the operator's answer.
func (g *InboxGate) Request(ctx context.Context, req ports.ApprovalRequest) (ports.Decision, error) {
	if g == nil || g.inbox == nil {
		return ports.Decision{}, ErrGateNotWired
	}
	// An unbound request cannot be checked against a decision later, so
	// queueing it would create a row whose eventual verdict authorises
	// nothing in particular.
	if req.Digest == "" {
		return ports.Decision{}, fmt.Errorf("%w: the request carries no digest", ErrUnbindableRequest)
	}
	now := g.now
	if now == nil {
		now = time.Now
	}
	if req.ExpiresAt.IsZero() {
		return ports.Decision{}, fmt.Errorf("%w: the request carries no expiry", ErrUnbindableRequest)
	}
	if !now().Before(req.ExpiresAt) {
		// Fails closed: a request that expired before it was queued cannot
		// be approved, and queueing it would put a dead row in front of an
		// operator.
		return ports.Decision{}, &ports.GateError{Reason: ports.GateExpired}
	}

	id, err := g.inbox.Propose(ctx, req)
	if err != nil {
		return ports.Decision{}, err
	}
	if id == "" {
		// Without a row id the verdict that comes back cannot be shown to
		// belong to this request, so accepting it would authorise a call on
		// the strength of an unrelated approval.
		return ports.Decision{}, fmt.Errorf("%w: the inbox returned no row id", ErrUnbindableRequest)
	}

	d, err := g.inbox.Await(ctx, id)
	if err != nil {
		return ports.Decision{}, err
	}
	// A verdict that names another row is not this call's verdict. The
	// request id is the only thing tying the two together once the inbox has
	// answered, so a mismatch is refused rather than repaired.
	if d.RequestID != "" && d.RequestID != id && d.RequestID != req.ID {
		return ports.Decision{}, fmt.Errorf("%w: the decision names request %q, not %q", ErrUnbindableRequest, d.RequestID, id)
	}
	if d.Decision != ports.ApprovalGranted {
		return d, nil
	}
	// The row was created from this request, so echoing its digest is
	// stating a fact this gate already holds. It is echoed rather than left
	// to the inbox because the kernel refuses an unbound grant — a grant
	// that reached the model without a binding would authorise whatever call
	// arrived next under the same id.
	if d.Digest == "" {
		d.Digest = req.Digest
	}
	return d, nil
}

// Pending answers a reconnecting console's question about a session's queue.
//
// A failure here is returned rather than swallowed: an empty queue and a
// queue that could not be read look identical to the console, and the second
// one must not render as "nothing is waiting for you".
func (g *InboxGate) Pending(ctx context.Context, sessionID string) ([]ports.ApprovalRequest, error) {
	if g == nil || g.inbox == nil {
		return nil, ErrGateNotWired
	}
	return g.inbox.Open(ctx, sessionID)
}

var _ ports.ApprovalGate = (*InboxGate)(nil)
