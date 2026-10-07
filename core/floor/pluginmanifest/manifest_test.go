package pluginmanifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// repoRoot walks up from the test's working directory to the repository
// root, using markers that are tracked in git rather than go.work, which is
// gitignored and absent from the checkout CI and a release build both use.
// See core/floor/reporoot for why that distinction is the whole point.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, ok := reporoot.Find(dir, 8)
	if !ok {
		t.Fatalf("could not locate the repository root from %s", dir)
	}
	return root
}

// TestShippedPluginsAreValid is the gate that keeps plugin governance from
// drifting. Every plugin under plugins/pig-ops must parse, satisfy every
// admission rule, and stay within the levels its own manifest declares. A
// manifest that fails here must never reach a node.
func TestShippedPluginsAreValid(t *testing.T) {
	root := repoRoot(t)
	base := filepath.Join(root, "plugins", "pig-ops")

	cat, err := LoadCatalog(base)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if len(cat.Plugins) == 0 {
		t.Fatal("expected at least one shipped plugin under plugins/pig-ops")
	}
	for _, p := range cat.Plugins {
		t.Run(p.Name(), func(t *testing.T) {
			if p.Manifest.APIVersion != domain.PluginAPIVersion {
				t.Errorf("apiVersion = %q", p.Manifest.APIVersion)
			}
			if !p.Manifest.Spec.Targets.Valid() {
				t.Errorf("targets %v are not valid", p.Manifest.Spec.Targets)
			}
			if p.Manifest.Spec.Audit.Mutates {
				t.Error("audit mutation must never be delegated to a plugin")
			}
			// A read-only plugin must not declare a blast radius: the
			// field would put a mutation scope in the record for a
			// package that cannot mutate.
			if !p.Manifest.Spec.SafetyLevel.RequiresApproval() &&
				p.Manifest.Spec.Approval.MaxBlastRadius != domain.RadiusNone {
				t.Errorf("read-only plugin carries blast radius %q",
					p.Manifest.Spec.Approval.MaxBlastRadius)
			}
			if len(p.Skills) == 0 {
				t.Error("expected the plugin to ship at least one skill")
			}
		})
	}
}

// TestReadOnlyPluginIsAdmittedOnAReadOnlyNode checks the end-to-end path a
// node takes: read a manifest, then admit it against that node's granted
// scopes and policy ceiling.
func TestReadOnlyPluginIsAdmittedOnAReadOnlyNode(t *testing.T) {
	root := repoRoot(t)
	cat, err := LoadCatalog(filepath.Join(root, "plugins", "pig-ops"))
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	p, ok := cat.ByName("opskeeper-sre-readonly")
	if !ok {
		t.Fatalf("opskeeper-sre-readonly not found; catalog has %v", cat.Names())
	}
	if !p.RunsOn(domain.TargetEdge) {
		t.Error("the read-only plugin should target the edge")
	}
	if p.HighestCapability() != domain.ClassRead {
		t.Errorf("highest capability = %q, want read", p.HighestCapability())
	}

	// A node that granted exactly what the plugin asked for must admit it.
	admission := sdk.Admission{
		GrantedScopes:  p.Manifest.Spec.RequiredScopes,
		MaxSafetyLevel: domain.SafetyL1,
		MaxBlastRadius: domain.RadiusNone,
	}
	if err := sdk.Admit(p.Manifest, admission); err != nil {
		t.Errorf("a node that granted the declared scopes should admit it: %v", err)
	}

	// A node missing one scope must refuse.
	withheld := append(domain.Scopes(nil), p.Manifest.Spec.RequiredScopes[:len(p.Manifest.Spec.RequiredScopes)-1]...)
	if err := sdk.Admit(p.Manifest, sdk.Admission{
		GrantedScopes:  withheld,
		MaxSafetyLevel: domain.SafetyL1,
		MaxBlastRadius: domain.RadiusNone,
	}); err == nil {
		t.Error("a node missing a declared scope must refuse the plugin")
	}
}

// TestShippedSkillHasFrontmatter guards the agent runtime's parser: a
// SKILL.md without a name and description is silently undiscoverable, which
// is the same failure as shipping no skill at all.
func TestShippedSkillHasFrontmatter(t *testing.T) {
	root := repoRoot(t)
	skillDir := filepath.Join(root, "plugins", "pig-ops", "opskeeper-sre-readonly",
		"skills", "diagnose-readonly")
	data, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	body := string(data)
	if !strings.HasPrefix(strings.TrimSpace(body), "---") {
		t.Fatal("SKILL.md must open with YAML frontmatter")
	}
	end := strings.Index(body[3:], "\n---")
	if end < 0 {
		t.Fatal("SKILL.md frontmatter is not closed")
	}
	fm := body[3 : 3+end]
	for _, key := range []string{"name:", "description:"} {
		if !strings.Contains(fm, key) {
			t.Errorf("frontmatter is missing %q", key)
		}
	}
}

func TestLoadRejectsAnInvalidPlugin(t *testing.T) {
	dir := t.TempDir()
	bad := `apiVersion: opskeeper.io/v1
kind: Plugin
metadata: {name: bad, version: "1"}
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [destructive]
  audit: {emits: true, mutates: false}
`
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(bad), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A plugin declaring a destructive capability under an L1 label is a
	// manifest that lies about itself; loading must refuse it.
	if _, err := Load(dir); err == nil {
		t.Error("Load accepted a manifest whose capability exceeds its level")
	}
}

func TestCatalogForTargetAndDescribe(t *testing.T) {
	p := Plugin{
		Manifest: domain.PluginManifest{
			Metadata: domain.PluginMeta{Name: "demo", Version: "0.2.0"},
			Spec: domain.PluginSpec{
				Targets:        domain.Targets{domain.TargetEdge, domain.TargetManager},
				SafetyLevel:    domain.SafetyL2,
				Capabilities:   []domain.ToolClass{domain.ClassRead, domain.ClassWrite},
				RequiredScopes: domain.Scopes{domain.ScopeHostRead, domain.ScopeAlertWrite},
			},
		},
	}
	cat := Catalog{Plugins: []Plugin{p}}
	if got := cat.ForTarget(domain.TargetEdge); len(got) != 1 {
		t.Errorf("ForTarget(edge) = %d, want 1", len(got))
	}
	if got := cat.ForTarget("cluster"); len(got) != 0 {
		t.Errorf("ForTarget(undeclared) = %d, want 0", len(got))
	}
	desc := Describe(p)
	for _, want := range []string{"demo", "0.2.0", "L2", "write", "edge", "host.read"} {
		if !strings.Contains(desc, want) {
			t.Errorf("Describe = %q, missing %q", desc, want)
		}
	}
}
