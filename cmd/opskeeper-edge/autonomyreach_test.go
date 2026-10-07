package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/autonomy"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/edge/toolbroker"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// autonomy_test.go proves the arbiter does the right thing. This file proves
// a request can *reach* it.
//
// Those are different claims and the difference was worth an entire
// subsystem: the arbiter, the local audit spool, the replay pump, the tunnel
// method and the centre's audit chain were all present, all signed, all
// individually tested — and none of it could ever execute, because the only
// path in required the management tunnel and the arbiter only acts without
// it. Nine tests in autonomy_test.go call builtin.AutonomyRun{}.Execute
// directly, which is precisely the step a model's tool call does not take.
//
// So every test here drives the two gates in front of the arbiter, in order,
// the way the composition root wires them: the approval policy, then the tool
// router. A change that quietly widens either of them turns something here
// red, which is the only reason a rule this security-shaped can be added at
// all.

// darkNode is a node whose centre stopped answering three minutes ago, with
// the disk over the trigger's threshold. It is the only state in which an
// autonomy action may run, so every test that expects one starts here.
func darkNode(t *testing.T) (*fakeObservations, *recordingRunner) {
	t.Helper()
	obs := &fakeObservations{online: false, values: map[string]float64{"node_disk_used_ratio": 0.99}}
	obs.goOffline(time.Now().Add(-3 * time.Minute))
	return obs, &recordingRunner{}
}

func autonomyAuthorizer(t *testing.T, obs autonomyObservations) toolbroker.Authorizer {
	t.Helper()
	reg, err := policygate.RegistryFromManifests([]domain.PluginManifest{autonomyPlugin().Manifest})
	if err != nil {
		t.Fatalf("RegistryFromManifests: %v", err)
	}
	// A gate that never produces a receipt. During an outage there is no
	// centre to ask one, so this is what the node actually has, and a test
	// that passed a generous gate would be testing the generous gate.
	return toolAuthorizer(reg, &fakeReceipts{}, obs)
}

func autonomyCall(t *testing.T) toolbroker.Call {
	t.Helper()
	args, err := json.Marshal(map[string]string{
		"action": "restart-orders-on-disk-full",
		"target": "orders-api",
		"window": "win-1",
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return toolbroker.Call{
		SessionID: "sess-1",
		ToolName:  builtin.ToolKey,
		Arguments: args,
		Actor:     RoleAdmin,
	}
}

// TestAnAutonomyRequestReachesTheArbiterWhileTheCenterIsAway is the seam
// that had no test.
//
// Before the repair this failed at the first step and could not have got
// further: the policy demanded a receipt the node had no way to obtain, and
// the router would have upcalled to a centre that was not there.
func TestAnAutonomyRequestReachesTheArbiterWhileTheCenterIsAway(t *testing.T) {
	obs, runner := darkNode(t)
	if stack := newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner); stack == nil {
		t.Fatal("a package with an autonomy block produced no stack")
	}

	call := autonomyCall(t)
	permitted, reason := autonomyAuthorizer(t, obs)(context.Background(), call)
	if !permitted {
		t.Fatalf("the node refused an autonomy request while the centre was away: %s", reason)
	}

	inv := &agentToolInvoker{client: &fakeTunnel{}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), obs: obs}
	if _, err := inv.Invoke(context.Background(), call); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	got := runner.runs()
	if len(got) != 1 {
		t.Fatalf("the runner saw %d calls, want 1: the request never reached the arbiter", len(got))
	}
	if strings.Join(got[0], " ") != strings.Join(autonomyDeclaredArgv, " ") {
		t.Errorf("the runner got %v, want the declared vector", got[0])
	}
}

// TestTheCenterReachablePathIsUntouched is the other half of the contract,
// and it is the half that must not move: while a human is reachable, this
// tool is an ordinary mutating call that needs an ordinary approval.
func TestTheCenterReachablePathIsUntouched(t *testing.T) {
	obs := &fakeObservations{online: true, values: map[string]float64{"node_disk_used_ratio": 0.99}}
	runner := &recordingRunner{}
	if stack := newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner); stack == nil {
		t.Fatal("no stack")
	}

	call := autonomyCall(t)
	permitted, reason := autonomyAuthorizer(t, obs)(context.Background(), call)
	if permitted {
		t.Fatal("the node permitted an unapproved destructive call while the control plane was answering")
	}
	if !strings.Contains(reason, "approval") {
		t.Errorf("reason = %q, want it to say a human has to approve this", reason)
	}

	// And if the gate had approved it, the router still sends it up the
	// tunnel rather than answering it here. That is the pre-existing
	// behaviour and the reason the exemption is narrow.
	client := &fakeTunnel{}
	inv := &agentToolInvoker{client: client, log: slog.New(slog.NewTextHandler(io.Discard, nil)), obs: obs}
	if _, err := inv.Invoke(context.Background(), call); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if client.method != tunnel.MethodAgentTool {
		t.Errorf("the router called %q, want the control plane (%q)", client.method, tunnel.MethodAgentTool)
	}
	if len(runner.runs()) != 0 {
		t.Error("the node ran the action itself while the control plane was answering")
	}
}

// TestTheAutonomyExemptionIsForThatOneTool is the security boundary, and the
// test that stops this from being read as "destructive tools stopped needing
// approval when the network is down".
func TestTheAutonomyExemptionIsForThatOneTool(t *testing.T) {
	obs, runner := darkNode(t)
	if stack := newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner); stack == nil {
		t.Fatal("no stack")
	}
	reg, err := policygate.RegistryFromManifests([]domain.PluginManifest{autonomyPlugin().Manifest})
	if err != nil {
		t.Fatalf("RegistryFromManifests: %v", err)
	}
	// The package declares a second destructive tool. The exemption is
	// keyed on the autonomy tool by name, and this is what proves it.
	reg.Bind(policygate.ToolBinding{
		Name: "host_drop_database", Class: domain.ClassDestructive, FromPlugin: "opskeeper-sre-autonomy",
	})
	auth := toolAuthorizer(reg, &fakeReceipts{}, obs)

	permitted, reason := auth(context.Background(), toolbroker.Call{
		SessionID: "sess-1", ToolName: "host_drop_database", Arguments: []byte(`{}`), Actor: RoleAdmin,
	})
	if permitted {
		t.Fatal("a destructive tool other than autonomy ran with no approval while the network was down")
	}
	if !strings.Contains(reason, "approval") {
		t.Errorf("reason = %q, want it to say a human has to approve this", reason)
	}
}

// TestTheAutonomyExemptionStillRequiresTheToolToBeDeclared keeps the
// exemption downstream of the allow-list. If it were not, a package that
// shipped no inventory at all could reach a destructive path by having the
// network go down.
func TestTheAutonomyExemptionStillRequiresTheToolToBeDeclared(t *testing.T) {
	obs, runner := darkNode(t)
	if stack := newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner); stack == nil {
		t.Fatal("no stack")
	}
	// A second package that declares nothing. The node holds both.
	empty := pluginmanifest.Plugin{Manifest: domain.PluginManifest{
		Metadata: domain.PluginMeta{Name: "opskeeper-sre-quiet", Version: "0.1.0"},
		Spec: domain.PluginSpec{
			Targets: domain.Targets{domain.TargetEdge}, SafetyLevel: domain.SafetyL1,
			Tools: domain.Tools{{Name: "host_dmesg", Class: domain.ClassRead}},
		},
	}}
	reg, err := policygate.RegistryFromManifests([]domain.PluginManifest{empty.Manifest})
	if err != nil {
		t.Fatalf("RegistryFromManifests: %v", err)
	}
	permitted, reason := toolAuthorizer(reg, &fakeReceipts{}, obs)(context.Background(), autonomyCall(t))
	if permitted {
		t.Fatal("the node served an autonomy request for a tool no installed package declares")
	}
	if !strings.Contains(reason, "not in this node's tool set") {
		t.Errorf("reason = %q, want the allow-list refusal", reason)
	}
}

// TestANodeWithNoAutonomyInstalledAnswersRatherThanActs is the last thing
// standing between this rule and a hole. The permission is the signed
// manifest, and on a node with no declarations there is no arbiter to
// consult, so the tool must say so instead of doing anything.
func TestANodeWithNoAutonomyInstalledAnswersRatherThanActs(t *testing.T) {
	obs, runner := darkNode(t)
	// A package with the tool in its inventory but no autonomy block: the
	// registry admits the call, and there is nothing behind it.
	noActions := autonomyPlugin()
	noActions.Manifest.Spec.Autonomy = domain.AutonomyPolicy{}
	if stack := newAutonomyStack(t, []pluginmanifest.Plugin{noActions}, obs, runner); stack != nil {
		t.Fatal("a package with no autonomy actions produced a stack")
	}
	out := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1")
	if out.Verdict != autonomy.Refuse.String() {
		t.Errorf("verdict = %q, want refuse", out.Verdict)
	}
	if !strings.Contains(out.Reason, "no autonomy") {
		t.Errorf("reason = %q, want it to say the node has no autonomy declared", out.Reason)
	}
	if len(runner.runs()) != 0 {
		t.Error("a node with no autonomy declared ran something")
	}
}
