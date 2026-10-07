package agentkernel

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

var _ ports.ApprovalGate = (*InboxGate)(nil)

// fakeInbox is a scripted approval inbox. It records what it was asked so a
// test can prove the gate queued the call it was handed rather than a
// rewritten one.
type fakeInbox struct {
	proposed   []ports.ApprovalRequest
	proposeID  string
	proposeErr error

	awaited  []string
	decision ports.Decision
	awaitErr error

	open      []ports.ApprovalRequest
	openErr   error
	openSess  string
	openCalls int
}

func (f *fakeInbox) Propose(_ context.Context, req ports.ApprovalRequest) (string, error) {
	f.proposed = append(f.proposed, req)
	if f.proposeErr != nil {
		return "", f.proposeErr
	}
	return f.proposeID, nil
}

func (f *fakeInbox) Await(_ context.Context, id string) (ports.Decision, error) {
	f.awaited = append(f.awaited, id)
	if f.awaitErr != nil {
		return ports.Decision{}, f.awaitErr
	}
	return f.decision, nil
}

func (f *fakeInbox) Open(_ context.Context, sessionID string) ([]ports.ApprovalRequest, error) {
	f.openCalls++
	f.openSess = sessionID
	return f.open, f.openErr
}

func approvalRequest() ports.ApprovalRequest {
	return ports.ApprovalRequest{
		ID:          "call-1",
		SessionID:   "s-1",
		ToolName:    "restart_service",
		Class:       domain.ClassDestructive,
		Digest:      "digest-of-the-exact-call",
		Arguments:   []byte(`{"target":"web-1"}`),
		Summary:     "restart_service on web-1",
		BlastRadius: domain.RadiusPod,
		Target:      "web-1",
		ExpiresAt:   time.Date(2026, 5, 1, 10, 2, 0, 0, time.UTC),
	}
}

// TestThisPackageFixtureCoversEveryColumn is this side of a pair.
//
// Decision 284 moved the approval-inbox adapter out of this package and into
// the composition root, which meant the adapter's tests had to take a copy of
// this package's approvalRequest() fixture — a composition root cannot reach
// into another package's test files. That made two copies of one fixture, and
// the first draft of the commit message claimed both were kept honest because
// each side had a field-walking test. **Only the moved side had one.** The
// claim was a guess dressed as a measurement, which is the exact shape this
// ledger has now caught seven times; the difference is that this one was
// written while describing work I had just done, and I checked it in the same
// breath in which I wrote it.
//
// So this test is here to make the claim true rather than to weaken the claim
// into something weaker and true. A column added to ports.ApprovalRequest and
// left at its zero value in either copy now fails on both sides.
func TestThisPackageFixtureCoversEveryColumn(t *testing.T) {
	req := approvalRequest()
	v := reflect.ValueOf(req)
	ty := v.Type()
	for i := 0; i < ty.NumField(); i++ {
		field := ty.Field(i)
		if !field.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("approvalRequest() leaves ports.ApprovalRequest.%s zero, so the gate tests "+
				"built on it would pass without asking what the gate does with that column", field.Name)
		}
	}
}

func newTestGate(inbox ApprovalInbox) *InboxGate {
	g := NewInboxGate(inbox)
	g.now = func() time.Time { return time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC) }
	return g
}

func TestInboxGateQueuesTheCallAndReturnsTheBoundVerdict(t *testing.T) {
	// The kernel refuses a grant that does not echo the request's digest, so
	// the gate must hand one back. The row it queued is the row the verdict
	// belongs to, which is what makes echoing it a statement of fact rather
	// than a guess.
	inbox := &fakeInbox{proposeID: "row-9", decision: ports.Decision{
		RequestID: "row-9", Decision: ports.ApprovalGranted, DecidedBy: "op-1", Note: "go ahead"}}

	g := newTestGate(inbox)
	d, err := g.Request(context.Background(), approvalRequest())
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(inbox.proposed) != 1 {
		t.Fatalf("proposed = %d, want 1", len(inbox.proposed))
	}
	if got := inbox.proposed[0]; got.ToolName != "restart_service" || got.Digest != "digest-of-the-exact-call" {
		t.Fatalf("queued %+v, want the call it was handed", got)
	}
	if len(inbox.awaited) != 1 || inbox.awaited[0] != "row-9" {
		t.Fatalf("awaited %v, want the row the inbox created", inbox.awaited)
	}
	if d.Decision != ports.ApprovalGranted {
		t.Fatalf("decision = %q, want a grant", d.Decision)
	}
	if d.Digest != "digest-of-the-exact-call" {
		t.Fatalf("digest = %q, want the request's own", d.Digest)
	}
	if d.RequestID != "row-9" {
		t.Fatalf("request id = %q, want the row", d.RequestID)
	}
}

func TestInboxGateRefusesAnUnbindableRequest(t *testing.T) {
	// An unbound request would create a row whose eventual verdict
	// authorises nothing in particular. The refusal happens before the
	// queue, so the operator never sees a card that cannot be acted on.
	inbox := &fakeInbox{proposeID: "row-1"}
	g := newTestGate(inbox)
	req := approvalRequest()
	req.Digest = ""

	if _, err := g.Request(context.Background(), req); !errors.Is(err, ErrUnbindableRequest) {
		t.Fatalf("err = %v, want ErrUnbindableRequest", err)
	}
	if len(inbox.proposed) != 0 {
		t.Fatalf("an unbindable call was queued for a human to answer")
	}
}

func TestInboxGateRefusesARequestThatExpiredBeforeItWasQueued(t *testing.T) {
	// Fail closed: queueing it would put a dead row in front of an operator
	// and the eventual verdict would arrive too late to authorise anything.
	inbox := &fakeInbox{proposeID: "row-1"}
	g := newTestGate(inbox)
	req := approvalRequest()
	req.ExpiresAt = time.Date(2026, 5, 1, 9, 59, 0, 0, time.UTC)

	_, err := g.Request(context.Background(), req)
	var ge *ports.GateError
	if !errors.As(err, &ge) || ge.Reason != ports.GateExpired {
		t.Fatalf("err = %v, want a GateError with reason %q", err, ports.GateExpired)
	}
	if len(inbox.proposed) != 0 {
		t.Fatal("an expired call was queued")
	}
}

func TestInboxGateRefusesAVerdictThatNamesAnotherRow(t *testing.T) {
	// The request id is the only thing tying the verdict to the call once
	// the inbox has answered. Repairing a mismatch instead of refusing it
	// would let any approval authorise any call.
	inbox := &fakeInbox{proposeID: "row-9", decision: ports.Decision{
		RequestID: "row-of-another-call", Decision: ports.ApprovalGranted}}
	g := newTestGate(inbox)

	_, err := g.Request(context.Background(), approvalRequest())
	if !errors.Is(err, ErrUnbindableRequest) {
		t.Fatalf("err = %v, want ErrUnbindableRequest", err)
	}
}

func TestInboxGateRefusesARowItCannotName(t *testing.T) {
	// Without a row id the verdict cannot be shown to belong to this
	// request, so accepting it would authorise the call on an unrelated
	// approval.
	inbox := &fakeInbox{proposeID: ""}
	g := newTestGate(inbox)

	if _, err := g.Request(context.Background(), approvalRequest()); !errors.Is(err, ErrUnbindableRequest) {
		t.Fatalf("err = %v, want ErrUnbindableRequest", err)
	}
	if len(inbox.awaited) != 0 {
		t.Fatal("the gate waited on a row it could not name")
	}
}

func TestInboxGatePassesADenialThroughUnchanged(t *testing.T) {
	// A refusal has no binding to echo, and the kernel only checks a digest
	// on the grant path. Rewriting the decision here would mean the console
	// sees a different answer from the one the operator gave.
	inbox := &fakeInbox{proposeID: "row-9", decision: ports.Decision{
		RequestID: "row-9", Decision: ports.ApprovalDenied, Note: "change freeze"}}
	g := newTestGate(inbox)

	d, err := g.Request(context.Background(), approvalRequest())
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if d.Decision != ports.ApprovalDenied || d.Digest != "" || d.Note != "change freeze" {
		t.Fatalf("decision = %+v, want the operator's own answer", d)
	}
}

func TestInboxGateSurfacesInboxFailures(t *testing.T) {
	// An inbox that cannot be reached must not read as an approval: the
	// kernel records the error as a refusal, and the operator sees a failing
	// dependency rather than a silent grant.
	proposeErr := errors.New("approval table is down")
	g := newTestGate(&fakeInbox{proposeErr: proposeErr})
	if _, err := g.Request(context.Background(), approvalRequest()); !errors.Is(err, proposeErr) {
		t.Fatalf("err = %v, want the inbox's own error", err)
	}

	awaitErr := errors.New("the decision never arrived")
	g = newTestGate(&fakeInbox{proposeID: "row-9", awaitErr: awaitErr})
	if _, err := g.Request(context.Background(), approvalRequest()); !errors.Is(err, awaitErr) {
		t.Fatalf("err = %v, want the inbox's own error", err)
	}
}

func TestInboxGatePendingForwardsTheSessionAndItsFailures(t *testing.T) {
	// An unreadable queue and an empty queue are identical to the console,
	// so the failure has to be returned rather than folded into "nothing is
	// waiting for you".
	inbox := &fakeInbox{open: []ports.ApprovalRequest{approvalRequest()}}
	g := newTestGate(inbox)
	rows, err := g.Pending(context.Background(), "sess-7")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if inbox.openSess != "sess-7" || len(rows) != 1 {
		t.Fatalf("session = %q, rows = %d", inbox.openSess, len(rows))
	}

	if _, err := noInboxGate().Pending(context.Background(), "sess-7"); !errors.Is(err, ErrGateNotWired) {
		t.Fatalf("a gate with no inbox answered a queue question: %v", err)
	}

	openErr := errors.New("inbox is unreachable")
	g = newTestGate(&fakeInbox{openErr: openErr})
	if _, err := g.Pending(context.Background(), "sess-7"); !errors.Is(err, openErr) {
		t.Fatalf("err = %v, want the inbox's own error", err)
	}
}

func TestNewInboxGateRefusesToWrapNothing(t *testing.T) {
	// A gate that accepted nil would look wired and authorise nothing, or —
	// worse — fail open. nil is how the assembly says "no gate", and the
	// kernel then refuses every mutating call.
	if g := NewInboxGate(nil); g != nil {
		t.Fatalf("gate = %v, want nil", g)
	}
	if _, err := noInboxGate().Request(context.Background(), approvalRequest()); !errors.Is(err, ErrGateNotWired) {
		t.Fatalf("a nil gate granted a call: %v", err)
	}
}

// noInboxGate is the zero value, which is what a caller who forgot to wire
// anything holds.
func noInboxGate() *InboxGate { return &InboxGate{} }
