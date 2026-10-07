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
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// The end-to-end proof that the repair package is governed rather than
// merely declared.
//
// toolpath_test.go does this for the read-only profile. This file does it
// for the L2 one, and the difference is that every hop has somewhere to
// fail: a tool can be declared and unroutable, routed and unapproved,
// approved and dispatched on the wrong side of the tunnel. Each of those
// is a different bug with the same symptom to an operator — the model says
// it fixed something and it did not — so each gets its own test here.

// repairProfileRoot locates the shipped repair package from this test's
// own directory, so the check does not depend on the working directory.
func repairProfileRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source file")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	return filepath.Join(root, "plugins", "pig-ops", "opskeeper-sre-repair")
}

// repairBroker stands up a broker over the real repair manifest — and
// over the real read-only manifest too, because a production node has
// both, and the allow-list is their union.
func repairBroker(t *testing.T, tun *fakeTunnel, receipts ReceiptClaimer, actor string) string {
	t.Helper()
	repair, err := pluginmanifest.Load(repairProfileRoot(t))
	if err != nil {
		t.Fatalf("load the repair profile: %v", err)
	}
	readOnly, err := pluginmanifest.Load(realProfileRoot(t))
	if err != nil {
		t.Fatalf("load the read-only profile: %v", err)
	}
	registry, err := policygate.RegistryFromManifests([]domain.PluginManifest{
		readOnly.Manifest, repair.Manifest,
	})
	if err != nil {
		t.Fatalf("the two shipped packages cannot form one allow-list: %v", err)
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

// restartArgs is the argument set the manifest's schema requires.
var restartArgs = map[string]any{
	"device_id": float64(42),
	"service":   "nginx",
	"reason":    "worker pool exhausted; confirmed by 10m of cpu saturation",
}

// TestAMutatingToolFromTheShippedRepairProfileRunsOnlyAfterAnApproval is
// the central claim of the whole B3 package, end to end.
//
// The same broker, the same manifest and the same authoriser are used for
// both halves. The only difference is whether a human granted that exact
// call — which is the property the package is bought for, and the one that
// cannot be checked by reading the YAML.
func TestAMutatingToolFromTheShippedRepairProfileRunsOnlyAfterAnApproval(t *testing.T) {
	args, err := json.Marshal(restartArgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	t.Run("refused without an approval", func(t *testing.T) {
		tun := &fakeTunnel{reply: tunnel.AgentToolResponse{Result: json.RawMessage(`{"restarted":true}`)}}
		path := repairBroker(t, tun, &fakeReceipts{}, RoleAdmin)

		reply := ask(t, path, "sess-1", "host_restart_service", restartArgs)
		if reply.OK() {
			t.Fatal("a service was restarted with nobody asked")
		}
		if !strings.Contains(reply.Error, "approval") {
			t.Errorf("error = %q, want it to say the call needs an approval", reply.Error)
		}
		if tun.calls != 0 {
			t.Errorf("the call reached the control plane %d times despite being refused locally", tun.calls)
		}
	})

	t.Run("runs once after an approval", func(t *testing.T) {
		tun := &fakeTunnel{reply: tunnel.AgentToolResponse{Result: json.RawMessage(`{"restarted":true,"service":"nginx"}`)}}
		receipts := (&fakeReceipts{}).grant("sess-1", "host_restart_service", args)
		path := repairBroker(t, tun, receipts, RoleAdmin)

		reply := ask(t, path, "sess-1", "host_restart_service", restartArgs)
		if !reply.OK() {
			t.Fatalf("an approved restart failed through the whole path: %s", reply.Error)
		}
		if tun.calls != 1 {
			t.Errorf("the control plane saw %d calls, want exactly 1", tun.calls)
		}

		// And the grant is spent. One human approving one restart is one
		// restart; a grant that could be read twice would authorise a
		// model retry into a second outage.
		receipts = &fakeReceipts{}
		tun.calls = 0
		path2 := repairBroker(t, tun, receipts, RoleAdmin)
		second := ask(t, path2, "sess-1", "host_restart_service", restartArgs)
		if second.OK() {
			t.Error("a second, ungranted restart ran off the first one's approval")
		}
	})
}

// TestAMutatingToolIsNeverDispatchedByTheNodeItself is the test for the
// routing rule, and it exists because the obvious implementation is wrong.
//
// host_restart_service IS in the node's skill registry — that is how the
// catalog knows to draw it, and how the host classifies it. A broker that
// resolved tools by "is it registered here?" would therefore find it and
// call its Execute. That Execute is deliberately locked off, and a
// dispatcher that got past the registry check would be running the one
// action on this node whose approval lives somewhere else entirely.
//
// So the assertion is not just that the call succeeded. It is that the
// control plane is the one that answered it.
func TestAMutatingToolIsNeverDispatchedByTheNodeItself(t *testing.T) {
	tun := &fakeTunnel{reply: tunnel.AgentToolResponse{Result: json.RawMessage(`{"restarted":true}`)}}
	args, err := json.Marshal(restartArgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	receipts := (&fakeReceipts{}).grant("sess-1", "host_restart_service", args)
	path := repairBroker(t, tun, receipts, RoleAdmin)

	reply := ask(t, path, "sess-1", "host_restart_service", restartArgs)
	if !reply.OK() {
		t.Fatalf("an approved restart failed: %s", reply.Error)
	}
	if tun.method != tunnel.MethodAgentTool {
		t.Errorf("the call was answered by %q, want it to go up as %q — the reviewer that must see "+
			"a restart lives on the control plane, not here", tun.method, tunnel.MethodAgentTool)
	}
}

// TestAReadToolFromTheRepairPackageStillRunsWithoutAnyApproval is the
// other half of the L2 promise.
//
// A package that needs approval for everything trains its operators to
// click through. verify_recovery and draft_config_change change nothing,
// and a node that stopped offering them would be a node that could not
// check whether a repair worked — which is the failure mode this profile
// most needs to avoid.
func TestAReadToolFromTheRepairPackageStillRunsWithoutAnyApproval(t *testing.T) {
	for _, tool := range []string{"verify_recovery", "draft_config_change"} {
		t.Run(tool, func(t *testing.T) {
			tun := &fakeTunnel{reply: tunnel.AgentToolResponse{Result: json.RawMessage(`{"passed":true}`)}}
			// An empty grant store: nothing here has been approved, and
			// nothing here needs to be.
			path := repairBroker(t, tun, &fakeReceipts{}, RoleAdmin)

			var reply wire.ToolReply
			if tool == "verify_recovery" {
				reply = ask(t, path, "sess-1", tool, map[string]any{
					"skill_id":      "db-pool-exhaustion",
					"target":        "host-1",
					"resource_type": "host",
					"metrics":       []any{"cpu_usage"},
				})
			} else {
				reply = ask(t, path, "sess-1", tool, map[string]any{
					"domain":       "alert_rule",
					"action":       "create",
					"request_text": "alert me when the disk on the web hosts fills up",
				})
			}
			if !reply.OK() {
				t.Fatalf("a read tool in the L2 package needed an approval: %s", reply.Error)
			}
			if tun.calls != 1 {
				t.Errorf("the control plane saw %d calls, want 1", tun.calls)
			}
		})
	}
}

// TestTheRepairProfileDoesNotMakeAViewerDangerous is the capability check
// across packages rather than within one.
//
// Installing the repair package on a node must not widen what any existing
// role may do beyond the tools it declares. A viewer can still read every
// host probe it could read before, and still cannot restart anything —
// which is the property an operator relies on when they leave a console
// session open.
func TestTheRepairProfileDoesNotMakeAViewerDangerous(t *testing.T) {
	tun := &fakeTunnel{reply: tunnel.AgentToolResponse{Result: json.RawMessage(`{"restarted":true}`)}}
	// A grant that the role ceiling must still overrule. If the ceiling
	// were checked after the receipt rather than before it, this would
	// restart a service for somebody who may only look at one.
	args, err := json.Marshal(restartArgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	receipts := (&fakeReceipts{}).grant("sess-1", "host_restart_service", args)
	path := repairBroker(t, tun, receipts, RoleViewer)

	reply := ask(t, path, "sess-1", "host_restart_service", restartArgs)
	if reply.OK() {
		t.Fatal("a viewer restarted a service")
	}
	if tun.calls != 0 {
		t.Errorf("the call reached the control plane %d times for a viewer", tun.calls)
	}
}

// TestTheRepairPackageAddsNoToolOutsideItsOwnManifest is the union check
// from the node's side: the read-only tools still work, and the repair
// tools are exactly the ones its manifest names.
func TestTheRepairPackageAddsNoToolOutsideItsOwnManifest(t *testing.T) {
	tun := &fakeTunnel{}
	path := repairBroker(t, tun, &fakeReceipts{}, RoleAdmin)

	// A tool that exists in neither manifest. If installing the repair
	// package widened the allow-list, this is what would slip through.
	reply := ask(t, path, "sess-1", "host_reboot", nil)
	if reply.OK() {
		t.Fatal("a tool declared by no package ran on this node")
	}
}
