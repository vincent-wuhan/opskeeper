package pigagent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The display target and summary used to be derived here, and the packaged
// gate courier inside the agent process derived them again with a DIFFERENT
// key order — so one call was described two ways depending on which process
// asked. Both now call wire.ToolSummary / wire.ToolTarget, and this test
// is what stops a third copy appearing here: a local copy compiles, passes
// every other test in this package, and reintroduces the drift silently.

func TestThisPackageDoesNotDeriveItsOwnApprovalDisplay(t *testing.T) {
	// The names both former implementations used, in either spelling.
	forbidden := map[string]bool{
		"toolSummary": true, "toolTarget": true,
		"summaryOf": true, "targetOf": true,
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv != nil || !forbidden[fd.Name.Name] {
					continue
				}
				t.Errorf("%s declares %s: the derivation lives in core/wire so that this "+
					"process, the node's broker and the packaged courier cannot disagree about "+
					"what one call touches",
					fset.Position(fd.Pos()), fd.Name.Name)
			}
		}
	}

	// And the positive half: this package must actually call the shared one.
	// toolTargetVocabulary is the set of argument names the shared derivation
	// looks for. A local slice literal holding several of them is the shape of a
	// re-implementation, whatever the enclosing function is called.
	var toolTargetVocabulary = map[string]bool{
		"target": true, "resource": true, "host": true, "node": true, "pod": true,
		"service": true, "namespace": true, "path": true, "name": true, "query": true,
	}

	// The name check above only catches the four spellings that existed. A
	// re-implementation under a fifth name is the same defect, so the real
	// signature is looked for too: a local list of argument names to search.
	// Delegating to core/wire is not a fork and does not have one, which is
	// why this is the second half of the check and not the whole of it.
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if _, isSlice := lit.Type.(*ast.ArrayType); !isSlice {
					return true
				}
				hits := 0
				for _, elt := range lit.Elts {
					bl, ok := elt.(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						continue
					}
					name, err := strconv.Unquote(bl.Value)
					if err != nil {
						continue
					}
					if toolTargetVocabulary[name] {
						hits++
					}
				}
				if hits >= 3 {
					t.Errorf("%s: builds a local list of %d argument names to search for the "+
						"resource a call touches; that derivation is core/wire's, and a second "+
						"list is how one call came to be described two ways",
						fset.Position(lit.Pos()), hits)
				}
				return true
			})
		}
	}

	called := false
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkgID, ok := sel.X.(*ast.Ident)
				if !ok || pkgID.Name != "wire" {
					return true
				}
				if sel.Sel.Name == "ToolSummary" || sel.Sel.Name == "ToolTarget" {
					called = true
				}
				return true
			})
		}
	}
	if !called {
		t.Error("no call to wire.ToolSummary / wire.ToolTarget in this package — the approval " +
			"request would go out with an empty summary and target again")
	}
}
