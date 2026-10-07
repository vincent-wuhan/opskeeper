package pluginmanifest

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// The observability profile is the control plane's half of the read story:
// the questions about the fleet rather than about the node. Every assertion
// here is paired with one about the read-only profile, and the pairing is
// the point. The two packages exist because they make different promises —
// "this node can examine itself" and "this node may read the fleet" — and a
// capability that leaks from one into the other destroys exactly one of
// them.
//
// Nothing in this file asserts that the tools work. The manager's own
// registry is the authority on that, and TestTheObservabilityToolsetMatchesTheRegistry
// in core/manager/biz/aiops/tools keeps the shipped schemas honest.
// What is asserted here is narrower and about the boundary: what the
// package claims, which node it runs on, and which data it can reach.

// observabilityProfile is the L1 package that reads the fleet.
const observabilityProfile = "opskeeper-sre-observability"

func observabilityRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "plugins", "pig-ops", observabilityProfile)
}

func loadObservabilityProfile(t *testing.T) Plugin {
	t.Helper()
	p, err := Load(observabilityRoot(t))
	if err != nil {
		t.Fatalf("Load %s: %v", observabilityProfile, err)
	}
	return p
}

func TestTheObservabilityProfileIsL1AndNeedsNoApproval(t *testing.T) {
	// The level and the approval flag fail in the same direction here, so
	// they are asserted together: an L2 profile would put every PromQL
	// query in front of a human, and a profile with approval.required true
	// would do the same while still claiming to be L1. Either one trains
	// operators to click through prompts for calls that change nothing,
	// which is the habit that makes the L2 package's prompts meaningless.
	p := loadObservabilityProfile(t)
	if p.Manifest.Spec.SafetyLevel != domain.SafetyL1 {
		t.Errorf("safety_level = %q, want L1", p.Manifest.Spec.SafetyLevel)
	}
	if p.Manifest.Spec.Approval.Required {
		t.Error("a package of reads must not require approval; it puts prompts in front of humans for calls that cannot change anything")
	}
	if got := p.Manifest.Spec.Approval.MaxBlastRadius; got != domain.RadiusNone {
		t.Errorf("max_blast_radius = %q, want none; a package that cannot mutate must declare no reach", got)
	}
}

func TestEveryToolInTheObservabilityProfileIsReadOnly(t *testing.T) {
	// Same claim as the read-only profile, and the same failure mode if it
	// is false: a write tool here would be permitted by an L1 manifest
	// whose approval.required is false, and would run unattended.
	p := loadObservabilityProfile(t)
	if len(p.Manifest.Spec.Tools) == 0 {
		t.Fatal("the profile declares no tools, so the node's allow-list is empty and every call is refused")
	}
	for i, tool := range p.Manifest.Spec.Tools {
		if tool.Class != domain.ClassRead {
			t.Errorf("spec.tools[%d] %q is %q; this package is L1 and every tool in it must be read",
				i, tool.Name, tool.Class)
		}
	}
}

func TestTheObservabilityProfilesToolListIsSortedAndUnique(t *testing.T) {
	// This list is the review surface. A diff between two versions of it
	// is the thing a reviewer actually reads, so it has to be sorted and
	// free of blanks for that diff to mean anything.
	p := loadObservabilityProfile(t)
	names := p.Manifest.Spec.Tools.Names()
	seen := make(map[string]bool, len(names))
	for i, n := range names {
		if strings.TrimSpace(n) == "" {
			t.Errorf("spec.tools[%d] has no name", i)
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

// TestTheObservabilityProfileAsksForReadScopesAndNothingWider is the
// credential assertion.
//
// observability.read is the metric and log history, db.read is the
// registered database sources, alert.read is the rule table the queries
// are interpreted against. What must NOT be here is host.read: this
// package reads the fleet, and granting it the local machine's view would
// be handing a fleet-wide toolset a scope whose purpose it does not have,
// in exchange for nothing. The mutating scopes are refused for the same
// reason the read-only profile refuses them, and more strongly.
func TestTheObservabilityProfileAsksForReadScopesAndNothingWider(t *testing.T) {
	p := loadObservabilityProfile(t)
	got := p.Manifest.Spec.RequiredScopes
	for _, s := range []domain.Scope{domain.ScopeMetricsRO, domain.ScopeDBRead, domain.ScopeAlertRO} {
		if !got.Has(s) {
			t.Errorf("the profile has a tool needing %q but does not declare it", s)
		}
	}
	for _, s := range got {
		switch s {
		case domain.ScopeHostRead:
			t.Errorf("the observability profile declares %q; the read-only profile is the one that reads the machine, "+
				"and a fleet-wide toolset holding it is a credential with no purpose", s)
		case domain.ScopeHostWrite, domain.ScopeDBWrite, domain.ScopeAlertWrite,
			domain.ScopeMQWrite, domain.ScopeMQRead, domain.ScopeK8sExec, domain.ScopeTopologyRO:
			t.Errorf("the observability profile declares the scope %q, which none of its tools needs", s)
		}
	}
}

func TestTheObservabilityProfileNeverClaimsToWriteTheAuditLedger(t *testing.T) {
	// A package that reads the fleet's log history is the one most able to
	// make a ledger entry disappear without anyone noticing. The refusal
	// is asserted here rather than trusted to the sdk, because the whole
	// point is that the package cannot be the thing that decides.
	p := loadObservabilityProfile(t)
	if p.Manifest.Spec.Audit.Mutates {
		t.Error("the observability profile claims audit-ledger writes, which are host-only")
	}
	if !p.Manifest.Spec.Audit.Emits {
		t.Error("the observability profile should declare that it reports tool activity")
	}
}

func TestTheObservabilityProfileRollsRatherThanPins(t *testing.T) {
	// The opposite of the repair profile's assertion, and deliberate.
	// Everything here is a read against an already-deployed backend, so a
	// version change adds schemas rather than actions, and the canary wave
	// is a real check rather than a formality. Pinning this package would
	// mean every observability schema fix waited for a deliberate rollout,
	// which is a worse outcome than the one being guarded against.
	p := loadObservabilityProfile(t)
	if p.Manifest.Spec.Install.Strategy != domain.InstallRolling {
		t.Errorf("install.strategy = %q, want rolling; a read-only package has no reason to pin", p.Manifest.Spec.Install.Strategy)
	}
	if strings.TrimSpace(p.Manifest.Spec.Install.MinEdgeVersion) == "" {
		t.Error("the package declares no min_edge_version, so a rolling install would hand it to a node that cannot run it")
	}
}

func TestTheObservabilityProfileTargetsTheEdgeAndNotTheControlPlane(t *testing.T) {
	// Its manifest was reviewed for running on a host. A profile that also
	// claimed the control plane would read as reviewed for both.
	p := loadObservabilityProfile(t)
	if !p.RunsOn(domain.TargetEdge) {
		t.Error("the observability profile must target the edge")
	}
	if p.RunsOn(domain.TargetManager) {
		t.Error("the observability profile must not claim the control plane")
	}
}

func TestTheObservabilityProfileShipsTheToolsetAndTheCourier(t *testing.T) {
	// The personas are useless without it. They describe how to gather
	// evidence, and the courier is what makes the gathering something the
	// host has approved — a package of skills with no gate is a package of
	// advice, and advice does not survive contact with a curious model.
	if _, err := os.Stat(filepath.Join(observabilityRoot(t), "extensions", observabilityProfile, "extension.go")); err != nil {
		t.Errorf("the profile does not ship its toolset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(observabilityRoot(t), "extensions", "opskeeper-gate", "extension.go")); err != nil {
		t.Errorf("the profile does not ship the gate courier: %v", err)
	}
}

func TestTheObservabilityProfileShipsAPersonaThatKnowsItsOwnLimits(t *testing.T) {
	// The toolset covers metrics, logs, traces, databases and source. It
	// does not cover Kubernetes objects or message queues, and a persona
	// that does not say so will stretch the nearest tool to answer a
	// question it was never built for and report the result as evidence.
	p := loadObservabilityProfile(t)
	if len(p.Skills) == 0 {
		t.Fatal("the profile ships no skills, so its twelve tools are a menu nobody knows how to use")
	}
	found := false
	for _, s := range p.Skills {
		body, err := os.ReadFile(filepath.Join(observabilityRoot(t), filepath.FromSlash(s), "SKILL.md"))
		if err != nil {
			t.Errorf("read %s: %v", s, err)
			continue
		}
		lower := strings.ToLower(string(body))
		if strings.Contains(lower, "kubernetes") && strings.Contains(lower, "not covered") {
			found = true
		}
	}
	if !found {
		t.Error("no shipped skill says that Kubernetes objects are outside what these tools can see; " +
			"a persona that cannot name its own gap will invent an answer instead of reporting one")
	}
}

// TestTheObservabilityProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel
// is the two-way agreement between the allow-list and the menu.
//
// A name in the manifest but not in the toolset is permitted and
// unreachable. A name in the toolset but not in the manifest is visible to
// the model and refused by the gate on every call. Both are silent until an
// incident, and the second is the more confusing of the two because the
// model can see the tool and will keep trying to call it.
func TestTheObservabilityProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel(t *testing.T) {
	p := loadObservabilityProfile(t)
	declared := make(map[string]bool, len(p.Manifest.Spec.Tools))
	for _, tool := range p.Manifest.Spec.Tools {
		declared[tool.Name] = true
	}

	shipped := readShippedToolNamesFor(t, observabilityRoot(t), observabilityProfile)
	if len(shipped) == 0 {
		t.Fatal("no tool names could be read out of the shipped toolset")
	}
	for name := range shipped {
		if !declared[name] {
			t.Errorf("the toolset offers %q but the manifest does not declare it, so the gate refuses every call to it", name)
		}
	}
	for name := range declared {
		if !shipped[name] {
			t.Errorf("the manifest declares %q but the toolset does not offer it, so it is permitted and unreachable", name)
		}
	}
}

// TestTheObservabilityProfileInstallsOnAReadOnlyNode is the counterpart to
// the repair profile's admission test.
//
// A node whose ceiling is L1 must still be able to get the fleet view. If
// the observability package needed L2, or asked for a blast radius, it
// would be unreachable on exactly the nodes where an operator most wants
// to ask a question during an incident.
func TestTheObservabilityProfileInstallsOnAReadOnlyNode(t *testing.T) {
	p := loadObservabilityProfile(t)
	if err := sdk.Admit(p.Manifest, sdk.Admission{
		GrantedScopes:  p.Manifest.Spec.RequiredScopes,
		MaxSafetyLevel: domain.SafetyL1,
		MaxBlastRadius: domain.RadiusNone,
	}); err != nil {
		t.Errorf("the observability package does not install on a read-only node: %v", err)
	}
}
