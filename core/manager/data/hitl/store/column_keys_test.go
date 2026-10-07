package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"sync"
	"testing"

	"gorm.io/gorm/schema"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/hitl"
)

// This file guards the one write path in the approval chain that the
// compiler cannot check.
//
// Transition and SetResult hand gorm a map[string]any. Inside gorm, a key
// that is not a field name is emitted into the SQL verbatim as a column
// name — see callbacks/update.go, where the fallback is
// clause.Assignment{Column: clause.Column{Name: k}}. There is no error and
// no second opinion: "paused_by" on a model with that column updates it,
// and "pausedBy" updates nothing at all, in production, silently. The row
// still moves to the new state, the operator still sees the transition
// happen, and the column that was supposed to say who paused the proposal
// stays NULL forever.
//
// The alternative — typed update structs, one per transition — is the right
// answer for a codebase that wants the guarantee structurally. It is also a
// large change to the two functions every state machine in this package
// calls, and this test buys the same protection for the thing that actually
// goes wrong, which is a misspelled key. The keys are checked against the
// model's real column names, derived by gorm's own namer rather than by a
// list written here: a list written here would be a second place for the
// schema to be described, and a second place is what this test exists to
// prevent.
//
// What it does not catch: a key that names a real column but the wrong one
// for the transition. approved_by is a real column, and writing it on a
// reject is a decision rather than a typo. That is a review question.

// columnsOf is the model's real column set, named the way gorm names it.
func columnsOf(t *testing.T, dest any) map[string]bool {
	t.Helper()
	s, err := schema.Parse(dest, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	out := map[string]bool{}
	for _, name := range s.DBNames {
		out[name] = true
	}
	return out
}

// updateMapKeys returns every string key this file hands to gorm as an
// update column: the keys of a map[string]any literal, and the keys of an
// index expression assigned into such a map.
func updateMapKeys(t *testing.T, file string) map[string][]string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	keys := map[string][]string{}
	note := func(lit *ast.BasicLit, node ast.Node) {
		name, err := strconv.Unquote(lit.Value)
		if err != nil {
			return
		}
		keys[name] = append(keys[name], fset.Position(node.Pos()).String())
	}
	ast.Inspect(parsed, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			// map[string]any{...}: the type is what makes these update
			// columns, and a map of any other key type is not.
			mt, ok := n.Type.(*ast.MapType)
			if !ok || !isAnyStringMap(mt) {
				return true
			}
			for _, elt := range n.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if lit, ok := kv.Key.(*ast.BasicLit); ok {
						note(lit, n)
					}
				}
			}
		case *ast.IndexExpr:
			// updates["decided_at"] = now
			if lit, ok := n.Index.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				note(lit, n)
			}
		}
		return true
	})
	return keys
}

func isAnyStringMap(mt *ast.MapType) bool {
	key, ok := mt.Key.(*ast.Ident)
	if !ok || key.Name != "string" {
		return false
	}
	switch v := mt.Value.(type) {
	case *ast.Ident:
		return v.Name == "any"
	case *ast.InterfaceType:
		return true
	}
	return false
}

func TestEveryUpdateKeyIsARealColumn(t *testing.T) {
	keys := updateMapKeys(t, "store.go")
	if len(keys) == 0 {
		t.Fatal("no update keys were read from store.go at all, which means the walk " +
			"stopped matching and this test is passing for the wrong reason")
	}
	columns := columnsOf(t, &model.Proposal{})
	for key, where := range keys {
		if !columns[key] {
			t.Errorf("%s writes column %q, which is not a column of the proposal table. gorm "+
				"emits an unmatched key into the SQL as a column name and updates nothing, so "+
				"this is a silent no-op rather than a failure: the transition still happens and "+
				"the row keeps its old value", where, key)
		}
	}
}

// The keys are read from source rather than listed here, and this is the
// assertion that keeps that honest: a list written in the test would be a
// second description of the schema, and the whole point is that there is
// only one.
func TestTheKeyListComesFromTheSourceNotFromThisFile(t *testing.T) {
	keys := updateMapKeys(t, "store.go")
	columns := columnsOf(t, &model.Proposal{})
	if len(keys) >= len(columns) {
		t.Fatalf("the file hands gorm %d distinct column names and the model has %d columns; "+
			"either the model lost columns or the walk is now matching something it should not",
			len(keys), len(columns))
	}
}
