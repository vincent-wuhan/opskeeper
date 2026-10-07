package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// This file is the third half of loop_crystallize.go, and it is the half
// that was missing until a mutation found it.
//
// loop_crystallize_test.go pins what the assembly *does* when it is called:
// with no tools the feature is off, with no alert repo it refuses, with both
// it builds a learner and hands the console the same ledger. Every one of
// those is a statement about a function.
//
// None of them is a statement about the boot path. Nothing in the repository
// asked whether main.go calls the assembly at all, and that question has an
// answer every other test is equally happy with. Measured this round:
// replacing the one line
//
//	crystallization, cerr := newLoopCrystallization(middlewareReg, alertRepo, aiopsHandler, log)
//
// with a zero value still compiles, still passes go test ./cmd/... , and
// still passes make crystallize-check. The feature the plan's item 7 asks
// for — a verified fix pattern promoting itself into a runbook so the next
// occurrence costs no inference — would be silently dead, and every gate
// would still report the mechanism as healthy.
//
// So this is a source-shape assertion, and the shape it pins is narrower
// than "the call appears somewhere". It pins that the object the boot builds
// is the object the orchestrator is handed, because those are two statements
// that can drift apart: building a learner and wiring it to nothing is a
// state the other half of the test file can still pass, because it is a
// state it never enters.

// TestTheCrystallizerTheBootBuildsIsTheOneTheOrchestratorIsGiven reads
// main.go and answers one question with the type system as a witness: the
// value bound from newLoopCrystallization is the value the loop orchestrator
// is constructed with.
func TestTheCrystallizerTheBootBuildsIsTheOneTheOrchestratorIsGiven(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	built := bootObjectBuiltBy(t, file, "newLoopCrystallization")
	deps := orchestratorDeps(t, fset, file)
	assertFieldComesFrom(t, deps, "Crystallizer", built)
	assertFieldComesFrom(t, deps, "Triggers", built)
}

// bootObjectBuiltBy returns the name main.go binds the result of a call to
// constructor.
//
// It fails rather than returning "" for a missing call, and that is the
// whole reason it is a function and not an inline walk. The tree guards in
// scripts/domaincheck spent seven rounds learning this: an assertion over a
// set that turns out to be empty announces the emptiness, or it reports
// green for having looked at nothing. A guard that returned "" here and let
// the caller compare it would be the ninth version of the same defect.
func bootObjectBuiltBy(t *testing.T, file *ast.File, constructor string) string {
	t.Helper()
	found := ""
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) == 0 {
			return true
		}
		for _, rhs := range assign.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != constructor {
				continue
			}
			if found != "" {
				t.Errorf("main.go calls %s more than once and binds two objects; this guard "+
					"reads the first and would keep passing if the orchestrator were handed "+
					"the second", constructor)
			}
			if name, ok := assign.Lhs[0].(*ast.Ident); ok {
				found = name.Name
			}
		}
		return true
	})
	if found == "" {
		t.Fatalf("main.go never calls %s, so nothing on this boot path can crystallise a "+
			"pattern. The mechanism and its own tests stay green either way, which is how "+
			"the plan's item 7 could be recorded as built-but-unwired without anyone "+
			"noticing that the wiring was never what was missing", constructor)
	}
	return found
}

// orchestratorDeps returns the dependency struct the loop orchestrator is
// constructed with.
func orchestratorDeps(t *testing.T, fset *token.FileSet, file *ast.File) *ast.CompositeLit {
	t.Helper()
	var found *ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewOrchestrator" {
			return true
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.CompositeLit); ok {
				found = lit
			}
		}
		return true
	})
	if found == nil {
		t.Fatalf("main.go does not construct the loop orchestrator with a dependency struct, "+
			"so this guard cannot see whether a crystalliser reaches it (file starts at %s)",
			fset.Position(file.Pos()))
	}
	return found
}

// assertFieldComesFrom reports whether a named field of the dependency
// struct is filled from the named object, and says which of the two it is
// when the answer is no.
func assertFieldComesFrom(t *testing.T, lit *ast.CompositeLit, field, object string) {
	t.Helper()
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != field {
			continue
		}
		if name := baseIdent(kv.Value); name != object {
			t.Errorf("OrchestratorDeps.%s is filled from %q, not from the object the boot "+
				"built (%s). A crystalliser that is built and not handed to the loop is a "+
				"feature that reports itself wired in the boot log while nothing is",
				field, name, object)
		}
		return
	}
	t.Errorf("OrchestratorDeps has no %s field, so the loop has nothing to crystallise "+
		"into and the feature ends here rather than ending quietly", field)
}

// baseIdent returns the name an expression hangs off, so a field written as
// `x.y` and a field written as `x` are compared by the object they name
// rather than by the text they are spelled with.
func baseIdent(e ast.Expr) string {
	for {
		switch v := e.(type) {
		case *ast.Ident:
			return v.Name
		case *ast.SelectorExpr:
			e = v.X
		default:
			return ""
		}
	}
}
