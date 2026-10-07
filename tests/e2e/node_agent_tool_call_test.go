//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/tests/e2e/testenv"
)

// TestTheNodeAgentCallsAToolAndGetsAnAnswerBack is the one path the delivery
// acceptance had never executed.
//
// Every other delivery test is a conversation, and a conversation in which the
// agent does nothing. The node's agent is offered tools, the plan says it
// should use them, and `pig-tool-scoping-check` proves five packages' worth are
// *offered* -- but offered is not called, and until this test existed nothing
// in the repository ever watched a tool go out of a node agent and come back.
// The machinery that decides is the part that has to work on a real node: the
// agent's own tool loop, the wire shape of a streamed tool call, and the
// result being appended to the conversation for the next turn.
//
// The tool it calls is one the node genuinely has. That is deliberate: a test
// that declared a tool in the package manifest and hoped an extension would
// appear behind it would be testing a fixture's imagination -- the manifest is
// a promise, and nothing produces the tool but a real extension. What this
// establishes is the node-agent tool round trip, not plugin delivery: the
// called tool is the agent's own `tool_search`, no extension is loaded, and
// manifest declarations do not by themselves create a tool.
func TestTheNodeAgentCallsAToolAndGetsAnAnswerBack(t *testing.T) {
	frontier := testenv.SharedFrontier(t)
	env := testenv.Start(t, testenv.WithFrontier(frontier))
	login := env.LoginAdmin()

	// Two turns, because that is what a tool-using turn is: the model asks
	// for something, and then answers with what came back. The fake returns
	// to plain text once the script is exhausted, so the turn ends the way a
	// real one does.
	env.FakeLLM().SetToolScript(testenv.LLMToolCall{
		ID:        "call_probe_1",
		Name:      "tool_search",
		Arguments: `{"query":"host"}`,
	})
	env.FakeLLM().SetLLMReply("工具已返回，我把结果汇总给你。")

	edgeID, access, secret := env.CreateEdge(t, login.AccessToken, "tool-call-node")
	edge := testenv.StartEdge(t, env, login.AccessToken, testenv.EdgeOptions{
		FrontierEdgeAddr: frontier.EdgeAddr,
		AccessKey:        access,
		SecretKey:        secret,
		GatewayBaseURL:   env.BaseURL() + "/v1",
		Model:            "fake-gpt",
	})
	edge.ID = edgeID
	edge.WaitForRunningAgent(t, env, login.AccessToken, edgeID, 3*time.Minute)

	sid := openConversation(t, env, edge, login.AccessToken, edgeID)
	frames, stopStream := env.StreamConversation(t, login.AccessToken, sid)
	defer stopStream()

	if _, _, err := env.DoJSON("POST",
		fmt.Sprintf("/api/v1/node-agents/sessions/%s/messages", sid),
		map[string]any{"content": "帮我找一下和主机相关的工具"}, login.AccessToken); err != nil {
		t.Fatalf("send: transport: %v", err)
	}
	for range frames {
	}

	// The node's agent must have been given something to call at all. Without
	// this, "the tool result came back" could be true of a turn where the
	// agent had no tools and the single response was text.
	advertised := env.FakeLLM().ToolsAdvertised()
	if len(advertised) == 0 {
		t.Fatalf("the model was never reached\n%s", env.ManagerLogs())
	}
	t.Logf("the node's agent advertised %d tool(s) on request 0: %v",
		len(advertised[0]), advertised[0])

	// The turn is two model calls, not one: a turn that answered immediately
	// never asked for the tool.
	if calls := env.FakeLLM().CallCount(); calls < 2 {
		t.Fatalf("the model was called %d time(s); a turn that called a tool and read the "+
			"answer costs two\nadvertised: %v\nresults: %v",
			calls, advertised, env.FakeLLM().ToolResults())
	}

	// The claim this file exists for. A message count would not do: "one more
	// message" is equally true of a stray assistant turn, and only the
	// content of a tool-role message says the result came back.
	results := env.FakeLLM().ToolResults()
	if len(results) == 0 {
		t.Fatalf("the agent asked for a tool and no tool-role message came back; the round "+
			"trip the node's tool loop exists for did not happen\nadvertised: %v\n%s",
			advertised, env.ManagerLogs())
	}
	for i, result := range results {
		if result == "" {
			t.Errorf("tool result %d is empty; a tool that ran and returned nothing is "+
				"indistinguishable, to the model, from a tool that never ran", i)
		}
	}
	t.Logf("tool results returned to the model: %d", len(results))

	assertNoRefusedModelCalls(t, env)
}
