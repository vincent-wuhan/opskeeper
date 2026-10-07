package chatruntime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoAliasInThisFileIsUnreferenced is decision 344's guard.
//
// This file is a re-export surface: every name in it is a **second name** for
// something in core/extension/biz/container. That is legitimate exactly once
// — while somebody still writes the first name. Once every caller has moved
// to the package it was re-exported from, the re-export is a fossil: a name
// that resolves, compiles, and is referenced by nothing, and that the next
// reader has to grep for before they learn it is not the real one.
//
// Fourteen of them were exactly that, and the tree's own deadcode report had
// been printing them on one line for decisions without anybody acting on it.
// The report is a measurement; this is a gate, and a measurement that nobody
// acts on is a number nobody reads.
//
// **What counts as a reference, and why it took two tries to get right.**
// Counting identifier *names* anywhere in the module is wrong twice over, and
// both failures were observed rather than imagined:
//
//   - `container.ContainerKind` mentions the name while binding the *other*
//     package's declaration. Counting a selector's Sel says a re-export is
//     alive because the thing it re-exports is alive, which is precisely the
//     case where the re-export is most likely to be a fossil.
//   - `core/domain` declares its own `ContainerKind`. A bare ident there binds
//     *that* one. Name-based counting across packages cannot tell the two
//     apart, so it read a dead alias as a live one.
//
// So the check is scoped, and a reference is one of exactly two things:
//
//  1. a bare identifier inside this package (the package the re-export is for —
//     its own comment says as much), or
//  2. a `chatruntime.<Name>` selector anywhere in the module, which is how an
//     outside caller reaches a re-export.
//
// It does not try to prove the alias is reachable from production: the
// deadcode ratchet already does that, and two gates answering the same
// question with different definitions of "reachable" is the asymmetry the
// modulecheck/arch-lint pair exists to avoid.
func TestNoAliasInThisFileIsUnreferenced(t *testing.T) {
	// The **repository** root, not this module's: the callers of a re-export
	// include cmd/opskeeper, which is a different module entirely. Scoping
	// the walk to core/manager is what made ContainerLoader read as dead
	// while cmd/opskeeper/main.go constructs one.
	moduleRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("abs module root: %v", err)
	}
	pkgDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs package dir: %v", err)
	}

	declared := aliasNames(t, "aliases.go")
	if len(declared) == 0 {
		t.Fatal("aliases.go declares nothing — the guard would pass vacuously")
	}

	inPackage := map[string]int{}
	viaSelector := map[string]int{}

	err = filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		inPkg := strings.HasPrefix(path, pkgDir+string(os.PathSeparator))
		isAliases := filepath.Base(path) == "aliases.go"
		if isAliases {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // a file that does not parse is the compiler's problem
		}

		// `<qualifier>.X` anywhere in the repository: an outside caller. The
		// qualifier is **not** always the package's own name — this tree uses
		// two, `chatruntime` in core/manager and `aiopschatruntime` in
		// cmd/opskeeper where the name collides — so the local name is
		// resolved from each file's import block.
		//
		// Hardcoding either one was tried and **stayed green**, and the reason
		// is worth keeping: `ContainerLoader` has both an unaliased caller
		// (core/manager/server/marketplace/import_test.go) and an aliased one
		// (cmd/opskeeper/main.go:2880), so getting the qualifier wrong is
		// masked by the other caller. Resolving it from the import path is
		// not a fix for an observed failure; it is not being wrong in a way
		// that today has a second witness to hide it. §4.277.3.
		qualifiers := localNamesFor(f, pkgImportPath)
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && qualifiers[x.Name] {
				viaSelector[sel.Sel.Name]++
			}
			return true
		})

		if !inPkg {
			return nil
		}
		// Bare identifiers inside this package only.
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if len(stack) > 0 {
				if sel, ok := stack[len(stack)-1].(*ast.SelectorExpr); ok && sel.Sel == n {
					stack = append(stack, n)
					return true
				}
			}
			if id, ok := n.(*ast.Ident); ok {
				inPackage[id.Name]++
			}
			stack = append(stack, n)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}

	for _, name := range declared {
		if inPackage[name] == 0 && viaSelector[name] == 0 {
			t.Errorf("aliases.go re-exports %s and nothing in this package or any other "+
				"references it as chatruntime.%s — delete the alias, or fix the caller that "+
				"was supposed to need it. A re-export with no caller is a second name for a "+
				"type, not a convenience.", name, name)
		}
	}
}

// pkgImportPath is this package's import path, used to recognise a caller
// that reaches it from outside without hardcoding a qualifier.
const pkgImportPath = "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/chatruntime"

// localNamesFor returns the set of identifiers by which a file may refer to
// this package: the explicit alias where one is given, else the last path
// element. Both forms occur in this tree.
func localNamesFor(f *ast.File, importPath string) map[string]bool {
	out := map[string]bool{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if imp.Name != nil {
			out[imp.Name.Name] = true
			continue
		}
		parts := strings.Split(path, "/")
		out[parts[len(parts)-1]] = true
	}
	return out
}

// aliasNames returns every name aliases.go declares at package scope: the
// type-alias specs, the const specs, and the var specs. It parses the file
// rather than regexing it because a regex would also match the names inside
// the doc comments — the very names this test exists to remove, mentioned in
// prose that outlives them.
func aliasNames(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				out = append(out, s.Name.Name)
			case *ast.ValueSpec:
				for _, n := range s.Names {
					out = append(out, n.Name)
				}
			}
		}
	}
	return out
}
