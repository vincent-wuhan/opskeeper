package main

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/edge/toolbroker"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The end-to-end proof that the observability package is what it claims.
//
// toolpath_test.go covers the node-local reads and repairpath_test.go the
// gated writes. This file covers the third shape: reads that are nobody's
// local business and are therefore entirely the control plane's.
//
// The property worth stating out loud, because nothing else about this
// package would catch it getting wrong: every tool in it is read class, and
// read class is exactly the condition under which agentToolInvoker
// dispatches locally. The routing rule from tools.go is correct — a local
// executor is used when the node has one — so the safety of this package
// rests entirely on none of these twelve names having a local executor. A
// future tool that is read class AND registered on the node would be run
// here, against the node, and would answer a fleet-wide question with a
// single machine's worth of evidence. The test below asserts the set is
// empty, and it is written as a walk over the manifest so a new tool is
// covered by being added rather than by being remembered.

// observabilityProfileRoot locates the shipped package from this test's
// own directory, so the check does not depend on the working directory.
func observabilityProfileRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source file")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	return filepath.Join(root, "plugins", "pig-ops", "opskeeper-sre-observability")
}

// observabilityManifest loads the shipped observability package.
func observabilityManifest(t *testing.T) domain.PluginManifest {
	t.Helper()
	p, err := pluginmanifest.Load(observabilityProfileRoot(t))
	if err != nil {
		t.Fatalf("load the observability profile: %v", err)
	}
	return p.Manifest
}

// observabilityBroker stands up a broker over the real observability
// manifest alongside the other two a production node carries, because the
// allow-list a node runs is their union and the questions that matter here
// — does installing this widen anything, does it break anything — are only
// visible against the whole set.
func observabilityBroker(t *testing.T, tun *fakeTunnel, receipts ReceiptClaimer, actor string) string {
	t.Helper()
	readOnly, err := pluginmanifest.Load(realProfileRoot(t))
	if err != nil {
		t.Fatalf("load the read-only profile: %v", err)
	}
	repair, err := pluginmanifest.Load(repairProfileRoot(t))
	if err != nil {
		t.Fatalf("load the repair profile: %v", err)
	}
	registry, err := policygate.RegistryFromManifests([]domain.PluginManifest{
		readOnly.Manifest, observabilityManifest(t), repair.Manifest,
	})
	if err != nil {
		t.Fatalf("the three shipped packages cannot form one allow-list: %v", err)
	}
	broker, err := toolbroker.Listen(toolbroker.Options{
		Authorize: toolAuthorizer(registry, receipts, nil),
		Invoke:    &agentToolInvoker{client: tun},
		Actor:     func(string) string { return actor },
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	return broker.Path()
}

// hasLocalExecutor reports whether the node's own skill registry can run
// this name.
//
// It asks the registry rather than inspecting the manifest, because the
// registry is what agentToolInvoker asks. The two are the same question
// only while they are both asked; a test that inferred "no local executor"
// from the absence of a host_ prefix would be asserting a naming
// convention, and the convention is exactly the thing that has been wrong
// before: host_restart_service is host_-prefixed and is not something a
// node may run by itself.
func hasLocalExecutor(name string) bool {
	exec, ok := skill.Get(name)
	return ok && runsOnThisNode(exec)
}

// TestNoToolInTheObservabilityPackageHasALocalExecutor is the test that
// this file exists for.
//
// The dispatcher runs a read locally whenever the node has an executor for
// it. That is right for host_probe_tcp and wrong for query_promql, and the
// two are told apart only by whether a local executor happens to exist. So
// the invariant is not "these tools are control-plane tools" — it is "none
// of these names is a key in the node's skill registry", and that is a
// fact about the node which has to be checked against the manifest rather
// than assumed in a comment.
func TestNoToolInTheObservabilityPackageHasALocalExecutor(t *testing.T) {
	checked := 0
	for _, tool := range observabilityManifest(t).Spec.Tools {
		checked++
		if hasLocalExecutor(tool.Name) {
			t.Errorf("%q is declared in the observability package AND registered on this node; "+
				"a read-class tool with a local executor is dispatched here and would answer a "+
				"fleet-wide question with one machine's evidence", tool.Name)
		}
	}
	if checked == 0 {
		t.Fatal("the manifest declares no tools, so this assertion is vacuous")
	}
}

// TestAnObservabilityQueryIsAnsweredByTheControlPlane runs the whole path
// for a real query, which is the only way to know the upcall is wired and
// not merely present.
func TestAnObservabilityQueryIsAnsweredByTheControlPlane(t *testing.T) {
	tun := &fakeTunnel{reply: tunnel.AgentToolResponse{
		Result: json.RawMessage(`{"result_type":"matrix","series":[{"metric":{"__name__":"up"},"values":[[1,"1"]]}]}`),
	}}
	path := observabilityBroker(t, tun, &fakeReceipts{}, RoleAdmin)

	reply := ask(t, path, "sess-obs", "query_promql", map[string]any{
		"query": "up",
	})
	if !reply.OK() {
		t.Fatalf("a declared observability query failed through the whole path: %s", reply.Error)
	}
	if tun.method != tunnel.MethodAgentTool {
		t.Errorf("the call was answered by %q, want it to go up as %q",
			tun.method, tunnel.MethodAgentTool)
	}
	if tun.calls != 1 {
		t.Errorf("the control plane saw %d calls, want 1", tun.calls)
	}

	// And the model's arguments arrived unaltered. A dispatcher that
	// re-encoded the query would make a PromQL expression into a
	// different one, which the metric store would answer cheerfully.
	var sent tunnel.AgentToolRequest
	if err := json.Unmarshal(tun.body, &sent); err != nil {
		t.Fatalf("the tunnel request did not decode: %v", err)
	}
	if sent.Tool != "query_promql" {
		t.Errorf("tool = %q, want query_promql", sent.Tool)
	}
	if sent.SessionID != "sess-obs" {
		t.Errorf("session = %q, want the conversation the host attributes the query to", sent.SessionID)
	}
	var args map[string]any
	if err := json.Unmarshal(sent.Arguments, &args); err != nil {
		t.Fatalf("the tunnel request's arguments did not decode: %v", err)
	}
	if args["query"] != "up" {
		t.Errorf("arguments = %v, want the model's query carried through unaltered", args)
	}

	var out map[string]any
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		t.Fatalf("the control plane's result did not survive the round trip: %v", err)
	}
	if out["result_type"] != "matrix" {
		t.Errorf("result_type = %v, want the manager's own answer", out["result_type"])
	}
}

// TestAnObservabilityReadNeverNeedsAnApproval is the L1 promise, checked
// against a grant store that is empty by construction.
//
// A package that put reads into the approval queue would be a package whose
// operators learn to click through, and the next L2 package inherits that
// habit. The check is deliberately at the whole-path level: the manifest
// saying required: false is a claim, and the broker is where claims become
// behaviour.
func TestAnObservabilityReadNeverNeedsAnApproval(t *testing.T) {
	for _, tool := range []string{"query_promql", "query_logql", "query_traceql", "analyze_database_status"} {
		t.Run(tool, func(t *testing.T) {
			tun := &fakeTunnel{reply: tunnel.AgentToolResponse{Result: json.RawMessage(`{"series":[]}`)}}
			// Nothing has been approved and nothing needs to be.
			path := observabilityBroker(t, tun, &fakeReceipts{}, RoleAdmin)

			reply := ask(t, path, "sess-obs", tool, map[string]any{"query": "{}"})
			if !reply.OK() {
				t.Fatalf("a read in the L1 observability package needed an approval: %s", reply.Error)
			}
			if tun.calls != 1 {
				t.Errorf("the control plane saw %d calls, want 1", tun.calls)
			}
		})
	}
}

// TestTheObservabilityPackageAddsNoToolOutsideItsManifest is the union
// check: installing this package must widen the allow-list by exactly the
// twelve names it declares and nothing else.
func TestTheObservabilityPackageAddsNoToolOutsideItsManifest(t *testing.T) {
	tun := &fakeTunnel{}
	path := observabilityBroker(t, tun, &fakeReceipts{}, RoleAdmin)

	// host_bash is the one name that must never appear in any manifest.
	// It is a shell. It is registered on the node as a skill, so a
	// dispatcher that resolved by registry rather than by allow-list would
	// find it — which is precisely why the broker checks the manifest
	// first and this test exists to prove the check is still there.
	reply := ask(t, path, "sess-obs", "host_bash", map[string]any{"command": "id"})
	if reply.OK() {
		t.Fatal("a shell ran on this node through the agent tool path")
	}
	if tun.calls != 0 {
		t.Errorf("the undeclared tool still reached the control plane %d times", tun.calls)
	}
}

// TestTheObservabilityPackageDoesNotDisturbTheOtherTwo is the regression
// guard for a shared broker.
//
// A third manifest joins the union that the other two tests build from. If
// one of the twelve names collided with a name either of the others owns,
// policygate would resolve the class from whichever manifest it saw — and
// the two packages make opposite promises about approval, so a collision is
// not a merge but a choice somebody did not make deliberately.
func TestTheObservabilityPackageDoesNotDisturbTheOtherTwo(t *testing.T) {
	observability := observabilityManifest(t).Spec.Tools.Names()
	own := map[string]bool{}
	for _, n := range observability {
		own[n] = true
	}

	readOnly, err := pluginmanifest.Load(realProfileRoot(t))
	if err != nil {
		t.Fatalf("load the read-only profile: %v", err)
	}
	repair, err := pluginmanifest.Load(repairProfileRoot(t))
	if err != nil {
		t.Fatalf("load the repair profile: %v", err)
	}
	for _, other := range []struct {
		who   string
		names []string
	}{
		{readOnly.Name(), readOnly.Manifest.Spec.Tools.Names()},
		{repair.Name(), repair.Manifest.Spec.Tools.Names()},
	} {
		for _, n := range other.names {
			if own[n] {
				t.Errorf("%q is declared by both the observability package and %s; "+
					"two packages that promise different things about the same tool name cannot both be honoured",
					n, other.who)
			}
		}
	}
}

// TestAnObservabilityToolOnANodeWithNoTunnelSaysSo is the degraded case.
//
// Every tool in this package is the control plane's, so a node whose
// tunnel is down cannot answer any of them. The failure has to be legible:
// an agent that says "connection refused" reads to the model like a broken
// tool, and a model that believes its tools are broken stops using the
// whole package rather than reporting that the control plane is
// unreachable. A viewer is a separate case, and it is the more important
// one — a read-only console session must not be handed the fleet's
// metric and log history by default.
func TestAnObservabilityToolOnANodeWithNoTunnelSaysSo(t *testing.T) {
	observability := observabilityManifest(t)
	readOnly, err := pluginmanifest.Load(realProfileRoot(t))
	if err != nil {
		t.Fatalf("load the read-only profile: %v", err)
	}
	registry, err := policygate.RegistryFromManifests([]domain.PluginManifest{
		readOnly.Manifest, observability,
	})
	if err != nil {
		t.Fatalf("RegistryFromManifests: %v", err)
	}
	// No tunnel at all.
	broker, err := toolbroker.Listen(toolbroker.Options{
		Authorize: toolAuthorizer(registry, &fakeReceipts{}, nil),
		Invoke:    &agentToolInvoker{},
		Actor:     func(string) string { return RoleAdmin },
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })

	reply := ask(t, broker.Path(), "sess-obs", "query_promql", map[string]any{"query": "up"})
	if reply.OK() {
		t.Fatal("a fleet query reported success on a node that cannot reach the control plane")
	}
	for _, want := range []string{"query_promql", "control plane"} {
		if !strings.Contains(reply.Error, want) {
			t.Errorf("error = %q, want it to mention %q", reply.Error, want)
		}
	}
}
