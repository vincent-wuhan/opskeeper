package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The census is only worth printing if it can go red. Each test below builds
// the smallest tree that satisfies every predicate, then breaks exactly one
// thing and asserts the corresponding item reports a shortfall. A predicate
// that survives its own mutation is decoration.

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// routesFixture mirrors the six routes the control plane actually registers,
// one line per route, because the predicate counts registrations and a
// fixture that invented a seventh would be testing the fixture.
func routesFixture() string {
	lines := []string{
		`r.With(h.requireAdmin).Post("/v1/plugins/releases", h.start)`,
		`r.With(h.requireAdmin).Get("/v1/plugins/releases", h.list)`,
		`r.With(h.requireAdmin).Get("/v1/plugins/releases/{name}", h.status)`,
		`r.With(h.requireAdmin).Post("/v1/plugins/releases/{name}/advance", h.advance)`,
		`r.With(h.requireAdmin).Post("/v1/plugins/releases/{name}/halt", h.halt)`,
		`r.With(h.requireAdmin).Post("/v1/plugins/releases/{name}/rollback", h.rollback)`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func pkgManifest(name, version, safety string, approval bool, tools ...[2]string) string {
	var b strings.Builder
	b.WriteString("apiVersion: opskeeper.io/v1\nkind: Plugin\nmetadata:\n  name: " + name + "\n  version: " + version + "\n  vendor: opskeeper\n")
	b.WriteString("spec:\n  targets: [edge]\n  safety_level: " + safety + "\n")
	if approval {
		b.WriteString("  approval:\n    required: true\n    blast_radius: pod\n")
	} else {
		b.WriteString("  approval:\n    required: false\n")
	}
	b.WriteString("  tools:\n")
	for _, t := range tools {
		b.WriteString("    - { name: " + t[0] + ", class: " + t[1] + " }\n")
	}
	return b.String()
}

// fixture writes a tree where every predicate holds, so a test only has to
// describe the one thing it breaks.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"opskeeper-sre-readonly", "opskeeper-sre-observability", "opskeeper-sre-middleware"} {
		write(t, root, filepath.Join("plugins", "pig-ops", name, "pig-ops.yaml"),
			pkgManifest(name, "0.1.0", "L1", false, [2]string{"probe_thing", "read"}))
	}
	write(t, root, "plugins/pig-ops/opskeeper-sre-repair/pig-ops.yaml",
		pkgManifest("opskeeper-sre-repair", "0.1.0", "L2", true, [2]string{"apply_change", "write"}))
	write(t, root, "plugins/pig-ops/opskeeper-sre-autonomy/pig-ops.yaml",
		pkgManifest("opskeeper-sre-autonomy", "0.1.0", "L3", true, [2]string{"recovery.execute", "destructive"}))

	write(t, root, "core/domains/server/plugin/http.go", routesFixture())
	write(t, root, "core/manager/biz/marketplace/signature.go", "package marketplace\n\nfunc VerifySignature(packPath string, expectedKey string) (SignatureState, error) { return 0, nil }\n")
	write(t, root, "core/edge/policygate/gate.go", "package policygate\n\nfunc (g *Gate) Admit(ctx context.Context, c Call) (Outcome, string, error) { return 0, \"\", nil }\n")
	write(t, root, "core/manager/server/marketplace/import.go", "package marketplace\n\nfunc (h *Handler) importContainer(w http.ResponseWriter, r *http.Request) {}\n")
	write(t, root, "core/manager/biz/pluginimport/importer.go", "package pluginimport\n\nfunc (i *Importer) Import(opts Options) (*Report, error) { return nil, nil }\n")
	for _, f := range []string{"manifest.go", "register.go", "negotiate.go"} {
		write(t, root, filepath.Join("sdk", f), "package sdk\n")
	}
	write(t, root, "core/floor/pluginmanifest/coverage.go",
		"package pluginmanifest\n\ntype GapReason struct{ Reason string; Searched []string }\n")
	write(t, root, "core/floor/pluginmanifest/coverage_test.go",
		"package pluginmanifest\n\nfunc gapReasonFailures(root, name string, gap GapReason) []string { return nil }\n\nfunc TestAGapReasonRulesRejectEachHistoricalError(t *testing.T) {}\n")
	write(t, root, "core/domains/service/plugin/compatibility.go",
		"package plugin\n\nfunc (m *Manager) Compatibility(ctx context.Context, req Requirement) (Matrix, error) { return Matrix{}, nil }\n")
	write(t, root, "core/floor/pluginmanifest/catalog_test.go",
		"package pluginmanifest\n\nfunc TestEveryShippedPackageDeclaresBothHostFloors(t *testing.T) {}\n")
	write(t, root, "core/floor/pluginmanifest/manifest.go",
		"package pluginmanifest\n\nfunc LoadCatalogSources(remote []Plugin, roots ...Root) (Catalog, error) { return Catalog{}, nil }\n")

	write(t, root, "core/manager/biz/marketplace/usecase.go", "package marketplace\n\nfunc (uc *Usecase) Catalog(ctx context.Context, caller Caller) ([]pluginmanifest.Entry, error) { return nil, nil }\n\nfunc (uc *Usecase) catalogRoots(tenantID uint64) []pluginmanifest.Root { return nil }\n")
	svcHTTP := routesFixture() + "\nr.Get(\"/v1/marketplace/catalog\", h.catalog)\n"
	write(t, root, "core/manager/server/marketplace/http.go", svcHTTP)
	// D16's four points: the document, the producer, the loader, and the
	// production wiring. All four are needed for the cell to hold, so all
	// four appear here and each gets its own mutation below.
	write(t, root, "core/floor/pluginmanifest/index.go",
		"package pluginmanifest\n\nfunc ParseIndex(data []byte) (Index, error) { return Index{}, nil }\n")
	write(t, root, "core/floor/pluginmanifest/catalog.go", "package pluginmanifest\n\nconst (\n\tOriginTenant = \"tenant\"\n\tOriginSystem = \"system\"\n\tOriginBuiltin = \"builtin\"\n\tOriginRegistry = \"registry\"\n)\n")
	write(t, root, "scripts/registryindex/main.go",
		"package main\n\nfunc build(registry, baseURL, root string) (pluginmanifest.Index, error) { return pluginmanifest.Index{}, nil }\n")
	write(t, root, "cmd/opskeeper/main.go", "package main\n\nconst _ = \"OPSKEEPER_MARKETPLACE_REGISTRIES\"\n")
	// D13, D15 and D17 all read the same real file, so the fixture carries
	// all of them; a second fixture file for the same path would mean one of
	// the cells is tested against a tree that does not exist.
	write(t, root, "core/manager/biz/marketplace/usecase.go", usecaseFixture(""))

	write(t, root, "Makefile", "eval-gates:\npig-tool-scoping-check:\n")
	write(t, root, "scripts/sync-pig-ops.sh", "#!/bin/sh\n")
	return root
}

// usecaseFixture builds the marketplace usecase fixture with one D17 element
// optionally left out.
//
// D17's needles all live in one file, so a mutation that replaces the whole
// file proves none of them individually — the cell would go red for a reason
// unrelated to the needle a reader came to inspect, which is the same
// vacuousness this file's other mutation cases are written against. Naming
// the element to omit makes each of them demonstrably load-bearing.
func usecaseFixture(omit string) string {
	keep := func(key string) bool { return omit != key }

	var b strings.Builder
	b.WriteString("package marketplace\n\n")
	// D13 and D15 read this file too.
	b.WriteString("func (uc *Usecase) Catalog(ctx context.Context, caller Caller) " +
		"([]pluginmanifest.Entry, error) { return nil, nil }\n\n")
	b.WriteString("func (uc *Usecase) catalogRoots(tenantID uint64) []pluginmanifest.Root { return nil }\n\n")

	if keep("resolve") {
		b.WriteString("func (uc *Usecase) resolveRegistryItem(ctx context.Context, src Source) " +
			"(pluginmanifest.IndexItem, error) { return pluginmanifest.IndexItem{}, nil }\n\n")
	}
	if keep("verify") {
		b.WriteString("func (uc *Usecase) verifyRegistryPackage(dir string, item pluginmanifest.IndexItem) error {\n")
		if keep("digest") {
			b.WriteString("\tgot, err := pluginmanifest.TreeDigest(dir)\n\t_, _ = got, err\n")
		}
		if keep("manifest-read") {
			b.WriteString("\tshipped, err := os.ReadFile(filepath.Join(dir, \"pig-ops.yaml\"))\n\t_ = shipped\n\t_ = err\n")
		}
		if keep("manifest-compare") {
			b.WriteString("\tif string(shipped) != item.ManifestYAML {\n\t\treturn nil\n\t}\n")
		}
		b.WriteString("\treturn nil\n}\n\n")
	}
	if keep("call") {
		b.WriteString("func (uc *Usecase) fetch(ctx context.Context, src Source) {\n" +
			"\titem, err := uc.resolveRegistryItem(ctx, src)\n\t_, _ = item, err\n}\n")
	}
	return b.String()
}

func byID(t *testing.T, id string) item {
	t.Helper()
	for _, it := range items() {
		if it.id == id {
			return it
		}
	}
	t.Fatalf("no item %s", id)
	return item{}
}

func assertHolds(t *testing.T, root, id string) {
	t.Helper()
	short, err := byID(t, id).check(root)
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", id, err)
	}
	if short != "" {
		t.Fatalf("%s should hold on the fixture, got: %s", id, short)
	}
}

func assertBreaks(t *testing.T, root, id, rel, body string) {
	t.Helper()
	if rel != "" {
		write(t, root, rel, body)
	}
	short, err := byID(t, id).check(root)
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", id, err)
	}
	if short == "" {
		t.Fatalf("%s still holds after breaking %s — the predicate is decoration", id, rel)
	}
}

func TestFixtureIsGreen(t *testing.T) {
	root := fixture(t)
	for _, it := range items() {
		assertHolds(t, root, it.id)
	}
}

func TestEveryPredicateCanGoRed(t *testing.T) {
	cases := []struct {
		id, rel, body string
	}{
		{"D1", "plugins/pig-ops/opskeeper-sre-readonly/pig-ops.yaml",
			"apiVersion: opskeeper.io/v2\nkind: Plugin\nmetadata:\n  name: opskeeper-sre-readonly\n  version: 0.1.0\nspec:\n  tools: []\n"},
		{"D2", "plugins/pig-ops/opskeeper-sre-observability/pig-ops.yaml",
			pkgManifest("opskeeper-sre-observability", "0.1.0", "L1", false, [2]string{"query_promql", "read"}, [2]string{"restart_thing", "write"})},
		{"D3", "plugins/pig-ops/opskeeper-sre-autonomy/pig-ops.yaml",
			pkgManifest("opskeeper-sre-autonomy", "0.1.0", "L2", true, [2]string{"recovery.execute", "write"})},
		{"D4", "plugins/pig-ops/opskeeper-sre-repair/pig-ops.yaml",
			pkgManifest("opskeeper-sre-repair", "0.1.0", "L2", false, [2]string{"apply_change", "write"})},
		{"D5", "core/domains/server/plugin/http.go",
			"\tr.With(h.requireAdmin).Post(\"/v1/plugins/releases\", h.start)\n"},
		{"D6", "core/manager/biz/marketplace/signature.go", "package marketplace\n"},
		{"D7", "core/edge/policygate/gate.go", "package policygate\n"},
		{"D8", "core/manager/server/marketplace/import.go", "package marketplace\n"},
		{"D9", "sdk/negotiate.go", ""},
		{"D10", "core/floor/pluginmanifest/coverage.go", "package pluginmanifest\n"},
		{"D10", "core/floor/pluginmanifest/coverage_test.go", "package pluginmanifest\n"},
		{"D11", "Makefile", "eval-gates:\n"},
		{"D12", "scripts/sync-pig-ops.sh", ""},
		{"D13", "core/manager/server/marketplace/http.go", routesFixture()},
		{"D13", "core/manager/biz/marketplace/usecase.go", "package marketplace\n"},
		{"D14", "core/domains/service/plugin/compatibility.go", "package plugin\n"},
		{"D14", "core/floor/pluginmanifest/catalog_test.go", "package pluginmanifest\n"},
		{"D15", "core/floor/pluginmanifest/manifest.go", "package pluginmanifest\n"},
		{"D15", "core/floor/pluginmanifest/catalog.go", "package pluginmanifest\n"},
		{"D15", "core/manager/biz/marketplace/usecase.go", "package marketplace\n"},
		{"D16", "core/floor/pluginmanifest/index.go", "package pluginmanifest\n"},
		{"D16", "scripts/registryindex/main.go", "package main\n"},
		{"D16", "core/floor/pluginmanifest/manifest.go", "package pluginmanifest\n"},
		{"D16", "core/floor/pluginmanifest/catalog.go", "package pluginmanifest\n"},
		{"D16", "cmd/opskeeper/main.go", "package main\n"},
		{"D17", "core/manager/biz/marketplace/usecase.go", usecaseFixture("resolve")},
		{"D17", "core/manager/biz/marketplace/usecase.go", usecaseFixture("verify")},
		{"D17", "core/manager/biz/marketplace/usecase.go", usecaseFixture("digest")},
		{"D17", "core/manager/biz/marketplace/usecase.go", usecaseFixture("manifest-read")},
		{"D17", "core/manager/biz/marketplace/usecase.go", usecaseFixture("manifest-compare")},
		{"D17", "core/manager/biz/marketplace/usecase.go", usecaseFixture("call")},
	}
	// One item may carry more than one mutation — D10 broke the reason type
	// and then the rule's proof — so this checks coverage of the items
	// rather than an equality of counts.
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.id] = true
	}
	for _, it := range items() {
		if !covered[it.id] {
			t.Errorf("item %s has no mutation case", it.id)
		}
	}
	for _, tc := range cases {
		t.Run(tc.id+"/"+filepath.Base(tc.rel), func(t *testing.T) {
			root := fixture(t)
			if tc.body == "" {
				if err := os.Remove(filepath.Join(root, tc.rel)); err != nil {
					t.Fatalf("remove %s: %v", tc.rel, err)
				}
				short, err := byID(t, tc.id).check(root)
				if err != nil {
					t.Fatalf("%s: unexpected error: %v", tc.id, err)
				}
				if short == "" {
					t.Fatalf("%s still holds after deleting %s", tc.id, tc.rel)
				}
				return
			}
			assertBreaks(t, root, tc.id, tc.rel, tc.body)
		})
	}
}

// A package added to the roster is a claim about the ecosystem, and a claim
// nobody checked is the failure mode this file exists to end. So the census
// refuses to run when the directory and the roster disagree.
func TestRosterMatchesTheTree(t *testing.T) {
	entries, err := os.ReadDir("../../plugins/pig-ops")
	if err != nil {
		t.Fatalf("read plugins/pig-ops: %v", err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			found[e.Name()] = true
		}
	}
	for _, name := range roster {
		if !found[name] {
			t.Errorf("rostered package %s is not on disk", name)
		}
		delete(found, name)
	}
	var extra []string
	for name := range found {
		extra = append(extra, name)
	}
	if len(extra) > 0 {
		t.Errorf("packages on disk but not on the roster: %v — add them on purpose", extra)
	}
}

// The census is run against this repository as part of the suite, so a
// package deleted or a route removed fails the build rather than quietly
// lowering a number in a document nobody re-reads.
func TestCensusOfThisTreeIsGreen(t *testing.T) {
	root := "../.."
	for _, it := range items() {
		short, err := it.check(root)
		if err != nil {
			t.Errorf("%s %s: %v", it.id, it.subject, err)
			continue
		}
		if short != "" {
			t.Errorf("%s %s: %s", it.id, it.subject, short)
		}
	}
}
