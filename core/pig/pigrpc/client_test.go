package pigrpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding/rpcclient"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

func TestNewDefaultsTheBinary(t *testing.T) {
	// An unqualified "pig" is right on a node whose package put it on
	// PATH; an empty path would produce a confusing spawn error instead.
	if got := New(Options{}).opts.Binary; got != "pig" {
		t.Errorf("Binary = %q, want pig", got)
	}
	if got := New(Options{Binary: "/opt/pig"}).opts.Binary; got != "/opt/pig" {
		t.Errorf("Binary = %q, want the configured path", got)
	}
}

func TestAClientThatHasNotRunReportsNotRunning(t *testing.T) {
	// A supervisor polls this before the first Start. Reporting "running"
	// or blocking forever on a channel with no process behind it would
	// both be wrong.
	c := New(Options{Binary: "/nonexistent/pig"})
	if c.Running() {
		t.Error("Running is true before Start")
	}
	select {
	case <-c.Exited():
	default:
		t.Error("Exited is not closed before Start: a supervisor would block on a process that never existed")
	}
	if err := c.LastError(); err != nil {
		t.Errorf("LastError = %v, want nil before anything happened", err)
	}
}

func TestOperationsBeforeStartFailWithOneClearMessage(t *testing.T) {
	c := New(Options{})
	ctx := context.Background()
	calls := map[string]func() error{
		"prompt": func() error { return c.Prompt(ctx, "hi") },
		"steer":  func() error { return c.Steer(ctx, "hi") },
		"abort":  func() error { return c.Abort(ctx) },
		// The operation name is what the error embeds, so the case has to
		// match the method's own wording.
		"set model": func() error { return c.SetModel(ctx, "openai", "gpt-5.6") },
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s before Start succeeded", name)
			continue
		}
		// One message naming both the operation and the cause. A caller
		// that has to distinguish "no process" from "call failed" should
		// be able to do it from the text an operator will also read.
		if !strings.Contains(err.Error(), "not running") || !strings.Contains(err.Error(), name) {
			t.Errorf("%s error = %q, want it to name the operation and the cause", name, err)
		}
	}
	if _, err := c.State(ctx); err == nil {
		t.Error("State before Start succeeded")
	}
}

func TestStopBeforeStartIsSafeAndIdempotent(t *testing.T) {
	// A shutdown path that stops a supervisor it never started is
	// ordinary, not exceptional.
	c := New(Options{})
	for i := 0; i < 3; i++ {
		if err := c.Stop(); err != nil {
			t.Errorf("Stop %d = %v, want nil", i, err)
		}
	}
}

func TestStartFailsWhenTheBinaryDoesNotExist(t *testing.T) {
	c := New(Options{Binary: "/nonexistent/definitely-not-pig"})
	err := c.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded against a missing binary")
	}
	if !strings.Contains(err.Error(), "/nonexistent/definitely-not-pig") {
		t.Errorf("err = %v, want it to name the binary: the node's most common misconfiguration is a bad path", err)
	}
	// A failed start must leave the client in a state a retry can use,
	// not one that blocks it.
	if c.Running() {
		t.Error("Running is true after a failed start")
	}
	select {
	case <-c.Exited():
	default:
		t.Error("Exited is not closed after a failed start: a supervisor would wait on a process that never ran")
	}
	if c.LastError() == nil {
		t.Error("LastError is nil after a failed start: health would report nothing to explain the failure")
	}
}

func TestStartRefusesASecondProcess(t *testing.T) {
	c := New(Options{Binary: "/nonexistent/pig"})
	// First Start fails, so use a state where the guard is meaningful:
	// a client that believes it is running must refuse another start.
	c.running = true
	if err := c.Start(context.Background()); err == nil {
		t.Error("Start succeeded on a client that is already running: two agents would fight over one session")
	}
}

// --- event projection ---------------------------------------------------

func TestProjectEventForwardsTheRawRecord(t *testing.T) {
	// The payload is relayed verbatim. A control plane that decoded it
	// would break the day the agent adds a field, and the console is the
	// only component that needs to understand it.
	raw := json.RawMessage(`{"type":"assistant_delta","sessionId":"s-1","iteration":2,"seq":7,"content":"hi"}`)
	got := projectEvent(rpcclient.JsonAgentSessionEvent{Type: "assistant_delta", Raw: raw})

	if got.Type != "assistant_delta" {
		t.Errorf("Type = %q", got.Type)
	}
	if string(got.Payload) != string(raw) {
		t.Errorf("Payload = %s, want the record forwarded unchanged", got.Payload)
	}
	if got.SessionID != "s-1" {
		t.Errorf("SessionID = %q, want s-1: a multiplexed stream has to be demultiplexable", got.SessionID)
	}
	if got.Iteration != 2 || got.Seq != 7 {
		t.Errorf("Iteration = %d, Seq = %d, want 2 and 7", got.Iteration, got.Seq)
	}
}

func TestProjectEventDoesNotAliasTheRawRecord(t *testing.T) {
	// The RPC client reuses its read buffer. Handing the same slice to a
	// relay that outlives the read would deliver a later record's bytes
	// under an earlier frame's type.
	raw := json.RawMessage(`{"type":"tool_start"}`)
	got := projectEvent(rpcclient.JsonAgentSessionEvent{Type: "tool_start", Raw: raw})
	raw[0] = 'X'
	if strings.Contains(string(got.Payload), "X") {
		t.Error("the projected payload aliases the client's read buffer")
	}
}

func TestProjectEventRelaysRecordsWithAnUnparseableEnvelope(t *testing.T) {
	// Dropping an event because its envelope did not parse would lose
	// exactly the output an operator is watching for. The payload goes
	// through either way; only the convenience fields are best-effort.
	for _, raw := range []string{
		`not json at all`,
		`{"type":"assistant_delta"`,
		`[1,2,3]`,
		``,
	} {
		got := projectEvent(rpcclient.JsonAgentSessionEvent{Type: "assistant_delta", Raw: json.RawMessage(raw)})
		if got.Type != "assistant_delta" {
			t.Errorf("raw %q: Type = %q, want the event relayed", raw, got.Type)
		}
		if got.SessionID != "" || got.Iteration != 0 || got.Seq != 0 {
			t.Errorf("raw %q: envelope fields = %+v, want zero values rather than a guess", raw, got)
		}
	}
}

func TestTerminalEventNames(t *testing.T) {
	// A relay needs to know when a turn is over. It must not need to
	// understand every other event, and an unrecognised name has to be
	// treated as non-terminal so a new intermediate event cannot be
	// mistaken for the end of a turn.
	for _, name := range []string{"agent_end", "agent_settled", "turn_end"} {
		if !terminalEvent(name) {
			t.Errorf("terminalEvent(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"", "assistant_start", "assistant_delta", "assistant_end",
		"tool_execution_start", "tool_execution_end", "message_end",
		// A name PiG might add later must not silently become terminal.
		"agent_retrying",
	} {
		if terminalEvent(name) {
			t.Errorf("terminalEvent(%q) = true, want false", name)
		}
	}
}

func TestProjectEventMarksTerminal(t *testing.T) {
	terminal := projectEvent(rpcclient.JsonAgentSessionEvent{Type: "agent_end", Raw: json.RawMessage(`{}`)})
	if !terminal.Terminal {
		t.Error("agent_end is not marked terminal")
	}
	mid := projectEvent(rpcclient.JsonAgentSessionEvent{Type: "assistant_delta", Raw: json.RawMessage(`{}`)})
	if mid.Terminal {
		t.Error("assistant_delta is marked terminal")
	}
}

// --- state projection ---------------------------------------------------

func TestProjectStateCarriesWhatAFleetViewNeeds(t *testing.T) {
	// Everything dropped here is still available by asking the node
	// directly. What stays is what a fleet list has to render without
	// dialling every node on every refresh.
	state := projectState(rpcclient.RpcSessionState{
		SessionID:           "s-1",
		IsStreaming:         true,
		PendingMessageCount: 2,
		Model:               &rpcclient.Model{ID: "gpt-5.6", Provider: "openai"},
	}, "0.3.0")

	if state.SessionID != "s-1" {
		t.Errorf("SessionID = %q", state.SessionID)
	}
	if !state.Running {
		t.Error("Running is false while the agent is streaming")
	}
	if state.PendingToolCalls != 2 {
		t.Errorf("PendingToolCalls = %d, want 2", state.PendingToolCalls)
	}
	if state.Model != "gpt-5.6" || state.Provider != "openai" {
		t.Errorf("Model/Provider = %q/%q", state.Model, state.Provider)
	}
	// The version comes from configuration, not from the agent's reply:
	// a node is only as current as the binary it actually launched, and
	// that is the supervisor's to report.
	if state.Version != "0.3.0" {
		t.Errorf("Version = %q, want 0.3.0", state.Version)
	}
}

func TestProjectStateSurvivesAnAbsentModel(t *testing.T) {
	// A node whose model was never resolved must still answer, or the
	// console would show a node as unreachable rather than misconfigured.
	state := projectState(rpcclient.RpcSessionState{SessionID: "s-1"}, "0.3.0")
	if state.Model != "" || state.Provider != "" {
		t.Errorf("Model/Provider = %q/%q, want empty", state.Model, state.Provider)
	}
	if state.Running {
		t.Error("Running is true on an idle agent")
	}
}

// --- listener fan-out ---------------------------------------------------

func TestOnEventFanOutReachesTheListener(t *testing.T) {
	c := New(Options{})
	var got []ports.ProcessEvent
	c.OnEvent(func(ev ports.ProcessEvent) { got = append(got, ev) })

	c.relay(rpcclient.JsonAgentSessionEvent{Type: "assistant_delta", Raw: json.RawMessage(`{"seq":1}`)})
	if len(got) != 1 || got[0].Type != "assistant_delta" {
		t.Fatalf("listener saw %+v, want one frame", got)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	c := New(Options{})
	count := 0
	unsubscribe := c.OnEvent(func(ports.ProcessEvent) { count++ })
	c.relay(rpcclient.JsonAgentSessionEvent{Type: "a"})
	unsubscribe()
	c.relay(rpcclient.JsonAgentSessionEvent{Type: "b"})
	if count != 1 {
		t.Errorf("delivery count = %d, want 1: unsubscribing must actually detach", count)
	}
}

func TestTwoListenersBothReceiveEveryFrame(t *testing.T) {
	// The supervisor rebinds every one of its subscriptions on each
	// restart. A single subscriber slot meant the second bind silently
	// starved the first, and the symptom was a console quietly receiving
	// nothing while the node reported itself healthy.
	c := New(Options{})
	var first, second int
	c.OnEvent(func(ports.ProcessEvent) { first++ })
	c.OnEvent(func(ports.ProcessEvent) { second++ })

	c.relay(rpcclient.JsonAgentSessionEvent{Type: "a"})

	if first != 1 || second != 1 {
		t.Errorf("delivery counts = %d/%d, want 1/1", first, second)
	}
}

func TestUnsubscribingOneLeavesTheOtherAttached(t *testing.T) {
	// A reconnecting console installs a new listener before dropping the
	// old one. A late unsubscribe from the old subscription must not take
	// the new one down with it — that would look exactly like the node
	// going quiet at the moment a console reconnects.
	c := New(Options{})
	var first, second int
	unsubscribeFirst := c.OnEvent(func(ports.ProcessEvent) { first++ })
	c.OnEvent(func(ports.ProcessEvent) { second++ })

	unsubscribeFirst()
	c.relay(rpcclient.JsonAgentSessionEvent{Type: "a"})

	if first != 0 {
		t.Errorf("the old listener received %d frames after unsubscribing", first)
	}
	if second != 1 {
		t.Errorf("the new listener received %d frames, want 1", second)
	}
}

func TestRelayWithNoListenerIsSafe(t *testing.T) {
	c := New(Options{})
	// The agent can emit during startup, before any subscriber exists.
	c.relay(rpcclient.JsonAgentSessionEvent{Type: "agent_start"})
}

// --- options plumbing ---------------------------------------------------

func TestOptionsAreCarriedThrough(t *testing.T) {
	// The working directory is not incidental: the agent discovers its
	// extensions and skills relative to where it was launched, so this is
	// how a node is pointed at the plugin bundle it may load.
	opts := Options{
		Binary:   "/opt/pig",
		Cwd:      "/var/lib/opskeeper/pig",
		Env:      map[string]string{"PIG_PROFILE": "readonly"},
		Provider: "openai",
		Model:    "gpt-5.6",
		Args:     []string{"--no-color"},
		Version:  "0.3.0",
	}
	c := New(opts)
	if c.opts.Cwd != "/var/lib/opskeeper/pig" {
		t.Errorf("Cwd = %q", c.opts.Cwd)
	}
	if c.opts.Env["PIG_PROFILE"] != "readonly" {
		t.Errorf("Env = %v", c.opts.Env)
	}
	if c.opts.Provider != "openai" || c.opts.Model != "gpt-5.6" {
		t.Errorf("Provider/Model = %q/%q", c.opts.Provider, c.opts.Model)
	}
	if len(c.opts.Args) != 1 || c.opts.Args[0] != "--no-color" {
		t.Errorf("Args = %v", c.opts.Args)
	}
}

func TestExitedChannelIsReplacedOnEachStart(t *testing.T) {
	// A supervisor waits on the channel its start handed it. Reusing a
	// closed channel would make a restart look like an instant second
	// crash.
	c := New(Options{Binary: "/nonexistent/pig"})
	first := c.Exited()
	_ = c.Start(context.Background())
	second := c.Exited()
	if first == second {
		t.Error("Start reused the previous exit channel: a restart would appear to crash instantly")
	}
	select {
	case <-second:
	default:
		t.Error("the new channel is not closed after a failed start")
	}
}

var _ = time.Second
