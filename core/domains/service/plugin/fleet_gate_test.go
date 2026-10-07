package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The gate's tests are about what never happens.
//
// Every one of them asserts that the node was not asked, because that is the
// property the gate exists to provide. A gate that refused *after* the tunnel
// call would still produce the right Outcome and still be wrong: the package
// would already be on the node's disk, one apply away from running, on a
// cluster whose root never blessed it.

type fixedGate struct {
	err   error
	asked []string
}

func (g *fixedGate) Check(name, _ string) error {
	g.asked = append(g.asked, name)
	return g.err
}

func installSpec() ports.PluginSpec {
	return ports.PluginSpec{
		Name: "acme", Version: "1.0.0", URL: "https://m/x.tar.gz",
		SHA256: strings.Repeat("b", 64), Signature: "ZW52", KeyID: "ops-2026",
	}
}

func installed() *wireNode {
	return &wireNode{respond: func(_ string, _ []byte) ([]byte, error) {
		return json.Marshal(tunnel.PluginInstallResponse{
			Status: tunnel.PluginStatusInstalled, Plugin: "acme", Version: "1.0.0", Digest: "tree",
		})
	}}
}

func TestNoGateIsTheOrdinarySingleClusterCase(t *testing.T) {
	w := installed()
	out := NewNodeFleet(w).Install(context.Background(), 7, installSpec())
	if out.Status != StatusInstalled {
		t.Fatalf("install = %+v; a nil gate must not change anything", out)
	}
}

func TestAGateThatAllowsChangesNothingAboutTheCall(t *testing.T) {
	w := installed()
	g := &fixedGate{}
	f := NewNodeFleet(w)
	f.SetPolicyGate(g)

	out := f.Install(context.Background(), 7, installSpec())
	if out.Status != StatusInstalled {
		t.Fatalf("install = %+v", out)
	}
	if len(w.methods) != 1 || w.methods[0] != tunnel.MethodPluginInstall {
		t.Fatalf("the node was asked %v; an allowing gate must still make exactly one call", w.methods)
	}
}

// The whole point. The control plane refuses, and no node is told anything.
func TestARefusedPackageIsNeverOfferedToANode(t *testing.T) {
	w := installed()
	g := &fixedGate{err: errors.New("the live policy does not carry that package")}
	f := NewNodeFleet(w)
	f.SetPolicyGate(g)

	out := f.Install(context.Background(), 7, installSpec())
	if out.Status != StatusRefused {
		t.Fatalf("status = %q, want %q", out.Status, StatusRefused)
	}
	if len(w.methods) != 0 {
		t.Fatalf("the node was asked %v despite the policy saying no", w.methods)
	}
	if out.Plugin != "acme" {
		t.Errorf("the refusal names plugin %q; a refusal with no subject is one an operator cannot act on", out.Plugin)
	}
	if !strings.Contains(out.Reason, "this control plane") {
		t.Errorf("reason %q does not say the refusal came from this control plane and not from the node", out.Reason)
	}
}

// A refusal is a decision. Reporting it as a failure would put a policy that
// says no into the rollout's retry path, once per wave, for as long as the
// operator leaves the release running — and answered() is what decides that.
func TestAPolicyRefusalIsARefusalAndNotSomethingToRetry(t *testing.T) {
	g := &fixedGate{err: errors.New("no")}
	f := NewNodeFleet(installed())
	f.SetPolicyGate(g)

	out := f.Install(context.Background(), 7, installSpec())
	if !answered(out.Status) {
		t.Errorf("answered(%q) is false, so a rollout would retry a decision that will not change", out.Status)
	}
	if out.Status == StatusFailed {
		t.Error("a policy refusal reported as failed sends it down the retry path")
	}
}

// A gate that can only add is a gate that can trap. A package the root has
// withdrawn must still be removable, or the cluster is left holding exactly
// the thing it was told to stop running.
func TestARemovedPackageDoesNotNeedThePolicysPermission(t *testing.T) {
	w := &wireNode{respond: func(_ string, _ []byte) ([]byte, error) {
		return json.Marshal(tunnel.PluginRemoveResponse{Status: "removed", Plugin: "acme", Version: "1.0.0"})
	}}
	f := NewNodeFleet(w)
	// A gate that refuses everything, including the removal.
	f.SetPolicyGate(&fixedGate{err: errors.New("the live policy does not carry that package")})

	out := f.Remove(context.Background(), 7, "acme", "1.0.0")
	if out.Status == StatusRefused {
		t.Fatalf("removal was refused by the install gate: %+v", out)
	}
	if len(w.methods) != 1 || w.methods[0] != tunnel.MethodPluginRemove {
		t.Fatalf("the node was asked %v; a removal must still reach the node", w.methods)
	}
}

// SetPolicyGate(nil) has to be able to take the gate away, or a control plane
// that loaded a policy could not go back to deciding for itself.
func TestTheGateCanBeRemoved(t *testing.T) {
	w := installed()
	f := NewNodeFleet(w)
	f.SetPolicyGate(&fixedGate{err: errors.New("no")})
	f.SetPolicyGate(nil)

	if out := f.Install(context.Background(), 7, installSpec()); out.Status != StatusInstalled {
		t.Fatalf("install = %+v after the gate was removed", out)
	}
}
