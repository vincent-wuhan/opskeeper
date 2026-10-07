package policygate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// harness builds a gate over the given policy with recording audit and
// frames, and a controllable clock.
type harness struct {
	gate   *Gate
	audit  *recordingAudit
	frames *frameRecorder
	now    *fakeClock
	ids    *countingIDs
}

type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func newHarness(t *testing.T, policy Policy, ttl time.Duration) *harness {
	t.Helper()
	audit := &recordingAudit{}
	frames := &frameRecorder{}
	clock := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	ids := &countingIDs{}
	g, err := New(Options{
		Policy: policy,
		Audit:  audit,
		Emit:   frames.sink(),
		Now:    clock.now,
		NewID:  ids.next,
		TTL:    ttl,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{gate: g, audit: audit, frames: frames, now: clock, ids: ids}
}

func scripted(permitted, needsApproval bool) *scriptedPolicy {
	return &scriptedPolicy{permitted: permitted, needsApproval: needsApproval}
}

// --- the allow path -----------------------------------------------------

func TestAReadRunsWithoutAskingAnyone(t *testing.T) {
	h := newHarness(t, scripted(true, false), time.Minute)
	outcome, reason, err := h.gate.Admit(context.Background(), readCall())
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if outcome != Allowed || reason != "" {
		t.Errorf("outcome = %s/%q, want allowed", outcome, reason)
	}
	if n := len(h.frames.all()); n != 0 {
		t.Errorf("%d approval frames for a read: nobody should be asked about looking at a process list", n)
	}
	if n := h.audit.count(ports.ActionToolCall); n != 1 {
		t.Errorf("ledger rows = %d, want 1: a read leaves a record, or the write rows are not believable", n)
	}
}

func TestARefusedCallIsBlockedAndSaysWhy(t *testing.T) {
	pol := scripted(false, false)
	pol.reason = "restart_service is not in this node's tool set"
	h := newHarness(t, pol, time.Minute)

	outcome, reason, err := h.gate.Admit(context.Background(), writeCall())
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if outcome != Blocked {
		t.Errorf("outcome = %s, want blocked", outcome)
	}
	if reason == "" {
		t.Error("a blocked call gave no reason: the agent would retry it verbatim forever")
	}
	if n := len(h.frames.all()); n != 0 {
		t.Errorf("%d approval frames for a blocked call: a refusal is not something a human can grant", n)
	}
	if n := h.audit.count(ports.ActionToolBlocked); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// --- the approval round trip --------------------------------------------

func TestAMutatingCallWaitsForAHumanAndThenRuns(t *testing.T) {
	h := newHarness(t, scripted(true, true), time.Minute)

	result := make(chan struct {
		outcome Outcome
		reason  string
	}, 1)
	go func() {
		o, r, err := h.gate.Admit(context.Background(), writeCall())
		if err != nil {
			t.Errorf("Admit: %v", err)
		}
		result <- struct {
			outcome Outcome
			reason  string
		}{o, r}
	}()

	// The request reaches the console before anyone is asked to decide.
	waitFor(t, "the approval request to reach the console", func() bool {
		return len(h.frames.all()) == 1
	})
	pending := h.frames.all()[0]
	if pending.Decision != "" {
		t.Errorf("the pending frame already carried a decision %q", pending.Decision)
	}
	if pending.Tool != "restart_service" {
		t.Errorf("frame tool = %q, want restart_service", pending.Tool)
	}
	if pending.BlastRadius == "" {
		t.Error("the frame carried no blast radius: the operator cannot judge a restart without knowing what it reaches")
	}
	if pending.RequestID == "" || pending.Digest == "" {
		t.Error("the frame is missing the request id or digest, so the decision could not be bound to this call")
	}

	if err := h.gate.Decide(ports.Decision{
		RequestID: pending.RequestID,
		Digest:    pending.Digest,
		Decision:  ports.ApprovalGranted,
		DecidedBy: "alice",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	select {
	case got := <-result:
		if got.outcome != Allowed {
			t.Errorf("outcome = %s (%s), want allowed", got.outcome, got.reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Admit never returned after a grant")
	}
	if n := h.audit.count(ports.ActionApprovalGrant); n != 1 {
		t.Errorf("grant rows = %d, want 1", n)
	}
	if n := h.frames.countOf(string(ports.ApprovalGranted)); n != 1 {
		t.Errorf("resolution frames = %d, want 1: the console has to learn the call is cleared to run", n)
	}
}

func TestADenialStopsTheCallAndExplainsItself(t *testing.T) {
	h := newHarness(t, scripted(true, true), time.Minute)
	go func() { _, _, _ = h.gate.Admit(context.Background(), writeCall()) }()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) == 1 })
	pending := h.frames.all()[0]

	if err := h.gate.Decide(ports.Decision{
		RequestID: pending.RequestID,
		Digest:    pending.Digest,
		Decision:  ports.ApprovalDenied,
		DecidedBy: "alice",
		Note:      "this is the primary during an incident",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	waitFor(t, "the resolution frame", func() bool { return h.frames.countOf(string(ports.ApprovalDenied)) == 1 })

	if n := h.audit.count(ports.ActionApprovalDeny); n != 1 {
		t.Errorf("deny rows = %d, want 1: a refusal is exactly as auditable as a grant", n)
	}
	resolved := h.frames.all()
	last := resolved[len(resolved)-1]
	if last.Note != "this is the primary during an incident" {
		t.Errorf("note = %q, want the operator's reason: a bare deny is not actionable", last.Note)
	}
}

// --- the three ways out that are all denials ----------------------------

func TestAGrantForADifferentCallDoesNotAuthoriseThisOne(t *testing.T) {
	// The digest is the whole binding. An agent that re-plans and produces
	// a different call must not be able to spend a grant made for the
	// previous one — that is how a "restart the canary" approval turns
	// into "restart the cluster".
	h := newHarness(t, scripted(true, true), time.Minute)
	result := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), writeCall())
		result <- o
	}()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) == 1 })
	pending := h.frames.all()[0]

	// A grant carrying the digest of a different call.
	err := h.gate.Decide(ports.Decision{
		RequestID: pending.RequestID,
		Digest:    Digest(Call{ToolName: "restart_service", Arguments: json.RawMessage(`{"service":"payments"}`)}),
		Decision:  ports.ApprovalGranted,
	})
	if err == nil {
		t.Fatal("a grant for a different call was accepted")
	}
	// Refusing it must not consume the request. The operator was shown this
	// call, so this call is still answerable - a gate that ate the request
	// on a bad digest would leave the agent waiting out its whole TTL for a
	// decision that can no longer arrive.
	if n := len(h.gate.Pending("")); n != 1 {
		t.Fatalf("pending = %d after a mismatched digest, want 1: the request was consumed by a decision that did not match it", n)
	}
	select {
	case got := <-result:
		t.Fatalf("Admit returned %s while a call was still awaiting a real decision", got)
	case <-time.After(200 * time.Millisecond):
	}

	// The right digest still applies, which is the point: the mismatch was
	// refused, not the request.
	if err := h.gate.Decide(ports.Decision{
		RequestID: pending.RequestID,
		Digest:    pending.Digest,
		Decision:  ports.ApprovalDenied,
	}); err != nil {
		t.Fatalf("Decide with the right digest: %v", err)
	}
	select {
	case got := <-result:
		if got == Allowed {
			t.Fatal("a grant for a different call authorised this one")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Admit never returned")
	}
}

func TestAnUnansweredRequestIsDeniedAtTheDeadline(t *testing.T) {
	// The agent's situation may no longer be the one the operator was
	// shown. Letting the call through on silence would make a restart
	// happen to a service nobody agreed to.
	h := newHarness(t, scripted(true, true), 50*time.Millisecond)
	result := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), writeCall())
		result <- o
	}()
	select {
	case got := <-result:
		if got == Allowed {
			t.Fatal("an unanswered approval allowed the call")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Admit blocked past the deadline: the queue would grow without bound")
	}
	if st := h.gate.Snapshot(); st.Expired != 1 {
		t.Errorf("expired counter = %d, want 1: a node that is silently failing closed must be able to say so", st.Expired)
	}
	if n := h.audit.count(ports.ActionApprovalDeny); n != 1 {
		t.Errorf("deny rows = %d, want 1: an expiry is a denial and belongs in the ledger", n)
	}
}

func TestACancelledCallIsDeniedAndTakesItsRequestWithIt(t *testing.T) {
	// Otherwise an operator answers an approval for a turn that no longer
	// exists, from a page they thought they had left.
	h := newHarness(t, scripted(true, true), time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(ctx, writeCall())
		result <- o
	}()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) == 1 })
	cancel()

	select {
	case got := <-result:
		if got == Allowed {
			t.Fatal("a cancelled call was allowed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Admit never returned after its caller went away")
	}
	if n := len(h.gate.Pending("")); n != 0 {
		t.Errorf("pending = %d after a cancellation, want 0: an orphan request would sit in the queue for ever", n)
	}
}

func TestADecisionForAnUnknownRequestIsRefusedNotApplied(t *testing.T) {
	// A console replaying a stale click must not be able to grant a
	// request that is no longer outstanding.
	h := newHarness(t, scripted(true, true), time.Minute)
	err := h.gate.Decide(ports.Decision{RequestID: "ar-nope", Decision: ports.ApprovalGranted})
	if err == nil {
		t.Fatal("a decision for an unknown request was accepted")
	}
}

func TestSomethingThatIsNotADecisionIsRefused(t *testing.T) {
	h := newHarness(t, scripted(true, true), time.Minute)
	go func() { _, _, _ = h.gate.Admit(context.Background(), writeCall()) }()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) == 1 })
	pending := h.frames.all()[0]

	if err := h.gate.Decide(ports.Decision{RequestID: pending.RequestID, Decision: "maybe"}); err == nil {
		t.Error("a non-decision was accepted: the gate's vocabulary is grant or deny and nothing else")
	}
}

func TestAGrantWithNoDigestIsRefused(t *testing.T) {
	// A request id is a handle, not a proof. It travels over a tunnel and
	// through a console, so an id alone cannot say which call an operator
	// was looking at - and a decision carrying only an id could be lifted
	// from one call and replayed onto another. The digest is the proof, so
	// its absence has to be a refusal.
	//
	// The cost is that a console which cannot echo the digest cannot grant
	// anything. That is a console bug to fix, and failing closed surfaces
	// it on the first click rather than after an incident.
	h := newHarness(t, scripted(true, true), time.Minute)
	result := make(chan Outcome, 1)
	go func() {
		o, _, _ := h.gate.Admit(context.Background(), writeCall())
		result <- o
	}()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) == 1 })
	pending := h.frames.all()[0]

	if err := h.gate.Decide(ports.Decision{
		RequestID: pending.RequestID,
		Decision:  ports.ApprovalGranted,
	}); err == nil {
		t.Fatal("a grant carrying no digest was accepted")
	}
	if n := len(h.gate.Pending("")); n != 1 {
		t.Errorf("pending = %d after a digestless grant, want 1: the request should survive a decision that did not match it", n)
	}
	select {
	case got := <-result:
		t.Fatalf("Admit returned %s on a digestless grant", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// --- the queue ----------------------------------------------------------

func TestPendingIsScopedToTheConversationThatOwnsIt(t *testing.T) {
	// A console reloading must see its own queue, and only its own. Two
	// operators on one node would otherwise be answering each other's
	// restarts.
	h := newHarness(t, scripted(true, true), time.Hour)
	for _, call := range []Call{
		{SessionID: "s-1", ToolName: "restart_service", Class: domain.ClassDestructive, Actor: "op-1"},
		{SessionID: "s-2", ToolName: "restart_service", Class: domain.ClassDestructive, Actor: "op-2"},
	} {
		go func() { _, _, _ = h.gate.Admit(context.Background(), call) }()
	}
	waitFor(t, "both requests", func() bool { return len(h.gate.Pending("")) == 2 })

	if got := len(h.gate.Pending("s-1")); got != 1 {
		t.Errorf("s-1 sees %d requests, want 1", got)
	}
	if got := h.gate.Pending("nobody"); len(got) != 0 {
		t.Errorf("an unrelated session sees %d requests", len(got))
	}
}

func TestClosingAConversationDropsItsApprovals(t *testing.T) {
	// An operator approving a restart from a page they left an hour ago is
	// approving a diagnosis that has since been superseded.
	h := newHarness(t, scripted(true, true), time.Hour)
	go func() {
		_, _, _ = h.gate.Admit(context.Background(), Call{
			SessionID: "s-1", ToolName: "restart_service", Class: domain.ClassDestructive, Actor: "op-1",
		})
	}()
	waitFor(t, "the request", func() bool { return len(h.gate.Pending("s-1")) == 1 })

	if n := h.gate.DropSession("s-1"); n != 1 {
		t.Errorf("DropSession dropped %d requests, want 1", n)
	}
	if n := len(h.gate.Pending("s-1")); n != 0 {
		t.Errorf("pending = %d after dropping the conversation, want 0", n)
	}
	// And the waiters it was holding are released rather than leaked.
	waitFor(t, "the blocked calls to be released", func() bool {
		return h.gate.Snapshot().Denied == 1
	})
}

// --- the ledger ---------------------------------------------------------

func TestEveryExitWritesExactlyOneRow(t *testing.T) {
	// The absence of a row is how a reader concludes nothing was
	// attempted. One row per exit, whatever the exit was, is what makes
	// the ledger worth reading.
	// The real policy, not a scripted one: this test is about the four
	// exits, and a policy that made reads need approval would turn the
	// first exit into an approval round trip and the counts meaningless.
	reg := NewRegistry()
	if err := reg.BindAll(
		ToolBinding{Name: "get_process_list", Class: domain.ClassRead, FromPlugin: "readonly"},
		ToolBinding{Name: "restart_service", Class: domain.ClassDestructive, FromPlugin: "restart"},
	); err != nil {
		t.Fatalf("BindAll: %v", err)
	}
	h := newHarness(t, reg.Policy(domain.ClassDestructive), 30*time.Millisecond)

	// Allowed read.
	if _, _, err := h.gate.Admit(context.Background(), readCall()); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	// Granted. A second request after the first has resolved, so the two
	// requests are distinguishable and the row count is meaningful.
	go func() { _, _, _ = h.gate.Admit(context.Background(), writeCall()) }()
	first := awaitPending(t, h, "the first request")
	if err := h.gate.Decide(ports.Decision{
		RequestID: first.RequestID, Digest: first.Digest, Decision: ports.ApprovalGranted,
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	waitFor(t, "the grant", func() bool { return h.audit.count(ports.ActionApprovalGrant) == 1 })

	// Denied at the deadline.
	_, _, _ = h.gate.Admit(context.Background(), writeCall())

	// One row for the read, two for each approval round trip: the request
	// and its resolution. Nothing is counted twice and nothing is skipped.
	if n := len(h.audit.entries); n != 5 {
		t.Errorf("ledger rows = %d, want 5: one for the read, then a request and a resolution for each of the two approvals", n)
	}
	if n := h.audit.count(ports.ActionApprovalRequest); n != 2 {
		t.Errorf("request rows = %d, want 2", n)
	}
}

func TestABrokenLedgerDoesNotFailTheCall(t *testing.T) {
	// Refusing every tool because the audit store is down would turn an
	// observability problem into an outage. The failure has to be loud in
	// the sink's own terms, not in the gate refusing work.
	h := newHarness(t, scripted(true, false), time.Minute)
	h.audit.fail = true
	outcome, _, err := h.gate.Admit(context.Background(), readCall())
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if outcome != Allowed {
		t.Error("a failing ledger stopped a read-only call")
	}
}

func TestALedgerRowNamesTheTargetAndTheClass(t *testing.T) {
	// A row that says only "tool_call" is not evidence of anything.
	h := newHarness(t, scripted(true, true), time.Minute)
	go func() { _, _, _ = h.gate.Admit(context.Background(), writeCall()) }()
	waitFor(t, "the request", func() bool { return h.audit.count(ports.ActionApprovalRequest) == 1 })

	entry, ok := h.audit.last()
	if !ok {
		t.Fatal("no ledger row")
	}
	if entry.Target != "orders-api-7d9" {
		t.Errorf("target = %q, want the resolved resource", entry.Target)
	}
	if entry.Class != string(domain.ClassDestructive) {
		t.Errorf("class = %q, want destructive", entry.Class)
	}
	if entry.Actor == "" {
		t.Error("the row named no actor: the ledger has to say who asked")
	}
	if len(entry.Detail) == 0 {
		t.Error("the row carried no detail: the request id and digest are what make it followable")
	}
}

// --- concurrency --------------------------------------------------------

func TestConcurrentCallsEachGetTheirOwnRequest(t *testing.T) {
	// A node runs several investigations at once. Two calls sharing one
	// request would mean one operator's decision silently covering both.
	//
	// The conversations differ while the call does not — same tool, same
	// arguments, eight sessions — because that is the pair the fence has to
	// keep apart. The idempotency key includes the session precisely so
	// this case does not collapse; the collapse it does perform is covered
	// in fence_test.go, and it is the same conversation asking twice.
	h := newHarness(t, scripted(true, true), time.Hour)
	const n = 8
	outcomes := make(chan Outcome, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			o, _, _ := h.gate.Admit(context.Background(), Call{
				SessionID: fmt.Sprintf("s-%d", i), ToolName: "restart_service",
				Class: domain.ClassDestructive, Actor: "op-1",
			})
			outcomes <- o
		}(i)
	}
	waitFor(t, "all the requests", func() bool { return len(h.gate.Pending("")) == n })

	// One decision per request, and each one releases exactly one call.
	// Granting only the first would prove nothing: the other seven would
	// still be indistinguishable from a shared request if the gate had
	// collapsed them into one.
	for _, req := range h.gate.Pending("") {
		if err := h.gate.Decide(ports.Decision{RequestID: req.ID, Digest: req.Digest, Decision: ports.ApprovalGranted}); err != nil {
			t.Errorf("Decide %s: %v", req.ID, err)
		}
	}
	for i := 0; i < n; i++ {
		select {
		case got := <-outcomes:
			if got != Allowed {
				t.Errorf("call %d = %s, want allowed: its own grant was not applied", i, got)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of %d calls returned", i, n)
		}
	}
}

func TestOnlyOneOfTwoRacingDecisionsTakesEffect(t *testing.T) {
	// A double-clicked approve button, or a replayed request. Whoever
	// loses must not overwrite the winner's answer in the ledger.
	h := newHarness(t, scripted(true, true), time.Hour)
	go func() { _, _, _ = h.gate.Admit(context.Background(), writeCall()) }()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) == 1 })
	first := h.frames.all()[0]
	id, digest := first.RequestID, first.Digest

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = h.gate.Decide(ports.Decision{RequestID: id, Digest: digest, Decision: ports.ApprovalGranted})
		}(i)
	}
	wg.Wait()
	accepted := 0
	for _, err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("%d of 2 decisions were accepted, want 1: a resolved request must not be answerable twice", accepted)
	}
}

// --- construction -------------------------------------------------------

func TestNewRefusesToBuildWithoutAPolicy(t *testing.T) {
	// A gate with no policy would have to decide by accident.
	if _, err := New(Options{}); err == nil {
		t.Error("a gate with no policy was built")
	}
}

func TestAMintedRequestIDIsNotGuessable(t *testing.T) {
	// The id is the only handle an operator's decision has.
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		id := NewRequestID()
		if seen[id] {
			t.Fatalf("NewRequestID repeated %q", id)
		}
		seen[id] = true
		if len(id) < 16 {
			t.Fatalf("NewRequestID = %q, too short to be unguessable", id)
		}
	}
}

func TestTheDigestCoversTheCallAndNotTheConversation(t *testing.T) {
	// A grant is about what the call does. Binding it to a session would
	// mean an operator who approved a restart on one conversation could not
	// approve the identical call on another — and, worse, that the same
	// call under two sessions would produce two different digests for one
	// decision.
	a := Call{ToolName: "restart_service", SessionID: "s-1", Arguments: json.RawMessage(`{"service":"a"}`)}
	b := Call{ToolName: "restart_service", SessionID: "s-2", Arguments: json.RawMessage(`{"service":"a"}`)}
	if Digest(a) != Digest(b) {
		t.Error("the digest changed with the conversation: one decision would no longer cover one call")
	}
	c := Call{ToolName: "restart_service", SessionID: "s-1", Arguments: json.RawMessage(`{"service":"b"}`)}
	if Digest(a) == Digest(c) {
		t.Error("the digest ignored the arguments: a grant for one service would authorise another")
	}
}

func TestAWaitingCallIsCountedAsPendingNotDenied(t *testing.T) {
	// A node health page that counts a waiting approval as a denial would
	// page somebody about a call that is working exactly as designed.
	h := newHarness(t, scripted(true, true), time.Hour)
	go func() { _, _, _ = h.gate.Admit(context.Background(), writeCall()) }()
	waitFor(t, "the request", func() bool { return len(h.gate.Pending("")) == 1 })
	st := h.gate.Snapshot()
	if st.Pending != 1 {
		t.Errorf("pending = %d, want 1", st.Pending)
	}
	if st.Denied != 0 || st.Granted != 0 {
		t.Errorf("a waiting request was counted as decided: %+v", st)
	}
}

func TestTheConsoleSeesBothHalvesOfARoundTrip(t *testing.T) {
	// A request with no resolution leaves an approval button spinning for
	// ever; a resolution with no request means nothing was asked.
	h := newHarness(t, scripted(true, true), time.Hour)
	go func() { _, _, _ = h.gate.Admit(context.Background(), writeCall()) }()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) == 1 })
	if err := h.gate.Decide(ports.Decision{
		RequestID: h.frames.all()[0].RequestID,
		Digest:    h.frames.all()[0].Digest,
		Decision:  ports.ApprovalGranted,
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	waitFor(t, "the resolution", func() bool { return len(h.frames.all()) == 2 })

	frames := h.frames.all()
	if frames[0].Decision != "" {
		t.Error("the first frame already carried a decision")
	}
	if frames[1].Decision != string(ports.ApprovalGranted) {
		t.Errorf("resolution decision = %q, want grant", frames[1].Decision)
	}
	if frames[0].RequestID != frames[1].RequestID {
		t.Error("the two halves of the round trip name different requests: the console cannot pair them")
	}
}

func TestFramesNeverCarryARawArgumentPayload(t *testing.T) {
	// The digest is echoed instead of the arguments: a console that
	// received the full payload would be storing a copy of whatever the
	// agent proposed, including whatever secrets it had helpfully inlined.
	h := newHarness(t, scripted(true, true), time.Hour)
	go func() {
		_, _, _ = h.gate.Admit(context.Background(), Call{
			SessionID: "s-1", ToolName: "restart_service", Class: domain.ClassDestructive,
			Actor: "op-1", Target: "orders-api",
			Arguments: json.RawMessage(`{"service":"orders","token":"secret-value"}`),
		})
	}()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) == 1 })
	body, err := json.Marshal(h.frames.all()[0])
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if strings.Contains(string(body), "secret-value") {
		t.Error("the approval frame carried the argument payload to the console")
	}
}

// awaitPending returns the frame for the approval request that is
// outstanding now.
//
// It picks the most recent pending frame rather than indexing a list: by
// the time a second test scenario runs, the first scenario's resolution
// frame is already in the same slice, and an index would silently pick the
// wrong one.
func awaitPending(t *testing.T, h *harness, what string) wire.ApprovalFrame {
	t.Helper()
	seen := len(h.frames.all())
	waitFor(t, what, func() bool {
		for _, f := range h.frames.all()[seen:] {
			if f.Decision == "" {
				return true
			}
		}
		return false
	})
	frames := h.frames.all()[seen:]
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].Decision == "" {
			return frames[i]
		}
	}
	t.Fatalf("no pending frame for %s", what)
	return wire.ApprovalFrame{}
}

// waitFor polls a condition, failing the test with a message an operator
// could act on.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var _ wire.ApprovalFrame

func TestACallIsRefusedRatherThanAdmittedUnderALiveRequestsHandle(t *testing.T) {
	// If a mint source hands out the same id twice, the second call must
	// not overwrite the first in the map. It would then share the first
	// call's decision, and an operator approving one restart would be
	// approving a second one they were never shown.
	g := newHarness(t, scripted(true, true), time.Hour)
	colliding, err := New(Options{
		Policy: scripted(true, true),
		Audit:  g.audit,
		Emit:   g.frames.sink(),
		Now:    g.now.now,
		NewID:  collidingIDs{}.next,
		TTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := make(chan Outcome, 1)
	go func() {
		o, _, _ := colliding.Admit(context.Background(), writeCall())
		first <- o
	}()
	waitFor(t, "the first request", func() bool { return len(colliding.Pending("")) == 1 })

	// Every id the mint source can produce is taken, so this one fails
	// closed instead of overwriting.
	//
	// It comes from another conversation on purpose. A second call in the
	// same conversation never reaches the mint at all: the session fence
	// holds it behind the first card, which is the behaviour under test in
	// fence_test.go and not this one.
	second := writeCall()
	second.SessionID = "s-2"
	outcome, reason, err := colliding.Admit(context.Background(), second)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if outcome != Blocked {
		t.Fatalf("outcome = %s, want blocked: there was no id left to admit it under", outcome)
	}
	if !strings.Contains(reason, "unique approval id") {
		t.Errorf("reason = %q, want it to name the id collision", reason)
	}
	// And the first call is untouched: still pending, still its own.
	if n := len(colliding.Pending("")); n != 1 {
		t.Errorf("pending = %d, want 1: the live request was overwritten", n)
	}
	if err := colliding.Decide(ports.Decision{
		RequestID: "ar-fixed", Digest: colliding.Pending("")[0].Digest, Decision: ports.ApprovalDenied,
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	select {
	case got := <-first:
		if got != Denied {
			t.Errorf("the first call = %s, want denied", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first call never returned")
	}
}

// gateDecision builds a grant for whatever request the gate currently has
// outstanding, so a composed test does not have to know the id scheme.
func gateDecision(t *testing.T, g *Gate) ports.Decision {
	t.Helper()
	pending := g.Pending("")
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	return ports.Decision{RequestID: pending[0].ID, Digest: pending[0].Digest, Decision: ports.ApprovalGranted}
}

func TestEveryFrameCarriesTheConversationItCameFrom(t *testing.T) {
	// The control plane routes a frame by conversation and drops one that
	// arrives without an id. An approval frame with no session would
	// therefore be delivered to nobody, and the operator would watch a
	// call sit blocked for the full TTL with no button anywhere.
	h := newHarness(t, scripted(true, true), time.Minute)
	go func() { _, _, _ = h.gate.Admit(context.Background(), writeCall()) }()
	waitFor(t, "the request", func() bool { return len(h.frames.all()) == 1 })

	if got := h.frames.sessionOf(0); got != "s-1" {
		t.Errorf("the request frame carried session %q, want s-1", got)
	}
	if err := h.gate.Decide(ports.Decision{
		RequestID: h.frames.all()[0].RequestID,
		Digest:    h.frames.all()[0].Digest,
		Decision:  ports.ApprovalDenied,
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	waitFor(t, "the resolution", func() bool { return len(h.frames.all()) == 2 })
	if got := h.frames.sessionOf(1); got != "s-1" {
		t.Errorf("the resolution frame carried session %q, want s-1", got)
	}
}
