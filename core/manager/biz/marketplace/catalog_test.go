package marketplace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The routes above are tested against a stub, which cannot catch the half of
// this that matters most: that the index is read from the directory
// packages actually land in. These tests use a real root with a real
// manifest in it.

func catalogRoot(t *testing.T, install string) string {
	t.Helper()
	root := t.TempDir()
	pkg := filepath.Join(root, "acme")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: acme
  version: 1.0.0
  vendor: acme
  homepage: https://example.invalid/acme
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  install:
    ` + install + `
  tools:
    - { name: acme.probe, class: read }
`
	if err := os.WriteFile(filepath.Join(pkg, "pig-ops.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return root
}

func catalogUsecase(root string) *Usecase {
	return NewUsecase(nil, nil, nil, Config{SystemSkillsRoot: root}, nil)
}

func catalogCaller() Caller { return Caller{UserID: 1, TenantID: 1, Role: "admin"} }

func TestCatalogReadsTheInstallRoot(t *testing.T) {
	uc := catalogUsecase(catalogRoot(t, "strategy: pin\n    min_edge_version: 0.8.0\n    min_pig_version: 0.4.0"))
	entries, err := uc.Catalog(context.Background(), catalogCaller())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries, want the one package in the root", len(entries))
	}
	if entries[0].Name != "acme" || entries[0].MinPigVersion != "0.4.0" {
		t.Errorf("entry = %#v", entries[0])
	}
}

// A tenant that has installed nothing is not an error. Answering 500 here
// would teach the SPA to render a failure where the truth is "empty".
func TestCatalogOnAnAbsentRootIsEmptyNotAnError(t *testing.T) {
	uc := catalogUsecase(filepath.Join(t.TempDir(), "never-created"))
	entries, err := uc.Catalog(context.Background(), catalogCaller())
	if err != nil {
		t.Fatalf("Catalog on an absent root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d entries from a root that does not exist", len(entries))
	}
}

// An unauthenticated call is refused before the filesystem is touched: the
// index says what a tenant may install, which is tenant-scoped information.
func TestCatalogRefusesAnAnonymousCaller(t *testing.T) {
	uc := catalogUsecase(catalogRoot(t, "strategy: rolling"))
	if _, err := uc.Catalog(context.Background(), Caller{}); err == nil {
		t.Error("an anonymous caller got the index")
	}
}

// A package in the cluster-wide root is installable by every tenant. Before
// these, the index read the tenant's root only, so it answered "what has this
// tenant installed" on a route documented as "what can this tenant install".
func TestCatalogIncludesTheClusterWideRoot(t *testing.T) {
	system := catalogRoot(t, "strategy: rolling")
	tenant := t.TempDir()

	uc := NewUsecase(nil, nil, nil, Config{
		SystemSkillsRoot: system,
		TenantSkillsRoot: func(uint64) string { return tenant },
	}, nil)

	entries, err := uc.Catalog(context.Background(), catalogCaller())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "acme" {
		t.Fatalf("entries = %#v; the cluster-wide package is not in the index", entries)
	}
	if entries[0].Origin != pluginmanifest.OriginSystem {
		t.Errorf("origin = %q, want %q; the row says where it came from and a reader "+
			"cannot tell an override from a cluster package without it",
			entries[0].Origin, pluginmanifest.OriginSystem)
	}
}

// The tenant's copy is the one that runs, so it wins over the cluster-wide
// copy of the same name — and the winner says so, because a tenant that
// overrode a package otherwise has no way to know it did.
func TestTheTenantRootShadowsTheClusterRootAndSaysSo(t *testing.T) {
	system := catalogRoot(t, "strategy: rolling")

	tenant := t.TempDir()
	pkg := filepath.Join(tenant, "acme")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "pig-ops.yaml"), []byte(`apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: acme
  version: 9.9.9
  vendor: acme
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  install:
    strategy: pin
    min_edge_version: 0.9.0
    min_pig_version: 0.4.0
  tools:
    - { name: acme.probe, class: read }
`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	uc := NewUsecase(nil, nil, nil, Config{
		SystemSkillsRoot: system,
		TenantSkillsRoot: func(uint64) string { return tenant },
	}, nil)

	entries, err := uc.Catalog(context.Background(), catalogCaller())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries; one name in two roots is one package, not two", len(entries))
	}
	if entries[0].Version != "9.9.9" {
		t.Errorf("version = %q; the cluster-wide 1.0.0 won over the tenant's own copy", entries[0].Version)
	}
	if entries[0].Origin != pluginmanifest.OriginTenant {
		t.Errorf("origin = %q, want %q", entries[0].Origin, pluginmanifest.OriginTenant)
	}
	if !entries[0].Shadowed {
		t.Error("the row is not marked shadowed; the tenant cannot tell it overrides a cluster package")
	}
}

// A single-tenant deployment has one root, and targetRoot returns the system
// root for it. Listing that root twice would mark every one of its rows
// shadowed — an index claiming everything here overrides something while
// overriding nothing.
func TestASingleTenantRootIsNotItsOwnShadow(t *testing.T) {
	root := catalogRoot(t, "strategy: rolling")
	uc := catalogUsecase(root)

	entries, err := uc.Catalog(context.Background(), catalogCaller())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries, want 1", len(entries))
	}
	if entries[0].Shadowed {
		t.Error("the only row is marked shadowed by nothing")
	}
}

// A root that does not exist is not an error. Answering 500 because tenant 7
// has not installed anything teaches the SPA to render a failure where the
// truth is "empty".
func TestCatalogSkipsAbsentRootsWithoutFailing(t *testing.T) {
	uc := NewUsecase(nil, nil, nil, Config{
		SystemSkillsRoot: catalogRoot(t, "strategy: rolling"),
		TenantSkillsRoot: func(uint64) string {
			return filepath.Join(t.TempDir(), "never-created")
		},
		BuiltinSkillsRoots: []string{filepath.Join(t.TempDir(), "also-never")},
	}, nil)

	entries, err := uc.Catalog(context.Background(), catalogCaller())
	if err != nil {
		t.Fatalf("Catalog with two absent roots: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("%d entries; the absent roots should have been skipped silently", len(entries))
	}
}
