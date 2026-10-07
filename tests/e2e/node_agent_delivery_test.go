//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/tests/e2e/testenv"
)

// assertNoRefusedModelCalls asserts the property the substituted model cannot
// prove by passing.
//
// Everything else these tests check is about the delivery path, and the fake
// LLM is what stands in for the provider. A fake that answers every request
// proves the pipe is open and says nothing about whether the request would
// survive a real one -- and the thing that breaks in production is the
// translation, not the connection. So the fake refuses what a provider
// refuses (see testenv.FakeLLM.Refusals) and this turns that record into an
// assertion at the point where it is still cheap to read.
//
// The failure message names the request that was refused rather than only
// saying that one was, because a refusal list is a specification of what to
// fix and a count is not.
func assertNoRefusedModelCalls(t *testing.T, env *testenv.Env) {
	t.Helper()
	refusals := env.FakeLLM().Refusals()
	if len(refusals) == 0 {
		return
	}
	t.Fatalf("this run sent %d request(s) a real provider would have refused, "+
		"so the delivery path being green says nothing about surviving a real provider:\n  - %s\n%s",
		len(refusals), strings.Join(refusals, "\n  - "), env.ManagerLogs())
}

// TestTheGatewayServesAStreamToANodeCredential isolates the first hop.
//
// The delivery path has three hops — node agent to gateway, gateway to
// provider, frames back — and a failure in a full-topology test cannot say
// which one broke. This test cuts the node out entirely: it mints a node
// credential through the same API an operator uses and calls the gateway
// with it, which is exactly what a node's agent does over the wire.
//
// It is here because a node that cannot get a model answer has no
// conversation to have, so "the turn produced no frames" is only
// interpretable once this hop is known to work.
func TestTheGatewayServesAStreamToANodeCredential(t *testing.T) {
	env := testenv.Start(t)
	login := env.LoginAdmin()
	env.FakeLLM().SetLLMReply("网关直连探针。")

	_, access, secret := env.CreateEdge(t, login.AccessToken, "gateway-probe-node")

	// Streamed, because that is what the node's agent asks for, and the two
	// paths are different code: the gateway settles the provider's reply and
	// re-encodes it as frames.
	status, body, err := env.DoJSON("POST", "/v1/chat/completions", map[string]any{
		"model": "fake-gpt",
		"messages": []map[string]any{
			{"role": "user", "content": "ping"},
		},
		"stream": true,
	}, access+":"+secret)
	if err != nil {
		t.Fatalf("gateway stream: transport: %v", err)
	}
	if status != 200 {
		t.Fatalf("gateway stream: status=%d body=%s", status, testenv.MustJSON(body))
	}
	if env.FakeLLM().CallCount() == 0 {
		t.Fatalf("the gateway answered without calling a model; it is not proxying\n%s", env.ManagerLogs())
	}
	// The body, not the status line. This assertion was the one that would
	// have caught the bug this test was written next to: the gateway settled
	// the provider reply with no content blocks, wrote a well-formed stream
	// with nothing in it, and answered 200. A hop test that checks the
	// status and stops is a hop test that cannot tell a working hop from a
	// silent one.
	stream, err := env.StreamBody("/v1/chat/completions", map[string]any{
		"model": "fake-gpt",
		"messages": []map[string]any{
			{"role": "user", "content": "ping"},
		},
		"stream": true,
	}, access+":"+secret)
	if err != nil {
		t.Fatalf("gateway stream body: transport: %v", err)
	}
	if !strings.Contains(stream, "网关直连探针") {
		t.Fatalf("the stream carried no model text; a gateway that answers 200 with an "+
			"empty stream is indistinguishable, to every client, from a broken one\nbody: %s", stream)
	}
	assertNoRefusedModelCalls(t, env)
}

// The delivery acceptance, run against real processes.
//
// What is real: the manager binary, the node binary, the node's agent —
// the pig binary built from core/pig with the workspace off, the same way
// a release builds it — a frontier broker, the node's own sockets, the
// OpenAI-compatible gateway on the manager, and the console's SSE frame
// contract. Every frame this test asserts on was produced by a model call
// that left the node, crossed a broker, and came back.
//
// What is substituted: the model. The upstream is the harness's fake LLM,
// so this proves the delivery path and says nothing about how good the
// answers are. The distinction is stated in the failure messages too,
// because a future reader who finds this test green must not be left
// thinking a node agent has been shown to reason.
//
// Why a separate file rather than another case in the nodefleet e2e: that
// suite substitutes the transport (an in-process loopback) and the agent
// process (a scripted stand-in), which is the right trade for three
// operational scenarios and the wrong one for this question. The question
// here is precisely whether the real halves fit together, and a test that
// replaces both halves cannot answer it.
func TestNodeAgentDelivery(t *testing.T) {
	// Decoys in the *test runner's* own environment, planted before a single
	// process is spawned. The harness scrubs credential-shaped variables out
	// of what it hands a child, and this is what proves it did: without a
	// decoy, "the node has no provider key" would also be true on a machine
	// that simply had none to leak, and a test that cannot fail is not a
	// test. One decoy per arm of the shape rule, because three names that all
	// happened to be caught by a single earlier filter would prove nothing
	// about the other two: an exact vendor key, a token only the token arm
	// catches, and a cloud prefix with no key in its name at all.
	//
	// An OpsKeeper-shaped one is deliberately absent from this list. It would
	// be caught by the configuration-isolation filter that predates the
	// credential one, so it cannot tell the two rules apart.
	t.Setenv("OPENAI_API_KEY", "sk-decoy-openai-must-not-reach-a-node")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "anthropic-decoy-must-not-reach-a-node")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-decoy-must-not-reach-a-node")

	frontier := testenv.SharedFrontier(t)
	env := testenv.Start(t, testenv.WithFrontier(frontier))
	login := env.LoginAdmin()

	// The gateway has to serve a model the manager can resolve, and the
	// harness's fake is wired in as the openai provider. "fake-gpt" is the
	// same slug the manager's env declares, so the node names a model the
	// cluster actually has rather than one the gateway has to invent.
	// The node names the model the cluster actually has. With the fake that
	// is the slug the manager's env declares; pointed at a real engine it is
	// the engine's model, because the manager resolves the node's request
	// against its provider and refuses a slug that provider does not offer.
	model := "fake-gpt"
	if testenv.UsingRealLLM() {
		model = testenv.RealLLMModel()
		t.Logf("this delivery run uses a REAL inference engine (%s): %s",
			testenv.RealLLMEnv, testenv.RealLLMLimits)
	}
	env.FakeLLM().SetLLMReply("节点 Agent 已通过网关完成一次对话。")

	edgeID, access, secret := env.CreateEdge(t, login.AccessToken, "delivery-node")

	edge := testenv.StartEdge(t, env, login.AccessToken, testenv.EdgeOptions{
		FrontierEdgeAddr: frontier.EdgeAddr,
		AccessKey:        access,
		SecretKey:        secret,
		GatewayBaseURL:   env.BaseURL() + "/v1",
		Model:            model,
	})
	edge.ID = edgeID

	health := edge.WaitForRunningAgent(t, env, login.AccessToken, edgeID, 3*time.Minute)
	t.Logf("node reports agent health: %s", testenv.MustJSON(health))

	// Which architectures this run covered used to be nobody's knowledge.
	//
	// Docker Desktop starts an amd64 image on an arm64 host under emulation,
	// prints one WARNING on stderr, and carries on. So this acceptance could
	// pass with a linux/amd64 broker under emulation and a native arm64 node
	// on the same machine — a mixed leg that satisfies neither half of "run
	// it on amd64 and on arm64" while reading exactly like one that does.
	//
	// The assertion is therefore not "the broker is arm64". It is that the
	// run *knows* and *says* which architecture each side ran on. A harness
	// that cannot name its own leg cannot be used to claim one.
	t.Run("the run declares the architectures it covered", func(t *testing.T) {
		if frontier.Architecture == "" {
			t.Fatalf("the harness did not learn the broker's architecture; this run cannot claim a leg")
		}
		t.Logf("architecture: node/manager %s (host), broker %s (container)",
			runtime.GOARCH, frontier.Architecture)
		if frontier.Architecture == runtime.GOARCH {
			t.Logf("both sides ran natively on the same architecture")
		} else {
			t.Logf("MIXED leg: broker %s under emulation on an %s host — this is not a single-architecture run",
				frontier.Architecture, runtime.GOARCH)
		}
	})

	t.Run("the agent is an independent process", func(t *testing.T) {
		pids := edge.AgentPIDs(t)
		if len(pids) == 0 {
			t.Fatalf("no pig process on the host; the node did not run an independent agent\n=== edge logs ===\n%s", edge.Logs())
		}
		// One node, one agent. A second would mean the supervisor started a
		// replacement without reaping the first, which is a leak that only
		// shows up on a node that has restarted its agent — the case a
		// single-pid check would miss.
		if len(pids) > 1 {
			t.Errorf("node is running %d agent processes (%v), want exactly 1", len(pids), pids)
		}
	})

	t.Run("the node holds no provider credential", func(t *testing.T) {
		// The property is the plan's: a node holds no cloud vendor key, so
		// a compromised node cannot spend the operator's budget. The only
		// secret the node was given is its own tunnel pair, and even that is
		// a *reference* in models.json rather than a value — the node
		// passes it in the process environment and the agent expands it.
		//
		// What this asserts is therefore two-sided: the manager's provider
		// key appears nowhere under the node's agent scope, and the node's
		// own pair appears in no file either. The second half matters
		// because a harness that only checked the first would still pass if
		// someone "fixed" a failure by writing the token into models.json.
		providerKey := "fake-test-key"
		offenders := scanDirFor(t, edge.ConfigDir, providerKey, access, secret)
		if len(offenders) > 0 {
			t.Errorf("node agent scope holds credential material: %s", strings.Join(offenders, ", "))
		}
	})

	t.Run("the node's process environment holds no provider credential", func(t *testing.T) {
		// The plan's wording is "the node's /etc/opskeeper-edge and process
		// environment are audited and found free of cloud vendor keys". The
		// subtest above answers the first half. This one answers the second,
		// which is the half that matters: the node is handed a credential in
		// its environment by design (its own tunnel pair, expanded from a
		// reference), so the environment is exactly where a provider key
		// would be smuggled, and exactly where a directory scan cannot see.
		//
		// Three things are asserted, and the first is what keeps the other
		// two honest: the decoys really are in this process's environment.
		// If that stopped being true the rest of the subtest would pass
		// vacuously, so it is checked rather than assumed.
		for _, decoy := range []struct{ name, value string }{
			{"OPENAI_API_KEY", "sk-decoy-openai-must-not-reach-a-node"},
			{"ANTHROPIC_AUTH_TOKEN", "anthropic-decoy-must-not-reach-a-node"},
			{"AWS_SECRET_ACCESS_KEY", "aws-decoy-must-not-reach-a-node"},
		} {
			if got := os.Getenv(decoy.name); got != decoy.value {
				t.Fatalf("precondition: %s is %q in the test runner, want the decoy %q; "+
					"without it this subtest cannot fail", decoy.name, got, decoy.value)
			}
		}

		env := edge.Environ()
		for _, needle := range []string{
			"sk-decoy-openai-must-not-reach-a-node",
			"anthropic-decoy-must-not-reach-a-node",
			"aws-decoy-must-not-reach-a-node",
			"fake-test-key", // the provider key the manager holds
		} {
			if offender := envHolding(env, needle); offender != "" {
				t.Errorf("the node's environment carries %q as %s; a node that holds a provider "+
					"credential can spend the operator's budget against their account", needle, offender)
			}
		}

		// The one credential the node is supposed to have is still there.
		// A scrub that took the tunnel pair with it would make every later
		// assertion in this test meaningless, because the node would not be
		// a node any more.
		if envHolding(env, access+":"+secret) == "" {
			t.Error("the node's own tunnel pair is missing from its environment; the harness " +
				"scrubbed a credential the node is supposed to carry")
		}

		// Stronger, where the kernel will show it: the running process's own
		// environment, for the node and for the agent it supervises. Skipped
		// rather than faked on an OS that will not answer — see LiveEnviron.
		checked := 0
		for _, target := range append([]int{edge.PID()}, edge.AgentPIDs(t)...) {
			if target == 0 {
				continue
			}
			live, ok := testenv.LiveEnviron(target)
			if !ok {
				t.Logf("pid %d: this OS will not show another process's environment; "+
					"the check above stands on what the harness constructed", target)
				continue
			}
			checked++
			for _, needle := range []string{
				"sk-decoy-openai-must-not-reach-a-node",
				"anthropic-decoy-must-not-reach-a-node",
				"aws-decoy-must-not-reach-a-node",
				"fake-test-key",
			} {
				if offender := envHolding(live, needle); offender != "" {
					t.Errorf("the live process %d holds %q as %s", target, needle, offender)
				}
			}
		}
		if checked == 0 {
			t.Log("no live process environment was readable on this platform; the constructed " +
				"environment above is the evidence here")
		} else {
			t.Logf("read the live environment of %d process(es)", checked)
		}
	})

	sid := openConversation(t, env, edge, login.AccessToken, edgeID)
	frames, stopStream := env.StreamConversation(t, login.AccessToken, sid)
	defer stopStream()

	status, body, err := env.DoJSON("POST",
		fmt.Sprintf("/api/v1/node-agents/sessions/%s/messages", sid),
		map[string]any{"content": "介绍一下你自己"}, login.AccessToken)
	if err != nil {
		t.Fatalf("send turn: transport: %v", err)
	}
	if status != 202 {
		t.Fatalf("send turn: status=%d body=%s", status, testenv.MustJSON(body))
	}

	seen := collectUntilDone(t, frames, env, edge, login.AccessToken, turnTimeout)

	t.Run("the turn streams back on the console's frame contract", func(t *testing.T) {
		var text strings.Builder
		var order []string
		for _, frame := range seen {
			kind, _ := frame["type"].(string)
			order = append(order, kind)
			if kind != "assistant_delta" {
				continue
			}
			assistant, _ := frame["assistant"].(map[string]any)
			if assistant == nil {
				continue
			}
			if chunk, ok := assistant["content"].(string); ok {
				text.WriteString(chunk)
			}
		}
		if len(order) == 0 {
			t.Fatalf("no frames at all\n=== edge logs ===\n%s\n=== manager logs ===\n%s", edge.Logs(), env.ManagerLogs())
		}
		if order[0] != "assistant_start" {
			t.Errorf("first frame is %q, want assistant_start (a console that renders a delta with no start has nothing to attach it to)", order[0])
		}
		if order[len(order)-1] != "done" {
			t.Errorf("last frame is %q, want done\nframes: %v", order[len(order)-1], order)
		}
		if !containsFrame(seen, "assistant_delta") {
			t.Errorf("no assistant_delta frame; the reply did not stream\nframes: %v", order)
		}
		// Two modes, two claims. In fake mode the reply is a known string
		// and matching it proves the frames carried the model's text rather
		// than something the harness assembled. With a real engine there is
		// no such string to match, and the claim becomes the weaker but
		// still meaningful one: the frames carry generated text at all --
		// which a gateway that settles an empty reply also satisfies with a
		// perfectly well-formed stream, so the emptiness check is the point.
		if got := text.String(); testenv.UsingRealLLM() {
			if strings.TrimSpace(got) == "" {
				t.Errorf("a real model answered but no text arrived\nframes: %v", order)
			}
		} else if !strings.Contains(got, "网关") {
			t.Errorf("streamed text is %q, want the model reply the fake served", got)
		}
		for _, frame := range seen {
			if kind, _ := frame["type"].(string); kind == "error" {
				t.Errorf("the turn produced an error frame: %s", testenv.MustJSON(frame))
			}
		}
	})

	t.Run("the reply came through the manager's gateway", func(t *testing.T) {
		// The node never held a provider key, so the only way a reply can
		// exist is if the node's agent called the manager with the node's
		// own credential and the manager served it. The gateway logs that
		// fact per request, tagged with the node it authenticated, which is
		// stronger evidence than "an answer appeared": it names the edge.
		logs := env.ManagerLogs()
		if !strings.Contains(logs, "llmgw: stream served") {
			t.Fatalf("the gateway served no stream\n=== manager logs ===\n%s", logs)
		}
		// Both spellings, because the manager's handler is JSON in this
		// configuration and logfmt in the other one, and an assertion
		// written against the wrong one is a test that fails on a working
		// gateway — which is how it read when the reply was still missing.
		named := strings.Contains(logs, fmt.Sprintf("edge_id=%d", edgeID)) ||
			strings.Contains(logs, fmt.Sprintf(`"edge_id":%d`, edgeID))
		if !named {
			t.Errorf("the gateway served a stream but never named node %d; the node's identity and its model traffic are not the same fact\n=== manager logs ===\n%s", edgeID, logs)
		}
		// Counting calls on the fake only proves something when the fake is
		// the upstream. Against a real engine the equivalent evidence is the
		// gateway line above, which names the edge that was served.
		if !testenv.UsingRealLLM() && env.FakeLLM().CallCount() == 0 {
			t.Errorf("the model was never called; the reply did not come from a model")
		}
	})

	t.Run("a turn with no watcher is refused", func(t *testing.T) {
		// The console's contract, and the reason StreamConversation exists:
		// a turn is answered to a stream somebody is reading. This is the
		// negative of the path above, and without it a harness could pass
		// by sending turns into the void and reading the frames from
		// somewhere else.
		lonely := openConversation(t, env, edge, login.AccessToken, edgeID)
		status, body, err := env.DoJSON("POST",
			fmt.Sprintf("/api/v1/node-agents/sessions/%s/messages", lonely),
			map[string]any{"content": "没有人看我"}, login.AccessToken)
		if err != nil {
			t.Fatalf("send unwatched turn: transport: %v", err)
		}
		if status != 409 {
			t.Errorf("an unwatched turn returned %d, want 409 not_streaming (body=%s)", status, testenv.MustJSON(body))
		}
	})
	assertNoRefusedModelCalls(t, env)
}

// openConversation opens a node conversation and returns its id.
func openConversation(t *testing.T, env *testenv.Env, edge *testenv.Edge, bearer string, edgeID uint64) string {
	t.Helper()
	status, body, err := env.DoJSON("POST", "/api/v1/node-agents/sessions", map[string]any{
		"edge_id": edgeID,
	}, bearer)
	if err != nil {
		t.Fatalf("open conversation: transport: %v\n=== edge logs ===\n%s", err, edge.Logs())
	}
	if status != 200 {
		t.Fatalf("open conversation: status=%d body=%s\n=== manager logs ===\n%s",
			status, testenv.MustJSON(body), env.ManagerLogs())
	}
	sid, _ := body["session_id"].(string)
	if sid == "" {
		t.Fatalf("open conversation: no session id: %s", testenv.MustJSON(body))
	}
	return sid
}

// turnTimeout bounds one turn.
//
// Sixty seconds is generous for a loopback fake and short enough that a
// hang is a failure message rather than a wait. It is not a performance
// budget: the model here is a fixture, so a turn that has not finished in a
// minute is not a slow turn, it is a turn that went somewhere nobody is
// watching.
const turnTimeout = 60 * time.Second

// collectUntilDone reads frames until the turn ends.
//
// A failure here prints the node's own audit ledger and the control plane's
// view of its open conversations, because "no frames" has at least three
// very different causes — the agent never ran, it ran and its frames were
// dropped in transit, or the frames arrived for a conversation the control
// plane could not attribute — and they need different fixes. The frame list
// alone cannot tell them apart.
func collectUntilDone(t *testing.T, frames <-chan map[string]any, env *testenv.Env, edge *testenv.Edge, bearer string, timeout time.Duration) []map[string]any {
	t.Helper()
	var out []map[string]any
	deadline := time.After(timeout)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatalf("the stream closed before the turn ended\nframes: %s", testenv.MustJSON(out))
			}
			out = append(out, frame)
			if kind, _ := frame["type"].(string); kind == "done" {
				return out
			}
		case <-deadline:
			t.Fatalf("the turn did not end within %s\nframes: %s\nmodel calls: %d %v\nconversations: %s\nnode ledger: %s\n=== edge logs ===\n%s\n=== manager logs ===\n%s",
				timeout, testenv.MustJSON(out),
				env.FakeLLM().CallCount(), env.FakeLLM().ModelsRequested(),
				testenv.MustJSON(env.NodeConversations(t, bearer)),
				testenv.ReadFileOrEmpty(t, filepath.Join(edge.WorkDir, "audit-ledger.jsonl")),
				edge.Logs(), env.ManagerLogs())
		}
	}
}

func containsFrame(frames []map[string]any, kind string) bool {
	for _, frame := range frames {
		if k, _ := frame["type"].(string); k == kind {
			return true
		}
	}
	return false
}

// envHolding returns the name of the variable whose value contains needle,
// or "" when none does.
//
// It reports the name rather than the value: a failure message that printed
// the value would put a credential into the test log, which is the one place
// this repository must never accumulate one.
func envHolding(env []string, needle string) string {
	if needle == "" {
		return ""
	}
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok && strings.Contains(value, needle) {
			return name
		}
	}
	return ""
}

// scanDirFor returns the files under root that contain any needle.
//
// It reads bytes rather than treating every file as text: the files that
// matter most to this assertion are the ones a credential would be smuggled
// into, and a "could not parse as text, so it is fine" rule is exactly how
// a key ends up in a file nobody reads.
func scanDirFor(t *testing.T, root string, needles ...string) []string {
	t.Helper()
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, needle := range needles {
			if needle == "" {
				continue
			}
			if strings.Contains(string(body), needle) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, fmt.Sprintf("%s contains %q", rel, needle))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	return offenders
}
