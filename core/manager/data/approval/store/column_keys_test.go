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

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/approval"
)

// This file guards the one write path in the approval chain that the
// compiler cannot check.
//
// Decide and SetResult hand gorm a map[string]any. Inside gorm, a key
// that is not a field name is emitted into the SQL verbatim as a column
// name — see callbacks/update.go, where the fallback is
// clause.Assignment{Column: clause.Column{Name: k}}. There is no error and
// no second opinion: "paused_by" on a model with that column updates it,
// and "approvedBy" updates nothing at all, in production, silently. The row
// still moves to the decided state, the inbox still shows the decision, and
// the column that was supposed to say who made it stays NULL forever — which
// for an approval row is the one field the whole table exists to hold.
//
// The alternative — typed update structs, one per transition — is the right
// answer for a codebase that wants the guarantee structurally. It is also a
// large change to the two functions every approval flow in this package
// calls, and this test buys the same protection for the thing that actually
// goes wrong, which is a misspelled key. The keys are checked against the
// model's real column names, derived by gorm's own namer rather than by a
// list written here: a list written here would be a second place for the
// schema to be described, and a second place is what this test exists to
// prevent.
//
// What it does not catch: a key that names a real column but the wrong one
// for the call. approved_by is a real column, and writing it on a reject is
// this model's documented choice (the comment on the field says it is "the
// human who decided"), not a typo. That is a review question.

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

// The keys are not all in this package, and that is the point worth writing
// down. SetResult builds its map here, but the Decide maps are written by the
// callers — biz/approval's Approve and Reject — because the approver and the
// reason only exist there. A guard that read repo.go alone would have covered
// three of the six column names this table's decision path writes, and would
// have said so with a green tick.
//
// So the rule is read from both files, and the two are named here rather than
// discovered, because a guard that quietly reads one file and passes is worse
// than no guard: it is a claim about the write path that covers only part of
// it.
var keyFiles = []string{
	"repo.go",                          // SetResult, in this package
	"../../../biz/approval/usecase.go", // Decide, at the call sites
}

func TestEveryUpdateKeyIsARealColumn(t *testing.T) {
	columns := columnsOf(t, &model.Approval{})
	total := 0
	for _, file := range keyFiles {
		keys := updateMapKeys(t, file)
		if len(keys) == 0 {
			t.Errorf("no update keys were read from %s at all, which means the walk stopped "+
				"matching and this test is passing for the wrong reason", file)
			continue
		}
		total += len(keys)
		for key, where := range keys {
			if !columns[key] {
				t.Errorf("%s writes column %q, which is not a column of the approvals table. gorm "+
					"emits an unmatched key into the SQL as a column name and updates nothing, so "+
					"this is a silent no-op rather than a failure: the row still moves to the "+
					"decided state and keeps the old value in the column nobody wrote", where, key)
			}
		}
	}
	// Both files have to contribute. A path that stopped being written would
	// otherwise look exactly like a path that stopped having keys, and the
	// six column names this decision path writes are more than the floor.
	if total < 5 {
		t.Fatalf("only %d distinct column names were read across %v; the decision path writes "+
			"more than that, so the file list is out of date", total, keyFiles)
	}
}

func TestTheKeyListComesFromTheSourceNotFromThisFile(t *testing.T) {
	seen := map[string]bool{}
	for _, file := range keyFiles {
		for key := range updateMapKeys(t, file) {
			seen[key] = true
		}
	}
	columns := columnsOf(t, &model.Approval{})
	if len(seen) >= len(columns) {
		t.Fatalf("the decision path hands gorm %d distinct column names and the model has %d "+
			"columns; either the model lost columns or the walk is now matching something it "+
			"should not", len(seen), len(columns))
	}
}
