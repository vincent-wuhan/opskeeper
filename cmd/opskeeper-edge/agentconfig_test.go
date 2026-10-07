package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// writePackage lays down a plugin bundle carrying a governance manifest.
func writePackage(t *testing.T, base, name string, manifest string) string {
	t.Helper()
	root := filepath.Join(base, name)
	skillDir := filepath.Join(root, "skills", "probe")
	if err := os.MkdirAll(skillDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "pig-ops.yaml"), []byte(manifest), 0o640); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	skill := "---\nname: probe\ndescription: a read-only probe\n---\n\nProbe.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o640); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	return root
}

const readonlyManifest = `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: %s
  version: 0.1.0
  vendor: opskeeper
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  required_scopes: [host.read]
  audit:
    emits: true
    mutates: false
  approval:
    required: false
  install:
    strategy: rolling
`

const managerOnlyManifest = `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: %s
  version: 0.1.0
  vendor: opskeeper
spec:
  targets: [manager]
  safety_level: L1
  capabilities: [read]
  required_scopes: [host.read]
  audit:
    emits: true
    mutates: false
  approval:
    required: false
  install:
    strategy: rolling
`

// admit runs a package set through the node's real admission path, with
// the environment the tests expect. Going through nodePluginPolicy rather
// than a hand-built Policy is deliberate: these tests are about the node's
// behaviour, and a policy assembled in the test would be testing the test.
func admit(t *testing.T, roots []string) ([]pluginmanifest.Plugin, error) {
	t.Helper()
	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("nodePluginPolicy: %v", err)
	}
	return admitPackages(roots, pluginmanifest.NewTrustStore(), pol)
}

func TestAValidPackageIsAdmitted(t *testing.T) {
	base := t.TempDir()
	root := writePackage(t, base, "readonly", strings.Replace(readonlyManifest, "%s", "readonly", 1))
	got, err := admit(t, []string{root})
	if err != nil {
		t.Fatalf("admitPackages: %v", err)
	}
	if len(got) != 1 || got[0].Root != root {
		t.Errorf("admitted = %v, want [%s]", got, root)
	}
}

func TestAPackageThatFailsValidationIsRefusedEntirely(t *testing.T) {
	// Refusing the whole set rather than skipping the bad one is the point:
	// an agent that started with a subset would answer questions it can no
	// longer see the answer to, with nothing to say why.
	base := t.TempDir()
	good := writePackage(t, base, "good", strings.Replace(readonlyManifest, "%s", "good", 1))
	bad := writePackage(t, base, "bad", "apiVersion: opskeeper.io/v1\nkind: Plugin\nmetadata:\n  name: bad\n")

	if _, err := admit(t, []string{good, bad}); err == nil {
		t.Fatal("a package with an unusable manifest was admitted")
	}
}

func TestAPackageThatDoesNotTargetTheEdgeIsRefused(t *testing.T) {
	// Its manifest was reviewed for the control plane. Running it on a host
	// anyway would read as reviewed where nobody looked.
	base := t.TempDir()
	root := writePackage(t, base, "ctl", strings.Replace(managerOnlyManifest, "%s", "ctl", 1))
	if _, err := admit(t, []string{root}); err == nil {
		t.Fatal("a manager-only package was admitted to a node")
	}
}

func TestAMissingPackageDirectoryIsABootError(t *testing.T) {
	// An operator who configured packages and found them gone needs to be
	// told, not handed a node that answers confidently with no tools.
	if _, err := admit(t, []string{filepath.Join(t.TempDir(), "gone")}); err == nil {
		t.Fatal("a package directory that does not exist was admitted")
	}
}

func TestSettingsPointTheAgentAtExactlyTheAdmittedPackages(t *testing.T) {
	dir := t.TempDir()
	path, err := writeAgentSettings(dir, []string{"/var/lib/p/a", "/var/lib/p/b"})
	if err != nil {
		t.Fatalf("writeAgentSettings: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got agentSettings
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"file:///var/lib/p/a", "file:///var/lib/p/b"}
	if len(got.Packages) != len(want) {
		t.Fatalf("packages = %v, want %v", got.Packages, want)
	}
	for i := range want {
		if got.Packages[i] != want[i] {
			t.Errorf("package %d = %q, want %q", i, got.Packages[i], want[i])
		}
	}
}

func TestSettingsAreReplacedNotMerged(t *testing.T) {
	// A stale entry would keep a removed plugin loaded for ever, and a
	// hand-added key could point the agent at something nobody reviewed.
	dir := t.TempDir()
	configDir := filepath.Join(dir, agentConfigDirName())
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := `{"packages":["file:///removed"],"skills":["file:///sneaky"]}`
	if err := os.WriteFile(filepath.Join(configDir, agentSettingsFile), []byte(stale), 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}

	path, err := writeAgentSettings(dir, []string{"/var/lib/p/only"})
	if err != nil {
		t.Fatalf("writeAgentSettings: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), "/removed") {
		t.Error("a removed package survived the rewrite: it would still be loaded")
	}
	if strings.Contains(string(body), "sneaky") {
		t.Error("a hand-added key survived the rewrite: this file decides what code runs on the host")
	}
}

func TestSettingsAreNotWorldReadable(t *testing.T) {
	// The file names the code that runs with the node's privileges. It
	// should not be readable by every account on the host.
	dir := t.TempDir()
	path, err := writeAgentSettings(dir, []string{"/var/lib/p/a"})
	if err != nil {
		t.Fatalf("writeAgentSettings: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("mode = %v, want no world or group read", perm)
	}
}

func TestAnEmptyWorkingDirectoryIsRefused(t *testing.T) {
	if _, err := writeAgentSettings("", []string{"/var/lib/p/a"}); err == nil {
		t.Fatal("settings were written with nowhere to write them")
	}
}

func TestTheConfigDirectoryMatchesTheAgentsOwnRule(t *testing.T) {
	// If the node and the agent disagree about where project config lives,
	// the node writes a file the agent never reads and boots with no
	// plugins while its settings say otherwise.
	t.Setenv("PIG_USE_PI_DIRS", "1")
	if got := agentConfigDirName(); got != ".pi" {
		t.Errorf("with PIG_USE_PI_DIRS=1 the dir is %q, want .pi", got)
	}
	t.Setenv("PIG_USE_PI_DIRS", "")
	if got := agentConfigDirName(); got != ".pig" {
		t.Errorf("without it the dir is %q, want .pig", got)
	}
}

func TestSplitListIgnoresBlanksAndTrims(t *testing.T) {
	got := splitList(" /a , ,/b ,, /c ")
	want := []string{"/a", "/b", "/c"}
	if len(got) != len(want) {
		t.Fatalf("splitList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------
// deployment profiles
// ---------------------------------------------------------------------
//
// A profile is the whole point of the cross-cloud story: the same binary
// on a trading host and on a SaaS tenant, differing by one environment
// variable that is checkable remotely. These tests are about the property
// that makes that safe — a host cannot be half in one profile and half out
// of it — because a partially-applied profile is a policy nobody wrote.

func TestAProfileSetsTheCeilingTheRadiusAndTheScopesTogether(t *testing.T) {
	t.Setenv(edgeProfileEnv, string(pluginmanifest.ProfileFinance))
	fin, err := pluginmanifest.ProfileForName(string(pluginmanifest.ProfileFinance))
	if err != nil {
		t.Fatal(err)
	}

	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("the finance profile did not load: %v", err)
	}
	if pol.MaxSafetyLevel != fin.Ceiling {
		t.Errorf("ceiling = %s, want the profile's %s", pol.MaxSafetyLevel, fin.Ceiling)
	}
	if pol.MaxBlastRadius != fin.MaxRadius {
		t.Errorf("radius = %q, want the profile's %q", pol.MaxBlastRadius, fin.MaxRadius)
	}
	if !fin.Granted.Satisfies(pol.GrantedScopes) {
		t.Errorf("granted = %v, want at least the profile's %v", pol.GrantedScopes, fin.Granted)
	}
	if pol.AllowUnsigned {
		t.Error("a profile turned on unsigned installs")
	}
}

func TestAnUnknownProfileIsABootErrorRatherThanADefault(t *testing.T) {
	// The node refuses to start rather than picking a profile. Falling
	// back to the narrow one would refuse work this fleet is entitled to
	// with an error about a package; falling back to the wide one would
	// widen the fleet where it matters most.
	t.Setenv(edgeProfileEnv, "finance-strong-consistancy")
	pol, err := nodePluginPolicy()
	if err == nil {
		t.Fatalf("a misspelled profile produced policy %+v", pol)
	}
	if !strings.Contains(err.Error(), "finance-strong-consistancy") {
		t.Errorf("error %q does not quote the setting, so the operator cannot see the typo", err)
	}
}

func TestAProfileRefusesAnEnvironmentThatContradictsIt(t *testing.T) {
	// The combination a fleet drifts into: somebody widened the ceiling
	// during an incident and the profile was adopted afterwards. Merging
	// them would produce a host whose policy is neither the profile nor
	// what the environment says, and whose answer depends on which file
	// was edited last.
	t.Setenv(edgeProfileEnv, string(pluginmanifest.ProfileFinance))
	for _, tc := range []struct{ name, env, value string }{
		{"a wider ceiling", "OPSKEEPER_EDGE_MAX_SAFETY_LEVEL", "L3"},
		{"a wider radius", "OPSKEEPER_EDGE_MAX_BLAST_RADIUS", "cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			if _, err := nodePluginPolicy(); err == nil {
				t.Fatalf("%s=%s was accepted alongside a profile that does not allow it", tc.env, tc.value)
			}
		})
	}
}

func TestAProfileRefusesANarrowedScopeListToo(t *testing.T) {
	// Narrowing is the safe direction and is still refused, because the
	// failure it causes is invisible: the release installs everywhere but
	// here, no error is raised anywhere, and the wave simply never
	// finishes.
	t.Setenv(edgeProfileEnv, string(pluginmanifest.ProfileSaaS))
	t.Setenv("OPSKEEPER_EDGE_PLUGIN_SCOPES", "host.read")
	_, err := nodePluginPolicy()
	if err == nil {
		t.Fatal("a scope list omitting most of the profile's grant was accepted")
	}
	if !strings.Contains(err.Error(), "k8s.exec") {
		t.Errorf("error %q does not name a missing scope, so the operator cannot fix it", err)
	}
}

func TestAProfileAcceptsAnEnvironmentThatAgreesWithIt(t *testing.T) {
	// The positive case, so the refusals above cannot pass by refusing
	// everything an operator sets. Restating a profile's own values is a
	// reasonable way to make a host's configuration self-documenting.
	t.Setenv(edgeProfileEnv, string(pluginmanifest.ProfileSaaS))
	saas, err := pluginmanifest.ProfileForName(string(pluginmanifest.ProfileSaaS))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPSKEEPER_EDGE_MAX_SAFETY_LEVEL", string(saas.Ceiling))
	t.Setenv("OPSKEEPER_EDGE_MAX_BLAST_RADIUS", string(saas.MaxRadius))
	t.Setenv("OPSKEEPER_EDGE_PLUGIN_SCOPES", strings.Join(scopeStrings(saas.Granted), ","))

	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("a configuration that agrees with its profile was refused: %v", err)
	}
	if pol.MaxBlastRadius != saas.MaxRadius {
		t.Errorf("radius = %q, want %q", pol.MaxBlastRadius, saas.MaxRadius)
	}
}

// scopeStrings renders scopes for an environment list.
func scopeStrings(scopes domain.Scopes) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, string(s))
	}
	return out
}

func TestAProfileIsNotAppliedWhenNoneIsNamed(t *testing.T) {
	// The default path must keep working: a node that names no profile
	// gets the env-driven policy it had before profiles existed, which is
	// what every host provisioned by an older install.sh has.
	t.Setenv(edgeProfileEnv, "")
	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("the default policy no longer loads: %v", err)
	}
	if pol.MaxSafetyLevel != domain.SafetyL1 {
		t.Errorf("default ceiling = %s, want L1", pol.MaxSafetyLevel)
	}
	if pol.MaxBlastRadius != domain.RadiusNone {
		t.Errorf("default radius = %q, want none", pol.MaxBlastRadius)
	}
	if pol.AllowUnsigned {
		t.Error("the default policy allows unsigned installs")
	}
}
