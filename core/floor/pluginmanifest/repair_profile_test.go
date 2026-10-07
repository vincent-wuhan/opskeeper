package pluginmanifest

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	// The executors have to be registered for skill.Get to find them.
	_ "github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// classOfSkillForTest mirrors the mapping the node's own authoriser uses.
//
// It is a copy rather than a shared function because the node's copy lives
// in a composition root that this package must not import, and because the
// duplication is the assertion: if the two mappings ever disagree, a
// manifest written against this one stops matching what the node will do
// with it, which is exactly the drift these tests exist to catch.
func classOfSkillForTest(c skill.Class) domain.ToolClass {
	switch c {
	case skill.ClassSafe:
		return domain.ClassRead
	case skill.ClassMutating:
		return domain.ClassWrite
	case skill.ClassDangerous:
		return domain.ClassDestructive
	default:
		return domain.ClassDestructive
	}
}

// readShippedToolNames extracts the tool names out of a shipped toolset.
//
// It reads the source rather than importing the extension module, because
// the extension is built by the agent runtime on the node and this package
// must not depend on anything the edge binary does not already link. A
// Name: field inside a toolSpec literal is the shape the file guarantees
// by construction, and a change to that shape breaks this test loudly
// rather than silently returning an empty list.
func readShippedToolNames(t *testing.T, pkgRoot string) map[string]bool {
	t.Helper()
	return readShippedToolNamesFor(t, pkgRoot, repairProfile)
}

// readShippedToolNamesFor is the same check against a named extension.
//
// The extension directory is a parameter rather than a constant because
// every package ships its toolset under its own name, and a helper that
// only knows the repair one would silently read the repair toolset when
// asked about a different package — returning a plausible non-empty map of
// names that belong to another package. That failure looks like a
// disagreement between manifest and toolset, which is exactly the sort of
// thing a reviewer would go looking for in the wrong file.
func readShippedToolNamesFor(t *testing.T, pkgRoot, extName string) map[string]bool {
	t.Helper()
	path := filepath.Join(pkgRoot, "extensions", extName, "tools.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shipped toolset: %v", err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Name:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "Name:"))
		// The comma goes first: trimming quotes first would leave the
		// trailing quote in place, because a comma after it stops the cut.
		rest = strings.TrimSpace(strings.TrimSuffix(rest, ","))
		rest = strings.Trim(rest, `"`)
		if rest != "" && rest != "spec.Name" {
			out[rest] = true
		}
	}
	return out
}

// The repair profile is the first package that can change something, so
// most of what follows is not about whether it loads — the sdk already
// refuses a manifest that is internally inconsistent. It is about whether
// the promises it makes match what the host will actually let happen.
//
// The two packages are deliberately kept apart. Every assertion in this
// file is paired with one in profile_test.go about the read-only package,
// and the pairing is the point: a capability that leaks from one into the
// other breaks one of the two guarantees the pair exists to give.

// repairProfile is the L2 package a node installs when its operators want
// a repair capability.
const repairProfile = "opskeeper-sre-repair"

// mutatingToolsInRepairProfile is what this package exists to offer,
// written out rather than derived from the manifest.
//
// A test that reads its expectation out of the thing it is testing checks
// only that the file parses. This list is a statement about what a repair
// package is allowed to contain, and it is the list a reviewer of a future
// change to spec.tools is really being asked about.
var mutatingToolsInRepairProfile = []string{
	"apply_config_change",
	"host_restart_service",
	"recovery.execute",
}

func repairProfileRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "plugins", "pig-ops", repairProfile)
}

func loadRepairProfile(t *testing.T) Plugin {
	t.Helper()
	p, err := Load(repairProfileRoot(t))
	if err != nil {
		t.Fatalf("Load %s: %v", repairProfile, err)
	}
	return p
}

func TestTheRepairProfileIsExactlyL2AndAsksForApproval(t *testing.T) {
	// These two fields are the package's entire reason for existing, and
	// they fail in opposite directions from each other. Set the level too
	// low and the sdk refuses the manifest at load, because MinimumClass
	// (L1) is read and the write tools exceed it. Set approval to false and
	// the manifest still loads — a write tool with no approval queue runs
	// on a schedule nobody is watching.
	p := loadRepairProfile(t)
	if p.Manifest.Spec.SafetyLevel != domain.SafetyL2 {
		t.Errorf("safety_level = %q, want L2", p.Manifest.Spec.SafetyLevel)
	}
	if !p.Manifest.Spec.Approval.Required {
		t.Error("a package of mutating tools that does not require approval runs unattended")
	}
}

func TestTheRepairProfileAsksForTheNarrowestBlastRadiusThereIs(t *testing.T) {
	// pod is the narrowest radius in the vocabulary. Every tool in this
	// package acts on one named target — one unit on one device, one
	// alert-rule draft — so pod is what the tools actually do.
	//
	// The reason to pin it rather than leave it unset is that the host
	// derives a radius from the class when the field is absent, which for
	// write is a namespace. That would be a wider promise than this
	// package makes, written down in a file nobody chose deliberately.
	p := loadRepairProfile(t)
	if got := p.Manifest.Spec.Approval.MaxBlastRadius; got != domain.RadiusPod {
		t.Errorf("max_blast_radius = %q, want pod; every tool here acts on exactly one target", got)
	}
}

func TestTheRepairProfileNeverClaimsToWriteTheAuditLedger(t *testing.T) {
	// A package that can change live systems is the one most tempted to
	// ask for this, so the refusal is asserted here rather than trusted to
	// the sdk. It is refused at load, not downgraded, which is why this
	// test can only fail by the rule having been removed.
	p := loadRepairProfile(t)
	if p.Manifest.Spec.Audit.Mutates {
		t.Error("the repair profile claims audit-ledger writes, which are host-only")
	}
	if !p.Manifest.Spec.Audit.Emits {
		t.Error("the repair profile should declare that it reports tool activity")
	}
}

func TestTheRepairProfileAsksForTheWriteScopesAndNothingMore(t *testing.T) {
	// A scope is a credential. host.write is the restart, alert.write is
	// the config change, and every other scope in the vocabulary would
	// hand this package a key to something it has no tool for.
	p := loadRepairProfile(t)
	got := p.Manifest.Spec.RequiredScopes
	for _, s := range got {
		switch s {
		case domain.ScopeHostRead, domain.ScopeTopologyRO, domain.ScopeAlertRO,
			domain.ScopeDBRead, domain.ScopeDBWrite, domain.ScopeMQRead,
			domain.ScopeMQWrite, domain.ScopeMetricsRO, domain.ScopeK8sExec:
			t.Errorf("the repair profile declares the scope %q, which none of its five tools needs", s)
		}
	}
	for _, s := range []domain.Scope{domain.ScopeHostWrite, domain.ScopeAlertWrite} {
		if !got.Has(s) {
			t.Errorf("the repair profile has a mutating tool but does not declare the scope %q", s)
		}
	}
}

func TestTheRepairProfilesToolListIsSortedUniqueAndCarriesTheMutatingOnes(t *testing.T) {
	p := loadRepairProfile(t)
	names := p.Manifest.Spec.Tools.Names()
	if len(names) == 0 {
		t.Fatal("the profile declares no tools, so the node's allow-list is empty and every call is refused")
	}

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

	for _, want := range mutatingToolsInRepairProfile {
		if !seen[want] {
			t.Errorf("the repair profile does not declare %q, so the host will refuse the tool the package exists to offer", want)
		}
	}
}

// TestTheRepairProfilesDeclaredClassesMatchWhatTheHostWillFind is the test
// that makes the manifest honest.
//
// The declared class is only a claim. What decides whether a call needs
// approval is the class the host derives at the call site, and for a tool
// this node can execute that comes from the skill registry — not from
// this file. A package that understates a tool here therefore does not get
// to run it quietly; the host refuses the call outright. Asserting the two
// agree here means that failure arrives as a test, rather than as an
// operator looking at a refusal during an incident.
func TestTheRepairProfilesDeclaredClassesMatchWhatTheHostWillFind(t *testing.T) {
	p := loadRepairProfile(t)
	declared := make(map[string]domain.ToolClass, len(p.Manifest.Spec.Tools))
	for _, tool := range p.Manifest.Spec.Tools {
		declared[tool.Name] = tool.Class
	}

	checked := 0
	for _, name := range mutatingToolsInRepairProfile {
		exec, ok := skill.Get(name)
		if !ok {
			// host_restart_service is registered on the node. The other
			// two are the control plane's, and are checked from the other
			// side in the manager's tools package — the same split the
			// read-only profile uses, for the same reason.
			if name != "host_restart_service" {
				continue
			}
			t.Errorf("%q is registered nowhere, so the node cannot classify it and every call to it is refused", name)
			continue
		}
		checked++
		if got := classOfSkillForTest(exec.Metadata().EffectiveClass()); got != declared[name] {
			t.Errorf("%q is declared %q but the executor on this node is %q; "+
				"the host refuses any call whose observed class is worse than the declaration",
				name, declared[name], got)
		}
	}
	if checked == 0 {
		t.Error("no tool was checked against the skill registry, so this test is vacuous")
	}
}

func TestTheRepairProfileIsPinnedRatherThanRolledOut(t *testing.T) {
	// A package that can restart a service is not one to auto-upgrade
	// across a fleet. An operator who approved a repair on yesterday's
	// version of it did not approve today's.
	p := loadRepairProfile(t)
	if p.Manifest.Spec.Install.Strategy != domain.InstallPin {
		t.Errorf("install.strategy = %q, want pin; a mutating package must not roll itself out", p.Manifest.Spec.Install.Strategy)
	}
	if strings.TrimSpace(p.Manifest.Spec.Install.MinEdgeVersion) == "" {
		t.Error("the package declares no min_edge_version, so a rolling install would hand it to a node that cannot run it")
	}
}

func TestTheRepairProfileShipsTheCourierThatEnforcesTheGate(t *testing.T) {
	// The approval this package depends on is enforced by the gate
	// courier, and the broker's second check is what makes it unbypassable.
	// A package that shipped mutating tools without the courier would be a
	// package whose tools nothing checks.
	if _, err := os.Stat(filepath.Join(repairProfileRoot(t), "extensions", "opskeeper-gate", "extension.go")); err != nil {
		t.Errorf("the repair profile does not ship the gate courier: %v", err)
	}
}

func TestTheRepairProfileShipsTheToolsetThatOffersItsTools(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repairProfileRoot(t), "extensions", repairProfile, "extension.go")); err != nil {
		t.Errorf("the repair profile does not ship its toolset: %v", err)
	}
}

func TestTheRepairProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel(t *testing.T) {
	// The manifest is the host's allow-list; the toolset is the model's
	// menu. A name in one and not the other is broken in one direction or
	// the other: in the manifest alone, the broker permits a tool no
	// extension offers; in the toolset alone, the gate refuses a tool the
	// model can see and call. Both are silent until an incident.
	p := loadRepairProfile(t)
	declared := make(map[string]bool, len(p.Manifest.Spec.Tools))
	for _, tool := range p.Manifest.Spec.Tools {
		declared[tool.Name] = true
	}

	shipped := readShippedToolNames(t, repairProfileRoot(t))
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

// TestEveryShippedPackageDeclaresEveryToolExactlyOnce is the property the
// set of packages exists to have.
//
// The host builds one allow-list from all the manifests and refuses a name
// that two packages claim — which is the right behaviour, and means a
// collision is a boot failure rather than a silent last-one-wins. Building
// the registry here is what turns that into a test that runs in CI instead
// of a node that will not start.
//
// It is a walk over every shipped package rather than a hand-written pair
// because the interesting case is the one nobody thought of: a collision
// between two read packages looks harmless on paper, because neither of
// them claims to mutate anything, and it is only the *joint* reading that
// makes it a problem — two manifests, two different promises about the
// same tool name, and one allow-list to honour both.
func TestEveryShippedPackageDeclaresEveryToolExactlyOnce(t *testing.T) {
	readOnly := loadProfile(t)
	repair := loadRepairProfile(t)
	observability := loadObservabilityProfile(t)

	registry, err := policygate.RegistryFromManifests([]domain.PluginManifest{
		readOnly.Manifest, observability.Manifest, repair.Manifest,
	})
	if err != nil {
		t.Fatalf("the shipped packages cannot form one allow-list: %v", err)
	}

	// A mutating tool appearing in the read-only package would be the
	// failure this whole arrangement is built to prevent: the L1 manifest
	// would then carry a write tool, its "needs no approval" claim would be
	// false, and the node would sit in an approval queue nobody was told
	// to watch.
	readOnlyNames := make(map[string]bool)
	for _, tool := range readOnly.Manifest.Spec.Tools {
		readOnlyNames[tool.Name] = true
	}
	for _, name := range mutatingToolsInRepairProfile {
		if readOnlyNames[name] {
			t.Errorf("%q is declared by both packages; the read-only package would then carry a mutating tool", name)
		}
	}

	// And the union must actually be a tool set, not an empty one, and
	// every name in it must be accounted for by exactly one manifest.
	// A sum check catches an overlap; the reverse walk catches a name the
	// registry dropped, which a sum check alone would miss because two
	// missing and one duplicated can add up.
	want := len(readOnly.Manifest.Spec.Tools) +
		len(observability.Manifest.Spec.Tools) +
		len(repair.Manifest.Spec.Tools)
	if got := len(registry.Names()); got != want {
		t.Errorf("the union holds %d tools, want %d — the sum of all manifests with no overlap", got, want)
	}

	owners := map[string]string{}
	for _, p := range []Plugin{readOnly, observability, repair} {
		for _, tool := range p.Manifest.Spec.Tools {
			if other, taken := owners[tool.Name]; taken {
				t.Errorf("%q is declared by both %s and %s; two packages that promise different things "+
					"about one tool name cannot both be honoured by a single allow-list",
					tool.Name, other, p.Name())
			}
			owners[tool.Name] = p.Name()
		}
	}
	for _, n := range registry.Names() {
		if owners[n] == "" {
			t.Errorf("the allow-list carries %q but no shipped manifest declares it", n)
		}
	}
}

// TestTheReadOnlyProfilesGuaranteeSurvivesTheRepairPackageExisting is the
// one that would have caught a well-meaning "let's just add restart to the
// main package" change.
func TestTheReadOnlyProfilesGuaranteeSurvivesTheRepairPackageExisting(t *testing.T) {
	readOnly := loadProfile(t)
	repair := loadRepairProfile(t)

	// Both packages coexist on the same node, so the node's effective
	// safety level is the worse of the two. That is expected and it is why
	// admission is per package: the read-only package is still admissible
	// on a node whose ceiling is read-only, even though the repair package
	// next to it is not.
	if err := sdk.Admit(readOnly.Manifest, sdk.Admission{
		GrantedScopes:  readOnly.Manifest.Spec.RequiredScopes,
		MaxSafetyLevel: domain.SafetyL1,
		MaxBlastRadius: domain.RadiusNone,
	}); err != nil {
		t.Errorf("the read-only package no longer installs on a read-only node, because a mutating package exists: %v", err)
	}

	if err := sdk.Admit(repair.Manifest, sdk.Admission{
		GrantedScopes:  repair.Manifest.Spec.RequiredScopes,
		MaxSafetyLevel: domain.SafetyL1,
		// A read-only node's ceiling is L1, and the package asks for L2.
		// Either ceiling refuses it; both are checked here because they are
		// different refusals and an operator should be able to tell which
		// one they hit.
		MaxBlastRadius: domain.RadiusNone,
	}); err == nil {
		t.Error("the repair package was admitted onto a node whose policy ceiling is L1")
	}
}
