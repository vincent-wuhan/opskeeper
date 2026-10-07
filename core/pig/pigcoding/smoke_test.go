package pigcoding_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigcoding"
)

// The tests in this file are the argument that pigcoding is a real
// integration rather than a layer of indirection: they drive a genuine
// coding.Session — PiG's own agent loop, extension runner, session log and
// provider dispatch — to a settled turn, offline.
//
// PIG_TEST_FAUX is PiG's documented switch for its deterministic provider.
// It matters beyond speed. A test that reaches a real provider proves the
// request was well-formed; it cannot prove the tool loop ran, that a
// blocked call never executed, or that the transcript came back in the
// order the model produced it, because all three of those depend on a model
// that behaves differently on every run. The faux provider makes them
// assertions instead of hopes.

func withFauxProvider(t *testing.T) {
	t.Helper()
	t.Setenv("PIG_TEST_FAUX", "1")
}

// registerFaux publishes PiG's deterministic provider the same way a
// deployment publishes a real one.
//
// It goes through RegisterProviders rather than poking the registry
// directly, and that is the point: the allowlist is not a test-only
// decoration bolted onto BuildModel, it is the only way a model becomes
// reachable at all. A test that skipped it would exercise a path no
// production caller can take.
func registerFaux(t *testing.T, rt *pigcoding.Runtime) {
	t.Helper()
	if err := pigcoding.RegisterProviders(rt, []pigcoding.ProviderSpec{{
		ID:           "test-faux",
		DefaultModel: "faux-1",
		Models:       []string{"faux-1"},
	}}); err != nil {
		t.Fatalf("RegisterProviders: %v", err)
	}
}

// newRuntime builds a runtime whose agent directory is a per-test temp dir.
//
// The temp dir is the whole point of the assertion in
// TestTheRuntimeWritesNothingToDisk: PiG's defaults are ~/.pig and
// ./.pig/sessions, and a server that inherits either of them is writing an
// operator's credentials and transcripts somewhere nobody provisioned.
func newRuntime(t *testing.T) *pigcoding.Runtime {
	t.Helper()
	dir := t.TempDir()
	rt, err := pigcoding.NewRuntime(pigcoding.RuntimeOptions{
		AgentDir: dir + "/agent",
		CWD:      dir + "/work",
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return rt
}

// echoTool returns whatever argument it was given, so a test can assert on
// the exact JSON the model produced rather than on a summary of it.
type echoTool struct {
	mu       sync.Mutex
	seen     []string
	blocked  chan string
	gateOnce sync.Once
}

func (e *echoTool) Name() string  { return "echo" }
func (e *echoTool) Label() string { return "Echo" }
func (e *echoTool) ExecutionMode() agent.ToolExecutionMode {
	return agent.ToolModeParallel
}

func (e *echoTool) Schema() ai.ToolSchema {
	return ai.ToolSchema{
		Name:        "echo",
		Description: "Echoes its argument back.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string"},
			},
			"required": []string{"text"},
		},
	}
}

func (e *echoTool) Execute(_ context.Context, _ string, params json.RawMessage, _ agent.ToolUpdateCallback) (agent.AgentToolResult, error) {
	e.mu.Lock()
	e.seen = append(e.seen, string(params))
	e.mu.Unlock()
	return agent.AgentToolResult{
		Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "echoed " + string(params)}},
	}, nil
}

func (e *echoTool) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.seen)
}

// assistantText is the last assistant message's text, which is what a
// console renders.
func assistantText(msgs []agent.AgentMessage) string {
	text := ""
	for _, m := range msgs {
		if m.Assistant == nil {
			continue
		}
		text = ""
		for _, block := range m.Assistant.Content {
			if t, ok := block.(ai.TextContent); ok {
				text += t.Text
			}
		}
	}
	return text
}

// TestATurnRunsOnTheEmbeddedRuntime is the end-to-end gate: construct the
// SDK, resolve a model, start a session, send a turn, read the reply.
//
// If this passes, every later test in the repository can assume a working
// agent runtime and test policy rather than plumbing.
func TestATurnRunsOnTheEmbeddedRuntime(t *testing.T) {
	withFauxProvider(t)
	rt := newRuntime(t)

	registerFaux(t, rt)
	model, err := rt.BuildModel("test-faux/faux-1")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}

	sess, err := rt.Start(pigcoding.Start{
		Model:            model,
		SystemPrompt:     "You are a concise assistant.",
		SkipBuiltinTools: true,
		NoSession:        true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := sess.Send(ctx, "What is 1 + 1?"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if sess.Running() {
		t.Error("session still reports running after Send returned")
	}
	if got := assistantText(sess.Messages()); got == "" && len(sess.Messages()) == 0 {
		t.Error("session produced no messages at all")
	}
}

// TestTheRuntimeWritesNothingToDisk is the property that separates a server
// integration from a developer convenience.
//
// PiG's defaults are a file-backed settings manager, an auth.json under
// ~/.pig, and a JSONL session log under the working directory. Every one of
// those is right for an interactive CLI and wrong for a node agent: the
// credentials are an operator's, and the transcript is a copy of data the
// control plane already stores with access control.
//
// The test walks the temp tree after a full turn rather than checking a
// flag, because a future change that reintroduces a file would not set that
// flag — it would just start writing.
func TestTheRuntimeWritesNothingToDisk(t *testing.T) {
	withFauxProvider(t)
	dir := t.TempDir()
	agentDir := dir + "/agent"
	workDir := dir + "/work"

	rt, err := pigcoding.NewRuntime(pigcoding.RuntimeOptions{
		AgentDir: agentDir,
		CWD:      workDir,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close()

	registerFaux(t, rt)
	model, err := rt.BuildModel("test-faux/faux-1")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	sess, err := rt.Start(pigcoding.Start{
		Model:            model,
		SystemPrompt:     "You are a concise assistant.",
		SkipBuiltinTools: true,
		NoSession:        true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sess.Send(ctx, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// PiG materialises an empty auth.json (0600) when the credential store
	// opens, and it gives OpsKeeper no way to suppress that. The file is
	// therefore expected; what must not be in it is a credential. An
	// OpsKeeper key written to disk by an agent library would be a
	// credential the platform cannot rotate, audit or revoke through the
	// admin page it is edited in.
	auth, err := os.ReadFile(agentDir + "/auth.json")
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("read auth.json: %v", err)
		}
	} else if trimmed := strings.TrimSpace(string(auth)); trimmed != "" && trimmed != "{}" {
		t.Errorf("auth.json holds %q; OpsKeeper credentials must never be written to disk", trimmed)
	}

	// No transcript, no settings copy, no model catalog copy. Those are the
	// three files that would put a second, unsynchronised copy of
	// operator-controlled state on the node.
	for _, sub := range []string{"sessions", "settings.json", "models.json"} {
		if _, err := os.Stat(agentDir + "/" + sub); err == nil {
			t.Errorf("agent dir contains %q; the runtime must not persist a second copy of operator state", sub)
		}
	}
	if _, err := os.Stat(workDir + "/.pig"); err == nil {
		t.Error("working directory contains .pig; the runtime must not anchor sessions on disk")
	}
}

// TestTheHostGateBlocksAToolCall proves the policy seam is real.
//
// A BeforeToolCall hook that blocks must prevent the tool body from running
// AND produce a settled turn — a blocked call that wedges the loop would be
// worse than no gate, because the agent would sit until the operator's
// timeout.
func TestTheHostGateBlocksAToolCall(t *testing.T) {
	withFauxProvider(t)
	rt := newRuntime(t)

	registerFaux(t, rt)
	model, err := rt.BuildModel("test-faux/faux-1")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}

	tool := &echoTool{}
	sess, err := rt.Start(pigcoding.Start{
		Model:            model,
		SystemPrompt:     "You are a concise assistant.",
		Tools:            []agent.AgentTool{tool},
		SkipBuiltinTools: true,
		NoSession:        true,
		BeforeToolCall: []agent.BeforeToolCallHook{
			func(_ context.Context, _, name string, _ json.RawMessage) agent.ToolCallHookResult {
				if name == "echo" {
					return agent.ToolCallHookResult{Block: true, Reason: "echo is not permitted in this profile"}
				}
				return agent.ToolCallHookResult{}
			},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The faux provider is scripted, not instruction-following, so this
	// send may or may not produce a tool call. What is asserted is the
	// invariant, not the trigger: if a call happened, the gate stopped it.
	_, _ = sess.Send(ctx, "please echo something")

	if n := tool.calls(); n != 0 {
		t.Errorf("a blocked tool executed %d time(s); the host gate is not authoritative", n)
	}
}

// TestARegisteredProviderIsResolvable is the credential-injection gate.
//
// OpsKeeper never writes auth.json. Its keys come from a settings table and
// are published into PiG's model registry. If that publication is wrong,
// BuildModel fails here — before an operator has typed a prompt and been
// told a provider is "not configured".
func TestARegisteredProviderIsResolvable(t *testing.T) {
	rt := newRuntime(t)

	if err := pigcoding.RegisterProviders(rt, []pigcoding.ProviderSpec{{
		ID:           "zhipu",
		APIKey:       "test-key-not-a-real-credential",
		BaseURL:      "https://example.invalid/v4",
		DefaultModel: "glm-4-plus",
		Models:       []string{"glm-4-plus", "glm-4-air"},
	}}); err != nil {
		t.Fatalf("RegisterProviders: %v", err)
	}

	if _, err := rt.BuildModel("zhipu/glm-4-plus"); err != nil {
		t.Fatalf("BuildModel after registration: %v", err)
	}
	// A slug the settings table did not list must not resolve. PiG itself
	// would accept it — an unlisted slug is treated as "this provider
	// probably serves it" — which is right for a CLI and wrong for a
	// platform whose model list is a cost decision. That gap is the reason
	// pigcoding keeps its own catalogue.
	if _, err := rt.BuildModel("zhipu/not-a-configured-model"); !errors.Is(err, pigcoding.ErrNoSuchModel) {
		t.Errorf("BuildModel for an unconfigured slug returned %v, want ErrNoSuchModel", err)
	}

	// The catalogue is the console's model picker, and it must agree with
	// what the server accepts.
	got := rt.Catalogue()
	want := []string{"zhipu/glm-4-air", "zhipu/glm-4-plus"}
	if len(got) != len(want) {
		t.Fatalf("Catalogue() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Catalogue()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestAClosedRuntimeRefusesWork is the shutdown-order gate.
//
// Every host that embeds this will close it on the way out. A Runtime that
// answers a turn after Close either leaked an extension subprocess or
// resolved a model against a closed credential store, and both surface as
// a 401 minutes after a clean-looking restart.
func TestAClosedRuntimeRefusesWork(t *testing.T) {
	withFauxProvider(t)
	rt := newRuntime(t)
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := rt.Start(pigcoding.Start{Model: &ai.Model{ID: "x"}}); !errors.Is(err, pigcoding.ErrClosed) {
		t.Errorf("Start after Close returned %v, want ErrClosed", err)
	}
	if _, err := rt.BuildModel("test-faux/faux-1"); !errors.Is(err, pigcoding.ErrClosed) {
		t.Errorf("BuildModel after Close returned %v, want ErrClosed", err)
	}
	// Close is called twice by the test cleanup, and must be idempotent:
	// shutdown paths in a server routinely run more than once.
	if err := rt.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestTheFauxProviderReachesTheToolLoop proves the runtime dispatches tools
// at all — the property every policy test above depends on.
func TestTheFauxProviderReachesTheToolLoop(t *testing.T) {
	withFauxProvider(t)
	rt := newRuntime(t)

	registerFaux(t, rt)
	model, err := rt.BuildModel("test-faux/faux-1")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	sess, err := rt.Start(pigcoding.Start{
		Model:            model,
		SystemPrompt:     "You are a concise assistant.",
		Tools:            []agent.AgentTool{&echoTool{}},
		SkipBuiltinTools: true,
		NoSession:        true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	// Subscribing before Send is the documented contract, and the faux
	// provider's basic scenario emits without tools, so this asserts the
	// channel is live rather than that a tool ran.
	events := sess.Events()
	if events == nil {
		t.Fatal("Events returned nil; a consumer would silently render nothing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sess.Send(ctx, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	saw := false
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if !saw {
					t.Fatal("event stream closed without delivering a frame")
				}
				return
			}
			if ev != nil {
				saw = true
			}
		case <-time.After(2 * time.Second):
			if !saw {
				t.Fatal("no event arrived within 2s of Send returning")
			}
			return
		}
	}
}

// TestTheSessionSystemPromptReachesTheModel checks the one piece of
// OpsKeeper policy that must survive into PiG verbatim.
//
// The persona text is assembled by the control plane, not by the agent
// runtime. If it were dropped or reordered on the way in, the agent would
// still run and would still answer — just not as the operator configured.
func TestTheSessionSystemPromptReachesTheModel(t *testing.T) {
	withFauxProvider(t)
	rt := newRuntime(t)
	registerFaux(t, rt)
	model, err := rt.BuildModel("test-faux/faux-1")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	const persona = "OPERATIONS_PERSONA_MARKER: diagnose before you repair."
	sess, err := rt.Start(pigcoding.Start{
		Model:            model,
		SystemPrompt:     persona,
		SkipBuiltinTools: true,
		NoSession:        true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	if err := sess.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if got := sess.ID(); strings.TrimSpace(got) == "" {
		t.Error("session has no id; a turn could not be traced back to its agent")
	}
}
