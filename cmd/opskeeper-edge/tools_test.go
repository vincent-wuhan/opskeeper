package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/edge/toolbroker"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	// The executors have to be registered for skill.Get to find them.
	_ "github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
)

// fakeTunnel records what a node asked the control plane for.
type fakeTunnel struct {
	// method and body are the last call made.
	method string
	body   []byte
	calls  int
	// reply is written into resp when set.
	reply tunnel.AgentToolResponse
	// err fails the call.
	err error
}

func (f *fakeTunnel) Dial(context.Context) error             { return nil }
func (f *fakeTunnel) RegisterHandler(string, tunnel.Handler) {}
func (f *fakeTunnel) OnReconnect(func())                     {}
func (f *fakeTunnel) AcceptStream() (tunnel.StreamConn, error) {
	return nil, errors.New("not used")
}
func (f *fakeTunnel) Close() error { return nil }

func (f *fakeTunnel) Call(_ context.Context, method string, req, resp any) error {
	f.method = method
	f.calls++
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	f.body = body
	if f.err != nil {
		return f.err
	}
	out, err := json.Marshal(f.reply)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, resp)
}

// localSkill is a registered skill standing in for the node's own probes.
// It exists so the routing assertions do not depend on whether dmesg
// happens to work on the machine running the tests.
type localSkill struct{ got chan json.RawMessage }

func (e *localSkill) Metadata() skill.Metadata {
	return skill.Metadata{Key: localSkillKey, Name: "local", Description: "test", Class: skill.ClassSafe}
}

// Execute records what it was handed and hands the same bytes back, so a
// test can check both directions of the hop against one value.
func (e *localSkill) Execute(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
	select {
	case e.got <- params:
	default:
	}
	return params, nil
}

// drainLocal empties the recording channel. The probe is shared by every
// test in this file, so a test that did not drain it would read the
// previous test's call and pass for the wrong reason.
func drainLocal() {
	for {
		select {
		case <-localProbe.got:
		default:
			return
		}
	}
}

// localSkillKey is the registry key the local-path tests dispatch to.
const localSkillKey = "test_local_probe"

var localProbe = &localSkill{got: make(chan json.RawMessage, 4)}

func init() {
	// Registered once, for this test binary. The registry rejects a
	// duplicate key loudly, which is right for production and means a
	// second registration here would be a bug worth failing on.
	skill.Register(localProbe)
}

func TestAToolTheNodeHoldsRunsLocallyAndNeverReachesTheTunnel(t *testing.T) {
	drainLocal()
	// The node can read its own kernel buffer. Spending a tunnel round
	// trip to do so would make every local observation depend on the
	// control plane being up, which is the opposite of what a node is for.
	tun := &fakeTunnel{}
	inv := &agentToolInvoker{client: tun}

	out, err := inv.Invoke(context.Background(), toolbroker.Call{
		SessionID: "s1",
		ToolName:  localSkillKey,
		Arguments: json.RawMessage(`{"levels":"err"}`),
	})
	if err != nil {
		t.Fatalf("a node-local tool failed: %v", err)
	}
	if len(out) == 0 {
		t.Error("a node-local tool returned nothing")
	}
	if tun.calls != 0 {
		t.Errorf("the tunnel was called %d times for a node-local tool", tun.calls)
	}
}

func TestTheLocalExecutorSeesTheHostsOwnReEncodedArguments(t *testing.T) {
	// The broker re-encodes what the model proposed so the host parses
	// what it validated. Passing the agent's bytes straight through would
	// put an unchecked payload in front of every executor on the node.
	drainLocal()
	inv := &agentToolInvoker{client: &fakeTunnel{}}
	out, err := inv.Invoke(context.Background(), toolbroker.Call{
		SessionID: "s1",
		ToolName:  localSkillKey,
		Arguments: json.RawMessage(`{"levels":"err","max_lines":5}`),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	select {
	case got := <-localProbe.got:
		var decoded map[string]any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("the executor got %s, which does not decode: %v", got, err)
		}
		if decoded["levels"] != "err" {
			t.Errorf("levels = %v, want the value the model proposed", decoded["levels"])
		}
		if decoded["max_lines"] != float64(5) {
			t.Errorf("max_lines = %v, want 5", decoded["max_lines"])
		}
	default:
		t.Fatal("the local executor was never reached")
	}

	// And the result is passed back to the broker verbatim.
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("the broker got %s, which does not decode: %v", out, err)
	}
	if back["levels"] != "err" {
		t.Errorf("the broker received %v, want the executor's own bytes", back)
	}
}

func TestAFailingLocalSkillIsReportedWithItsCause(t *testing.T) {
	// A container that cannot read the kernel ring buffer is a normal
	// answer, not an extension fault. The investigator persona has to be
	// able to read it, so the cause travels with the name of the tool.
	exec, ok := skill.Get("host_dmesg")
	if !ok {
		t.Skip("host_dmesg is not registered in this build")
	}
	inv := &agentToolInvoker{client: &fakeTunnel{}}

	_, err := inv.Invoke(context.Background(), toolbroker.Call{
		SessionID: "s1", ToolName: exec.Metadata().Key, Arguments: json.RawMessage(`{"levels":"err"}`),
	})
	if err == nil {
		t.Skip("this environment can read the kernel buffer; the failure path is not exercised")
	}
	if !strings.Contains(err.Error(), "host_dmesg") {
		t.Errorf("error = %q, want it to name the tool that failed", err)
	}
}

func TestAControlPlaneToolIsAskedForOverTheTunnel(t *testing.T) {
	tun := &fakeTunnel{reply: tunnel.AgentToolResponse{
		Result: json.RawMessage(`{"edge_count":42,"online":40}`),
	}}
	inv := &agentToolInvoker{client: tun}

	out, err := inv.Invoke(context.Background(), toolbroker.Call{
		SessionID: "sess-9",
		ToolName:  "get_topology",
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(out) != `{"edge_count":42,"online":40}` {
		t.Errorf("result = %s, want the control plane's bytes unchanged", out)
	}
	if tun.method != tunnel.MethodAgentTool {
		t.Errorf("method = %q, want %q", tun.method, tunnel.MethodAgentTool)
	}

	var sent tunnel.AgentToolRequest
	if err := json.Unmarshal(tun.body, &sent); err != nil {
		t.Fatalf("the upcall body is not the wire shape: %v", err)
	}
	if sent.Tool != "get_topology" {
		t.Errorf("tool = %q, want get_topology", sent.Tool)
	}
	if sent.SessionID != "sess-9" {
		t.Errorf("session = %q, want the session the call came from", sent.SessionID)
	}
}

func TestAControlPlaneRefusalReachesTheModelAsADecision(t *testing.T) {
	// The manager's refusal is not a transport fault. It arrives as an
	// error string so the model reads it as an answer; the node must not
	// flatten it into something that says "try again".
	tun := &fakeTunnel{reply: tunnel.AgentToolResponse{
		Error: "get_topology: not found: tool \"nope\"",
	}}
	inv := &agentToolInvoker{client: tun}

	_, err := inv.Invoke(context.Background(), toolbroker.Call{
		SessionID: "s", ToolName: "get_topology", Arguments: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("a refused upcall reported success")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want the control plane's own reason", err)
	}
}

func TestAnUnreachableControlPlaneIsReportedAsSuchNotAsAToolFailure(t *testing.T) {
	// The two are different incidents: one is a broken node, the other is a
	// broken network. An operator reading the transcript has to be able to
	// tell them apart.
	tun := &fakeTunnel{err: errors.New("tunnel closed")}
	inv := &agentToolInvoker{client: tun}

	_, err := inv.Invoke(context.Background(), toolbroker.Call{
		SessionID: "s", ToolName: "get_topology", Arguments: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("an unreachable control plane reported success")
	}
	if !strings.Contains(err.Error(), "could not be reached") {
		t.Errorf("error = %q, want it to name the transport failure", err)
	}
}

func TestAControlPlaneToolOnANodeWithNoTunnelSaysSo(t *testing.T) {
	// Better one legible sentence than a tool that appears to exist and
	// fails with something the model cannot interpret.
	inv := &agentToolInvoker{}

	_, err := inv.Invoke(context.Background(), toolbroker.Call{
		SessionID: "s", ToolName: "get_topology", Arguments: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("a call with no tunnel reported success")
	}
	if !strings.Contains(err.Error(), "no tunnel") {
		t.Errorf("error = %q, want it to say the node has no tunnel", err)
	}
}
