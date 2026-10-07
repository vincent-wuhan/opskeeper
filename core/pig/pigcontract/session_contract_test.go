package pigcontract

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

// This file holds the second half of the PiG contract: the behaviours that
// OpsKeeper's control plane reasons about but that keep their types when
// upstream changes them.
//
// contract.go answers "does this name still exist". Everything here answers
// "does it still mean what we assumed it meant", and the four assumptions
// below are the ones a change to any of them would break silently:
//
//  1. AddBeforeToolCallHook APPENDS while SetFinishTurn REPLACES. This is
//     the whole reason the turn round cap is enforced through the
//     BeforeToolCall panel instead of through a finish-turn hook: a
//     replacing setter would discard the gate extension's hook on one
//     construction path and leave the cap silently off.
//  2. AcknowledgeEvent answers exactly one kind of event, and that event
//     only exists while someone is calling FlushEvents. A consumer that
//     stops acknowledging, or a consumer that filters the marker out
//     instead of answering it, turns every turn into a hang.
//  3. NoSession drops the file and nothing else. The session id is what
//     OpsKeeper's own transcript is keyed on, so a change that dropped the
//     id with the file would orphan every stored conversation.
//  4. SkipBuiltinTools removes a known set of general-purpose coding tools.
//     OpsKeeper sets it on every production turn, and the claim it makes in
//     its own comments is that the operations agent has no filesystem
//     editor — a claim that is only as good as this flag's behaviour.
//  5. CWDOverride does NOT move a fresh session's working directory. A
//     SessionKernel option claimed it did, and the claim was load-bearing:
//     a turn's relative paths resolve somewhere, and "somewhere we chose"
//     is a different assertion from "wherever the manager was started".
//     The assumption was measured and is currently false, which is why the
//     option is gone and the runtime's cwd is now the deployment's.
//
// These are behavioural assertions against a real coding.Session driven by
// PiG's faux provider, not against a re-implementation, so they fail when
// upstream changes and not when a test fixture drifts.

// fauxModel builds the deterministic model a contract turn runs on.
//
// PIG_TEST_FAUX is PiG's documented switch. The alternative — a live
// provider — could not answer any of the four questions above, because all
// four are about what the harness does with a tool call rather than about
// what a model decides to say.
func fauxModel(t *testing.T, steps ...ai.FauxResponseStep) *ai.Model {
	t.Helper()
	t.Setenv("PIG_TEST_FAUX", "1")
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

// textStep is a model reply that ends its turn.
func textStep(text string) ai.FauxResponseStep {
	return ai.FauxStaticStep(ai.FauxResponse{
		Content:    []ai.FauxContentBlock{ai.FauxText(text)},
		StopReason: string(ai.StopReasonStop),
	})
}

// toolStep is a model reply that asks for one tool call, which is the only
// way to make the tool-call hook panels observable.
func toolStep(name string, args map[string]any) ai.FauxResponseStep {
	return ai.FauxStaticStep(ai.FauxResponse{
		Content:    []ai.FauxContentBlock{ai.FauxToolCall(name, args, "tc-1")},
		StopReason: string(ai.StopReasonToolUse),
	})
}

// contractSession is one live session plus the runtime that owns it.
//
// The teardown order is the one PiG documents — session, then runtime — and
// it is written out rather than left to t.Cleanup's LIFO accident because
// closing the runtime first is the documented way to leak an extension
// process, and a contract test that leaks is a contract test that hangs the
// next run.
type contractSession struct {
	sess    *coding.Session
	runtime *coding.Runtime
	// cwd is the Services' working directory, which is what a fresh
	// session's own cwd is expected to equal. Kept here rather than
	// recomputed by each test so the two cannot drift.
	cwd string
}

func newContractSession(t *testing.T, steps []ai.FauxResponseStep, tools []agent.AgentTool, tune func(*coding.SessionStartOptions)) *contractSession {
	t.Helper()

	dir := t.TempDir()
	trusted := true
	manager, err := coding.NewInMemorySessionManager(dir)
	if err != nil {
		t.Fatalf("NewInMemorySessionManager: %v", err)
	}
	services, err := coding.NewServices(coding.ServicesOptions{
		CWD:             dir,
		AgentDir:        dir + "/agent",
		SessionManager:  manager,
		SettingsManager: coding.NewInMemorySettingsManager(coding.Settings{}),
		ProjectTrusted:  &trusted,
	})
	if err != nil {
		t.Fatalf("NewServices: %v", err)
	}
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	opts := coding.SessionStartOptions{
		Model:            fauxModel(t, steps...),
		SystemPrompt:     "You are an operations agent.",
		ExtraTools:       tools,
		SkipBuiltinTools: true,
		NoSession:        true,
	}
	if tune != nil {
		tune(&opts)
	}
	sess, err := runtime.New(opts)
	if err != nil {
		_ = runtime.Close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = sess.Close()
		if err := runtime.Close(); err != nil {
			t.Errorf("Runtime.Close: %v", err)
		}
	})
	return &contractSession{sess: sess, runtime: runtime, cwd: dir}
}

// eventTap is a consumer of the session's event stream.
//
// It exists as a goroutine that lives as long as the session, because that
// is the shape of the real thing: a coding.Session closes its Events channel
// only when the Session is closed, not at the end of a turn. A test that
// waits for the channel to close after one turn therefore waits forever,
// and a test that never drains it at all blocks the run once the buffer
// fills — which is why the harness this file is pinning has to get this
// right, and why the tap is written to make both mistakes impossible.
//
// TurnEndEvent is the settlement signal. Waiting for it rather than for the
// channel close is what makes "every event of this turn was examined" a
// statement the assertions can actually make.
type eventTap struct {
	settled chan struct{}
	once    sync.Once

	mu      sync.Mutex
	claimed int
	total   int
}

func (e *eventTap) run(sess *coding.Session) {
	go func() {
		for ev := range sess.Events() {
			if ev == nil {
				continue
			}
			e.mu.Lock()
			e.total++
			if coding.AcknowledgeEvent(ev) {
				e.claimed++
			}
			e.mu.Unlock()
			if _, ok := ev.(agent.TurnEndEvent); ok {
				e.once.Do(func() { close(e.settled) })
			}
		}
	}()
}

// waitSettled blocks until the turn has ended, failing the test if it has
// not within the budget. A run that never settles is the failure this whole
// file is about, so it must not be reported as an empty result.
func (e *eventTap) waitSettled(t *testing.T) {
	t.Helper()
	select {
	case <-e.settled:
	case <-time.After(30 * time.Second):
		t.Fatal("the turn did not settle within 30s")
	}
}

func (e *eventTap) counts() (claimed, total int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.claimed, e.total
}

// startTapped returns a session with a consumer already attached, because
// subscribing after Send is the mistake the PiG docs call out and the one
// this harness would otherwise be modelling.
func startTapped(cs *contractSession) *eventTap {
	tap := &eventTap{settled: make(chan struct{})}
	tap.run(cs.sess)
	return tap
}

// ── 5. the working directory a turn actually runs in ────────────────────────

// TestCWDOverrideDoesNotMoveAFreshSession pins the assumption that decided
// where a control-plane turn's relative paths resolve.
//
// PiG's SessionStartOptions.CWDOverride reads, in the upstream comment,
// like "selects an effective cwd for an opened Session". OpsKeeper read it
// that way and exposed it as a SessionKernel option whose doc said a tool
// with a relative path could not walk into the manager's own tree. It never
// set it, which hid a second problem: it would not have worked.
//
// Measured here against a real session, because the two readings differ by
// exactly the thing that matters — whether an operations turn runs in a
// directory the deployment chose or in whatever directory the manager
// process was started from.
//
// If this test ever fails, upstream has started honouring the override for
// fresh sessions, and the kernel can have its containment option back. That
// is a better outcome than the current one; it is simply not the current
// one, and the field that pretended otherwise has been removed.
func TestCWDOverrideDoesNotMoveAFreshSession(t *testing.T) {
	other := t.TempDir()
	cs := newContractSession(t, []ai.FauxResponseStep{textStep("ok")}, nil,
		func(opts *coding.SessionStartOptions) {
			opts.CWDOverride = &other
		})
	if other == cs.cwd {
		t.Fatal("the override directory and the runtime's cwd are the same " +
			"directory, so this test cannot tell them apart")
	}
	if got := cs.sess.CWD(); got == other {
		t.Fatalf("CWDOverride moved a fresh session to %q. That is upstream "+
			"implementing something it did not implement when decision 171 "+
			"removed the kernel's containment option; re-add it and point it "+
			"at a per-turn directory", other)
	}
	// The other half of the assertion: the override did not silently move
	// the session somewhere else entirely. Its cwd is the Services' cwd —
	// which in this harness is a temp dir, and in production is
	// RuntimeOptions.CWD.
	if got, want := cs.sess.CWD(), cs.cwd; got != want {
		t.Errorf("session cwd is %q, want the runtime's %q", got, want)
	}
}

// ── 1. hook installation: append versus replace ──────────────────────────────

// TestToolCallHooksAppendWhileSettersReplace pins the asymmetry the round
// cap's design rests on.
//
// If AddBeforeToolCallHook ever became a replacing setter, the order
// pigcoding.Start composes — budget hook first, caller hooks after — would
// stop mattering, and worse, a session that added the audit hook after
// construction would lose the policy gate that was installed at start. The
// failure mode is not a crash: it is a turn with no gate, on a node, which
// is the shape of incident this project exists to prevent.
func TestToolCallHooksAppendWhileSettersReplace(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	hook := func(name string) agent.BeforeToolCallHook {
		return func(context.Context, string, string, json.RawMessage) agent.ToolCallHookResult {
			record(name)
			return agent.ToolCallHookResult{}
		}
	}

	cs := newContractSession(t,
		[]ai.FauxResponseStep{toolStep("noop", map[string]any{}), textStep("done")},
		[]agent.AgentTool{&contractTool{name: "noop"}},
		nil,
	)

	cs.sess.Agent().AddBeforeToolCallHook(hook("first"))
	cs.sess.Agent().AddBeforeToolCallHook(hook("second"))

	tap := startTapped(cs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cs.sess.Send(ctx, "call the tool"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	tap.waitSettled(t)

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("before-tool-call hooks ran as %v; an appended second hook must run after the first, not instead of it", got)
	}
}

// TestFinishTurnReplacesThePreviousHook is the other half of the asymmetry,
// and it is why the round cap is not a finish-turn hook.
//
// A finish-turn hook that continued the loop until a cap was spent looks
// equivalent to blocking the tool call. It is not: SetFinishTurn replaces,
// so a gate extension or an audit hook that installs its own finish-turn
// hook after ours would silently switch the cap off for that turn, and the
// turn would then run until the provider stopped offering to continue.
func TestFinishTurnReplacesThePreviousHook(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	finish := func(name string) agent.FinishTurn {
		return func(context.Context, agent.AgentTurnContext) (*agent.AgentTurnDecision, error) {
			mu.Lock()
			ran = append(ran, name)
			n := len(ran)
			mu.Unlock()
			// One continuation, then end. A hook that always continued
			// would ask the provider for a turn the queue cannot answer,
			// and the test would be measuring a provider error rather
			// than which hook survived.
			action := agent.AgentTurnContinue
			if n > 1 {
				action = agent.AgentTurnEnd
			}
			return &agent.AgentTurnDecision{Action: action}, nil
		}
	}

	cs := newContractSession(t,
		[]ai.FauxResponseStep{textStep("one"), textStep("two")},
		nil,
		nil,
	)

	first, second := finish("first"), finish("second")
	cs.sess.Agent().SetFinishTurn(first)
	cs.sess.Agent().SetFinishTurn(second)

	// The getter is the direct statement of the contract, so it is asserted
	// before the behavioural half: if it ever stops reporting the installed
	// hook, the behavioural assertion below would fail for a reason that is
	// hard to read.
	if got := cs.sess.Agent().FinishTurnHook(); got == nil {
		t.Fatal("FinishTurnHook is nil after SetFinishTurn")
	} else if reflect.ValueOf(got).Pointer() != reflect.ValueOf(second).Pointer() {
		t.Fatal("FinishTurnHook is not the last hook installed; SetFinishTurn must replace, not accumulate")
	}

	tap := startTapped(cs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cs.sess.Send(ctx, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	tap.waitSettled(t)

	mu.Lock()
	defer mu.Unlock()
	for _, name := range ran {
		if name == "first" {
			t.Fatalf("the replaced finish-turn hook still ran (saw %v); a replacing setter must not leave the earlier hook in the chain", ran)
		}
	}
	if len(ran) == 0 {
		t.Fatal("no finish-turn hook ran at all; the assertion above would pass against a loop that ignores the panel")
	}
}

// ── 2. the event barrier ────────────────────────────────────────────────────

// TestAcknowledgeEventOnlyAnswersTheBarrier pins both halves of the
// barrier contract: the marker is the only event it claims, and the marker
// only exists while a caller is inside FlushEvents.
//
// The first half is what lets a consumer call Acknowledge unconditionally
// without swallowing a real frame. The second is what a turn's event stream
// looks like in OpsKeeper today: no host calls FlushEvents, so no marker is
// ever emitted and the consumer's Acknowledge is always false. If upstream
// started emitting a marker unconditionally, every OpsKeeper event would be
// classified as a barrier and the console would render an empty turn.
func TestAcknowledgeEventOnlyAnswersTheBarrier(t *testing.T) {
	cs := newContractSession(t, []ai.FauxResponseStep{textStep("hello")}, nil, nil)

	tap := startTapped(cs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cs.sess.Send(ctx, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	tap.waitSettled(t)

	claimed, total := tap.counts()
	if total == 0 {
		t.Fatal("the turn produced no events; the predicate was never exercised")
	}
	if claimed != 0 {
		t.Fatalf("a turn with no FlushEvents caller emitted %d acknowledged barriers; OpsKeeper's consumer would drop those frames as if they were internal markers", claimed)
	}
}

// TestFlushEventsWaitsForAnAcknowledgement is the reason the consumer must
// answer the marker rather than filter it out.
//
// The test asserts the block first and the release second, in that order,
// because either half alone is satisfiable by a broken harness: an
// implementation that never blocked would pass a release-only assertion, and
// one that never released would pass a block-only assertion while hanging
// every turn in production.
func TestFlushEventsWaitsForAnAcknowledgement(t *testing.T) {
	cs := newContractSession(t, []ai.FauxResponseStep{textStep("hello")}, nil, nil)

	// The consumer answers nothing, so the marker stays outstanding and
	// FlushEvents must keep waiting.
	events := cs.sess.Events()
	leaked := make(chan agent.AgentEvent, 16)
	go func() {
		for ev := range events {
			leaked <- ev
		}
		close(leaked)
	}()

	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFlush()

	released := make(chan error, 1)
	go func() { released <- cs.sess.FlushEvents(flushCtx) }()

	select {
	case err := <-released:
		t.Fatalf("FlushEvents returned (%v) while the marker was unacknowledged; the barrier would be decorative", err)
	case <-time.After(250 * time.Millisecond):
	}

	// Now answer the marker, exactly as SessionKernel's consumer loop does.
	acked := 0
	for ev := range leaked {
		if coding.AcknowledgeEvent(ev) {
			acked++
			break
		}
	}
	if acked != 1 {
		t.Fatalf("acknowledged %d barriers, want exactly 1", acked)
	}

	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("FlushEvents: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("FlushEvents did not return after the marker was acknowledged")
	}
}

// ── 3. NoSession ────────────────────────────────────────────────────────────

// TestNoSessionKeepsTheIdentityAndDropsTheFile pins what OpsKeeper's
// persistence decision actually costs.
//
// OpsKeeper runs every turn with NoSession because its own session table is
// the transcript of record, and that is only sound while the id survives:
// the id is the join key between the stored conversation, the audit chain
// and the node's report of which session it served. A NoSession that also
// dropped the id would not fail loudly — it would return a fresh id per turn
// and quietly scatter a conversation across rows that share nothing.
func TestNoSessionKeepsTheIdentityAndDropsTheFile(t *testing.T) {
	cs := newContractSession(t, []ai.FauxResponseStep{textStep("hello")}, nil, nil)

	if id := cs.sess.ID(); id == "" {
		t.Fatal("NoSession produced an empty session id; stored conversations would have nothing to join on")
	}
	if path := cs.sess.Path(); path != "" {
		t.Fatalf("NoSession session reports path %q; OpsKeeper's transcript of record is its own table, not a file on the manager's disk", path)
	}

	tap := startTapped(cs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cs.sess.Send(ctx, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	tap.waitSettled(t)

	if len(cs.sess.Messages()) == 0 {
		t.Fatal("NoSession left the session with no in-memory messages; the loop ran against an empty transcript")
	}
}

// ── 4. SkipBuiltinTools ─────────────────────────────────────────────────────

// TestSkipBuiltinToolsRemovesTheCodingToolkit pins the flag behind the
// claim pigcoding.Start makes about not shipping a filesystem editor.
//
// The subset assertion is the general one: with the flag set, every tool
// the session can see must be one OpsKeeper handed it. The name assertion
// is the specific one, and it is written as literals rather than as a
// derived list so that an upstream rename surfaces here as a question —
// "is this still the tool we thought it was?" — instead of silently
// reclassifying a renamed tool as harmless.
func TestSkipBuiltinToolsRemovesTheCodingToolkit(t *testing.T) {
	builtin := newContractSession(t, []ai.FauxResponseStep{textStep("hi")}, nil, func(o *coding.SessionStartOptions) {
		o.SkipBuiltinTools = false
	})
	skipped := newContractSession(t, []ai.FauxResponseStep{textStep("hi")}, nil, func(o *coding.SessionStartOptions) {
		o.SkipBuiltinTools = true
	})

	all := map[string]struct{}{}
	for _, tool := range builtin.sess.Tools() {
		all[tool.Name()] = struct{}{}
	}
	if len(all) == 0 {
		t.Fatal("a session with the built-in tools enabled has none; the flag's effect is unmeasurable")
	}
	for _, name := range []string{"bash", "read", "write", "edit"} {
		if _, ok := all[name]; !ok {
			t.Errorf("built-in toolkit no longer contains %q; the security claim in pigcoding.Start and pigprofile must be re-examined against what replaced it", name)
		}
	}

	for _, tool := range skipped.sess.Tools() {
		if _, ok := all[tool.Name()]; !ok {
			t.Errorf("SkipBuiltinTools left %q on the session", tool.Name())
		}
		if tool.Name() == "bash" || tool.Name() == "read" || tool.Name() == "write" || tool.Name() == "edit" {
			t.Errorf("SkipBuiltinTools did not remove the general-purpose tool %q", tool.Name())
		}
	}
}

// TestCallerToolsSurviveTheSkip is the complement, and it is the assertion
// that would catch the flag being fixed by accident.
//
// SkipBuiltinTools must remove PiG's catalogue without touching the tool bag
// OpsKeeper assembled. A fix that cleared the whole registry — or that
// reordered ExtraTools after the skip — would satisfy the previous test and
// leave every operations turn with no tools at all, which is a console that
// answers in prose and never touches a system.
func TestCallerToolsSurviveTheSkip(t *testing.T) {
	cs := newContractSession(t,
		[]ai.FauxResponseStep{toolStep("get_topology", map[string]any{}), textStep("done")},
		[]agent.AgentTool{&contractTool{name: "get_topology"}},
		func(o *coding.SessionStartOptions) { o.SkipBuiltinTools = true },
	)

	names := map[string]struct{}{}
	for _, tool := range cs.sess.Tools() {
		names[tool.Name()] = struct{}{}
	}
	if _, ok := names["get_topology"]; !ok {
		t.Fatalf("SkipBuiltinTools removed the caller's own tool; the session can see %v", names)
	}

	tap := startTapped(cs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cs.sess.Send(ctx, "what is the topology?"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	tap.waitSettled(t)
}

// contractTool is the smallest tool a loop will accept: it answers every
// call with a fixed text block and records nothing, so the tests above
// assert about the harness and not about tool behaviour.
type contractTool struct {
	name string
}

func (t *contractTool) Name() string          { return t.name }
func (t *contractTool) Label() string         { return t.name }
func (t *contractTool) Schema() ai.ToolSchema { return ai.ToolSchema{} }
func (t *contractTool) ExecutionMode() agent.ToolExecutionMode {
	return agent.ToolModeParallel
}

func (t *contractTool) Execute(context.Context, string, json.RawMessage, agent.ToolUpdateCallback) (agent.AgentToolResult, error) {
	return agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "ok"}}}, nil
}
