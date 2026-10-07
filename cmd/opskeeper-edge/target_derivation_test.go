package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The node's own tool broker hands the gate a policygate.Call, and the gate
// copies Target and Summary straight into the approval request it emits. This
// path filled in neither, so an operator was asked to approve a card that
// named no resource at all — and the ledger entry fell back to naming the call
// by its tool name, because targetOf had nothing else to work with.
//
// The derivation is core/wire's, shared with the control plane's kernel and
// the packaged courier, so one call cannot be described three ways. A local
// copy here compiles and passes every other test in this binary, which is
// exactly why this test exists.

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
				t.Errorf("%s declares %s: the derivation lives in core/wire so this binary, the "+
					"control plane and the packaged courier cannot disagree",
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
				case "ToolTarget", "ToolSummary":
					called = true
				}
				return true
			})
		}
	}
	if !called {
		t.Error("no call to wire.ToolTarget / wire.ToolSummary in this package — every " +
			"in-package tool call would reach the operator as an approval card with no target and " +
			"no summary")
	}
}
