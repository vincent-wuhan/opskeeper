package pluginmanifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// catalogFixture writes one package whose install block is a parameter, so a
// test only has to describe the declaration it is about.
func catalogFixture(t *testing.T, minEdge, minPig string) Catalog {
	t.Helper()
	root := filepath.Join(t.TempDir(), "catalog")
	pkg := filepath.Join(root, "acme")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	install := "  install:\n    strategy: pin\n"
	if minEdge != "" {
		install += "    min_edge_version: " + minEdge + "\n"
	}
	if minPig != "" {
		install += "    min_pig_version: " + minPig + "\n"
	}
	manifest := `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: acme
  version: 1.2.3
  vendor: acme
  homepage: https://example.invalid/acme
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  required_scopes: [host.read]
` + install + `  tools:
    - { name: acme.probe, class: read }
`
	if err := os.WriteFile(filepath.Join(pkg, "pig-ops.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	catalog, err := LoadCatalog(root)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	return catalog
}

func TestIndexCountsToolsAndKeepsTheDeclaration(t *testing.T) {
	entries := catalogFixture(t, "0.8.0", "0.4.0").Entries()
	if len(entries) != 1 {
		t.Fatalf("%d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Name != "acme" || e.Version != "1.2.3" {
		t.Errorf("identity = %s@%s", e.Name, e.Version)
	}
	if e.ToolCount != 1 {
		t.Errorf("tool count = %d, want it counted from the manifest rather than a table beside it", e.ToolCount)
	}
	if e.MinEdgeVersion != "0.8.0" || e.MinPigVersion != "0.4.0" {
		t.Errorf("floors = edge %q pig %q", e.MinEdgeVersion, e.MinPigVersion)
	}
	if e.Strategy != "pin" || e.SafetyLevel != "L1" {
		t.Errorf("policy = %s / %s", e.Strategy, e.SafetyLevel)
	}
	if strings.Join(e.Scopes, ",") != "host.read" {
		t.Errorf("scopes = %v", e.Scopes)
	}
}

// that hid that would make an undeclared axis look like a satisfied one.
func TestIndexNamesTheFloorsAPackageDidNotDeclare(t *testing.T) {
	entries := catalogFixture(t, "", "").Entries()
	got := strings.Join(entries[0].UndeclaredFloors, ",")
	if got != "min_edge_version,min_pig_version" {
		t.Errorf("undeclared = %q, want both axes named", got)
	}
	if declared := catalogFixture(t, "0.8.0", "0.4.0").Entries()[0].UndeclaredFloors; len(declared) != 0 {
		t.Errorf("a fully declared package reported %v", declared)
	}
}

// Every package this repository ships must state both floors.
//
// This is the gate that makes the matrix mean anything: a fleet whose
// packages declare no pig floor has a matrix whose rows can never go red,
// and an always-green matrix is indistinguishable from no matrix at all.
// The build is pinned to one PiG version, so the honest floor is that
// version rather than a guess about what a node might run.
func TestEveryShippedPackageDeclaresBothHostFloors(t *testing.T) {
	shipped, err := LoadAll(filepath.Join(repoRoot(t), "plugins", "pig-ops"))
	if err != nil {
		t.Fatalf("load the shipped packages: %v", err)
	}
	if len(shipped) == 0 {
		t.Fatal("no shipped packages to check: this gate must not pass by reading an empty set")
	}
	catalog := Catalog{Plugins: shipped}
	for _, e := range catalog.Entries() {
		for _, field := range e.UndeclaredFloors {
			t.Errorf("%s declares no %s, so it would install on a node that cannot run it. "+
				"The build pins one PiG version; a package with no floor is a row of the "+
				"compatibility matrix that can never go red.", e.Name, field)
		}
	}
}
