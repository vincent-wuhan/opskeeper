package pluginmanifest

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	// The executors have to be registered for skill.Get to find them.
	_ "github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
)

// The seven worker personas are the operational reason the read-only
// profile exists: an investigation, a triage, a plan, a verification and a
// review, each done by something that knows only its own job. Shipping six
// of the seven is a package that looks complete and is not, so the list is
// asserted rather than trusted.
var readonlyPersonas = []string{
	"opskeeper-alerter",
	"opskeeper-critic",
	"opskeeper-investigator",
	"opskeeper-postmortem",
	"opskeeper-repairer",
	"opskeeper-reviewer",
	"opskeeper-verifier",
}

// readOnlyProfile is the L1 package every node starts with.
const readOnlyProfile = "opskeeper-sre-readonly"

func profileRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "plugins", "pig-ops", readOnlyProfile)
}

func loadProfile(t *testing.T) Plugin {
	t.Helper()
	p, err := Load(profileRoot(t))
	if err != nil {
		t.Fatalf("Load %s: %v", readOnlyProfile, err)
	}
	return p
}

func TestTheReadOnlyProfileShipsEveryPersona(t *testing.T) {
	p := loadProfile(t)
	// Skills are reported relative to the package root; the personas are
	// identified by their directory, so the prefix is not part of the name.
	shipped := make(map[string]bool, len(p.Skills))
	for _, s := range p.Skills {
		shipped[filepath.Base(s)] = true
	}
	for _, persona := range readonlyPersonas {
		if !shipped[persona] {
			t.Errorf("the profile is missing the %s persona; it ships %v", persona, p.Skills)
		}
	}
}

func TestTheReadOnlyProfileShipsTheCourierThatEnforcesTheGate(t *testing.T) {
	// The personas are useless without it. They describe how to gather
	// evidence, and the courier is what makes the gathering something the
	// host has approved — a package of skills with no gate is a package of
	// advice, and advice does not survive contact with a curious model.
	if _, err := os.Stat(filepath.Join(profileRoot(t), "extensions", "opskeeper-gate", "extension.go")); err != nil {
		t.Errorf("the profile does not ship the gate courier: %v", err)
	}
}

func TestThePackagedCourierMatchesTheCanonicalSource(t *testing.T) {
	// The courier is copied into the package because the agent builds it
	// from source on the node, and a node has no access to an unpublished
	// OpsKeeper module. The cost of the copy is drift, and this test is
	// what pays it: a courier that differs from the reviewed one is a node
	// whose gate is not the gate anybody looked at.
	canonical := filepath.Join(repoRoot(t), "core", "pig", "extensions", "opskeeper-gate")
	packaged := filepath.Join(profileRoot(t), "extensions", "opskeeper-gate")
	for _, name := range []string{"extension.go", "client.go"} {
		want, err := os.ReadFile(filepath.Join(canonical, name))
		if err != nil {
			t.Fatalf("read canonical %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(packaged, name))
		if err != nil {
			t.Fatalf("read packaged %s: %v", name, err)
		}
		// The one permitted difference is the import of the broker
		// vocabulary, which travels inside the packaged extension so a
		// node can build it; see TestEveryPackagedExtensionCarriesTheWireVocabulary.
		if string(got) != asPackaged(string(want), readOnlyProfile, "opskeeper-gate") {
			t.Errorf("the packaged %s has drifted from core/pig/extensions/opskeeper-gate/%s; "+
				"copy it across so the node runs the courier that was reviewed", name, name)
		}
	}
}

func TestEveryToolInTheReadOnlyProfileIsReadOnly(t *testing.T) {
	// The profile's whole claim is that it needs no approval round trip.
	// One write-class tool breaks that claim, and breaks it silently: the
	// node would sit in an approval queue nobody was told to watch.
	p := loadProfile(t)
	if len(p.Manifest.Spec.Tools) == 0 {
		t.Fatal("the profile declares no tools, so the node's allow-list is empty and every call is refused")
	}
	for i, tool := range p.Manifest.Spec.Tools {
		if tool.Class != domain.ClassRead {
			t.Errorf("spec.tools[%d] %q is %q; the read-only profile is L1 and every tool in it must be read",
				i, tool.Name, tool.Class)
		}
	}
}

func TestTheProfilesToolListIsTheAllowListTheHostWillBuild(t *testing.T) {
	// The list is the review surface, so it is worth asserting that it is
	// a list a reviewer can read: no duplicates, no blanks, and the names
	// sorted so a diff between two versions is a diff a human can scan.
	p := loadProfile(t)
	names := p.Manifest.Spec.Tools.Names()
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			t.Error("a tool with no name is in the allow-list")
		}
		if seen[n] {
			t.Errorf("tool %q is declared twice", n)
		}
		seen[n] = true
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for i := range names {
		if names[i] != sorted[i] {
			t.Errorf("spec.tools is not sorted: %v; sort it so a version diff is reviewable", names)
			break
		}
	}
}

func TestTheReadOnlyProfileRequestsNoApprovalAndNoBlastRadius(t *testing.T) {
	// A read-only package that asked for an approval queue would put
	// prompts in front of operators for calls that cannot change anything.
	p := loadProfile(t)
	if p.Manifest.Spec.Approval.Required {
		t.Error("a read-only profile must not require approval")
	}
	if p.Manifest.Spec.Approval.MaxBlastRadius != domain.RadiusNone {
		t.Errorf("blast radius = %q, want none", p.Manifest.Spec.Approval.MaxBlastRadius)
	}
}

func TestTheReadOnlyProfileAsksForTheScopesItsToolsNeed(t *testing.T) {
	// A scope the profile never uses is a credential handed to code that
	// does not need it, and a missing one is a tool that fails at the
	// moment it is needed.
	p := loadProfile(t)
	got := p.Manifest.Spec.RequiredScopes
	want := []domain.Scope{domain.ScopeHostRead, domain.ScopeTopologyRO, domain.ScopeAlertRO}
	for _, s := range want {
		if !got.Has(s) {
			t.Errorf("the profile uses tools needing %q but does not declare it", s)
		}
	}
	// It must not ask for anything that implies mutation. A read-only
	// profile holding host.write is a credential waiting for a bug.
	for _, s := range got {
		switch s {
		case domain.ScopeHostWrite, domain.ScopeK8sExec, domain.ScopeDBWrite,
			domain.ScopeMQWrite, domain.ScopeAlertWrite:
			t.Errorf("the read-only profile declares the mutating scope %q", s)
		}
	}
}

func TestTheReadOnlyProfileTargetsTheEdgeAndNotTheControlPlane(t *testing.T) {
	// Its manifest was reviewed for running on a host. A profile that
	// also claimed the control plane would read as reviewed for both.
	p := loadProfile(t)
	if !p.RunsOn(domain.TargetEdge) {
		t.Error("the read-only profile must target the edge")
	}
	if p.RunsOn(domain.TargetManager) {
		t.Error("the read-only profile must not claim the control plane")
	}
}

func TestTheReadOnlyProfileShipsTheToolsetThatOffersItsTools(t *testing.T) {
	// The personas describe how to investigate and the courier decides
	// whether a call may run. Neither produces an observation: the
	// toolset is what actually reads the machine, and a profile whose
	// manifest names eighteen tools while shipping no extension to offer
	// them is a node whose agent is fluent and inert.
	if _, err := os.Stat(filepath.Join(profileRoot(t), "extensions", "opskeeper-sre-readonly", "extension.go")); err != nil {
		t.Errorf("the profile does not ship the read-only toolset: %v", err)
	}
}

func TestThePackagedToolsetMatchesTheCanonicalSource(t *testing.T) {
	// Same reasoning as the courier, with more surface. The toolset is
	// where the tool names, descriptions and schemas live — the model's
	// entire view of what this node can do — so a copy that drifts is a
	// node offering the model a different menu from the reviewed one.
	canonical := filepath.Join(repoRoot(t), "core", "pig", "extensions", "opskeeper-sre-readonly")
	packaged := filepath.Join(profileRoot(t), "extensions", "opskeeper-sre-readonly")
	for _, name := range []string{"extension.go", "client.go", "tools.go"} {
		want, err := os.ReadFile(filepath.Join(canonical, name))
		if err != nil {
			t.Fatalf("read canonical %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(packaged, name))
		if err != nil {
			t.Fatalf("read packaged %s: %v", name, err)
		}
		// The one permitted difference is the import of the broker
		// vocabulary, which travels inside the packaged extension so a
		// node can build it; see TestEveryPackagedExtensionCarriesTheWireVocabulary.
		if string(got) != asPackaged(string(want), readOnlyProfile, "opskeeper-sre-readonly") {
			t.Errorf("the packaged %s has drifted from core/pig/extensions/opskeeper-sre-readonly/%s; "+
				"copy it across so the node offers the tools that were reviewed", name, name)
		}
	}
}

func TestEveryHostToolTheProfileDeclaresHasAnExecutorOnThisNode(t *testing.T) {
	// The broker dispatches by name into the edge's skill registry. A
	// declared tool with no executor behind it is not a tool the model can
	// use — it is a tool that fails on first call, in production, during
	// an incident. host_ prefixed tools are the ones the node itself must
	// satisfy; the unprefixed ones are the control plane's, and they are
	// checked from the other side in the manager's tools package.
	p := loadProfile(t)
	checked := 0
	for _, tool := range p.Manifest.Spec.Tools {
		if !strings.HasPrefix(tool.Name, "host_") {
			continue
		}
		checked++
		if _, ok := skill.Get(tool.Name); !ok {
			t.Errorf("the profile declares %q but this node has no executor for it; "+
				"register the skill or drop it from the allow-list", tool.Name)
		}
	}
	if checked == 0 {
		t.Error("no host_ tools were checked, so the executor drift test is vacuous")
	}
}
