package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The profile and the gate are two independent checks on the same node, and
// the profile is the weaker of the two by construction.
//
// PiG's ScopeTools is deliberately permissive about extensions a profile
// does not name - toolAllowed(nil, name) returns true, and PiG pins that as
// its own contract (TestScopeToolsUsesExactAllowlists expects an ambient
// tool to survive). That is a reasonable default for a general agent
// harness and the wrong default for an opskeeper node. It is not a bug we
// can fix here: that behaviour is upstream's published contract, and PiG
// offers no way to say "only these extensions".
//
// So the profile cannot be the layer that is right, and the gate is. This
// file is the evidence that the gate is, which is the whole reason the gap
// above is survivable. Two things have to hold at once:
//
//   - a tool the profile over-offers is by construction a tool no admitted
//     manifest declares, because one admitted set feeds both; and
//   - the gate refuses a tool no admitted manifest declares, and no
//     operator's approval can talk it out of that.
//
// Either half alone is not a defence. If the two were fed different sets, an
// unadmitted tool could sit in the gate's registry with no manifest behind
// it. If the gate were permissive about unknown tools, the profile's
// over-offer would be the whole story. Both are checked below, and both are
// driven from the real admitted set rather than from hand-written fixtures,
// so a refactor that splits those sets fails here and not in production.

// admittedPackage is a node that admitted one package declaring one
// extension and one read-only tool.
func admittedPackage() pluginmanifest.Plugin {
	return pluginmanifest.Plugin{
		Root:       "/nonexistent",
		Extensions: []string{"extensions/diagnose-postgres"},
		Manifest: domain.PluginManifest{
			APIVersion: "opskeeper.io/v1",
			Kind:       "Plugin",
			Metadata:   domain.PluginMeta{Name: "opskeeper-sre-readonly", Version: "0.1.0"},
			Spec: domain.PluginSpec{
				Targets:     domain.Targets{domain.TargetEdge},
				SafetyLevel: domain.SafetyL1,
				Tools:       domain.Tools{{Name: "host_dmesg", Class: domain.ClassRead}},
			},
		},
	}
}

// A tool the gate has never heard of stays refused even when an operator
// has already approved this exact call.
//
// The approval is granted deliberately. host_reboot is a registered skill,
// so the call site classifies it destructive and the gate would otherwise
// stop at "needs an operator's approval" - a refusal a permissive gate
// could survive by asking for a receipt. Handing it the receipt first
// leaves the allow-list as the only thing between this call and execution.
// Approval buys consent to run an admitted tool; it cannot buy a tool the
// node never admitted.
func TestTheGateRefusesAnUnadmittedToolEvenWithAnApprovalInHand(t *testing.T) {
	admitted := []pluginmanifest.Plugin{admittedPackage()}
	args := []byte(`{}`)

	auth := toolAuthorizer(registryOf(t, manifestsOf(admitted)...),
		(&fakeReceipts{}).grant("s", "host_reboot", args), nil)
	permitted, reason := permit(auth, RoleAdmin, "s", "host_reboot", args)
	if permitted {
		t.Fatal("a tool no admitted manifest declares was permitted even with an " +
			"operator's approval in hand; the profile's ambient over-offer would " +
			"then be the whole story")
	}
	if !strings.Contains(reason, "not in this node's tool set") {
		t.Errorf("reason = %q, want the allow-list refusal", reason)
	}

	// The admitted tool still runs, so the refusal above is a decision
	// rather than a node that refuses everything.
	permitted, reason = permit(auth, RoleAdmin, "s", "host_dmesg", args)
	if !permitted {
		t.Errorf("the admitted tool was refused too: %s", reason)
	}
}

// The profile and the gate are built from one admitted set, so the tool the
// profile names is exactly the tool the gate knows.
//
// A wider gate than profile is the failure this guards: the gate would hold
// a tool that no manifest behind it declares, and the refusal above would
// stop being evidence of anything.
func TestTheProfileAndTheGateNameTheSameTools(t *testing.T) {
	admitted := []pluginmanifest.Plugin{admittedPackage()}

	extensions, err := agentExtensions(admitted)
	if err != nil {
		t.Fatalf("agentExtensions: %v", err)
	}
	profileTools := make([]string, 0, 4)
	for _, ext := range extensions {
		profileTools = append(profileTools, ext.Tools...)
	}
	slices.Sort(profileTools)
	registry := registryOf(t, manifestsOf(admitted)...)

	if got := registry.Names(); !slices.Equal(got, profileTools) {
		t.Fatalf("the gate permits %v but the profile names %v; one admitted set "+
			"has to feed both, or the gate holds a tool no manifest declares",
			got, profileTools)
	}
}
