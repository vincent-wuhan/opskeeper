package main

// crystallized_release_wiring_test.go — the release hop has to be *in* the
// boot.
//
// The route exists, the seam exists, the adapter exists, and the release
// manager it would call already ships a fleet. That is four pieces, and a
// process that never hands the handler a releaser still compiles, still
// passes every test in this file's own package, and still answers 503 on a
// route whose whole reason for existing is the opposite.
//
// This is the same guard §4.108.3 wrote for the crystallizer, applied to the
// hop that came after it: the object the boot builds is the object the
// surface is given.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestTheReleasePathTheBootBuildsIsTheOneTheCrystallizedSurfaceIsGiven reads
// main.go and answers one question: is the release manager that this process
// constructs the one handed to the crystallised review surface?
func TestTheReleasePathTheBootBuildsIsTheOneTheCrystallizedSurfaceIsGiven(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// main.go calls it as a selector (managersvcplugin.NewManager), so the
	// bound name is found here rather than through the helper that looks for
	// a bare identifier. Looking at the bound variable rather than at the
	// call is the point: the question is which object the surface is handed,
	// not that some call with that name exists somewhere in the file.
	built := releaseManagerBuiltBy(t, file)

	// The setter is called on the aiops handler, and its argument has to be
	// the adapter over that manager — not a fresh one, and not a stub.
	var (
		calls    int
		argument string
	)
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetDraftReleaser" || len(call.Args) != 1 {
			return true
		}
		calls++
		if lit, ok := call.Args[0].(*ast.CompositeLit); ok {
			if ident, ok := lit.Type.(*ast.Ident); ok {
				argument = ident.Name
			}
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "mgr" {
						if ident, ok := kv.Value.(*ast.Ident); ok {
							argument = ident.Name
						}
					}
				}
			}
		}
		return true
	})

	if calls != 1 {
		t.Fatalf("main.go calls SetDraftReleaser %d times, want 1; the review surface either "+
			"has no release path or has one that was replaced", calls)
	}
	if argument != built {
		t.Errorf("the crystallised surface was handed %q, but the boot builds %q; "+
			"the release route would answer 503 while a working release manager sits "+
			"in the same process", argument, built)
	}
}

// releaseManagerBuiltBy returns the name main.go binds the plugin release
// manager to, and fails rather than returning "" — a guard that returned ""
// and let the caller compare it would report green for having looked at
// nothing, which is the defect §4.109.4 spent a decision naming.
func releaseManagerBuiltBy(t *testing.T, file *ast.File) string {
	t.Helper()
	found := ""
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE {
			return true
		}
		// The call is wrapped — main.go writes
		//
		//	pluginReleaseMgr := managersvcplugin.NewManager(...).WithVersions(...)
		//
		// so the constructor is not the RHS's own Fun. The subtree is walked
		// rather than the top node, because the question is which object this
		// statement built, not what the outermost call happens to be named.
		for i, rhs := range assign.Rhs {
			built := false
			ast.Inspect(rhs, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewManager" {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "managersvcplugin" {
					return true
				}
				built = true
				return false
			})
			if !built {
				continue
			}
			if found != "" {
				t.Errorf("main.go binds the plugin release manager twice (%s and %s); this "+
					"guard reads the first and would keep passing if the surface were handed "+
					"the second", found, exprName(assign.Lhs[i]))
			}
			found = exprName(assign.Lhs[i])
		}
		return true
	})
	if found == "" {
		t.Fatal("main.go builds no plugin release manager, so nothing can be released from " +
			"anywhere; the surface would answer 503 while every gate reported the mechanism " +
			"healthy")
	}
	return found
}

// exprName renders one LHS expression for a message. It is only ever called
// on an identifier, so a nil for anything else is a message that loses a
// name rather than a guard that stops checking.
func exprName(e ast.Expr) string {
	if ident, ok := e.(*ast.Ident); ok {
		return ident.Name
	}
	return "<not an identifier>"
}
