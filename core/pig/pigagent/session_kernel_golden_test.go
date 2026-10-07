package pigagent

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigcoding"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// This file is the acceptance gate for the second agent driver.
//
// Kernel drives agent.Agent; SessionKernel drives coding.Session. They share
// the mapper, the policy gate, the prompt assembly and the tool adapters —
// everything that decides what the console sees — and differ only in the
// loop underneath. That is a claim, and like every claim of that shape it
// is worth exactly as much as the test that would fail if it stopped being
// true. A new field dropped on one path, a hook that only one driver
// installs, an event kind the Session forwards and the bare loop does not:
// each of those is invisible in a unit test of either driver alone, and
// each of them is a console regression.
//
// So the gate is a differential one. Both drivers run the same incident —
// read the topology, ask for a frozen change, get refused, answer — against
// their own faux provider, and the two frame streams and the two
// transcripts are compared byte for byte. The script is
// goldenTurnScript from streamgolden_test.go rather than a new one, because
// a second script would be a second thing to keep honest and this file is
// about the driver, not about the scenario.
//
// # What this gate cannot catch, proven rather than assumed
//
// A differential comparison only sees disagreement. A regression both drivers
// share is invisible to it by construction, and that was not a theoretical
// worry: deleting the tool_start frame from the mapper — one line, in code
// both drivers share — leaves this file green and turns
// TestStreamGoldenMatchesTheConsoleContract red. The console loses every
// tool call it renders and the differential gate reports a perfect match,
// because the two drivers are indeed identical.
//
// So this file is not a replacement for the absolute golden in
// streamgolden_test.go; it is the half that golden cannot be. The absolute
// one pins what the frames are, and cannot tell you that a second driver
// produces the same ones. Neither is sufficient alone, and the pair is the
// gate.

// recordingPersister captures every transcript row a kernel writes, in the
// order it wrote them.
//
// Order is the whole point. A transcript is replayed into the next turn, so
// a driver that persisted the same rows in a different order would produce
// a conversation the provider rejects, and a set comparison would call the
// two equal.
type recordingPersister struct {
	rows []ports.AgentMessage
}

func (p *recordingPersister) Persist(_ context.Context, _ string, msg ports.AgentMessage) error {
	p.rows = append(p.rows, msg)
	return nil
}

// rows are rendered rather than compared with reflect.DeepEqual so a
// failure prints the two transcripts as something a person can read.
func (p *recordingPersister) render() []string {
	out := make([]string, 0, len(p.rows))
	for i, r := range p.rows {
		line := r.Role + " " + r.Content
		if r.ToolName != "" {
			line += " tool=" + r.ToolName
		}
		if r.ToolCallID != "" {
			line += " id=" + r.ToolCallID
		}
		for _, c := range r.ToolCalls {
			line += " calls=" + c.Name + string(c.Arguments)
		}
		out = append(out, itoa(i)+": "+line)
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// goldenTurnDeps is the host policy both drivers run against.
//
// It is shared deliberately. A difference in the frames has to be
// attributable to the loop, and two separately written Deps values would
// make every difference ambiguous.
func goldenTurnDeps(gate *recordingGate) DepsProvider {
	readonly := classTool("get_topology", domain.ClassRead)
	mutating := &fakeTool{schema: ports.ToolSchema{
		Name:       "restart_service",
		Class:      domain.ClassDestructive,
		Parameters: json.RawMessage(`{"type":"object","properties":{"service":{"type":"string"}}}`),
	}, out: "restarted"}
	return func(context.Context, ports.AgentRequest) (Deps, error) {
		return Deps{Tools: staticBag{tools: []ports.Tool{readonly, mutating}}, Gate: gate}, nil
	}
}

func goldenGate() *recordingGate {
	return &recordingGate{decide: func(req ports.ApprovalRequest) (ports.Decision, error) {
		if req.ToolName == "restart_service" {
			return ports.Decision{
				RequestID: req.ID,
				Digest:    CallDigest(req.ToolName, req.Arguments),
				Decision:  ports.ApprovalDenied,
				DecidedBy: "op-1",
				Note:      "change freeze",
			}, nil
		}
		return grantFor("")(req)
	}}
}

// runKernelGolden drives the bare loop over the golden script.
func runKernelGolden(t *testing.T) ([]wire.StreamEvent, []string) {
	t.Helper()
	persister := &recordingPersister{}
	sink := &collectSink{}
	k, err := NewKernel(KernelOptions{
		Models:  &fauxResolver{model: newGoldenFauxModel(t, goldenTurnScript()...)},
		Deps:    goldenTurnDeps(goldenGate()),
		Persist: persister,
		Now:     fixedClock(),
	})
	if err != nil {
		t.Fatalf("NewKernel: %v", err)
	}
	if _, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{
		SessionID: "s-1", UserText: "map prod and restart web", Role: "admin",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return coalesceDeltas(sink.Frames()), persister.render()
}

// runSessionGolden drives the embedded SDK over the same script, through a
// real coding.Runtime with real in-memory settings and a real session log.
//
// The runtime is not mocked out. The whole claim under test is that a
// Session-driven turn behaves like a loop-driven one, and a fake Runtime
// would be asserting that about the fake.
func runSessionGolden(t *testing.T) ([]wire.StreamEvent, []string) {
	t.Helper()
	rt, err := pigcoding.NewRuntime(pigcoding.RuntimeOptions{
		AgentDir: t.TempDir(),
		CWD:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	persister := &recordingPersister{}
	sink := &collectSink{}
	k, err := NewSessionKernel(SessionKernelOptions{
		Runtime: rt,
		Models:  &fauxResolver{model: newGoldenFauxModel(t, goldenTurnScript()...)},
		Deps:    goldenTurnDeps(goldenGate()),
		Persist: persister,
		Now:     fixedClock(),
	})
	if err != nil {
		t.Fatalf("NewSessionKernel: %v", err)
	}
	if _, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{
		SessionID: "s-1", UserText: "map prod and restart web", Role: "admin",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return coalesceDeltas(sink.Frames()), persister.render()
}

// isApprovalFrame reports whether a frame is one the policy gate emitted
// directly rather than one the agent's event stream produced.
//
// The distinction is not cosmetic and it is the whole reason this file
// compares frames in three passes instead of one. See
// TestTheApprovalCardsLandInTheSamePlaceInBothDrivers for what it costs.
func isApprovalFrame(f wire.StreamEvent) bool {
	return f.Type == wire.StreamApprovalPending || f.Type == wire.StreamApprovalResolved
}

// withoutApprovals drops the directly-emitted frames, leaving the ordered
// stream of everything the agent itself produced.
func withoutApprovals(frames []wire.StreamEvent) []wire.StreamEvent {
	out := make([]wire.StreamEvent, 0, len(frames))
	for _, f := range frames {
		if !isApprovalFrame(f) {
			out = append(out, f)
		}
	}
	return out
}

// toolScopedFrames renders, for each tool call, the frames that belong to
// that call — sorted, not in arrival order.
//
// Sorting is not a weakening here, it is the point. The approval cards and
// the tool's own frames travel two different paths (see
// TestTheApprovalCardsAreBoundToTheirCallInBothDrivers), so their relative
// order is a scheduling outcome. What must hold is that a call carries the
// same frames in both drivers: an opening, a settle, and the same cards.
// An earlier version of this compared arrival order and failed on roughly
// one run in six.
func toolScopedFrames(frames []wire.StreamEvent) []string {
	byCall := map[string][]string{}
	var order []string
	for _, f := range frames {
		id := ""
		switch f.Type {
		case wire.StreamToolStart, wire.StreamToolEnd:
			id = f.Tool.ToolCallID
		case wire.StreamApprovalPending, wire.StreamApprovalResolved:
			id = f.Approval.RequestID
		default:
			continue
		}
		if _, seen := byCall[id]; !seen {
			order = append(order, id)
		}
		byCall[id] = append(byCall[id], string(f.Type))
	}
	out := make([]string, 0, len(order))
	for _, id := range order {
		group := byCall[id]
		sort.Strings(group)
		out = append(out, id+" "+strings.Join(group, " "))
	}
	return out
}

// orderedFrameLines renders the frames in arrival order with the sequence
// number stripped.
//
// The number is dropped rather than compared because it is a property of
// the whole stream, not of any one frame. The counter is shared, so a frame
// that arrives between two others is what decides what numbers those two
// get — and the approval cards arrive at a position neither driver
// guarantees. Two streams can therefore carry the same frames in the same
// order and differ in every number past the first card, which is exactly
// what the first version of this comparison did and why it failed
// intermittently.
//
// What still has to hold is that the numbers are internally sane, and that
// is asserted separately by TestBothDriversProduceAStrictlyIncreasingSequence
// and by TestFramesFromTheRunStateAndTheMapperDoNotCollide.
func orderedFrameLines(frames []wire.StreamEvent) string {
	raw := strings.Split(strings.TrimRight(string(renderStreamFrames(frames)), "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if fields := strings.SplitN(line, " ", 2); len(fields) == 2 {
			out = append(out, fields[1])
		} else {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// sortedFrameLines renders every frame and sorts the lines.
//
// A sorted comparison is how the gate asserts that neither driver invents
// or loses a frame. It is deliberately the weaker of the three passes: it
// cannot see order at all, which is exactly why the two ordered passes
// exist alongside it rather than being replaced by it.
func sortedFrameLines(frames []wire.StreamEvent) []string {
	raw := strings.Split(strings.TrimRight(string(renderStreamFrames(frames)), "\n"), "\n")
	stripped := make([]string, 0, len(raw))
	for _, line := range raw {
		fields := strings.SplitN(line, " ", 2)
		if len(fields) == 2 {
			stripped = append(stripped, fields[1])
		} else {
			stripped = append(stripped, line)
		}
	}
	sort.Strings(stripped)
	return stripped
}

// TestBothDriversProduceTheSameConsoleFrames is the primary gate, in three
// passes, because "the same frames" has three separate meanings and a
// single comparison can only check one of them.
func TestBothDriversProduceTheSameConsoleFrames(t *testing.T) {
	loopFrames, _ := runKernelGolden(t)
	sessionFrames, _ := runSessionGolden(t)

	// Pass one: the ordered stream of agent-produced frames. Bubbles,
	// deltas, tool starts, tool results and the terminal frame. This is
	// the console's skeleton and it must be character-identical.
	loopCore := orderedFrameLines(withoutApprovals(loopFrames))
	sessionCore := orderedFrameLines(withoutApprovals(sessionFrames))
	if loopCore != sessionCore {
		t.Fatalf("the two drivers disagree on the agent-produced frame stream\n--- loop driver ---\n%s\n--- sdk driver ---\n%s", loopCore, sessionCore)
	}

	// Pass two: the same set of frames, order discarded. This is what
	// catches an approval card that one driver emits and the other does
	// not, which pass one cannot see because it filters them out.
	loopAll := sortedFrameLines(loopFrames)
	sessionAll := sortedFrameLines(sessionFrames)
	if strings.Join(loopAll, "\n") != strings.Join(sessionAll, "\n") {
		t.Fatalf("the two drivers produced different frames\n--- only the loop driver ---\n%s\n--- only the sdk driver ---\n%s",
			onlyIn(loopAll, sessionAll), onlyIn(sessionAll, loopAll))
	}

	// Pass three: per tool call, the frames that call carries.
	loopTools := strings.Join(toolScopedFrames(loopFrames), "\n")
	sessionTools := strings.Join(toolScopedFrames(sessionFrames), "\n")
	if loopTools != sessionTools {
		t.Fatalf("the two drivers disagree on what a tool call carried\n--- loop driver ---\n%s\n--- sdk driver ---\n%s", loopTools, sessionTools)
	}
}

func onlyIn(have, want []string) string {
	set := make(map[string]int, len(want))
	for _, w := range want {
		set[w]++
	}
	var b strings.Builder
	for _, h := range have {
		if set[h] > 0 {
			set[h]--
			continue
		}
		b.WriteString("  " + h + "\n")
	}
	if b.Len() == 0 {
		return "  (none)\n"
	}
	return b.String()
}

func TestBothDriversWriteTheSameTranscript(t *testing.T) {
	_, loopRows := runKernelGolden(t)
	_, sessionRows := runSessionGolden(t)

	if len(loopRows) != len(sessionRows) {
		t.Fatalf("transcript length differs: loop driver wrote %d rows (%v), sdk driver wrote %d (%v)",
			len(loopRows), loopRows, len(sessionRows), sessionRows)
	}
	for i := range loopRows {
		if loopRows[i] != sessionRows[i] {
			t.Fatalf("transcript row %d differs\n  loop driver: %s\n  sdk driver: %s", i, loopRows[i], sessionRows[i])
		}
	}
}

// TestTheSDKDriverStillEnforcesThePolicyGate is the security half of the
// gate, and it is deliberately not a golden.
//
// Frame equality says the two drivers rendered the same console output. It
// says nothing about whether either of them refused the write. A driver
// that dropped BeforeToolCall would produce a *shorter* frame stream — no
// approval card, no blocked tool — and a comparison against a golden
// recorded from the buggy driver would happily pass, because the golden
// would have been re-recorded with the bug in it.
//
// So the gate is asserted directly: the approval request is observed, the
// decision comes back denied, the tool never runs, and the refusal is
// visible to the turn rather than only to the ledger.
func TestTheSDKDriverStillEnforcesThePolicyGate(t *testing.T) {
	gate := goldenGate()
	rt, err := pigcoding.NewRuntime(pigcoding.RuntimeOptions{AgentDir: t.TempDir(), CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	ran := &fakeTool{schema: ports.ToolSchema{
		Name:       "restart_service",
		Class:      domain.ClassDestructive,
		Parameters: json.RawMessage(`{"type":"object"}`),
	}, out: "restarted"}

	k, err := NewSessionKernel(SessionKernelOptions{
		Runtime: rt,
		Models:  &fauxResolver{model: newGoldenFauxModel(t, goldenTurnScript()...)},
		Deps: func(context.Context, ports.AgentRequest) (Deps, error) {
			return Deps{Tools: staticBag{tools: []ports.Tool{ran}}, Gate: gate}, nil
		},
		Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("NewSessionKernel: %v", err)
	}
	sink := &collectSink{}
	if _, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{
		SessionID: "s-1", UserText: "map prod and restart web", Role: "admin",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(gate.requests) != 1 {
		t.Fatalf("expected the gate to be consulted once, saw %d requests", len(gate.requests))
	}
	if gate.requests[0].ToolName != "restart_service" {
		t.Fatalf("gate saw tool %q, want restart_service", gate.requests[0].ToolName)
	}
	if ran.Calls() != 0 {
		t.Fatalf("a denied tool executed %d times; the gate must run before the tool, not after", ran.Calls())
	}
	blocked := false
	for _, f := range coalesceDeltas(sink.Frames()) {
		if f.Type == wire.StreamToolEnd && f.Tool.Status == wire.ToolBlocked {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("the console never saw a blocked tool frame; frames were:\n%s",
			renderStreamFrames(coalesceDeltas(sink.Frames())))
	}
}

// TestTheSDKDriverRoundCapIsReportedAsACap pins the one place the two
// drivers are allowed to differ, and pins the difference rather than
// leaving it to a reader of the diff.
//
// The bare loop takes MaxTurns as an agent option, and PiG enforces it by
// returning ErrMaxTurnsReached — an error, which the console renders as a
// failed turn. The Session has no such field, so the cap is a
// BeforeToolCall budget: the model is refused its next tool, told why, and
// gets to answer from what it already has.
//
// Both are the same operator-facing fact — this investigation did not
// converge — reached by different means, and the SDK one is the better of
// the two: an answer with an explanation beats a red box. What must not
// drift is the stop reason the host reads, because
// chatruntime.kernelpath branches on exactly that to decide whether to
// apologise, and a turn that ran long reported as end_turn is an operator
// reading a truncated investigation as a complete one.
func TestTheSDKDriverRoundCapIsReportedAsACap(t *testing.T) {
	// Three tool calls against a cap of one: the model is refused twice and
	// then answers, which is the shape the budget is designed for.
	loopy := func() []ai.FauxResponseStep {
		return []ai.FauxResponseStep{
			goldenToolStep("tc-1", "get_topology", map[string]any{"root": "prod"}),
			goldenToolStep("tc-2", "get_topology", map[string]any{"root": "prod"}),
			goldenToolStep("tc-3", "get_topology", map[string]any{"root": "prod"}),
			textStep("I ran out of rounds; here is what the topology shows so far."),
		}
	}

	rt, err := pigcoding.NewRuntime(pigcoding.RuntimeOptions{AgentDir: t.TempDir(), CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	k, err := NewSessionKernel(SessionKernelOptions{
		Runtime: rt,
		Models:  &fauxResolver{model: newGoldenFauxModel(t, loopy()...)},
		Deps: func(context.Context, ports.AgentRequest) (Deps, error) {
			return Deps{Tools: staticBag{tools: []ports.Tool{
				classTool("get_topology", domain.ClassRead),
			}}}, nil
		},
		MaxIterations:  1,
		SessionTimeout: time.Minute,
		Now:            fixedClock(),
	})
	if err != nil {
		t.Fatalf("NewSessionKernel: %v", err)
	}
	sink := &collectSink{}
	res, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{
		SessionID: "s-1", UserText: "map prod", Role: "admin",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stopped != TurnMaxIterations {
		t.Fatalf("stop reason is %q, want %q; the host turns this into the console's apology, and a bare end_turn here reads as a complete answer",
			res.Stopped, TurnMaxIterations)
	}
}

// TestTheApprovalCardsAreBoundToTheirCallInBothDrivers pins what the
// approval cards actually guarantee, and records what they do not.
//
// # What is not a contract, and why this test exists
//
// Where an approval card lands relative to the agent-produced frames around
// it is undefined. beforeToolCall runs on the tool's own goroutine and
// writes its frames straight to the sink, because the human decision it is
// about to ask for is synchronous and cannot be queued. The frames the
// agent produces travel a different path — inline through OnEvent on the
// bare loop, and through a session-owned forwarding goroutine on the SDK
// driver. Two paths, no ordering between them.
//
// That was not obvious. An earlier version of this file asserted a fixed
// order for the gated round and passed, repeatedly, in isolation — and the
// same assertion failed on every run once the package's other tests were
// compiled back in. The order is a scheduling outcome, and pinning it would
// have produced a test that was green on the author's machine and red on
// everyone else's, which is the failure mode a golden exists to prevent.
//
// So the cards are asserted on identity and on their own internal order,
// both of which are real, and the interleaving is left alone. Pass one of
// TestBothDriversProduceTheSameConsoleFrames already covers the part that
// is ordered: the agent-produced stream, with the cards removed.
func TestTheApprovalCardsAreBoundToTheirCallInBothDrivers(t *testing.T) {
	loopFrames, _ := runKernelGolden(t)
	sessionFrames, _ := runSessionGolden(t)

	for _, tc := range []struct {
		name   string
		frames []wire.StreamEvent
	}{
		{"loop driver", loopFrames},
		{"sdk driver", sessionFrames},
	} {
		var cards []string
		opened, closed := false, false
		for _, f := range tc.frames {
			switch f.Type {
			case wire.StreamApprovalPending:
				cards = append(cards, f.Approval.RequestID+":pending")
				if f.Approval.Tool != "restart_service" {
					t.Fatalf("%s: approval card is for tool %q, want restart_service", tc.name, f.Approval.Tool)
				}
			case wire.StreamApprovalResolved:
				cards = append(cards, f.Approval.RequestID+":resolved")
			case wire.StreamToolStart:
				if f.Tool.ToolCallID == "tc-web" {
					opened = true
				}
			case wire.StreamToolEnd:
				if f.Tool.ToolCallID == "tc-web" {
					closed = true
				}
			}
		}
		// The card carries the tool call id as its request id, which is what
		// lets the console bind it without relying on position.
		if want := []string{"tc-web:pending", "tc-web:resolved"}; strings.Join(cards, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: approval cards are %v, want %v", tc.name, cards, want)
		}
		if !opened || !closed {
			t.Fatalf("%s: the gated call did not open and settle (opened=%v closed=%v)", tc.name, opened, closed)
		}
	}
}

// TestTheSDKDriverWritesTheAuditLedger is the one gate in this file that is
// not about the console, and it is here because the hook it covers has no
// other test anywhere in the repository.
//
// SessionStartOptions has no AfterToolCall field. The settled-call hook has
// to be appended to the session's loop after construction, through a panel
// this package added for the purpose, and a panel that is never called
// fails silently: the turn looks perfect, the frames match, the transcript
// matches, and the ledger simply has no row saying a tool ran on a
// production node. Nothing downstream notices, because the thing that broke
// is the absence of a record.
//
// The refused call is asserted here too, and for the opposite reason:
// PiG settles a call the host blocked as an immediate outcome that never
// reaches AfterToolCall, so its ledger row is written from the event stream
// instead. The two rows come from different places and the difference is
// load-bearing — a gate that only audited the happy path would leave a
// denied destructive call unrecorded, which is the one event an incident
// review most needs.
func TestTheSDKDriverWritesTheAuditLedger(t *testing.T) {
	rt, err := pigcoding.NewRuntime(pigcoding.RuntimeOptions{AgentDir: t.TempDir(), CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	ledger := &recordingAudit{}
	k, err := NewSessionKernel(SessionKernelOptions{
		Runtime: rt,
		Models:  &fauxResolver{model: newGoldenFauxModel(t, goldenTurnScript()...)},
		Deps: func(context.Context, ports.AgentRequest) (Deps, error) {
			deps, derr := goldenTurnDeps(goldenGate())(context.Background(), ports.AgentRequest{})
			if derr != nil {
				return Deps{}, derr
			}
			deps.Audit = ledger
			return deps, nil
		},
		Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("NewSessionKernel: %v", err)
	}
	sink := &collectSink{}
	if _, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{
		SessionID: "s-1", UserText: "map prod and restart web", Role: "admin",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := map[ports.AuditAction]int{}
	for _, e := range ledger.entries {
		got[e.Action]++
	}
	// The read was granted and executed: written by the AfterToolCall hook.
	if got[ports.ActionToolCall] != 1 {
		t.Fatalf("ledger has %d tool_call rows, want 1 (the granted read); entries were %+v",
			got[ports.ActionToolCall], ledger.entries)
	}
	// The write was refused: written from the event stream, because the
	// hook never sees a call that never ran.
	if got[ports.ActionToolBlocked] != 1 {
		t.Fatalf("ledger has %d tool_blocked rows, want 1 (the denied restart); entries were %+v",
			got[ports.ActionToolBlocked], ledger.entries)
	}
}

// TestAnMCPToolIsClassifiedAndGatedLikeAnyOther is the SDK-driver half of
// the answer to "does the control plane's MCP still work now that the turn
// runs on a Session?".
//
// The question has to be split in two, because no single test in this
// repository can span it. The manager owns the MCP servers and builds
// basetool tools from them; core/pig cannot import the manager, and the root
// module cannot import PiG, so the two halves live on opposite sides of a
// boundary neither can cross.
//
// This is the half that can only be true of the SDK driver. An MCP tool
// arrives in the turn as a ports.Tool in the bag, and from there it is an
// ordinary tool — which means its blast-radius class decides whether it runs
// or stops for approval, and the class is inferred from the tool's own name
// because MCP servers rarely set a readOnly hint. Get that wrong in the
// direction of permissive and an agent runs an unapproved mutating MCP call;
// get it wrong in the direction of strict and every dashboard query raises an
// approval card, which trains operators to click through the only card the
// product raises.
//
// Both directions are asserted, and the read case asserts the *absence* of a
// card rather than the presence of a result, because a test that only checks
// the answer cannot tell "ran freely" from "was approved by a gate that
// happens to grant everything".
func TestAnMCPToolIsClassifiedAndGatedLikeAnyOther(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tool      string
		class     domain.ToolClass
		wantCalls int
		wantCard  bool
	}{
		{
			name:  "a read query runs without an approval card",
			tool:  ports.ComposeMCPToolName("grafana", "query_dashboard"),
			class: domain.ClassRead,
			// The whole point: no card. An operator who has to approve
			// "list dashboards" stops reading the cards.
			wantCalls: 1,
			wantCard:  false,
		},
		{
			name:  "a mutating call is refused when nothing can approve it",
			tool:  ports.ComposeMCPToolName("k8s", "delete_pod"),
			class: domain.ClassDestructive,
			// No gate at all: the kernel must refuse rather than run, which
			// is the fail-closed half of "classified like any other tool".
			wantCalls: 0,
			wantCard:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := pigcoding.NewRuntime(pigcoding.RuntimeOptions{AgentDir: t.TempDir(), CWD: t.TempDir()})
			if err != nil {
				t.Fatalf("NewRuntime: %v", err)
			}
			t.Cleanup(func() { _ = rt.Close() })

			tool := &fakeTool{schema: ports.ToolSchema{
				Name:       tc.tool,
				Class:      tc.class,
				Parameters: json.RawMessage(`{"type":"object"}`),
			}, out: `{"panels":3}`}

			// No Gate: a mutating call must be refused rather than run.
			k, err := NewSessionKernel(SessionKernelOptions{
				Runtime: rt,
				Models: &fauxResolver{model: newGoldenFauxModel(t,
					goldenToolStep("tc-mcp", tc.tool, map[string]any{"uid": "abc"}),
					textStep("done"),
				)},
				Deps: func(context.Context, ports.AgentRequest) (Deps, error) {
					return Deps{Tools: staticBag{tools: []ports.Tool{tool}}}, nil
				},
				Now: fixedClock(),
			})
			if err != nil {
				t.Fatalf("NewSessionKernel: %v", err)
			}
			sink := &collectSink{}
			if _, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{
				SessionID: "s-1", UserText: "check the dashboard", Role: "admin",
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if tool.Calls() != tc.wantCalls {
				t.Fatalf("%s ran %d times, want %d", tc.tool, tool.Calls(), tc.wantCalls)
			}
			sawCard := false
			for _, f := range sink.Frames() {
				if f.Type == wire.StreamApprovalPending {
					sawCard = true
				}
			}
			if sawCard != tc.wantCard {
				t.Fatalf("approval card raised = %v, want %v", sawCard, tc.wantCard)
			}
		})
	}
}

// TestBothDriversNeverIssueTheSameSequenceNumber takes over the part of the
// golden that pass one gave up.
//
// The frame sequence is what the console orders by, so "the frames are the
// same" is only half a claim. The other half is that a stream never hands
// the same number to two frames — the failure mode a shared counter across
// the loop goroutine and the tool goroutines actually produces, and the one
// that renders a card out of place with nothing to indicate why.
//
// The differential comparison cannot make this claim, because the number a
// frame gets depends on how many cards were inserted before it and that
// differs between the drivers. So it is asserted per stream, where it is
// actually a property, and the check is deliberately about uniqueness and
// about the terminal frame carrying the highest number rather than about
// contiguity: these are coalesced streams, and coalescing merges a run of
// text deltas into one frame, so the numbers between are legitimately
// absent. Contiguity of the counter itself is what
// TestFramesFromTheRunStateAndTheMapperDoNotCollide holds.
func TestBothDriversNeverIssueTheSameSequenceNumber(t *testing.T) {
	loopFrames, _ := runKernelGolden(t)
	sessionFrames, _ := runSessionGolden(t)

	for _, tc := range []struct {
		name   string
		frames []wire.StreamEvent
	}{
		{"loop driver", loopFrames},
		{"sdk driver", sessionFrames},
	} {
		if len(tc.frames) == 0 {
			t.Fatalf("%s produced no frames at all", tc.name)
		}
		seen := make(map[int64]wire.StreamEventType, len(tc.frames))
		var highest int64
		for _, f := range tc.frames {
			if prev, dup := seen[f.Seq]; dup {
				t.Fatalf("%s: seq %d was issued twice (%s and %s); the console orders by it, so one of "+
					"them renders in the wrong place", tc.name, f.Seq, prev, f.Type)
			}
			seen[f.Seq] = f.Type
			if f.Seq > highest {
				highest = f.Seq
			}
		}
		last := tc.frames[len(tc.frames)-1]
		if last.Type != wire.StreamDone {
			t.Fatalf("%s: the last frame emitted is %s, want the terminal done frame", tc.name, last.Type)
		}
		if last.Seq != highest {
			t.Fatalf("%s: the terminal frame carries seq %d but the highest issued was %d; a frame "+
				"emitted after the turn closed would never be shown", tc.name, last.Seq, highest)
		}
	}
}
