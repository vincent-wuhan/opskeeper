package opskeepergate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// This courier used to carry its own targetOf / summaryOf, and its key order
// disagreed with the control plane's copy of the same rule: one call came to
// be described two ways depending on which process asked the host. It calls
// core/wire now. This test is what stops the fork from happening again — a
// local copy compiles and passes every other test in this package.
//
// It matters more here than elsewhere in the tree: this code ships as source
// into the agent process on the node and is built there, so a divergence is
// not a review question but a version question.

func TestThisPackageDoesNotDeriveItsOwnApprovalDisplay(t *testing.T) {
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
				t.Errorf("%s declares %s: the derivation lives in core/wire, and a second copy "+
					"is how one call came to be described two ways",
					fset.Position(fd.Pos()), fd.Name.Name)
			}
		}
	}

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
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != "wire" {
					return true
				}
				switch sel.Sel.Name {
				case "ToolTargetMap", "ToolSummaryMap":
					called = true
				}
				return true
			})
		}
	}
	if !called {
		t.Error("no call to wire.ToolTargetMap / wire.ToolSummaryMap in this package — every " +
			"call this courier relays would reach the operator as an approval card with no target " +
			"and no summary")
	}
}
