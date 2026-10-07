package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree writes a set of files into a temp dir and returns its path.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func run(t *testing.T, dir string) *result {
	t.Helper()
	pkgs, err := scan([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	return analyse(pkgs)
}

// find returns the package record for a directory suffix.
func find(res *result, suffix string) *pkg {
	for _, p := range append(append([]*pkg{}, res.unreferenced...), append(res.suites, res.testOnly...)...) {
		if strings.HasSuffix(p.dir, suffix) {
			return p
		}
	}
	return nil
}

func isCandidate(res *result, suffix string) bool {
	for _, p := range res.unreferenced {
		if strings.HasSuffix(p.dir, suffix) {
			return true
		}
	}
	return false
}

func isSuite(res *result, suffix string) bool {
	for _, p := range res.suites {
		if strings.HasSuffix(p.dir, suffix) {
			return true
		}
	}
	return false
}

func isEntryPoint(res *result, suffix string) bool {
	for _, p := range res.entryPoints {
		if strings.HasSuffix(p.dir, suffix) {
			return true
		}
	}
	return false
}

// TestAPackageSomeProductionFileImportsIsNotReported is the base case: the
// walk has to see an import edge at all before any of its tiers apply.
func TestAPackageSomeProductionFileImportsIsNotReported(t *testing.T) {
	res := run(t, tree(t, map[string]string{
		"go.mod": "module example.com/x\n\ngo 1.21\n",
		"a/a.go": "package a\n\nfunc Helper() int { return 1 }\n",
		"b/b.go": "package b\n\nimport \"example.com/x/a\"\n\nfunc Use() int { return a.Helper() }\n",
	}))
	if isCandidate(res, "/a") || isSuite(res, "/a") {
		t.Fatal("package a is imported by b.go and was reported anyway")
	}
	if find(res, "/a") != nil {
		t.Fatal("package a should not appear in any reported tier")
	}
}

// TestAPackageNothingImportsIsADeletionCandidate is the answer stage 3 needs:
// an entire directory can go, and no file inside it has to be read to know
// that.
func TestAPackageNothingImportsIsADeletionCandidate(t *testing.T) {
	res := run(t, tree(t, map[string]string{
		"go.mod":      "module example.com/x\n\ngo 1.21\n",
		"b/b.go":      "package b\n\nfunc Use() int { return 2 }\n",
		"orphan/o.go": "package orphan\n\nfunc Nobody() int { return 3 }\n",
	}))
	if !isCandidate(res, "/orphan") {
		t.Fatal("orphan is imported by nothing and has no tests, so it is a deletion candidate")
	}
}

// TestAPackageOnlyItsOwnTestsRunIsASuiteNotDeadWeight pins the tier that keeps
// this tool from telling someone to delete a check. core/pig/pigcontract is
// in the tree precisely to pin a shape; nothing imports it, and `go test` is
// what runs it. Deleting it would remove a guarantee, not remove weight.
func TestAPackageOnlyItsOwnTestsRunIsASuiteNotDeadWeight(t *testing.T) {
	res := run(t, tree(t, map[string]string{
		"go.mod":             "module example.com/x\n\ngo 1.21\n",
		"b/b.go":             "package b\n\nfunc Use() int { return 2 }\n",
		"contract/c.go":      "package contract\n\nfunc Shape() string { return \"shape\" }\n",
		"contract/c_test.go": "package contract\n\nimport \"testing\"\n\nfunc TestShape(t *testing.T) {\n\tif Shape() != \"shape\" {\n\t\tt.Fail()\n\t}\n}\n",
	}))
	if !isSuite(res, "/contract") {
		t.Fatal("contract is imported by nothing but runs its own tests, so it is a suite")
	}
	if isCandidate(res, "/contract") {
		t.Fatal("contract must not be a deletion candidate")
	}
}

// TestAnEntryPointIsNotADeletionCandidate is what the first version got
// wrong: it counted 60 packages, and the top of the list was every binary and
// tool in the repository. Nothing is supposed to import `cmd/opskeeper`.
func TestAnEntryPointIsNotADeletionCandidate(t *testing.T) {
	res := run(t, tree(t, map[string]string{
		"go.mod":           "module example.com/x\n\ngo 1.21\n",
		"cmd/tool/main.go": "package main\n\nfunc main() {}\n",
		"b/b.go":           "package b\n\nfunc Use() int { return 2 }\n",
	}))
	if !isEntryPoint(res, "/cmd/tool") {
		t.Fatal("a directory with func main is an entry point")
	}
	if isCandidate(res, "/cmd/tool") {
		t.Fatal("an entry point must not be a deletion candidate")
	}
}

// TestAPackageOnlyATestImportsIsItsOwnTier keeps "a test imports it" apart
// from "nothing imports it": the first means somebody wrote down what it was
// for.
func TestAPackageOnlyATestImportsIsItsOwnTier(t *testing.T) {
	res := run(t, tree(t, map[string]string{
		"go.mod":      "module example.com/x\n\ngo 1.21\n",
		"lib/l.go":    "package lib\n\nfunc Thing() int { return 1 }\n",
		"b/b.go":      "package b\n\nfunc Use() int { return 2 }\n",
		"b/b_test.go": "package b\n\nimport (\n\t\"testing\"\n\n\t\"example.com/x/lib\"\n)\n\nfunc TestThing(t *testing.T) {\n\tif lib.Thing() != 1 {\n\t\tt.Fail()\n\t}\n}\n",
	}))
	if !isTestOnly(res, "/lib") {
		t.Fatal("lib is imported by a test file only, so it belongs in the test-only tier")
	}
	if isCandidate(res, "/lib") {
		t.Fatal("lib must not be a deletion candidate")
	}
}

// TestANestedModuleResolvesItsOwnImportPath is why modpath is shared with
// deadcode rather than reimplemented here: this repository has 14 modules,
// and a lookup that walks to the repository root instead of the nearest
// go.mod would mis-resolve every one of them.
func TestANestedModuleResolvesItsOwnImportPath(t *testing.T) {
	res := run(t, tree(t, map[string]string{
		"go.mod":         "module example.com/root\n\ngo 1.21\n",
		"outer/o.go":     "package outer\n\nimport \"example.com/inner/lib\"\n\nfunc Use() int { return lib.Thing() }\n",
		"inner/go.mod":   "module example.com/inner\n\ngo 1.21\n",
		"inner/lib/l.go": "package lib\n\nfunc Thing() int { return 1 }\n",
	}))
	if isCandidate(res, "inner/lib") {
		t.Fatal("inner/lib is imported by outer across a module boundary and must not be reported")
	}
}

func isTestOnly(res *result, suffix string) bool {
	for _, p := range res.testOnly {
		if strings.HasSuffix(p.dir, suffix) {
			return true
		}
	}
	return false
}
