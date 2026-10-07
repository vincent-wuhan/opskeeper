package main

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"

	"bufio"
	"net"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/edge/toolbroker"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// brokerClient is the agent's side of the tool protocol, written out here
// rather than imported. The shipping client lives in the toolset extension,
// which the edge binary does not link — that separation is the point of the
// design, so a test that reached across it would be testing something the
// node never does.
type brokerClient struct {
	path string
	conn net.Conn
	rd   *bufio.Reader
}

func (c *brokerClient) run(req wire.ToolRequest) (wire.ToolReply, error) {
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return wire.ToolReply{}, err
	}
	c.conn = conn
	c.rd = bufio.NewReader(conn)
	body, err := json.Marshal(req)
	if err != nil {
		return wire.ToolReply{}, err
	}
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return wire.ToolReply{}, err
	}
	line, err := c.rd.ReadBytes('\n')
	if err != nil {
		return wire.ToolReply{}, err
	}
	var reply wire.ToolReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return wire.ToolReply{}, err
	}
	return reply, nil
}

func (c *brokerClient) close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// The end-to-end proof that the read-only profile is real.
//
// Every other test in this file exercises one hop. This one starts from
// the shipped pig-ops.yaml and runs a tool the way the agent would: through
// the broker socket, past the authoriser, into an actual executor on this
// node. It is the test that would fail if the manifest and the host had
// drifted apart in a way none of the individual checks noticed.

// realProfileRoot locates the shipped package from this test's own
// directory, so the check does not depend on the working directory.
func realProfileRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source file")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	return filepath.Join(root, "plugins", "pig-ops", "opskeeper-sre-readonly")
}

// brokerFor stands up a real broker over the real profile's allow-list and
// a real invoker, and returns its socket path.
func brokerFor(t *testing.T, tun *fakeTunnel) string {
	t.Helper()
	p, err := pluginmanifest.Load(realProfileRoot(t))
	if err != nil {
		t.Fatalf("load the read-only profile: %v", err)
	}
	registry, err := policygate.RegistryFromManifests([]domain.PluginManifest{p.Manifest})
	if err != nil {
		t.Fatalf("RegistryFromManifests: %v", err)
	}
	broker, err := toolbroker.Listen(toolbroker.Options{
		Authorize: toolAuthorizer(registry, &fakeReceipts{}, nil),
		Invoke:    &agentToolInvoker{client: tun},
		Actor:     func(string) string { return RoleAdmin },
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	return broker.Path()
}

// ask sends one request over the broker socket the way the extension does.
func ask(t *testing.T, path, session, tool string, args map[string]any) wire.ToolReply {
	t.Helper()
	c := &brokerClient{path: path}
	t.Cleanup(c.close)
	reply, err := c.run(wire.ToolRequest{SessionID: session, ToolName: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return reply
}

func TestTheShippedProfileRunsAProbeEndToEndOnThisNode(t *testing.T) {
	path := brokerFor(t, &fakeTunnel{})

	// host_probe_tcp is the one probe whose answer is deterministic enough
	// to assert on: a closed port on loopback is closed on every machine.
	// A tool that ran and reported a refusal is a tool that worked; a tool
	// that could not be reached at all would report something else.
	reply := ask(t, path, "sess-1", "host_probe_tcp", map[string]any{
		"target": "127.0.0.1:1", "timeout_ms": 200,
	})
	if !reply.OK() {
		t.Fatalf("a shipped read-only probe failed through the whole path: %s", reply.Error)
	}
	if len(reply.Result) == 0 {
		t.Error("the probe reported no result")
	}
}

func TestTheShippedProfileRefusesAToolItNeverDeclared(t *testing.T) {
	// The whole value of spec.tools is that a name the manifest does not
	// carry is a name the host does not have. This runs through the real
	// manifest, so a tool quietly added to the profile without being
	// implemented would fail here rather than in an incident.
	path := brokerFor(t, &fakeTunnel{})

	reply := ask(t, path, "sess-1", "host_reboot", nil)
	if reply.OK() {
		t.Fatal("an undeclared tool ran through the broker")
	}
	if reply.Error == "" {
		t.Error("the refusal carried no reason for the model to read")
	}
}

func TestTheShippedProfileRefusesAMutatingToolForAViewer(t *testing.T) {
	// The role ceiling is checked over the same path a real call takes, so
	// this is the assertion that a read-only console session cannot be
	// talked into a restart.
	p, err := pluginmanifest.Load(realProfileRoot(t))
	if err != nil {
		t.Fatalf("load the read-only profile: %v", err)
	}
	registry, err := policygate.RegistryFromManifests([]domain.PluginManifest{p.Manifest})
	if err != nil {
		t.Fatalf("RegistryFromManifests: %v", err)
	}
	broker, err := toolbroker.Listen(toolbroker.Options{
		Authorize: toolAuthorizer(registry, &fakeReceipts{}, nil),
		Invoke:    &agentToolInvoker{client: &fakeTunnel{}},
		Actor:     func(string) string { return RoleViewer },
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })

	c := &brokerClient{path: broker.Path()}
	t.Cleanup(c.close)
	// draft_config_change is not in this package, so this asserts the
	// allow-list rather than the ceiling: a write tool this node does not
	// have is refused before the role is ever consulted. The ceiling
	// itself needs a package that declares a write tool, which is the
	// repair profile — see TestTheRepairProfileDoesNotMakeAViewerDangerous
	// in repairpath_test.go.
	reply, err := c.run(wire.ToolRequest{SessionID: "s", ToolName: "draft_config_change"})
	if err != nil {
		t.Fatalf("draft_config_change: %v", err)
	}
	if reply.OK() {
		t.Fatal("a tool this node does not have ran for a viewer")
	}
}

func TestAControlPlaneToolFromTheShippedProfileGoesUpTheTunnel(t *testing.T) {
	// get_topology is in the profile, has no local executor, and can only
	// be answered by the manager. This proves the upcall is what carries
	// it rather than a silent "unknown tool".
	tun := &fakeTunnel{reply: tunnel.AgentToolResponse{Result: json.RawMessage(`{"manager_version":"2.0.0","edge_count":12}`)}}
	path := brokerFor(t, tun)

	reply := ask(t, path, "sess-7", "get_topology", map[string]any{})
	if !reply.OK() {
		t.Fatalf("a control-plane tool failed through the whole path: %s", reply.Error)
	}
	var out map[string]any
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		t.Fatalf("the manager's result did not survive the round trip: %v", err)
	}
	if out["edge_count"] != float64(12) {
		t.Errorf("edge_count = %v, want the manager's 12", out["edge_count"])
	}
	if tun.method != "agent.tool" {
		t.Errorf("method = %q, want agent.tool", tun.method)
	}
}
