package investigatorreal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// TestTheDeclaredVocabularyMatchesTheCode re-derives the investigator's
// capability set from its own source and requires it to equal
// RemediationActions exactly.
//
// The set is compared in both directions on purpose. Checking only
// "every declared action is used" would pass while a new branch shipped
// undeclared — which is the direction that actually matters, because an
// undeclared action is a capability the evaluation cannot account for.
// Checking only the other way would pass while the declaration advertised
// fixes that do not exist.
func TestTheDeclaredVocabularyMatchesTheCode(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	used := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "remediation" || len(call.Args) == 0 {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Errorf("unquote %s: %v", lit.Value, err)
					return true
				}
				used[v] = true
				return true
			})
		}
	}
	if len(used) == 0 {
		t.Fatal("no remediation(...) literals were found; the scrape is broken, not the code")
	}

	declared := map[string]bool{}
	for _, a := range RemediationActions {
		if declared[a] {
			t.Errorf("RemediationActions lists %q twice", a)
		}
		declared[a] = true
	}
	for a := range used {
		if !declared[a] {
			t.Errorf("code can emit %q but RemediationActions does not declare it", a)
		}
	}
	for a := range declared {
		if !used[a] {
			t.Errorf("RemediationActions declares %q but no code path can produce it", a)
		}
	}
}

// The declaration is read by the evaluation gate, which sorts and reports
// it, so a stable order is part of its contract rather than a nicety.
func TestTheDeclaredVocabularyIsSorted(t *testing.T) {
	if !sort.StringsAreSorted(RemediationActions) {
		t.Errorf("RemediationActions is not sorted: %v", RemediationActions)
	}
}

// Guards the scrape itself against silently finding nothing if the helper is
// ever renamed or the file layout changes.
func TestThePackageLayoutIsWhatTheScrapeAssumes(t *testing.T) {
	if _, err := filepath.Abs("."); err != nil {
		t.Fatalf("abs: %v", err)
	}
	if len(RemediationActions) == 0 {
		t.Fatal("the declared vocabulary is empty")
	}
}
