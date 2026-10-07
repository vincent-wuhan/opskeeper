package policygate

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// This file is the plan's stage-1 item 1.3, tested at the kernel: one
// approval answers one call, no matter how many times the call is
// submitted, and a refusal cannot be retried into a different question.
//
// The three mechanisms under test live in fence.go. Each test here is
// written to fail if the corresponding mechanism is removed, not merely
// to exercise it: an idempotency check that asserted only "two calls, two
// outcomes" would pass just as happily without byKey.

// --- idempotency: one approval, one execution --------------------------

func TestTheSameCallSubmittedEightTimesIsOneQuestionAndOneExecution(t *testing.T) {
	// The failure this prevents is not a crash. It is a model that has
	// learned to submit the same restart three times and an operator who
	// approves the first card with no way to know the other two are the
	// same question. Eight identical submissions must therefore be one
	// card, and — the half that is easy to forget — one execution.
	h := newHarness(t, scripted(true, true), time.Hour)

	const n = 8
	outcomes := make(chan struct {
		outcome Outcome
		reason  string
	}, n)
	submit := func() {
		go func() {
			o, r, err := h.gate.Admit(context.Background(), writeCall())
			if err != nil {
				t.Errorf("Admit: %v", err)
			}
			outcomes <- struct {
				outcome Outcome
				reason  string
			}{o, r}
		}()
	}
	submit()

	waitFor(t, "the first submission to raise its request", func() bool {
		return len(h.gate.Pending("")) == 1
	})
	waitFor(t, "the first submission to render its card", func() bool {
		return len(h.frames.all()) == 1
	})
	for i := 1; i < n; i++ {
		submit()
	}

	// A second card would be indistinguishable from a second question in
	// the operator's queue, which is the whole point.
	waitFor(t, "the duplicates to have joined", func() bool {
		return len(h.frames.all()) == 1
	})
	waitFor(t, "all duplicate submissions to wait on that request", func() bool {
		h.gate.mu.Lock()
		defer h.gate.mu.Unlock()
		pending, ok := h.gate.byKey[keyOf(writeCall())]
		return ok && pending.waiters == n
	})

	pending := h.gate.Pending("")[0]
	if err := h.gate.Decide(ports.Decision{
		RequestID: pending.ID, Digest: pending.Digest,
		Decision: ports.ApprovalGranted, DecidedBy: "alice",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	for i := 0; i < n; i++ {
		select {
		case got := <-outcomes:
			if got.outcome != Allowed {
				t.Fatalf("submission %d = %s (%s), want allowed: it joined a granted request", i, got.outcome, got.reason)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of %d submissions returned", i, n)
		}
	}

	// Every submission was answered, and the answer was yes, every time.
	// But a grant is collectable once: one human click authorises one run,
	// and the broker refuses the other seven.
	if !h.gate.ClaimReceipt(writeCall()) {
		t.Error("the granted call could not claim its receipt: the approval answered nobody")
	}
	if h.gate.ClaimReceipt(writeCall()) {
		t.Error("the receipt was claimable twice: one approval ran the tool more than once")
	}
}

func TestACallCanBeAskedAgainOnceItsCardIsSettled(t *testing.T) {
	// The join covers the card that is in flight and nothing else. A fence
	// that remembered the answer would be a policy: a node that restarted
	// orders-api at 09:00 could never ask again at 10:00, and the operator
	// would have written a rule they never stated.
	//
	// A refusal is the deliberate exception, and the next tests cover it: a
	// "no" needs an index of its own precisely because the card is gone.
	h := newHarness(t, scripted(true, true), time.Hour)
	if got := admitAndSettle(t, h, writeCall(), ports.ApprovalGranted); got != Allowed {
		t.Fatalf("the call = %s, want allowed", got)
	}

	settled := make(chan struct {
		outcome Outcome
		reason  string
	}, 1)
	go func() {
		o, r, _ := h.gate.Admit(context.Background(), writeCall())
		settled <- struct {
			outcome Outcome
			reason  string
		}{o, r}
	}()
	waitFor(t, "a second card for a settled call", func() bool {
		return len(h.gate.Pending("")) == 1
	})
	decideAll(t, h, ports.ApprovalDenied)
	select {
	case got := <-settled:
		if got.outcome != Denied {
			t.Errorf("the repeat = %s (%s), want denied", got.outcome, got.reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the repeat never returned")
	}
}

// --- the receipt lease ---------------------------------------------------

func TestAGrantIsCollectableInsideItsLeaseAndNotAfterIt(t *testing.T) {
	// The grant and the execution are two events in two places. The
	// receipt is the evidence that bridges them, and evidence that is old
	// enough is worse than none: the broker would be running a tool on the
	// strength of an answer to a situation that has since changed.
	clock := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	audit := &recordingAudit{}
	frames := &frameRecorder{}
	ids := &countingIDs{}
	g, err := New(Options{
		Policy: scripted(true, true), Audit: audit, Emit: frames.sink(),
		Now: clock.now, NewID: ids.next, TTL: time.Hour, ReceiptTTL: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := &harness{gate: g, audit: audit, frames: frames, now: clock, ids: ids}

	if got := admitAndSettle(t, h, writeCall(), ports.ApprovalGranted); got != Allowed {
		t.Fatalf("the call = %s, want allowed", got)
	}
	// The plan's window: the handoff from gate to broker is inside one
	// extension Execute, so a grant is executable immediately and dead
	// shortly after.
	clock.advance(9 * time.Second)
	if !g.ClaimReceipt(writeCall()) {
		t.Fatal("a grant inside its lease was not collectable")
	}
	if g.ClaimReceipt(writeCall()) {
		t.Error("a claimed grant was collectable a second time")
	}

	// A second grant, aged out.
	if got := admitAndSettle(t, h, writeCall(), ports.ApprovalGranted); got != Allowed {
		t.Fatalf("the second call = %s, want allowed", got)
	}
	clock.advance(11 * time.Second)
	if g.ClaimReceipt(writeCall()) {
		t.Error("a grant past its lease still executed: the fail-closed direction is backwards")
	}
}

// --- the session fence ---------------------------------------------------

func TestASecondMutatingCallInOneConversationWaitsRatherThanQueuing(t *testing.T) {
	// One conversation, one human decision at a time. The operator is being
	// asked about a restart while the agent proposes a config change on the
	// same target; those may be the same mistake wearing two hats, and an
	// operator cannot compare two cards for one conversation as a decision.
	h := newHarness(t, scripted(true, true), time.Hour)

	first := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), callFor("orders-api"))
		first <- o
	}()
	waitFor(t, "the first card", func() bool { return len(h.gate.Pending("")) == 1 })

	sibling := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), callFor("payments-api"))
		sibling <- o
	}()

	// Give it long enough that a fence that was not there would have shown
	// a second card by now. A wait is not a proof, so the assertion is
	// made after a real delay rather than immediately.
	time.Sleep(50 * time.Millisecond)
	if n := len(h.gate.Pending("")); n != 1 {
		t.Fatalf("pending = %d, want 1: the sibling queued behind the operator instead of behind the fence", n)
	}
	select {
	case got := <-sibling:
		t.Fatalf("the sibling returned %s while the first card was still open", got)
	default:
	}

	// The first decision releases both: each call is still adjudicated on
	// its own merits, which is the difference between serialising and
	// dropping.
	decideAll(t, h, ports.ApprovalGranted)
	if got := <-first; got != Allowed {
		t.Errorf("first = %s, want allowed", got)
	}
	waitFor(t, "the sibling's own card", func() bool { return len(h.gate.Pending("")) == 1 })
	decideAll(t, h, ports.ApprovalGranted)
	if got := <-sibling; got != Allowed {
		t.Errorf("sibling = %s, want allowed", got)
	}
}

func TestAReadIsNotHeldBehindAnOpenApproval(t *testing.T) {
	// A human deciding about a restart should not also freeze the reads
	// that would tell the agent what to do next. Holding a read would make
	// the fence a liveness problem in the one place liveness is cheap.
	h := newHarness(t, &writeNeedsApproval{scriptedPolicy{permitted: true}}, time.Hour)
	go func() { _, _, _ = h.gate.Admit(context.Background(), callFor("orders-api")) }()
	waitFor(t, "the approval to open", func() bool { return len(h.gate.Pending("")) == 1 })

	read := readCall()
	read.SessionID = "s-1"
	done := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), read)
		done <- o
	}()
	select {
	case got := <-done:
		if got != Allowed {
			t.Errorf("read = %s, want allowed", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a read waited behind an approval: the fence must exempt it")
	}
}

func TestAClosedConversationReleasesTheCallsWaitingBehindIt(t *testing.T) {
	// The operator closed the tab. The waiter behind the open card is a
	// turn that no longer exists, and it would sit on its fence until its
	// own context expired — holding a conversation the node believes is
	// still running.
	h := newHarness(t, scripted(true, true), time.Hour)
	go func() { _, _, _ = h.gate.Admit(context.Background(), callFor("orders-api")) }()
	waitFor(t, "the first card", func() bool { return len(h.gate.Pending("")) == 1 })

	waiter := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), callFor("payments-api"))
		waiter <- o
	}()
	time.Sleep(50 * time.Millisecond)

	if dropped := h.gate.DropSession("s-1"); dropped != 1 {
		t.Errorf("DropSession dropped %d, want 1", dropped)
	}
	select {
	case got := <-waiter:
		if got != Denied {
			t.Errorf("the waiter returned %s, want denied: its conversation is gone", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter outlived the conversation it belonged to")
	}
	if n := len(h.gate.Pending("")); n != 0 {
		t.Errorf("pending = %d, want 0: a closed conversation left its queue behind", n)
	}
}

// --- the denial fence ----------------------------------------------------

func TestARefusalSurvivesTheLoopThatProducedTheQuestion(t *testing.T) {
	// A human says no. The agent asks again, immediately, with identical
	// arguments — and the second card looks like a fresh decision rather
	// than a repeat of a refusal already made. This is the fence's sharpest
	// edge, and the one with the most damage: "no" that means "not now, try
	// again" is a way to get a yes the operator never gave.
	h := newHarness(t, scripted(true, true), time.Hour)
	first := writeCall()
	if got := admitAndSettle(t, h, first, ports.ApprovalDenied); got != Denied {
		t.Fatalf("the first call = %s, want denied", got)
	}

	again := make(chan struct {
		outcome Outcome
		reason  string
	}, 1)
	go func() {
		o, r, _ := h.gate.Admit(context.Background(), writeCall())
		again <- struct {
			outcome Outcome
			reason  string
		}{o, r}
	}()

	select {
	case got := <-again:
		if got.outcome != Denied {
			t.Fatalf("the repeat = %s, want denied: a refusal is not a retry hint", got.outcome)
		}
		if got.reason == "" {
			t.Error("the repeat gave no reason: the agent cannot tell a remembered refusal from a fresh one")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the repeat waited for a human: it should have been refused by the remembered denial")
	}
	// No second card was ever raised, and the repeat was recorded as a
	// repeat so the ledger can be read as an answer to one question.
	if n := h.frames.countOf(string(ports.ApprovalDenied)); n != 1 {
		t.Errorf("denial frames = %d, want 1: the refusal produced a second card", n)
	}
	if n := len(h.gate.Pending("")); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
}

func TestARefusalOfOneCallDoesNotFenceItsSiblings(t *testing.T) {
	// The fence is keyed on the whole call — session, tool and arguments —
	// so a refusal to restart orders-api must not silently refuse a
	// different service in the same conversation. Over-fencing here would
	// be a denial the operator never made, which is the one failure mode
	// this package cannot have.
	h := newHarness(t, scripted(true, true), time.Hour)
	if got := admitAndSettle(t, h, callFor("orders-api"), ports.ApprovalDenied); got != Denied {
		t.Fatalf("the first call = %s, want denied", got)
	}
	pending := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), callFor("payments-api"))
		pending <- o
	}()
	waitFor(t, "the sibling's own card", func() bool { return len(h.gate.Pending("")) == 1 })
	decideAll(t, h, ports.ApprovalGranted)
	if got := <-pending; got != Allowed {
		t.Errorf("the sibling = %s, want allowed: it is a different call", got)
	}
}

func TestARefusalLapsesWithItsWindowAndALaterYesClearsAnEarlierNo(t *testing.T) {
	// A "no" that outlives its window is a policy nobody wrote, and a "no"
	// that survives a later "yes" for the same call is a decision that
	// cannot be overturned by the human who made it. Both directions are
	// the same requirement: a refusal is bounded like a grant.
	h := newHarness(t, scripted(true, true), time.Hour)
	if got := admitAndSettle(t, h, writeCall(), ports.ApprovalDenied); got != Denied {
		t.Fatalf("the first call = %s, want denied", got)
	}

	// Past the denial window, the same call is a new question.
	h.now.advance(DefaultReceiptTTL + time.Second)
	askable := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), writeCall())
		askable <- o
	}()
	waitFor(t, "the lapsed call to be askable again", func() bool {
		return len(h.gate.Pending("")) == 1
	})
	decideAll(t, h, ports.ApprovalGranted)
	if got := <-askable; got != Allowed {
		t.Fatalf("the lapsed call = %s, want allowed", got)
	}

	// And a grant clears the refusal that was there, so the call is not
	// half-refused: the receipt must be collectable straight away.
	if !h.gate.ClaimReceipt(writeCall()) {
		t.Error("a granted call was still fenced by an earlier refusal: the 'no' outlived the question")
	}
}

// --- helpers -------------------------------------------------------------

// writeNeedsApproval is the policy a real node runs with: a read is answered
// by the host, a write is answered by a human. The scripted policy applies
// one verdict to every call, which is why the read exemption of the fence
// cannot be expressed with it.
type writeNeedsApproval struct{ scriptedPolicy }

func (p *writeNeedsApproval) NeedsApproval(c Call) bool {
	return c.Class != domain.ClassRead
}

// callFor is a mutating call distinguished by the service it acts on.
func callFor(service string) Call {
	return Call{
		SessionID: "s-1", ToolName: "restart_service", Class: domain.ClassDestructive,
		Actor: "op-1", Target: service, Summary: "restart " + service,
		Arguments: json.RawMessage(fmt.Sprintf(`{"service":%q}`, service)),
	}
}

// admitAndSettle runs one call to its decision and reports the outcome.
func admitAndSettle(t *testing.T, h *harness, c Call, decision ports.ApprovalDecision) Outcome {
	t.Helper()
	before := len(h.gate.Pending(""))
	done := make(chan Outcome, 1)
	go func() {
		o, _, err := h.gate.Admit(context.Background(), c)
		if err != nil {
			t.Errorf("Admit: %v", err)
		}
		done <- o
	}()
	waitFor(t, "the card", func() bool { return len(h.gate.Pending("")) == before+1 })
	decideAll(t, h, decision)
	return <-done
}

// decideAll answers every outstanding request in the queue.
func decideAll(t *testing.T, h *harness, decision ports.ApprovalDecision) {
	t.Helper()
	for _, req := range h.gate.Pending("") {
		if err := h.gate.Decide(ports.Decision{
			RequestID: req.ID, Digest: req.Digest, Decision: decision, DecidedBy: "alice",
		}); err != nil {
			t.Errorf("Decide %s: %v", req.ID, err)
		}
	}
}
