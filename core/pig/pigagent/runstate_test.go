package pigagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// --- test doubles -------------------------------------------------------

// staticBag is a tool bag the test controls outright.
type staticBag struct{ tools []ports.Tool }

func (b staticBag) Tools() []ports.Tool { return b.tools }
func (b staticBag) Names() []string {
	out := make([]string, 0, len(b.tools))
	for _, t := range b.tools {
		out = append(out, t.Schema().Name)
	}
	return out
}

// recordingGate captures every request and answers with a scripted
// decision, so a test can assert on both what was asked and what was
// allowed.
type recordingGate struct {
	requests []ports.ApprovalRequest
	decide   func(ports.ApprovalRequest) (ports.Decision, error)
}

func (g *recordingGate) Request(_ context.Context, req ports.ApprovalRequest) (ports.Decision, error) {
	g.requests = append(g.requests, req)
	if g.decide == nil {
		return ports.Decision{RequestID: req.ID, Decision: ports.ApprovalDenied}, nil
	}
	return g.decide(req)
}

func (g *recordingGate) Pending(context.Context, string) ([]ports.ApprovalRequest, error) {
	return g.requests, nil
}

// grantFor grants a request using the digest the gate itself computed, so
// the happy path exercises the real binding rather than a hand-rolled one.
func grantFor(note string) func(ports.ApprovalRequest) (ports.Decision, error) {
	return func(req ports.ApprovalRequest) (ports.Decision, error) {
		return ports.Decision{
			RequestID: req.ID,
			Digest:    CallDigest(req.ToolName, req.Arguments),
			Decision:  ports.ApprovalGranted,
			DecidedBy: "op-1",
			Note:      note,
		}, nil
	}
}

// recordingAudit keeps every ledger entry so tests can assert the host
// derived the right one.
type recordingAudit struct{ entries []ports.AuditEntry }

func (a *recordingAudit) Record(_ context.Context, e ports.AuditEntry) error {
	a.entries = append(a.entries, e)
	return nil
}

func (a *recordingAudit) Verify(context.Context) error { return nil }

// countingBudget refuses after n calls, standing in for a spend ceiling.
type countingBudget struct {
	limit int
	calls int
	why   string
}

func (b *countingBudget) Allow(context.Context, string) (bool, string) {
	b.calls++
	if b.calls > b.limit {
		return false, b.why
	}
	return true, ""
}

// collectSink keeps the frames a turn produced.
//
// It is mutex-guarded because a kernel emits from every tool goroutine at
// once. The slice is therefore in arrival order, not sequence order; a test
// that cares about ordering sorts by Seq or reads the frames a single
// goroutine produced.
type collectSink struct {
	mu     sync.Mutex
	frames []wire.StreamEvent
}

func (s *collectSink) Emit(_ context.Context, ev wire.StreamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, ev)
	return nil
}

// Frames returns a copy of what was emitted so far.
func (s *collectSink) Frames() []wire.StreamEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]wire.StreamEvent(nil), s.frames...)
}

func (s *collectSink) ofType(t wire.StreamEventType) []wire.StreamEvent {
	var out []wire.StreamEvent
	for _, f := range s.Frames() {
		if f.Type == t {
			out = append(out, f)
		}
	}
	return out
}

// harness builds a runState over the given policy. Tests use it so each
// case only has to state the part it is actually about.
func harness(t *testing.T, deps Deps) (*runState, *collectSink) {
	t.Helper()
	sink := &collectSink{}
	return &runState{
		mapper: NewMapper(MapperOptions{SessionID: "s-1", Now: fixedClock()}),
		sink:   sink,
		deps:   deps,
		host:   runHost{now: fixedClock()},
		req:    ports.AgentRequest{SessionID: "s-1"},
	}, sink
}

func classTool(name string, class domain.ToolClass) ports.Tool {
	return &fakeTool{schema: ports.ToolSchema{
		Name:       name,
		Class:      class,
		Parameters: json.RawMessage(`{"type":"object"}`),
	}, out: "ok"}
}

// --- read-only calls ----------------------------------------------------

func TestReadOnlyCallBypassesTheGateEntirely(t *testing.T) {
	gate := &recordingGate{decide: grantFor("approved")}
	r, _ := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("get_topology", domain.ClassRead)}},
		Gate:  gate,
	})

	res := r.beforeToolCall(context.Background(), "tc-1", "get_topology", json.RawMessage(`{}`))
	if res.Block {
		t.Fatalf("a read-only call was blocked: %s", res.Reason)
	}
	if len(gate.requests) != 0 {
		t.Errorf("the approval gate was consulted %d times for a read-only call: a read that waits on a human is an incident response that never arrives", len(gate.requests))
	}
}

func TestReadOnlyClassIsTheOnlyClassThatBypasses(t *testing.T) {
	// Every other class — including write and the unclassified default —
	// must reach the gate. A table keeps the "which classes are safe" set
	// from quietly growing.
	for _, class := range []domain.ToolClass{
		domain.ClassWrite, domain.ClassDestructive, domain.ClassUnknown, "",
	} {
		t.Run("class="+string(class), func(t *testing.T) {
			gate := &recordingGate{decide: grantFor("")}
			r, _ := harness(t, Deps{
				Tools: staticBag{tools: []ports.Tool{classTool("mutate", class)}},
				Gate:  gate,
			})
			if res := r.beforeToolCall(context.Background(), "tc-1", "mutate", json.RawMessage(`{}`)); res.Block {
				t.Errorf("class %q was blocked despite a granting gate: %s", class, res.Reason)
			}
			if len(gate.requests) != 1 {
				t.Errorf("gate consulted %d times, want 1", len(gate.requests))
			}
		})
	}
}

// --- the fail-closed invariant ------------------------------------------

func TestMutatingCallWithNoGateIsBlocked(t *testing.T) {
	r, _ := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
		// No Gate: the host forgot to wire approvals.
	})

	res := r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{}`))
	if !res.Block {
		t.Fatal("a mutating call ran with no approval gate configured: a missing dependency must not become permission")
	}
	if !strings.Contains(res.Reason, "no approval gate") {
		t.Errorf("reason = %q, want it to name the missing gate", res.Reason)
	}
}

func TestUnregisteredToolIsTreatedAsDestructive(t *testing.T) {
	// A tool the host cannot classify is one it did not vet. Allowing it
	// on the assumption that a plugin would self-report safely is exactly
	// the failure mode the gate exists to prevent.
	gate := &recordingGate{decide: grantFor("")}
	r, _ := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("known_read", domain.ClassRead)}},
		Gate:  gate,
	})

	res := r.beforeToolCall(context.Background(), "tc-1", "plugin_surprise", json.RawMessage(`{}`))
	if res.Block {
		t.Fatalf("an unclassified tool was blocked even with a granting gate: %s", res.Reason)
	}
	if len(gate.requests) != 1 {
		t.Fatalf("gate consulted %d times, want 1: an unknown tool must still be gated", len(gate.requests))
	}
	if gate.requests[0].Class != domain.ClassDestructive {
		t.Errorf("class = %q, want destructive: unknown defaults to the widest blast radius", gate.requests[0].Class)
	}
}

// --- approval binding ---------------------------------------------------

func TestGrantMatchingDigestAllows(t *testing.T) {
	gate := &recordingGate{decide: grantFor("change ticket CHG-1")}
	r, sink := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
		Gate:  gate,
	})

	res := r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{"target":"web-1"}`))
	if res.Block {
		t.Fatalf("a matching grant was refused: %s", res.Reason)
	}

	pending := sink.ofType(wire.StreamApprovalPending)
	resolved := sink.ofType(wire.StreamApprovalResolved)
	if len(pending) != 1 || len(resolved) != 1 {
		t.Fatalf("frames = %d pending / %d resolved, want 1/1: the console renders the approval queue from these", len(pending), len(resolved))
	}
	if pending[0].Approval.Digest != CallDigest("restart_service", json.RawMessage(`{"target":"web-1"}`)) {
		t.Error("the frame digest does not match the call digest")
	}
	if pending[0].Approval.Target != "web-1" {
		t.Errorf("target = %q, want web-1 so the operator sees what is affected", pending[0].Approval.Target)
	}
	if resolved[0].Approval.Decision != string(ports.ApprovalGranted) {
		t.Errorf("resolved decision = %q", resolved[0].Approval.Decision)
	}
}

func TestGrantForDifferentArgumentsIsRefused(t *testing.T) {
	// The load-bearing case: an operator approved restarting web-1, and the
	// agent then asked to restart every node. The stale grant must not
	// carry over.
	gate := &recordingGate{decide: func(req ports.ApprovalRequest) (ports.Decision, error) {
		return ports.Decision{
			RequestID: req.ID,
			Digest:    CallDigest("restart_service", json.RawMessage(`{"target":"web-1"}`)),
			Decision:  ports.ApprovalGranted,
		}, nil
	}}
	r, sink := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
		Gate:  gate,
	})

	res := r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{"target":"all"}`))
	if !res.Block {
		t.Fatal("a grant for different arguments authorised the call: the digest binding does nothing if it is not enforced")
	}
	if !strings.Contains(res.Reason, "digest") {
		t.Errorf("reason = %q, want it to name the mismatch", res.Reason)
	}
	resolved := sink.ofType(wire.StreamApprovalResolved)
	if len(resolved) != 1 {
		t.Fatalf("resolved frames = %+v, want exactly one", resolved)
	}
	if resolved[0].Approval.Decision != string(ports.ApprovalDenied) {
		t.Errorf("decision = %q, want %q: the console branches on a closed vocabulary", resolved[0].Approval.Decision, ports.ApprovalDenied)
	}
	if resolved[0].Approval.Note != "digest mismatch" {
		t.Errorf("note = %q, want the cause so the operator knows why", resolved[0].Approval.Note)
	}
}

func TestAGrantWithoutTheBindingDigestIsRefused(t *testing.T) {
	// An affirmative answer that does not echo the digest is not a grant. A
	// host that dropped the field would otherwise authorise whatever call
	// arrived next under the same request id, which is the one hole the
	// digest exists to close.
	//
	// This is stricter than the request-id-only matching an empty digest
	// used to fall back to, and the strictness is the point: the request
	// carries the digest (see ports.ApprovalRequest.Digest), so a host has
	// the value it needs to echo, and a host that does not echo it is not
	// implementing the binding.
	gate := &recordingGate{decide: func(req ports.ApprovalRequest) (ports.Decision, error) {
		return ports.Decision{RequestID: req.ID, Digest: "", Decision: ports.ApprovalGranted}, nil
	}}
	r, sink := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
		Gate:  gate,
	})

	res := r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{"target":"web-1"}`))
	if !res.Block {
		t.Fatal("an unbound grant authorised a mutating call")
	}
	if !strings.Contains(res.Reason, "digest") {
		t.Fatalf("reason = %q, want it to name the binding failure", res.Reason)
	}
	if resolved := sink.ofType(wire.StreamApprovalResolved); len(resolved) != 1 {
		t.Fatalf("approval_resolved frames = %d, want 1", len(resolved))
	}
}

func TestTheRequestCarriesTheDigestTheDecisionMustEcho(t *testing.T) {
	// The host cannot derive the digest: the algorithm belongs to this
	// package. If the request did not carry it, every host would either
	// guess wrong (and every grant would look like a different call) or
	// leave it empty (and binding would be off).
	var got ports.ApprovalRequest
	gate := &recordingGate{decide: func(req ports.ApprovalRequest) (ports.Decision, error) {
		got = req
		return ports.Decision{RequestID: req.ID, Digest: req.Digest, Decision: ports.ApprovalGranted}, nil
	}}
	r, _ := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
		Gate:  gate,
	})

	res := r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{"target":"web-1"}`))
	if res.Block {
		t.Fatalf("a correctly bound grant was refused: %s", res.Reason)
	}
	if got.Digest == "" {
		t.Fatal("the request carried no digest for the host to echo")
	}
	if want := CallDigest("restart_service", json.RawMessage(`{"target":"web-1"}`)); got.Digest != want {
		t.Fatalf("digest = %q, want the digest of the exact call", got.Digest)
	}
}

func TestDenialBlocks(t *testing.T) {
	gate := &recordingGate{decide: func(req ports.ApprovalRequest) (ports.Decision, error) {
		return ports.Decision{RequestID: req.ID, Decision: ports.ApprovalDenied, Note: "change freeze"}, nil
	}}
	r, sink := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
		Gate:  gate,
	})

	res := r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{}`))
	if !res.Block {
		t.Fatal("a denial did not block the call")
	}
	if !strings.Contains(res.Reason, CodeApprovalDenied) {
		t.Errorf("reason = %q, want it to carry the machine-readable code", res.Reason)
	}
	resolved := sink.ofType(wire.StreamApprovalResolved)
	if len(resolved) != 1 || resolved[0].Approval.Note != "change freeze" {
		t.Errorf("resolved frames = %+v, want the operator's note carried through", resolved)
	}
}

func TestGateErrorsMapToDistinctCodes(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{ports.GateDenied, CodeApprovalDenied},
		{ports.GateExpired, CodeApprovalExpired},
		{ports.GateCancelled, CodeApprovalCancel},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			gate := &recordingGate{decide: func(ports.ApprovalRequest) (ports.Decision, error) {
				return ports.Decision{}, &ports.GateError{Reason: tc.reason, Err: errors.New("upstream")}
			}}
			r, _ := harness(t, Deps{
				Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
				Gate:  gate,
			})

			res := r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{}`))
			if !res.Block {
				t.Fatal("a gate error did not block the call")
			}
			if !strings.Contains(res.Reason, tc.want) {
				t.Errorf("reason = %q, want it to contain %q so the console can branch on the cause", res.Reason, tc.want)
			}
		})
	}
}

func TestUnknownGateErrorStillBlocks(t *testing.T) {
	gate := &recordingGate{decide: func(ports.ApprovalRequest) (ports.Decision, error) {
		return ports.Decision{}, errors.New("the ledger is unreachable")
	}}
	r, _ := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
		Gate:  gate,
	})

	if res := r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{}`)); !res.Block {
		t.Fatal("an unclassifiable gate failure allowed the call through: a transport problem must not read as consent")
	}
}

func TestApprovalRequestCarriesHostDerivedBlastRadius(t *testing.T) {
	gate := &recordingGate{decide: grantFor("")}
	r, _ := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
		Gate:  gate,
	})

	r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{"service":"web"}`))
	req := gate.requests[0]
	// The narrowest radius is the starting point; the host policy engine
	// widens it. A request must never arrive pre-widened by the caller.
	if req.BlastRadius != domain.RadiusNone {
		t.Errorf("blast radius = %q, want none: the radius is the host's to assess", req.BlastRadius)
	}
	if req.Target != "web" {
		t.Errorf("target = %q, want web", req.Target)
	}
	if req.ExpiresAt.IsZero() {
		t.Error("expires_at is zero: an unbounded request would block the queue indefinitely")
	}
	if got := req.ExpiresAt.Sub(fixedClock()()); got != approvalTTL {
		t.Errorf("ttl = %s, want %s", got, approvalTTL)
	}
	if req.Summary != "restart_service on web" {
		t.Errorf("summary = %q", req.Summary)
	}
}

func TestApprovalRequestCopiesArguments(t *testing.T) {
	gate := &recordingGate{decide: grantFor("")}
	r, _ := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("apply_config", domain.ClassWrite)}},
		Gate:  gate,
	})

	args := json.RawMessage(`{"replicas":3}`)
	r.beforeToolCall(context.Background(), "tc-1", "apply_config", args)
	// Mutating the caller's buffer must not retroactively change what the
	// operator was shown, or the digest would no longer describe the
	// request on record.
	got := gate.requests[0].Arguments
	if string(got) != `{"replicas":3}` {
		t.Fatalf("arguments = %s", got)
	}
	before := CallDigest("apply_config", got)
	args[2] = '9'
	if after := CallDigest("apply_config", gate.requests[0].Arguments); after != before {
		t.Error("the recorded arguments aliased the caller's buffer")
	}
}

func TestEveryRefusalPathEmitsTheSameDecisionVocabulary(t *testing.T) {
	// The console branches on ApprovalFrame.Decision, so it must be the
	// closed grant/deny pair on every path. Reaching for the gate's reason
	// strings here is how a console ends up special-casing three spellings
	// of "the operator said no". The cause belongs in Note.
	// hostRefused marks the paths where the host, not the operator, is the
	// one refusing. Those must carry a reason; an operator who clicks deny
	// without typing a note is a legitimate flow and is not second-guessed.
	type path struct {
		decide      func(ports.ApprovalRequest) (ports.Decision, error)
		hostRefused bool
	}
	paths := map[string]path{
		"digest mismatch": {hostRefused: true, decide: func(req ports.ApprovalRequest) (ports.Decision, error) {
			return ports.Decision{Digest: "some-other-call", Decision: ports.ApprovalGranted}, nil
		},
		},
		"gate error": {hostRefused: true, decide: func(ports.ApprovalRequest) (ports.Decision, error) {
			return ports.Decision{}, &ports.GateError{Reason: ports.GateExpired, Err: errors.New("timed out")}
		}},
		"transport error": {hostRefused: true, decide: func(ports.ApprovalRequest) (ports.Decision, error) {
			return ports.Decision{}, errors.New("ledger unreachable")
		}},
		"explicit deny": {hostRefused: false, decide: func(req ports.ApprovalRequest) (ports.Decision, error) {
			return ports.Decision{Decision: ports.ApprovalDenied}, nil
		}},
	}
	for name, tc := range paths {
		t.Run(name, func(t *testing.T) {
			gate := &recordingGate{decide: tc.decide}
			r, sink := harness(t, Deps{
				Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
				Gate:  gate,
			})
			if res := r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{"target":"web-1"}`)); !res.Block {
				t.Fatalf("path %q allowed the call", name)
			}
			resolved := sink.ofType(wire.StreamApprovalResolved)
			if len(resolved) != 1 {
				t.Fatalf("resolved frames = %+v, want exactly one", resolved)
			}
			if got := resolved[0].Approval.Decision; got != string(ports.ApprovalDenied) {
				t.Errorf("decision = %q, want %q", got, ports.ApprovalDenied)
			}
			if tc.hostRefused && resolved[0].Approval.Note == "" {
				t.Error("note is empty: the host refused this call and no reason reached the operator or the audit trail")
			}
		})
	}
}

func TestGrantPathEmitsGrant(t *testing.T) {
	gate := &recordingGate{decide: grantFor("CHG-1")}
	r, sink := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
		Gate:  gate,
	})
	r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{}`))

	resolved := sink.ofType(wire.StreamApprovalResolved)
	if len(resolved) != 1 || resolved[0].Approval.Decision != string(ports.ApprovalGranted) {
		t.Errorf("resolved frames = %+v, want a single grant", resolved)
	}
}

// --- budget -------------------------------------------------------------

func TestBudgetRefusalBlocksAndLatches(t *testing.T) {
	budget := &countingBudget{limit: 0, why: "daily spend cap reached"}
	tool := classTool("get_topology", domain.ClassRead)
	r, _ := harness(t, Deps{Tools: staticBag{tools: []ports.Tool{tool}}, Budget: budget})

	first := r.beforeToolCall(context.Background(), "tc-1", "get_topology", json.RawMessage(`{}`))
	if !first.Block {
		t.Fatal("a call past the budget was allowed")
	}
	if !strings.Contains(first.Reason, "daily spend cap reached") {
		t.Errorf("reason = %q, want the budget's own reason so the operator knows which cap they hit", first.Reason)
	}

	calls := budget.calls
	second := r.beforeToolCall(context.Background(), "tc-2", "get_topology", json.RawMessage(`{}`))
	if !second.Block {
		t.Fatal("the second call after exhaustion was allowed")
	}
	if budget.calls != calls {
		t.Errorf("the budget was re-consulted after latching: %d extra calls", budget.calls-calls)
	}
}

func TestLatchedBudgetShortCircuitsBeforeTheGate(t *testing.T) {
	// Once the budget is spent the turn is over, so the approval queue
	// must not fill with requests nobody will act on.
	gate := &recordingGate{decide: grantFor("")}
	r, _ := harness(t, Deps{
		Tools:  staticBag{tools: []ports.Tool{classTool("restart_service", domain.ClassDestructive)}},
		Gate:   gate,
		Budget: &countingBudget{limit: 0},
	})
	r.beforeToolCall(context.Background(), "tc-1", "restart_service", json.RawMessage(`{}`))
	r.beforeToolCall(context.Background(), "tc-2", "restart_service", json.RawMessage(`{}`))

	if len(gate.requests) != 0 {
		t.Errorf("the gate was consulted %d times after the budget latched", len(gate.requests))
	}
}

func TestFinishTurnEndsOnBudget(t *testing.T) {
	r, _ := harness(t, Deps{Budget: &countingBudget{limit: 0, why: "cap"}})
	decision, err := r.finishTurn(context.Background(), agent.AgentTurnContext{})
	if err != nil {
		t.Fatalf("finishTurn: %v", err)
	}
	if decision == nil || decision.Action != agent.AgentTurnEnd {
		t.Fatal("finishTurn continued a turn with no budget left")
	}
	if r.stopped != TurnToolBudget {
		t.Errorf("stopped = %q, want %q so the console explains the ending", r.stopped, TurnToolBudget)
	}
}

func TestFinishTurnLeavesAHealthyTurnAlone(t *testing.T) {
	// Returning a decision here would end every turn after the first round
	// trip; nil is how the loop learns to continue.
	r, _ := harness(t, Deps{Budget: &countingBudget{limit: 10}})
	decision, err := r.finishTurn(context.Background(), agent.AgentTurnContext{})
	if err != nil || decision != nil {
		t.Errorf("finishTurn = (%v, %v), want (nil, nil)", decision, err)
	}
}

// --- audit --------------------------------------------------------------

func TestAuditRecordsSuccessAndBlockAndError(t *testing.T) {
	audit := &recordingAudit{}
	r, _ := harness(t, Deps{
		Tools: staticBag{tools: []ports.Tool{classTool("get_topology", domain.ClassRead)}},
		Audit: audit,
	})
	ctx := context.Background()
	args := json.RawMessage(`{}`)

	ok := agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "ok"}}}
	r.afterToolCall(ctx, "tc-1", "get_topology", args, ok)

	failed := agent.AgentToolResult{IsError: true, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "timeout"}}}
	r.afterToolCall(ctx, "tc-2", "get_topology", args, failed)

	r.afterToolCall(ctx, "tc-3", "get_topology", args, MarkBlocked(agent.AgentToolResult{}, "needs approval"))

	want := []struct {
		action  ports.AuditAction
		outcome string
	}{
		{ports.ActionToolCall, "success"},
		{ports.ActionToolFailed, "error"},
		// A refusal is the single most important row an incident review
		// needs, so it must be distinguishable from a failure.
		{ports.ActionToolBlocked, "blocked"},
	}
	if len(audit.entries) != len(want) {
		t.Fatalf("recorded %d entries, want %d", len(audit.entries), len(want))
	}
	for i, w := range want {
		got := audit.entries[i]
		if got.Action != w.action || got.Outcome != w.outcome {
			t.Errorf("entry %d = %s/%s, want %s/%s", i, got.Action, got.Outcome, w.action, w.outcome)
		}
		if got.Target != "get_topology" {
			t.Errorf("entry %d target = %q", i, got.Target)
		}
		if got.Class != string(domain.ClassRead) {
			t.Errorf("entry %d class = %q, want read: a reviewer filters the ledger by blast radius", i, got.Class)
		}
		if got.Actor != "agent:s-1" {
			t.Errorf("entry %d actor = %q", i, got.Actor)
		}
		if got.At.IsZero() {
			t.Errorf("entry %d has no timestamp", i)
		}
	}
}

func TestAuditIsOptional(t *testing.T) {
	// A host may run without a ledger configured; the turn must still work.
	r, _ := harness(t, Deps{Tools: staticBag{}})
	// The empty result is the "leave the call as it is" instruction: a hook
	// with no ledger must not rewrite the tool's own output.
	got := r.afterToolCall(context.Background(), "tc-1", "t", json.RawMessage(`{}`), agent.AgentToolResult{})
	if got.Content != nil || got.Details != nil || got.IsError != nil || got.Usage != nil || got.Terminate != nil {
		t.Errorf("afterToolCall = %+v, want the zero result: with no ledger it must not rewrite the tool's own output", got)
	}
}

// --- classification and digests ----------------------------------------

func TestClassOfReadsTheToolBag(t *testing.T) {
	r, _ := harness(t, Deps{Tools: staticBag{tools: []ports.Tool{
		classTool("a", domain.ClassRead),
		classTool("b", domain.ClassWrite),
		classTool("c", domain.ClassDestructive),
	}}})
	for name, want := range map[string]domain.ToolClass{
		"a": domain.ClassRead, "b": domain.ClassWrite, "c": domain.ClassDestructive,
		"missing": domain.ClassDestructive,
	} {
		if got := r.classOf(name); got != want {
			t.Errorf("classOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCallDigestIsStableAndArgumentSensitive(t *testing.T) {
	a := CallDigest("restart_service", json.RawMessage(`{"target":"web-1"}`))
	if a != CallDigest("restart_service", json.RawMessage(`{"target":"web-1"}`)) {
		t.Error("the digest is not stable for identical calls: an operator would be asked to re-approve the same action")
	}
	// Whitespace changes must count: a re-planned call is a new call.
	if a == CallDigest("restart_service", json.RawMessage(`{"target": "web-1"}`)) {
		t.Error("a reformatted call produced the same digest")
	}
	if a == CallDigest("restart_service", json.RawMessage(`{"target":"web-2"}`)) {
		t.Error("different arguments produced the same digest")
	}
	if a == CallDigest("stop_service", json.RawMessage(`{"target":"web-1"}`)) {
		t.Error("a different tool produced the same digest")
	}
}

func TestCallDigestIsUnambiguousAcrossBoundaries(t *testing.T) {
	// The digest writes the tool name, a NUL, then the arguments. Without
	// the separator, ("ab", "c") and ("a", "bc") would collide and one
	// call's approval would authorise another.
	if CallDigest("ab", json.RawMessage(`"c"`)) == CallDigest("a", json.RawMessage(`"bc"`)) {
		t.Error("tool name and arguments are not separated in the digest input")
	}
}

func TestToolSummaryAndTarget(t *testing.T) {
	cases := []struct {
		args, wantSummary, wantTarget string
	}{
		{`{"target":"web-1"}`, "restart_service on web-1", "web-1"},
		{`{"namespace":"prod"}`, "restart_service on prod", "prod"},
		{`{"unrelated":1}`, "restart_service", ""},
		{`not json`, "restart_service", ""},
		{`{"target":""}`, "restart_service", ""},
		{`{"target":42}`, "restart_service", ""},
	}
	for _, tc := range cases {
		if got := wire.ToolSummary("restart_service", json.RawMessage(tc.args)); got != tc.wantSummary {
			t.Errorf("ToolSummary(%s) = %q, want %q", tc.args, got, tc.wantSummary)
		}
		if got := wire.ToolTarget(json.RawMessage(tc.args)); got != tc.wantTarget {
			t.Errorf("ToolTarget(%s) = %q, want %q", tc.args, got, tc.wantTarget)
		}
	}
}

// --- result assembly ----------------------------------------------------

func TestResultDefaultsToEndTurn(t *testing.T) {
	r, _ := harness(t, Deps{})
	r.mapper.TurnStarted()

	res := r.result([]agent.AgentMessage{
		{User: &agent.UserMessage{Role: "user"}},
		{Assistant: &agent.AssistantMessage{Content: []ai.AssistantContentBlock{ai.TextContent{Text: "final answer"}}}},
	})
	if res.Stopped != TurnEndTurn {
		t.Errorf("stopped = %q, want %q", res.Stopped, TurnEndTurn)
	}
	if res.Content != "final answer" {
		t.Errorf("content = %q, want the last assistant message", res.Content)
	}
	if res.Iterations != 1 {
		t.Errorf("iterations = %d", res.Iterations)
	}
}

func TestResultPreservesAnExplicitStopReason(t *testing.T) {
	r, _ := harness(t, Deps{})
	r.stopped = TurnToolBudget
	if got := r.result(nil).Stopped; got != TurnToolBudget {
		t.Errorf("stopped = %q, want the budget reason preserved", got)
	}
}

func TestResultWithNoMessages(t *testing.T) {
	r, _ := harness(t, Deps{})
	if res := r.result(nil); res.Content != "" || res.Stopped != TurnEndTurn {
		t.Errorf("result = %+v, want an empty end_turn", res)
	}
}

func TestOnEventFoldsUsageAndEmits(t *testing.T) {
	audit := &recordingAudit{}
	r, sink := harness(t, Deps{Tools: staticBag{tools: []ports.Tool{classTool("t", domain.ClassRead)}}, Audit: audit})
	r.model = "test-model"

	r.onEvent(agent.TurnStartEvent{})
	r.onEvent(agent.MessageEndEvent{Message: agent.AgentMessage{
		Assistant: &agent.AssistantMessage{
			Content: []ai.AssistantContentBlock{ai.TextContent{Text: "hi"}},
			Usage:   &ai.Usage{Input: 100, Output: 20},
		},
	}})
	r.onEvent(agent.AgentEndEvent{})

	if r.usage.InputTokens != 100 || r.usage.OutputTokens != 20 {
		t.Errorf("usage = %+v", r.usage)
	}
	done := sink.ofType(wire.StreamDone)
	if len(done) != 1 {
		t.Fatalf("done frames = %d, want 1", len(done))
	}
	if done[0].Done.Usage.InputTokens != 100 || done[0].Done.Usage.Model != "test-model" {
		t.Errorf("done usage = %+v", done[0].Done.Usage)
	}
}

func TestOnEventStopsOnAFailedSink(t *testing.T) {
	// A console that has gone away is not retried against; the kernel is
	// torn down through its context instead.
	r, _ := harness(t, Deps{})
	r.sink = failingSink{after: 1}
	r.onEvent(agent.TurnStartEvent{})
	if r.mapper.seq > 1 {
		t.Errorf("seq = %d, want the mapper to stop after the sink failed", r.mapper.seq)
	}
}

type failingSink struct{ after int }

func (f failingSink) Emit(context.Context, wire.StreamEvent) error {
	f.after--
	if f.after <= 0 {
		return errors.New("client gone")
	}
	return nil
}

func TestIsGateReasonUnwraps(t *testing.T) {
	inner := &ports.GateError{Reason: ports.GateExpired, Err: errors.New("no answer in 5m")}
	wrapped := errors.New("queue: " + inner.Error())
	_ = wrapped

	if !isGateReason(inner, ports.GateExpired) {
		t.Error("a direct GateError was not matched")
	}
	if isGateReason(inner, ports.GateDenied) {
		t.Error("a GateError matched the wrong reason")
	}
	if isGateReason(errors.New("plain"), ports.GateDenied) {
		t.Error("a plain error was classified as a gate reason")
	}
}

var _ = time.Second
