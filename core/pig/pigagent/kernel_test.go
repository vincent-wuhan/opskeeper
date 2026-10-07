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

// fauxHarness drives a real PiG agent loop without a network. Everything
// below goes through the same Kernel.Run the production paths use, so these
// are integration tests, not unit tests of a stand-in.
//
// PiG's faux provider is the only piece that is simulated: the loop, the
// hooks, and the event stream are the real implementations. That matters
// because the security tests below are only meaningful if the hook ordering
// they depend on is PiG's actual ordering.

// The provider type is unexported upstream, so only the model is returned;
// the provider stays owned by the test that made it.
func newFauxModel(t *testing.T, steps ...ai.FauxResponseStep) *ai.Model {
	t.Helper()
	provider := ai.NewFauxProvider(ai.FauxConfig{
		Model:           "faux-1",
		Models:          []ai.FauxModelDefinition{{ID: "faux-1", Name: "Faux"}},
		TokensPerSecond: 1_000_000, // no artificial delay between deltas
	})
	provider.SetResponses(steps)
	model := provider.GetModel("faux-1")
	if model == nil {
		t.Fatal("faux provider did not register faux-1")
	}
	return model
}

// fauxResolver hands the kernel a fixed model, standing in for the
// settings-backed registry.
type fauxResolver struct {
	model *ai.Model
	err   error
	// seen records the selection each turn asked for, so a test can assert
	// the kernel passed the request's selection through unchanged.
	seen []domain.ModelSelection
}

func (r *fauxResolver) Model(_ context.Context, sel domain.ModelSelection) (*ai.Model, ai.StreamOptions, error) {
	r.seen = append(r.seen, sel)
	if r.err != nil {
		return nil, ai.StreamOptions{}, r.err
	}
	return r.model, ai.StreamOptions{APIKey: "test-key"}, nil
}

// textStep is a model reply that is only text.
func textStep(text string) ai.FauxResponseStep {
	return ai.FauxStaticStep(ai.FauxResponse{
		Content:    []ai.FauxContentBlock{ai.FauxText(text)},
		StopReason: string(ai.StopReasonStop),
	})
}

// toolStep is a model reply that asks for one tool call.
func toolStep(name string, args map[string]any) ai.FauxResponseStep {
	return ai.FauxStaticStep(ai.FauxResponse{
		Content:    []ai.FauxContentBlock{ai.FauxToolCall(name, args, "tc-1")},
		StopReason: string(ai.StopReasonToolUse),
	})
}

func frameTypes(frames []wire.StreamEvent) []wire.StreamEventType {
	out := make([]wire.StreamEventType, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Type)
	}
	return out
}

func hasType(seq []wire.StreamEventType, want wire.StreamEventType) bool {
	for _, t := range seq {
		if t == want {
			return true
		}
	}
	return false
}

// --- a plain read-only turn --------------------------------------------

func TestKernelRunsATextOnlyTurn(t *testing.T) {
	model := newFauxModel(t, textStep("The database is healthy."))
	sink := &collectSink{}
	k, err := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps: func(context.Context, ports.AgentRequest) (Deps, error) {
			return Deps{Tools: staticBag{}}, nil
		},
		Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("NewKernel: %v", err)
	}

	ctx := ports.WithSink(context.Background(), sink)
	res, err := k.Run(ctx, ports.AgentRequest{SessionID: "s-1", UserText: "is the db ok?", Role: "admin"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stopped != TurnEndTurn {
		t.Errorf("stopped = %q, want %q", res.Stopped, TurnEndTurn)
	}
	if !strings.Contains(res.Content, "The database is healthy.") {
		t.Errorf("content = %q, want the model's answer", res.Content)
	}

	types := frameTypes(sink.Frames())
	// The console opens a bubble, receives the text, and closes the turn.
	if !hasType(types, wire.StreamAssistantStart) {
		t.Error("no assistant_start frame: the console would render nothing")
	}
	if !hasType(types, wire.StreamAssistantDelta) {
		t.Error("no assistant_delta frame: the console would show the answer only at the end, not streaming")
	}
	if !hasType(types, wire.StreamAssistantEnd) {
		t.Error("no assistant_end frame")
	}
	if !hasType(types, wire.StreamDone) {
		t.Error("no done frame: the console would keep waiting for a turn that already finished")
	}
	if hasType(types, wire.StreamError) {
		t.Error("an error frame was emitted on a successful turn")
	}

	// Every frame belongs to this session and carries a sequence number.
	for i, f := range sink.Frames() {
		if f.SessionID != "s-1" {
			t.Errorf("frame %d session = %q", i, f.SessionID)
		}
		if f.Seq != int64(i+1) {
			t.Errorf("frame %d seq = %d, want %d: a gap means the console believes a frame was lost", i, f.Seq, i+1)
		}
	}
}

func TestKernelRecordsNoToolCallsForATextTurn(t *testing.T) {
	// The audit must reflect what happened, including nothing: a turn that
	// called no tool should write no tool row.
	audit := &recordingAudit{}
	model := newFauxModel(t, textStep("all clear"))
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps: func(context.Context, ports.AgentRequest) (Deps, error) {
			return Deps{Tools: staticBag{}, Audit: audit}, nil
		},
		Now: fixedClock(),
	})
	if _, err := k.Run(context.Background(), ports.AgentRequest{SessionID: "s-1", UserText: "hi"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, e := range audit.entries {
		if strings.HasPrefix(string(e.Action), "tool_") {
			t.Errorf("audit recorded %s for a turn that called no tool", e.Action)
		}
	}
}

// --- a read-only tool round trip ----------------------------------------

func TestKernelRunsAReadOnlyToolRoundTrip(t *testing.T) {
	tool := classTool("get_topology", domain.ClassRead)
	audit := &recordingAudit{}
	gate := &recordingGate{decide: grantFor("")}
	deps := Deps{
		Tools: staticBag{tools: []ports.Tool{tool}},
		Gate:  gate,
		Audit: audit,
	}
	model := newFauxModel(t,
		toolStep("get_topology", map[string]any{"root": "prod"}),
		textStep("I found three tiers."),
	)
	sink := &collectSink{}
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return deps, nil },
		Now:    fixedClock(),
	})

	res, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{SessionID: "s-1", UserText: "map the topology"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Content, "I found three tiers.") {
		t.Errorf("content = %q, want the final answer, not the tool round", res.Content)
	}

	types := frameTypes(sink.Frames())
	for _, want := range []wire.StreamEventType{wire.StreamToolStart, wire.StreamToolEnd, wire.StreamDone} {
		if !hasType(types, want) {
			t.Errorf("no %s frame: got %v", want, types)
		}
	}

	// A read must not have consulted the approval queue.
	if len(gate.requests) != 0 {
		t.Errorf("the approval gate saw %d requests for a read-only call", len(gate.requests))
	}
	// It must have been audited, with the class recorded for filtering.
	if len(audit.entries) != 1 {
		t.Fatalf("audit recorded %d entries, want 1", len(audit.entries))
	}
	if audit.entries[0].Action != ports.ActionToolCall || audit.entries[0].Outcome != "success" {
		t.Errorf("audit entry = %s/%s", audit.entries[0].Action, audit.entries[0].Outcome)
	}
}

// --- a mutating tool through the gate -----------------------------------

func TestKernelAllowsAGrantedMutatingTool(t *testing.T) {
	tool := &fakeTool{schema: ports.ToolSchema{
		Name: "restart_service", Class: domain.ClassDestructive,
		Parameters: json.RawMessage(`{"type":"object","properties":{"service":{"type":"string"}}}`),
	}, out: "restarted"}
	audit := &recordingAudit{}
	gate := &recordingGate{decide: grantFor("CHG-9")}
	deps := Deps{Tools: staticBag{tools: []ports.Tool{tool}}, Gate: gate, Audit: audit}
	model := newFauxModel(t,
		toolStep("restart_service", map[string]any{"service": "web"}),
		textStep("web is back up."),
	)
	sink := &collectSink{}
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return deps, nil },
		Now:    fixedClock(),
	})

	if _, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{SessionID: "s-1", UserText: "restart web"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(gate.requests) != 1 {
		t.Fatalf("the gate saw %d requests, want 1", len(gate.requests))
	}
	if tool.out == "" {
		t.Error("the tool did not run")
	}
	// The tool_end frame must report success, not blocked: a granted call
	// that the console renders as refused would train operators to
	// distrust the approval queue.
	var ended *wire.ToolFrame
	for _, f := range sink.Frames() {
		if f.Type == wire.StreamToolEnd {
			ended = f.Tool
		}
	}
	if ended == nil || ended.Status != wire.ToolSuccess {
		t.Errorf("tool_end = %+v, want success", ended)
	}
}

func TestKernelBlocksAMutatingToolWhenDenied(t *testing.T) {
	tool := &fakeTool{schema: ports.ToolSchema{
		Name: "restart_service", Class: domain.ClassDestructive,
		Parameters: json.RawMessage(`{"type":"object"}`),
	}, out: "restarted"}
	audit := &recordingAudit{}
	gate := &recordingGate{decide: func(ports.ApprovalRequest) (ports.Decision, error) {
		return ports.Decision{Decision: ports.ApprovalDenied, Note: "change freeze"}, nil
	}}
	deps := Deps{Tools: staticBag{tools: []ports.Tool{tool}}, Gate: gate, Audit: audit}
	model := newFauxModel(t,
		toolStep("restart_service", map[string]any{"service": "web"}),
		textStep("I could not restart web; the change is frozen."),
	)
	sink := &collectSink{}
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return deps, nil },
		Now:    fixedClock(),
	})

	if _, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{SessionID: "s-1", UserText: "restart web"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The tool itself never ran — the refusal is the load-bearing fact.
	if tool.gotArgs != nil {
		t.Error("the tool was invoked despite a denial: the gate blocked the call in the hook, not after the fact")
	}

	var ended *wire.ToolFrame
	for _, f := range sink.Frames() {
		if f.Type == wire.StreamToolEnd {
			ended = f.Tool
		}
	}
	if ended == nil || ended.Status != wire.ToolBlocked {
		t.Fatalf("tool_end = %+v, want blocked: a denial is not a tool failure", ended)
	}
	if ended.Error == "" {
		t.Error("tool_end carries no reason: the operator needs to know why the call was refused")
	}

	// The audit must record the refusal, not a success.
	if len(audit.entries) != 1 {
		t.Fatalf("audit recorded %d entries, want 1", len(audit.entries))
	}
	if audit.entries[0].Action != ports.ActionToolBlocked || audit.entries[0].Outcome != "blocked" {
		t.Errorf("audit entry = %s/%s, want tool_blocked/blocked", audit.entries[0].Action, audit.entries[0].Outcome)
	}
}

func TestKernelBlocksAMutatingToolWithNoGate(t *testing.T) {
	// The end-to-end version of the fail-closed invariant: a kernel
	// misconfigured without an approval gate must refuse the write rather
	// than perform it.
	tool := &fakeTool{schema: ports.ToolSchema{
		Name: "delete_volume", Class: domain.ClassDestructive,
		Parameters: json.RawMessage(`{"type":"object"}`),
	}, out: "deleted"}
	deps := Deps{Tools: staticBag{tools: []ports.Tool{tool}}, Audit: &recordingAudit{}}
	model := newFauxModel(t,
		toolStep("delete_volume", map[string]any{"name": "vol-1"}),
		textStep("I did not delete the volume."),
	)
	sink := &collectSink{}
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return deps, nil },
		Now:    fixedClock(),
	})

	if _, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{SessionID: "s-1", UserText: "delete vol-1"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tool.gotArgs != nil {
		t.Fatal("a destructive tool ran with no approval gate configured")
	}
	var ended *wire.ToolFrame
	for _, f := range sink.Frames() {
		if f.Type == wire.StreamToolEnd {
			ended = f.Tool
		}
	}
	if ended == nil || ended.Status != wire.ToolBlocked {
		t.Errorf("tool_end = %+v, want blocked", ended)
	}
}

// --- kernel contract ----------------------------------------------------

func TestNewKernelRequiresItsDependencies(t *testing.T) {
	// A kernel with no model or no host services could only fail at the
	// first turn, in production, under load. Rejecting it at construction
	// turns a silent misconfiguration into a startup error.
	if _, err := NewKernel(KernelOptions{}); err == nil {
		t.Error("a kernel with no Models was accepted")
	}
	if _, err := NewKernel(KernelOptions{Models: &fauxResolver{}}); err == nil {
		t.Error("a kernel with no Deps was accepted")
	}
}

func TestRunRejectsAnEmptySessionID(t *testing.T) {
	model := newFauxModel(t, textStep("hi"))
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return Deps{}, nil },
	})
	if _, err := k.Run(context.Background(), ports.AgentRequest{UserText: "hi"}); err == nil {
		t.Error("a turn with no session id was accepted: the console could not scope the frames")
	}
}

func TestRunSurfacesDepsFailure(t *testing.T) {
	// A host that cannot assemble its policy must not get a turn that
	// would run tools outside it.
	model := newFauxModel(t, textStep("hi"))
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps: func(context.Context, ports.AgentRequest) (Deps, error) {
			return Deps{}, errors.New("tool registry unavailable")
		},
	})
	_, err := k.Run(context.Background(), ports.AgentRequest{SessionID: "s-1", UserText: "hi"})
	if err == nil || !strings.Contains(err.Error(), "tool registry unavailable") {
		t.Errorf("err = %v, want the host's failure surfaced", err)
	}
}

func TestRunSurfacesModelFailure(t *testing.T) {
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{err: errors.New("no provider configured for opus")},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return Deps{}, nil },
	})
	_, err := k.Run(context.Background(), ports.AgentRequest{SessionID: "s-1", UserText: "hi"})
	if err == nil || !strings.Contains(err.Error(), "no provider configured") {
		t.Errorf("err = %v, want the resolution failure surfaced", err)
	}
}

func TestRunPassesTheSelectionThrough(t *testing.T) {
	resolver := &fauxResolver{}
	model := newFauxModel(t, textStep("hi"))
	resolver.model = model
	k, _ := NewKernel(KernelOptions{
		Models: resolver,
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return Deps{}, nil },
		Now:    fixedClock(),
	})
	sel := domain.ModelSelection{Provider: domain.ProviderAnthropic, Model: "claude-opus-5"}
	if _, err := k.Run(context.Background(), ports.AgentRequest{SessionID: "s-1", UserText: "hi", Selection: sel}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resolver.seen) != 1 || resolver.seen[0] != sel {
		t.Errorf("resolver saw %+v, want the request's selection %+v", resolver.seen, sel)
	}
}

func TestRunReleasesTheSessionSlot(t *testing.T) {
	// A finished turn must not hold its slot, or a session could only ever
	// run one turn.
	model := newFauxModel(t, textStep("one"), textStep("two"))
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return Deps{}, nil },
		Now:    fixedClock(),
	})
	for i, text := range []string{"first", "second"} {
		if _, err := k.Run(context.Background(), ports.AgentRequest{SessionID: "s-1", UserText: text}); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if got := k.LiveSessions(); got != 0 {
		t.Errorf("LiveSessions = %d, want 0 after every turn settled", got)
	}
}

func TestRunRefusesOverlappingTurnsOnOneSession(t *testing.T) {
	// Two assistants writing into one shared incident view would interleave
	// their messages. The second turn must be refused, not queued silently.
	tool := newBlockingTool(ports.ToolSchema{
		Name: "slow_probe", Class: domain.ClassRead,
		Parameters: json.RawMessage(`{"type":"object"}`),
	})
	model := newFauxModel(t,
		toolStep("slow_probe", map[string]any{}),
		textStep("done"),
	)
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps: func(context.Context, ports.AgentRequest) (Deps, error) {
			return Deps{Tools: staticBag{tools: []ports.Tool{tool}}}, nil
		},
		Now: fixedClock(),
	})

	first := make(chan error, 1)
	go func() {
		_, err := k.Run(context.Background(), ports.AgentRequest{SessionID: "s-1", UserText: "probe"})
		first <- err
	}()

	// Wait until the first turn is genuinely in flight inside the tool, so
	// the second Run races a live session rather than an idle one.
	tool.waitUntilEntered(t)

	if _, err := k.Run(context.Background(), ports.AgentRequest{SessionID: "s-1", UserText: "again"}); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second turn err = %v, want %v", err, ErrAlreadyRunning)
	}

	close(tool.release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("first turn: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first turn never settled after the tool was released")
	}
}

func TestRunProducesNoFramesWithoutASink(t *testing.T) {
	// The kernel reads the sink from the context, so a caller that does not
	// want streaming simply does not install one. It must not panic on the
	// nil path.
	model := newFauxModel(t, textStep("hi"))
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return Deps{}, nil },
		Now:    fixedClock(),
	})
	if _, err := k.Run(context.Background(), ports.AgentRequest{SessionID: "s-1", UserText: "hi"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := ports.FromContext(context.Background()); got == nil {
		t.Error("FromContext returned nil for a bare context")
	}
}

func TestAbortAndSteerRequireALiveSession(t *testing.T) {
	model := newFauxModel(t, textStep("hi"))
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return Deps{}, nil },
	})
	if err := k.Abort(context.Background(), "nope"); err != nil {
		t.Errorf("Abort on an unknown session = %v, want nil: aborting nothing is not an error", err)
	}
	if err := k.Steer(context.Background(), "nope", "hello"); err == nil {
		t.Error("Steer on an unknown session succeeded: there is no turn to steer")
	}
}

func TestNotifyRejectsAWorkerWithNoLiveParent(t *testing.T) {
	model := newFauxModel(t, textStep("hi"))
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return Deps{}, nil },
	})
	// Delivering a notification to a settled parent would put a frame in
	// front of nobody.
	if err := k.Notify(context.Background(), "w-gone", "completed", "x"); !errors.Is(err, ErrNoRun) {
		t.Errorf("Notify err = %v, want %v", err, ErrNoRun)
	}
}

func TestCloseIsSafeWithNoSessions(t *testing.T) {
	model := newFauxModel(t, textStep("hi"))
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return Deps{}, nil },
	})
	if err := k.Close(); err != nil {
		t.Errorf("Close on an idle kernel = %v", err)
	}
}

func TestSpawnRequiresASessionID(t *testing.T) {
	model := newFauxModel(t, textStep("hi"))
	k, _ := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps:   func(context.Context, ports.AgentRequest) (Deps, error) { return Deps{}, nil },
	})
	if _, err := k.Spawn(context.Background(), ports.AgentRequest{UserText: "hi"}); err == nil {
		t.Error("a worker with no session id was accepted: its terminal state would have nowhere to report")
	}
}

// userText reads the plain text out of a user message, failing loudly on a
// shape the kernel never builds.
func userText(m agent.AgentMessage) string {
	text, ok := m.User.Content.(ai.UserText)
	if !ok {
		return ""
	}
	return string(text)
}

// --- prompt assembly ----------------------------------------------------

func TestBuildPromptOrdersTheReminderBeforeTheTurn(t *testing.T) {
	// The reminder is a separate user turn, not an appended system line, so
	// that a provider's prompt cache does not freeze it at the head of the
	// transcript. Order matters: it has to be seen before the request.
	msgs := buildPrompt(ports.AgentRequest{
		CriticalReminder: "  Only read-only tools are permitted.  ",
		UserText:         "  why is the pod crashlooping?  ",
	})
	if len(msgs) != 2 {
		t.Fatalf("built %d messages, want 2", len(msgs))
	}
	if got := userText(msgs[0]); !strings.Contains(got, "read-only") {
		t.Errorf("first message = %q, want the reminder first", got)
	}
	if got := userText(msgs[1]); !strings.Contains(got, "crashlooping") {
		t.Errorf("second message = %q, want the user's turn second", got)
	}
}

func TestBuildPromptFallsBackWhenThereIsNothingToSay(t *testing.T) {
	// An empty turn still has to send something, or the provider charges
	// for a call with nothing to answer.
	for _, req := range []ports.AgentRequest{
		{},
		{UserText: "   \n\t "},
		{CriticalReminder: "  "},
	} {
		msgs := buildPrompt(req)
		if len(msgs) != 1 {
			t.Errorf("buildPrompt(%+v) built %d messages, want 1", req, len(msgs))
		}
	}
}

func TestBuildPromptOmitsAnEmptyReminder(t *testing.T) {
	msgs := buildPrompt(ports.AgentRequest{UserText: "hi"})
	if len(msgs) != 1 {
		t.Fatalf("built %d messages, want 1: a blank reminder must not become a turn", len(msgs))
	}
	if got := userText(msgs[0]); !strings.Contains(got, "hi") {
		t.Errorf("content = %q", got)
	}
}

// --- blockedTool is a real tools.Tool ------------------------------------

// blockingTool holds a turn open until the test releases it, so a test can
// observe the kernel from inside a live turn.
//
// The two channels are one-way: the tool announces that it is in, and the
// test announces that it may leave. Neither waits on the other to start, so
// the test cannot deadlock on a rendezvous it is also responsible for
// satisfying.
type blockingTool struct {
	schema  ports.ToolSchema
	entered chan struct{} // closed when the tool starts
	release chan struct{} // closed when the tool may return
	once    sync.Once
}

func newBlockingTool(schema ports.ToolSchema) *blockingTool {
	return &blockingTool{
		schema:  schema,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *blockingTool) Schema() ports.ToolSchema { return b.schema }

func (b *blockingTool) Invoke(ctx context.Context, _ json.RawMessage) (string, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return "probed", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// waitUntilEntered blocks until the tool is running, or fails the test. The
// timeout is generous because the faux provider is fast but not instant.
func (b *blockingTool) waitUntilEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the tool never ran: the turn finished without reaching it")
	}
}

func TestBlockingToolSatisfiesThePort(t *testing.T) {
	var _ ports.Tool = (*blockingTool)(nil)
}

var (
	_ = time.Second
	_ agent.AgentEvent
	_ ai.AssistantMessageEvent
)
