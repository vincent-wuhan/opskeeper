// Package tablecheck answers one question about the shipped tree: does any
// table have more than one GORM model pointing at it?
//
// It exists because of a specific finding. core/domains/model/proposal and
// core/manager/model/hitl each declared a `Proposal` whose `TableName()`
// returned "proposal" -- two structs with different columns, different id
// types (uint64 vs string) and different state fields, both claiming the same
// table. One of them had zero importers, so nothing broke; the moment anything
// had imported the wrong one, the failure would have been AutoMigrate writing
// a column set, or a query reading a row whose primary key is a char(36)
// into a uint64. Neither error names the other model.
//
// So the property is not "the models are correct" -- this tool cannot judge
// that, and a tool that tries will train people to write "trust me"
// comments. The property is narrower and it is mechanical: a table name is
// claimed once. Duplicates are reported with both file:line so the decision
// of which one survives is a person's, and the tool's job is only to make
// sure the decision gets made before a second model reaches a migrator.
//
// The walk cannot see a model whose TableName is computed rather than
// returned as a literal. That is stated in the report rather than hidden,
// for the reason decision 199 recorded: an unreliable gate is worse than no
// gate, because it produces confidence.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Claim is one `func (T) TableName() string { return "literal" }`.
type Claim struct {
	Table   string
	Model   string
	Path    string
	Line    int
	Literal bool
}

// Collect walks root and returns every TableName method it can read.
//
// A method whose body is not a single returned string literal is recorded
// with Literal false: it is still a claim on a table this tool cannot name,
// and dropping it would make the report read as "these are all the claims"
// when it is not.
func Collect(root string) ([]Claim, error) {
	var claims []Claim
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git", "dist", "vendor", "openspec", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "TableName" {
				continue
			}
			claim := Claim{Path: filepath.ToSlash(rel), Line: fset.Position(fn.Pos()).Line}
			if fn.Recv.List != nil {
				if t := receiverType(fn.Recv.List[0].Type); t != "" {
					claim.Model = t
				}
			}
			claim.Table, claim.Literal = tableLiteral(fn)
			claims = append(claims, claim)
		}
		return nil
	})
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].Table != claims[j].Table {
			return claims[i].Table < claims[j].Table
		}
		return claims[i].Path < claims[j].Path
	})
	return claims, err
}

func receiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverType(t.X)
	}
	return ""
}

// tableLiteral returns the string a TableName body returns, and whether it
// was a literal at all.
func tableLiteral(fn *ast.FuncDecl) (string, bool) {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return "", false
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return "", false
	}
	lit, ok := ret.Results[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// Duplicates returns the table names claimed by more than one model, with
// the claims attached, sorted by table name.
func Duplicates(claims []Claim) map[string][]Claim {
	byTable := map[string][]Claim{}
	for _, c := range claims {
		if c.Table == "" {
			continue
		}
		byTable[c.Table] = append(byTable[c.Table], c)
	}
	dupes := map[string][]Claim{}
	for table, cs := range byTable {
		if len(cs) > 1 {
			dupes[table] = cs
		}
	}
	return dupes
}

func report(claims []Claim) string {
	var b strings.Builder
	dupes := Duplicates(claims)
	fmt.Fprintf(&b, "tablecheck: %d TableName methods read", len(claims))
	if len(dupes) == 0 {
		fmt.Fprintf(&b, "; no table is claimed twice\n")
	} else {
		fmt.Fprintf(&b, "; %d tables are claimed more than once\n", len(dupes))
	}
	tables := make([]string, 0, len(dupes))
	for t := range dupes {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		fmt.Fprintf(&b, "  %s\n", t)
		for _, c := range dupes[t] {
			fmt.Fprintf(&b, "    %s:%d  %s\n", c.Path, c.Line, c.Model)
		}
	}
	unreadable := 0
	for _, c := range claims {
		if !c.Literal {
			unreadable++
		}
	}
	if unreadable > 0 {
		fmt.Fprintf(&b, "tablecheck: %d TableName methods return something other than a string literal, "+
			"so this walk cannot say which table they claim\n", unreadable)
	}
	fmt.Fprintf(&b, "tablecheck: report only, exit 0 — a table name is a schema decision, "+
		"and this tool can only say that two people made it\n")
	return b.String()
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	claims, err := Collect(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tablecheck: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(report(claims))
}
