package dbx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
)

// One place opens the control plane's database.
//
// The pool ceilings this package applies are only as good as being the only
// way in. A second gorm.Open anywhere under core/manager produces a handle
// that never sees DBPoolConfig, and nothing about that handle announces
// itself: it looks exactly like the tuned one until the first time it
// saturates the server's connection budget. That is the shape of the defect
// this package's own history had — the knobs were read, defaulted,
// documented, and dropped, so no test was red anywhere.
//
// The walk is deliberately a parse, not a grep. A grep for "gorm.Open"
// matches the word inside a comment and misses an aliased import; a parse
// answers the question actually being asked, which is which files call it.

// openPathsAllowed is every gorm.Open under core/manager that is NOT this
// package, each with the reason it is allowed to exist.
//
// Every entry is a debt with a name. The list is compared for exact set
// equality in BOTH directions: closing one of these without deleting its
// line fails the test, so an entry cannot outlive the gap it excuses, and
// adding a fourth fails immediately rather than at the next review that
// nobody remembers to do.
var openPathsAllowed = map[string]string{
	"core/manager/higress/store.go": "higress console consumer database: a separate file " +
		"behind a standalone binary, not the control plane's handle. It has no " +
		"pool ceiling and no sqlite pragmas, which is a hardening gap of its " +
		"own rather than a claim that it is tuned.",

	"cmd/opskeeper-migrate-runtime/main.go": "the migration binary " +
		"runs alone before anything else is up, opens each dialect itself so it " +
		"can migrate a database whose schema the current build cannot open yet.",

	"cmd/repair-preview-runner/main.go": "one-shot preview tool, " +
		"reads a target control database and exits.",

	"cmd/incident-seed/main.go": "one-shot seeding tool, writes a " +
		"target database and exits.",
}

// TestOnlyDbxOpensTheControlPlaneDatabase is the gate.
//
// It is a test rather than a script because it is a property of the tree,
// not of a release: `go test ./...` in the manager module is the command
// that already runs on every change, and a rule that lives anywhere else is
// a rule that stops being run.
func TestOnlyDbxOpensTheControlPlaneDatabase(t *testing.T) {
	// The repository root, not the module root: the binaries under cmd/ are
	// the other half of the control plane and three of them open a database
	// themselves. A gate scoped to core/manager would have reported a clean
	// tree over the three handles that matter just as much.
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	// Outside this repository there is nothing for this gate to govern, and
	// a failure here would be about the sandbox rather than about a handle
	// somebody opened. That is the one case where skipping is the honest
	// answer, and it is worth being explicit about why it differs from the
	// delivery gate in tests/e2e: there the subject (a broker container)
	// exists and merely could not be started, whereas here the subject
	// (this tree) is genuinely absent.
	if !isRepositoryRoot(root) {
		t.Skipf("not inside the opskeeper repository (%v); nothing to govern", root)
	}

	found := map[string]string{}
	for _, subtree := range []string{"core/manager", "cmd"} {
		base := filepath.Join(root, filepath.FromSlash(subtree))
		err = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if info.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == "core/base/pkg/dbx/dbx.go" {
				return nil // this file is the one place that is allowed
			}
			if callsGormOpen(path) {
				found[rel] = rel
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", subtree, err)
		}
	}

	for path := range found {
		if _, ok := openPathsAllowed[path]; !ok {
			t.Errorf("%s opens a gorm handle outside dbx, so it never gets the configured pool ceilings.\n"+
				"Either route it through dbx.Open, or add it to openPathsAllowed with a reason.", path)
		}
	}
	for path := range openPathsAllowed {
		if _, ok := found[path]; !ok {
			t.Errorf("openPathsAllowed still excuses %s, which no longer opens a handle.\n"+
				"Delete the line: an allowance that outlives its gap is a comment nobody reads.", path)
		}
	}
}

// callsGormOpen reports whether a file actually calls gorm.Open, resolving
// the selector through the file's imports so a renamed or aliased import is
// still found.
func callsGormOpen(path string) bool {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		// A file that does not parse is a build failure somewhere else;
		// reporting it here would only bury the real error.
		return false
	}

	// Collect the local name each gorm import binds to.
	gormNames := map[string]bool{}
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if p != "gorm.io/gorm" {
			continue
		}
		name := "gorm"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		gormNames[name] = true
	}
	if len(gormNames) == 0 {
		return false
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if gormNames[ident.Name] && sel.Sel.Name == "Open" {
			found = true
		}
		return true
	})
	return found
}

// isRepositoryRoot is the skip condition, pulled out so it can be tested
// rather than asserted. A guard nobody has ever seen fail is a guard nobody
// should trust to be keeping anything out.
//
// It asks the shared marker question (core/floor/reporoot) rather than
// looking for go.work: go.work is gitignored, so in CI and in a release
// build this predicate used to be false at the repository root itself, and
// the gate below skipped itself green in exactly the checkouts it exists to
// protect.
func isRepositoryRoot(dir string) bool {
	return reporoot.IsRoot(dir)
}

func TestTheRepositoryGuardTellsThisRepositoryFromAnyOther(t *testing.T) {
	// The real one, reached the same way the gate reaches it: four levels
	// up from the package directory.
	real, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	if !isRepositoryRoot(real) {
		t.Fatalf("isRepositoryRoot(%s) = false, want true — the gate would skip itself at home", real)
	}

	// Anywhere else. A directory that exists but is not this repository has
	// no subject to govern, and that must read as false rather than as an
	// error.
	elsewhere := t.TempDir()
	if isRepositoryRoot(elsewhere) {
		t.Fatalf("isRepositoryRoot(%s) = true, want false", elsewhere)
	}
	if isRepositoryRoot(filepath.Join(elsewhere, "does", "not", "exist")) {
		t.Fatal("isRepositoryRoot on a missing path = true, want false")
	}
}
