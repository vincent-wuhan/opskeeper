package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
)

// A configuration field is a promise to an operator.
//
// Someone reads the struct, sees the field, sets the variable, deploys, and
// waits for a behaviour change that never arrives. Nothing tells them: the
// loader accepted the value, the struct carries it, and the process starts
// cleanly every time. That is a worse failure than a rejected configuration,
// because it looks like the setting simply had nothing to do today.
//
// This gate is the general form of a bug that already happened here. The
// pool ceilings in DBPoolConfig were read from the environment, given
// defaults, documented, and then dropped on the floor by the one function
// that was supposed to apply them (decision 150). The field-level version of
// the same mistake is a knob nothing ever reads, and the one that shipped is
// TunnelAddr: OPSKEEPER_TUNNEL_ADDR looked exactly like the edge's tunnel
// address, and Edge.CloudAddr — a different variable — was the one that
// actually did it (decision 152).
//
// The rule is narrow on purpose. A field counts as dead only when its every
// mention in the repository is its own declaration or its assignment inside
// this package. Anything else — a read in this package, a read anywhere else,
// a pass-through — keeps it alive. Guessing the other way would flag live
// configuration, and a gate that cries wolf is a gate that gets turned off.
//
// What this gate does NOT see, stated plainly because the alternative is a
// reader assuming more than it should: it counts mentions, not uses. Code
// that mentions a field on a path that never executes still counts as a
// read. Verified: neutering tunePool into an early `return` leaves its
// references in the source and this gate stays green, while
// TestOpenAppliesTheConfiguredPoolCeilings in pkg/dbx goes red. Deleting
// the wiring outright — the shape a real revert takes — is caught by both.
// So the two are complementary: this one catches a knob nobody wired, that
// one catches a knob somebody wired to nothing. Neither replaces reachability
// analysis, and this repository is not going to grow one for a config field.

// configField is one declared field and where it was declared.
type configField struct {
	Struct string
	Name   string
}

// TestEveryConfigFieldIsActuallyRead is the gate.
func TestEveryConfigFieldIsActuallyRead(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	if !isRepositoryRoot(root) {
		t.Skipf("not inside the opskeeper repository (%v); nothing to govern", root)
	}

	fields := declaredFields(t)
	if len(fields) == 0 {
		t.Fatal("no configuration fields were parsed; the gate is not looking at anything")
	}

	// One pass over the tree, tallying every selector read per field name.
	// Counting by name rather than by struct is deliberate: a field name
	// shared by two structs can mask a dead one, but it can never invent a
	// read that is not there. A gate with false negatives is disappointing;
	// a gate with false positives gets deleted.
	reads := map[string]int{}
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "dist", "testdata":
				return filepath.SkipDir
			}
			// web/ carries a vendored SPA; its Go files are not ours.
			if strings.HasSuffix(filepath.ToSlash(path), "/web") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		countReads(t, path, reads)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, f := range fields {
		if reads[f.Name] == 0 {
			t.Errorf("config field %s.%s is never read.\n"+
				"Load() accepts a value for it and nothing consumes it, so setting the\n"+
				"environment variable does nothing and reports no error. Either wire it\n"+
				"up, or delete the field and the Load() line that fills it.", f.Struct, f.Name)
		}
	}
}

// declaredFields reads the struct definitions out of this package.
func declaredFields(t *testing.T) []configField {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse config package: %v", err)
	}
	var out []configField
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			// This gate is about configuration, and a test helper struct is
			// not configuration. Leaving them in made the gate fail on the
			// gate's own file, which is a fast way to teach everyone that
			// a red gate here is noise.
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.TYPE {
					continue
				}
				for _, spec := range gen.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, f := range st.Fields.List {
						if len(f.Names) == 0 {
							continue
						}
						for _, name := range f.Names {
							out = append(out, configField{Struct: ts.Name.Name, Name: name.Name})
						}
					}
				}
			}
		}
	}
	return out
}

// countReads tallies selector expressions in one file, skipping the ones
// that are an assignment target rather than a read.
//
// `c.DB.Pool.MaxOpen = getEnvInt(...)` mentions the field and uses nothing.
// `db.Pool.MaxOpen` uses it. Counting both alike would let a field that is
// only ever filled in pass, which is precisely the case this gate is for.
func countReads(t *testing.T, path string, reads map[string]int) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		// A file that does not parse fails the build elsewhere; saying so
		// here would bury the real error under a confusing one.
		return
	}

	// Positions used as an assignment target anywhere in this file.
	assigned := map[token.Pos]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			if sel, ok := lhs.(*ast.SelectorExpr); ok {
				assigned[sel.Sel.Pos()] = true
			}
		}
		return true
	})

	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if assigned[sel.Sel.Pos()] {
			return true
		}
		// A field's own name appearing in a struct tag is a document, not
		// a use, and one that changes whenever the comment is reworded.
		reads[sel.Sel.Name]++
		return true
	})
}

// isRepositoryRoot reports whether dir is the OpsKeeper repository root.
//
// It used to ask for go.work, which is gitignored and therefore absent from
// exactly the checkouts where the question matters — CI and a release build.
// "No go.work" then read as "not this repository" and the whole gate skipped
// itself green. The markers it asks for now are tracked (see
// core/floor/reporoot), so the answer is the same on a developer machine and
// in a clean clone.
func isRepositoryRoot(dir string) bool {
	return reporoot.IsRoot(dir)
}

func TestTheRepositoryGuardTellsThisRepositoryFromAnyOther(t *testing.T) {
	real, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	if !isRepositoryRoot(real) {
		t.Fatalf("isRepositoryRoot(%s) = false, want true — the gate would skip itself at home", real)
	}
	elsewhere := t.TempDir()
	if isRepositoryRoot(elsewhere) {
		t.Fatalf("isRepositoryRoot(%s) = true, want false", elsewhere)
	}
}

// The counting has one subtlety in it and it deserves a test of its own:
// the difference between being filled in and being used. A field assigned
// by a statement mentions itself, and a gate that cannot tell that apart
// would pass the exact bug it was written for.
func TestAnAssignmentIsNotARead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.go")
	src := `package sample

type Cfg struct{ MaxOpen int }

func fill(c *Cfg) *Cfg {
	c.MaxOpen = 25
	return c
}

func use(c *Cfg) int { return c.MaxOpen }
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	reads := map[string]int{}
	countReads(t, path, reads)
	if reads["MaxOpen"] != 1 {
		t.Fatalf("MaxOpen reads = %d, want 1 (the assignment target does not count)", reads["MaxOpen"])
	}
}
