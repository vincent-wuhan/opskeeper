package policygate

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// A grant is two events in two places: a human says yes at the gate, and
// the tool runs later at the broker. The broker is host code the agent
// cannot reach, so it is the only place a mutating call can be refused a
// second time — and it needs evidence the first check happened.
//
// These tests are about that evidence: who can spend it, how often, for
// how long, and what happens when it is missing.

// grant runs one gated call to completion with a human granting it, and
// returns the gate.
func granted(t *testing.T, h *harness, c Call) {
	t.Helper()
	// Wait for a frame that is new, not for the first one. A second grant
	// on the same gate would otherwise find the earlier, already-answered
	// request and answer that one instead.
	before := len(h.frames.all())
	done := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), c)
		done <- o
	}()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) > before })
	pending := h.frames.all()[len(h.frames.all())-1]
	if err := h.gate.Decide(ports.Decision{
		RequestID: pending.RequestID,
		Digest:    pending.Digest,
		Decision:  ports.ApprovalGranted,
		DecidedBy: "op-1",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if o := <-done; o != Allowed {
		t.Fatalf("Admit returned %s after a grant, want Allowed", o)
	}
}

func TestAGrantedCallLeavesAReceiptTheBrokerCanClaim(t *testing.T) {
	h := newHarness(t, scripted(true, true), time.Minute)
	granted(t, h, writeCall())

	if !h.gate.ClaimReceipt(writeCall()) {
		t.Fatal("a granted call left no receipt for the broker to claim")
	}
}

func TestACallNobodyWasAskedAboutLeavesNoReceipt(t *testing.T) {
	// A read mints nothing. A receipt answers "did a human agree?", and
	// nobody was asked — so minting one per read would turn the gate into a
	// capability store for calls that never needed a capability.
	h := newHarness(t, scripted(true, false), time.Minute)
	read := Call{
		SessionID: "s-1", ToolName: "host_dmesg", Actor: "op-1",
		Arguments: json.RawMessage(`{"levels":"err"}`),
	}
	if o, _, _ := h.gate.Admit(context.Background(), read); o != Allowed {
		t.Fatalf("a read returned %s, want Allowed", o)
	}
	if h.gate.ClaimReceipt(read) {
		t.Error("a read left a receipt: the broker would be looking for a human who was never asked")
	}
	if n := h.gate.ReceiptCount(); n != 0 {
		t.Errorf("receipts = %d after a read, want none", n)
	}
}

func TestADeniedCallLeavesNoReceipt(t *testing.T) {
	// The refusal path is the one that matters most: a call a human said
	// no to must never be executable afterwards, whatever the agent does
	// next.
	h := newHarness(t, scripted(true, true), time.Minute)
	done := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), writeCall())
		done <- o
	}()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) == 1 })
	pending := h.frames.all()[0]
	if err := h.gate.Decide(ports.Decision{
		RequestID: pending.RequestID, Digest: pending.Digest, Decision: ports.ApprovalDenied,
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if o := <-done; o != Denied {
		t.Fatalf("Admit returned %s after a denial, want Denied", o)
	}
	if h.gate.ClaimReceipt(writeCall()) {
		t.Error("a denied call left a receipt the broker would have honoured")
	}
}

func TestAReceiptIsSpentOnTheFirstClaim(t *testing.T) {
	// One approval is one execution. A receipt that could be read twice
	// would let a model call an approved mutating tool a hundred times
	// with the same arguments on the strength of one click, which is not
	// what the operator agreed to.
	h := newHarness(t, scripted(true, true), time.Minute)
	granted(t, h, writeCall())

	if !h.gate.ClaimReceipt(writeCall()) {
		t.Fatal("the first claim found no receipt")
	}
	if h.gate.ClaimReceipt(writeCall()) {
		t.Error("the same grant was honoured twice")
	}
}

func TestAReceiptIsForOneExactCall(t *testing.T) {
	// The operator was shown a tool and its arguments. A different tool, or
	// the same tool with one argument changed, is a different call and
	// needs a different yes.
	h := newHarness(t, scripted(true, true), time.Minute)
	granted(t, h, writeCall())

	same := writeCall()
	if !h.gate.ClaimReceipt(same) {
		t.Fatal("the identical call was refused")
	}

	granted(t, h, writeCall())
	different := writeCall()
	different.Arguments = json.RawMessage(`{"service":"payments"}`)
	if h.gate.ClaimReceipt(different) {
		t.Error("a grant for restarting orders-api authorised restarting payments")
	}

	granted(t, h, writeCall())
	other := writeCall()
	other.ToolName = "delete_database"
	if h.gate.ClaimReceipt(other) {
		t.Error("a grant for one tool authorised another")
	}
}

func TestAReceiptDoesNotTravelBetweenConversations(t *testing.T) {
	// The digest covers the tool and the arguments — what the operator saw
	// — so two conversations can legitimately produce the same one. Without
	// the session in the key, one operator's approval in one conversation
	// would authorise the identical call in another operator's.
	h := newHarness(t, scripted(true, true), time.Minute)
	granted(t, h, writeCall())

	elsewhere := writeCall()
	elsewhere.SessionID = "s-2"
	if h.gate.ClaimReceipt(elsewhere) {
		t.Error("a grant in one conversation was spendable in another")
	}
}

func TestAReceiptExpiresRatherThanLingering(t *testing.T) {
	// The grant was an answer to a question about a situation that may no
	// longer hold. Past its window it is treated as if it had never
	// happened, which is the fail-closed direction.
	h := newHarness(t, scripted(true, true), time.Minute)
	granted(t, h, writeCall())

	h.now.advance(DefaultReceiptTTL + time.Second)
	if h.gate.ClaimReceipt(writeCall()) {
		t.Error("a grant older than its window was still honoured")
	}
	if n := h.gate.ReceiptCount(); n != 0 {
		t.Errorf("receipts = %d after the window, want them pruned", n)
	}
}

func TestClosingAConversationTakesItsUnspentGrantsWithIt(t *testing.T) {
	// A closed conversation takes its queue for the same reason: the
	// operator answered about a turn that no longer exists, so there is
	// nothing left for the answer to authorise.
	h := newHarness(t, scripted(true, true), time.Minute)
	granted(t, h, writeCall())
	if n := h.gate.ReceiptCount(); n != 1 {
		t.Fatalf("receipts = %d, want 1 before the close", n)
	}

	h.gate.DropSession("s-1")
	if h.gate.ClaimReceipt(writeCall()) {
		t.Error("a closed conversation's grant was still spendable")
	}
}

func TestReceiptsAreCountedSoAQueueThatIsNotBeingSpentIsVisible(t *testing.T) {
	// A gate that is granting and never having its receipts claimed means
	// something is answering the gate and not running the tool, which is
	// worth an operator's attention rather than a silent metric.
	h := newHarness(t, scripted(true, true), time.Minute)
	if n := h.gate.ReceiptCount(); n != 0 {
		t.Errorf("a fresh gate holds %d receipts", n)
	}
	// Two different calls, because two grants of the *same* call share one
	// receipt key by design: the second overwrites the first rather than
	// accumulating, so one operator's two approvals of an identical call
	// still buy one execution. The second run is refused legibly rather
	// than performed twice.
	granted(t, h, writeCall())
	other := writeCall()
	other.Arguments = json.RawMessage(`{"service":"payments"}`)
	granted(t, h, other)
	if n := h.gate.ReceiptCount(); n != 2 {
		t.Errorf("receipts = %d, want 2", n)
	}
	h.gate.ClaimReceipt(writeCall())
	if n := h.gate.ReceiptCount(); n != 1 {
		t.Errorf("receipts = %d after one claim, want 1", n)
	}
}

func TestAReceiptIsBoundToTheActorTheGateJudged(t *testing.T) {
	// The gate judged one actor. The broker re-resolves the actor from its
	// own record, and a receipt cannot be moved between actors to launder a
	// grant past a role check.
	h := newHarness(t, scripted(true, true), time.Minute)
	granted(t, h, writeCall())

	// The session and arguments match, so the key matches; the actor is not
	// part of it because the broker calls ClaimReceipt only after its own
	// role check has already passed. This test pins that ordering: a
	// receipt is evidence about a call, not a licence for an identity.
	if !h.gate.ClaimReceipt(writeCall()) {
		t.Fatal("the granted call could not be claimed")
	}
	other := writeCall()
	other.Actor = "someone-else"
	if h.gate.ClaimReceipt(other) {
		t.Error("the same receipt was claimable by a different actor")
	}
}
