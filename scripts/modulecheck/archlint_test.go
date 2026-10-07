package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archLintFixture builds a repository root with a .go-arch-lint.yml and the
// source files the test names.
//
// The yml is written inline rather than derived from the real one. A test
// that copies the production config cannot fail in the way these tests need
// to fail: every component in it is used, so a rule about unused grants
// would have nothing to say, and the test would pass whether or not the
// check works.
func archLintFixture(t *testing.T, yml string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".go-arch-lint.yml"), []byte(yml), 0o644); err != nil {
		t.Fatalf("write yml: %v", err)
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(rel), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

const twoLayerYml = `version: 3
workdir: .
allow:
  depOnAnyVendor: true
components:
  manager_biz:    { in: core/manager/biz/** }
  manager_data:   { in: core/manager/data/** }
  manager_service: { in: core/manager/service/** }
deps:
  manager_biz:
    mayDependOn: [manager_data, manager_service]
  manager_data:
    mayDependOn: [manager_biz]
  manager_service:
    mayDependOn: [manager_biz]
`

// The two failures this check exists to catch, each exercised in both
// directions: reported when it should be, silent when it should not be. A
// gate that fires on everything is as useless as one that fires on nothing,
// and only the negative cases tell the two apart.

func TestAnUpwardEdgeWithNoLedgerEntryIsReported(t *testing.T) {
	root := archLintFixture(t, twoLayerYml, map[string]string{
		"core/manager/biz/incident/usecase.go": "package incident\n\nimport (\n\t\"context\"\n\n\t\"github.com/vincent-wuhan/opskeeper/core/manager/service/incident\"\n)\n\nvar _ = incident.New\nvar _ = context.Background\n",
		"core/manager/service/incident/svc.go": "package incident\n\nvar New = 1\n",
	})
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint: %v", err)
	}
	if !containsSubstring(got, "core/manager/biz/incident/usecase.go") ||
		!containsSubstring(got, "layerInversion") {
		t.Errorf("an unlisted biz → service import must name the file and the ledger; got %v", got)
	}
}

// data → biz is the *intended* direction: biz declares the interface and
// data implements it. A rank function would flag it, which is why the rule
// is a written-out list rather than a computed one — and this test is what
// keeps that list honest.
func TestTheIntendedDataToBizDirectionIsNotAnUpwardEdge(t *testing.T) {
	root := archLintFixture(t, twoLayerYml, map[string]string{
		"core/manager/data/incident/store.go": "package incident\n\nimport \"github.com/vincent-wuhan/opskeeper/core/manager/biz/incident\"\n\nvar _ incident.Port\n",
		"core/manager/biz/incident/port.go":   "package incident\n\ntype Port interface{ Get() string }\n",
	})
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint: %v", err)
	}
	for _, v := range got {
		if strings.Contains(v, "layerInversion") {
			t.Errorf("data → biz is the intended direction and must not be an inversion: %s", v)
		}
	}
}

func TestADeadGrantIsReported(t *testing.T) {
	root := archLintFixture(t, twoLayerYml, map[string]string{
		"core/manager/service/incident/svc.go": "package incident\n\nvar New = 1\n",
	})
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint: %v", err)
	}
	// manager_service mayDependOn manager_biz, and nothing imports it.
	if !containsSubstring(got, "manager_service mayDependOn manager_biz") {
		t.Errorf("a grant nothing uses must be reported; got %v", got)
	}
}

func TestAGrantWithARealImportIsNotReported(t *testing.T) {
	root := archLintFixture(t, twoLayerYml, map[string]string{
		"core/manager/service/incident/svc.go": "package incident\n\nimport \"github.com/vincent-wuhan/opskeeper/core/manager/biz/incident\"\n\nvar _ incident.Port\n",
		"core/manager/biz/incident/port.go":    "package incident\n\ntype Port interface{ Get() string }\n",
	})
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint: %v", err)
	}
	for _, v := range got {
		if strings.Contains(v, "manager_service mayDependOn manager_biz") {
			t.Errorf("a grant with a real import must not be reported: %s", v)
		}
	}
}

// The symmetric half: a permission nobody exercises, and a dependency
// nobody permitted, are the same omission seen from two ends. Only the first
// end was checked.

const noGrantsYml = `version: 3
workdir: .
allow:
  depOnAnyVendor: true
components:
  manager_biz:    { in: core/manager/biz/** }
  manager_data:   { in: core/manager/data/** }
  manager_service: { in: core/manager/service/** }
deps:
  manager_biz:
    anyVendorDeps: true
  manager_data:
    anyVendorDeps: true
`

func TestAnImportNoRulePermitsIsReported(t *testing.T) {
	root := archLintFixture(t, noGrantsYml, map[string]string{
		"core/manager/biz/incident/usecase.go": "package incident\n\nimport \"github.com/vincent-wuhan/opskeeper/core/manager/data/incident\"\n\nvar _ = incident.NewRepo\n",
		"core/manager/data/incident/repo.go":   "package incident\n\nvar NewRepo = 1\n",
	})
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint: %v", err)
	}
	// manager_biz declares no project grants at all, so the edge is
	// unpermitted no matter which direction it points.
	if !containsSubstring(got, "no rule in .go-arch-lint.yml permits") {
		t.Errorf("an import nothing permits must be reported; got %v", got)
	}
}

func TestAPermittedImportIsNotReportedByTheSymmetricCheck(t *testing.T) {
	root := archLintFixture(t, twoLayerYml, map[string]string{
		"core/manager/service/incident/svc.go": "package incident\n\nimport \"github.com/vincent-wuhan/opskeeper/core/manager/biz/incident\"\n\nvar _ incident.Port\n",
		"core/manager/biz/incident/port.go":    "package incident\n\ntype Port interface{ Get() string }\n",
	})
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint: %v", err)
	}
	// Only this check is under test. The fixture's other two grants are
	// unused on purpose, so check 2 has something true to say about them
	// and a blanket "no violations" assertion would be testing the
	// fixture rather than the rule.
	for _, v := range got {
		if strings.Contains(v, "no rule in .go-arch-lint.yml permits") {
			t.Errorf("a granted import with a real use must not be reported: %s", v)
		}
	}
}

// A component emptied to "may depend on nothing" is where the missing check
// would have bitten first. go-arch-lint's schema refuses an empty
// mayDependOn, so those five components carry anyVendorDeps and no grants —
// and before the symmetric check existed, adding a cross-component import to
// one of them drew no complaint from this tool at all.
func TestAComponentWithNoGrantsThatGainsAnImportIsReported(t *testing.T) {
	// data -> biz is the *intended* direction: biz declares the interface
	// and data implements it, so it is deliberately absent from
	// upwardEdges. That makes this the one crossing in the fixture that
	// only the symmetric check can see — point the edge the other way and
	// check 1 reports it first, and this test would pass with check 3
	// deleted, which is the kind of test that looks like coverage and is
	// not.
	root := archLintFixture(t, noGrantsYml, map[string]string{
		"core/manager/data/incident/repo.go": "package incident\n\nimport \"github.com/vincent-wuhan/opskeeper/core/manager/biz/incident\"\n\nvar _ incident.Port\n",
		"core/manager/biz/incident/port.go":  "package incident\n\ntype Port interface{ Get() string }\n",
	})
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint: %v", err)
	}
	if !containsSubstring(got, "no rule in .go-arch-lint.yml permits") {
		t.Errorf("a new cross-component import into a grantless component must be reported; got %v", got)
	}
}

// Two checks, one mistake. An upward edge with no ledger entry is a
// violation, and both the ledger check and the symmetric check can see it;
// saying so twice with two different suggested fixes is how a gate teaches
// people to read violations instead of fixing them.
func TestAnUpwardEdgeIsReportedOnceNotTwice(t *testing.T) {
	root := archLintFixture(t, noGrantsYml, map[string]string{
		"core/manager/biz/incident/usecase.go": "package incident\n\nimport \"github.com/vincent-wuhan/opskeeper/core/manager/service/incident\"\n\nvar _ = incident.New\n",
		"core/manager/service/incident/svc.go": "package incident\n\nvar New = 1\n",
	})
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint: %v", err)
	}
	var layer, permits int
	for _, v := range got {
		if strings.Contains(v, "a layer above itself") {
			layer++
		}
		if strings.Contains(v, "no rule in .go-arch-lint.yml permits") {
			permits++
		}
	}
	if layer != 1 {
		t.Errorf("the ledger check reported the upward edge %d times, want 1: %v", layer, got)
	}
	if permits != 0 {
		t.Errorf("the symmetric check re-reported an edge the ledger check already owns (%d times): %v", permits, got)
	}
}

// The regression that matters most in this file.
//
// filepath.Walk reports the walk root itself first, and when the tool is
// invoked as `modulecheck .` — which is what the Makefile does and what
// every developer types — that root's base name is ".". A dot-directory
// rule applied to it classifies the whole repository as hidden and skips
// everything, so the walk completes with no error, every component looks
// like it imports nothing, and every grant in the config is reported dead.
//
// The failure is silent, total, and points the opposite way from a real
// problem, which is why it gets its own test rather than being folded into
// the general "the real tree is clean" one: that test passes for a tree
// that was never walked at all.
func TestTheWalkVisitsTheTreeWhenTheRootIsADot(t *testing.T) {
	files := map[string]string{
		"core/manager/service/incident/svc.go": "package incident\n\nimport \"github.com/vincent-wuhan/opskeeper/core/manager/biz/incident\"\n\nvar _ incident.Port\n",
		"core/manager/biz/incident/port.go":    "package incident\n\ntype Port interface{ Get() string }\n",
	}
	root := archLintFixture(t, twoLayerYml, files)

	// The same tree, reached the way the tool is actually invoked.
	dot := archLintFixture(t, twoLayerYml, files)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dot); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	fromAbs, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint(abs): %v", err)
	}
	fromDot, err := checkArchLint(".")
	if err != nil {
		t.Fatalf("checkArchLint(.): %v", err)
	}
	// The two runs must agree exactly. With the walk root skipped they do
	// not: the dot run sees an empty tree and reports every grant dead,
	// while the absolute run sees the two files and reports only the grants
	// that genuinely have no import. That difference is the whole signal.
	if len(fromAbs) == 0 {
		t.Fatal("the absolute-root run found nothing, so there is nothing to compare against")
	}
	if strings.Join(fromDot, "\n") != strings.Join(fromAbs, "\n") {
		t.Errorf("a dot root and an absolute root disagree about the same tree:\n dot: %v\n abs: %v",
			fromDot, fromAbs)
	}
	// The grant that does have a real import is the one the bug erases, so
	// it is named directly rather than left to the set comparison.
	for _, v := range fromDot {
		if strings.Contains(v, "manager_service mayDependOn manager_biz") {
			t.Errorf("a grant with a real import was reported dead under a dot root: %s", v)
		}
	}
}

// A file outside every component is invisible to the rest of this file,
// which is why it has to be reported by name before anything else runs.
//
// Without this the checker says nothing about a package that landed with no
// component: checks 1-3 all start from `from == ""` and skip, so the file is
// exempt from every rule while looking exactly like a file that follows
// them. That is not hypothetical — core/pig/pigmcp arrived that way, and
// only the hand-run linter noticed.
func TestAFileNoComponentClaimsIsReported(t *testing.T) {
	root := archLintFixture(t, twoLayerYml, map[string]string{
		"core/manager/biz/incident/usecase.go": "package incident\n",
		"core/manager/nowhere/orphan.go":       "package nowhere\n\nimport \"github.com/vincent-wuhan/opskeeper/core/manager/service/incident\"\n\nvar _ = incident.New\n",
	})
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint: %v", err)
	}
	if !containsSubstring(got, "core/manager/nowhere/orphan.go is not attached to any component") {
		t.Fatalf("a file no component claims was not reported; got %v", got)
	}
	// And the import it took is reported by nothing else, which is the
	// whole reason this check exists rather than being left to the linter:
	// the file is outside the graph those checks walk.
	for _, v := range got {
		if strings.Contains(v, "orphan.go") && !strings.Contains(v, "not attached to any component") {
			t.Errorf("an unattached file produced a second, different violation: %s", v)
		}
	}
}

// The negative half. A check that fires on every file is as useless as one
// that fires on none, and only this case tells the two apart: a tree whose
// files are all claimed must produce no unattached report even while it
// produces others.
func TestEveryFileInsideAComponentIsNotReportedAsUnattached(t *testing.T) {
	root := archLintFixture(t, twoLayerYml, map[string]string{
		"core/manager/biz/incident/usecase.go": "package incident\n",
		"core/manager/data/incident/store.go":  "package incident\n",
	})
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint: %v", err)
	}
	for _, v := range got {
		if strings.Contains(v, "not attached to any component") {
			t.Fatalf("a file inside a component was reported unattached: %s", v)
		}
	}
}

// The ledger has to be maintained, or it becomes a list of permissions
// nobody can tell apart from live ones — the same bargain layerDebt makes.
func TestTheLayerInversionLedgerIsCurrent(t *testing.T) {
	root := filepath.Join("..", "..")
	for rel, reason := range layerInversion {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is in the inversion ledger with no reason", rel)
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s is in the inversion ledger but cannot be read (%v); remove the entry", rel, err)
			continue
		}
		// The entry is only honest while the file still reaches upward.
		// Checking it that way — against the file's own imports rather than
		// against a hardcoded path list — is what lets the entry survive a
		// package move and still mean what it says.
		cfg, err := loadArchLint(root)
		if err != nil {
			t.Fatalf("loadArchLint: %v", err)
		}
		names := archLintSortedComponents(cfg)
		from := archLintComponentOf(cfg, names, rel)
		if from == "" {
			t.Errorf("%s is in the inversion ledger but no component claims it", rel)
			continue
		}
		fromBC, fromLayer := splitComponent(from)
		stillUpward := false
		for _, imp := range importsOf(filepath.Join(root, filepath.FromSlash(rel))) {
			to := archLintImportComponent(cfg, names, imp)
			if to == "" {
				continue
			}
			toBC, toLayer := splitComponent(to)
			if toBC != fromBC {
				continue
			}
			for _, pair := range upwardEdges {
				if pair[0] == fromLayer && pair[1] == toLayer {
					stillUpward = true
				}
			}
		}
		if !stillUpward {
			t.Errorf("%s no longer takes an upward edge; the debt is paid, so remove the entry", rel)
		}
		_ = src
	}
}

// The real repository is the only place the two gates meet, and a clean run
// here is the evidence that the tightened config describes the code rather
// than a smaller codebase.
func TestTheRealTreeHasNoArchLintViolations(t *testing.T) {
	root := filepath.Join("..", "..")
	got, err := checkArchLint(root)
	if err != nil {
		t.Fatalf("checkArchLint on the real tree: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the real tree violates its own arch-lint rules: %v", got)
	}
}

func containsSubstring(list []string, want string) bool {
	for _, v := range list {
		if strings.Contains(v, want) {
			return true
		}
	}
	return false
}
