//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/tests/e2e/testenv"
)

// TestAPluginToolCrossesTheGateAndRunsOnTheNodeHost is the path the delivery
// acceptance has never walked, and it is the one that matters most: the whole
// plugin architecture is a promise that a tool declared in a package manifest
// becomes a tool the model can call, that the call is judged by the host, and
// that the answer comes back as the host's own execution of it.
//
// Everything asserted here is a separate process boundary, so every step can
// fail alone:
//
//	pig (agent, builds the package's real Go extension)
//	  -> gate socket          (may this run? the allow-list built from the
//	                           admitted manifests, and the approval queue)
//	  -> tool-broker socket   (run it: the host re-checks the same list
//	                           against its own registry, then dispatches)
//	  -> builtin executor     (reads the file)
//	  -> back up both sockets, into the model as a tool-role message
//
// The package is the SHIPPED one, copied verbatim: opskeeper-sre-readonly
// with its two extensions. testenv's other fixture declares tools in a
// manifest with no extension behind it, which proves the node admitted a
// declaration and nothing more -- a manifest is a promise, and only an
// extension keeps it.
//
// host_tail_file is the tool chosen for one reason: it is the shipped
// read-only tool whose answer is deterministic on any host. A test that
// asserted on dmesg or a systemd journal would assert on the machine it ran
// on, and would pass for the wrong reason or fail for the wrong one. This one
// reads a file the test wrote, so the sentinel string in the result can only
// have come from the host opening that path.
func TestAPluginToolCrossesTheGateAndRunsOnTheNodeHost(t *testing.T) {
	frontier := testenv.SharedFrontier(t)
	env := testenv.Start(t, testenv.WithFrontier(frontier))
	login := env.LoginAdmin()

	// The sentinel is written before the node starts and read back after a
	// turn, so its presence in the model's input can only be explained by
	// the node host having opened the file.
	evidence := filepath.Join(t.TempDir(), "gate-evidence.log")
	const sentinel = "opskeeper-gate-crossed-7f3a91c2"
	if err := os.WriteFile(evidence, []byte("boot ok\n"+sentinel+"\n"), 0o640); err != nil {
		t.Fatalf("write the evidence file: %v", err)
	}

	env.FakeLLM().SetToolScript(testenv.LLMToolCall{
		ID:        "call_tail_1",
		Name:      "host_tail_file",
		Arguments: fmt.Sprintf(`{"path":%q,"lines":5}`, evidence),
	})
	env.FakeLLM().SetLLMReply("日志尾部已确认，网关放行成功。")

	edgeID, access, secret := env.CreateEdge(t, login.AccessToken, "plugin-tool-node")
	edge := testenv.StartEdge(t, env, login.AccessToken, testenv.EdgeOptions{
		FrontierEdgeAddr: frontier.EdgeAddr,
		AccessKey:        access,
		SecretKey:        secret,
		GatewayBaseURL:   env.BaseURL() + "/v1",
		Model:            "fake-gpt",
		// The shipped package, not the declaration-only fixture.
		ShippedPackageTools: []string{"host_tail_file"},
	})
	edge.ID = edgeID
	// The generous window is not politeness: the node has to build two Go
	// extensions from source before its agent can host them, and on a cold
	// build cache that is tens of seconds. A timeout here would report as
	// "the agent did not come up", which is indistinguishable from the bug
	// this test exists to catch.
	edge.WaitForRunningAgent(t, env, login.AccessToken, edgeID, 8*time.Minute)

	sid := openConversation(t, env, edge, login.AccessToken, edgeID)
	frames, stopStream := env.StreamConversation(t, login.AccessToken, sid)
	defer stopStream()

	if _, _, err := env.DoJSON("POST",
		fmt.Sprintf("/api/v1/node-agents/sessions/%s/messages", sid),
		map[string]any{"content": "看一下这台机器上的 gate 证据日志尾部"}, login.AccessToken); err != nil {
		t.Fatalf("send: transport: %v", err)
	}
	for range frames {
	}

	// Step one: the package's extension was built and its tools registered.
	// If the extension had failed to build, or the manifest had been
	// refused, the agent would be running with no plugin tool at all and
	// this is where that shows up rather than as a mysterious refusal
	// three steps later.
	advertised := env.FakeLLM().ToolsAdvertised()
	offered := false
	for _, names := range advertised {
		for _, name := range names {
			if name == "host_tail_file" {
				offered = true
			}
		}
	}
	if !offered {
		t.Fatalf("the node's agent never offered host_tail_file, so the package's extension "+
			"did not load; nothing below could have worked\nadvertised: %v\nnode logs:\n%s",
			advertised, edge.Logs())
	}
	t.Logf("host_tail_file was offered by the agent (advertised sets: %d)", len(advertised))

	// Step two: the host ADJUDICATED it. The gate's verdict is written to
	// the node's audit ledger by the host, from the host's own gate event --
	// the agent cannot write that line, so its presence is evidence that
	// the call crossed the socket and was decided there rather than
	// somewhere inside the agent process.
	ledger := testenv.ReadFileOrEmpty(t, filepath.Join(edge.WorkDir, "audit-ledger.jsonl"))
	if !strings.Contains(ledger, `"action":"tool_call"`) || !strings.Contains(ledger, `"outcome":"allowed"`) {
		t.Fatalf("the node's audit ledger records no allowed tool_call; the gate either refused "+
			"the declared read tool or the call never reached it\nledger: %s\nnode logs:\n%s",
			ledger, edge.Logs())
	}
	if !strings.Contains(ledger, "gate-evidence.log") {
		t.Fatalf("the allowed call carries no target; the model asked for the evidence file "+
			"and the ledger does not name it\nledger: %s", ledger)
	}
	t.Log("the host adjudicated the call and allowed it, with the file as the target")
	// The node's own log of the two legs after the gate, printed on every
	// run rather than only on failure: this is the line that says whether
	// the broker dispatched the call at all, and the difference between
	// "the broker never saw it" and "the broker ran it and the answer was
	// empty" is the whole of what is left to find.
	logs := edge.Logs()
	if len(logs) > 4000 {
		logs = logs[len(logs)-4000:]
	}
	t.Logf("node log tail:\n%s", logs)

	// The gate leg is proven above and the broker leg is proven by the
	// sentinel below. There was a third leg between them that looked like
	// both: the call was adjudicated and executed, and the agent still
	// rendered "(no tool output)". The extensions answered in their own
	// JSON rather than in the shape the runtime reads a tool result in,
	// and it decodes that shape by ignoring every field it does not know.
	// See ledger section 4.382 (decision 448).

	// Step two: the host ran it, and its output came back. Not "the agent
	// tried something" -- the string in the result is the one this test
	// wrote into a file only the node host could open.
	results := env.FakeLLM().ToolResults()
	if len(results) == 0 {
		t.Fatalf("the model asked for host_tail_file and no tool-role message came back\n%s",
			edge.Logs())
	}
	joined := strings.Join(results, "\n")
	if !strings.Contains(joined, sentinel) {
		t.Fatalf("host_tail_file was called but its answer never carried the sentinel; the "+
			"round trip stopped somewhere between the gate, the broker and the executor\nresults: %v\n"+
			"audit ledger:\n%s\nnode logs:\n%s",
			results, testenv.ReadFileOrEmpty(t, filepath.Join(edge.WorkDir, "audit-ledger.jsonl")), edge.Logs())
	}
	t.Logf("the host executed the tool and the sentinel came back through the gate: %d result(s)", len(results))

	// A refusal is not a pass. The gate must have allowed this call, and
	// the way to tell an allow from a host error is that the host's own
	// answer is here -- but a refusal would also put text in this message,
	// so the sentinel assertion above is the one that decides it.
	if strings.Contains(joined, "refused") || strings.Contains(joined, "denied") {
		t.Fatalf("the gate refused the declared read tool; the manifest declared it as read "+
			"and read tools do not enter the approval queue\nresults: %v", results)
	}

	assertNoRefusedModelCalls(t, env)
}
