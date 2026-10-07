package main

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

// Every migration this binary knows how to run is in one list, and that list
// is the only thing that creates a table. A `data/*/store` package that
// declares a Migrate and is not in that list is not "a migration we might run
// later": it is a schema that no deployment will ever have, and every query
// written against it fails at runtime with "no such table" — the one error
// message that reads like an operator's typo rather than a missing wire.
//
// core/manager/data/middleware/store was exactly that, and nothing caught it
// for the whole life of the tree: both its migrators were dead code by the
// deadcode tool's own reading, its three tables were named by no query, and
// the migrate CLI that targets middleware_resources posts at an HTTP route
// this router does not have. Four independent tools each reported a slice of
// it and none of them was a gate.
//
// So this is a gate, and it is a source-shape assertion for the same reason
// the crystallize boot test is: the failure mode is a function that exists,
// compiles, and is never called — a shape no behavioural test can see.

// migrationDeclaringDirs walks the shipped tree and returns every directory
// that declares `func Migrate(db *gorm.DB) error`, keyed by import path.
func migrationDeclaringDirs(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.Walk(filepath.Join("..", ".."), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := info.Name()
			if base == "node_modules" || base == ".git" || base == "dist" ||
				base == "vendor" || base == "openspec" {
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
		declares := false
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "Migrate" {
				continue
			}
			// The signature dbx.Migrator is the one that matters: a
			// Migrate with a different parameter list is not a boot step.
			if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
				continue
			}
			if n := len(fn.Type.Params.List[0].Names); n != 1 {
				continue
			}
			if fn.Type.Params.List[0].Names[0].Name != "db" {
				continue
			}
			declares = true
		}
		if !declares {
			return nil
		}
		dir := filepath.Dir(path)
		rel, rerr := filepath.Rel(filepath.Join("..", ".."), dir)
		if rerr != nil {
			return nil
		}
		importPath := "github.com/vincent-wuhan/opskeeper/" +
			filepath.ToSlash(rel)
		found[importPath] = filepath.ToSlash(dir)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	return found
}

// TestEveryMigrationIsWiredIntoTheBootPath is the gate. It fails when a
// package declares a migrator the manager never calls, which is the state in
// which a table's name appears in documentation and in a migration CLI's
// target list while no deployment has ever had the table.
func TestEveryMigrationIsWiredIntoTheBootPath(t *testing.T) {
	declaring := migrationDeclaringDirs(t)
	if len(declaring) == 0 {
		t.Fatal("no Migrate found anywhere in the tree; this test would pass for the wrong reason")
	}
	// A migrator counts as wired only when it is CALLED, never when it is
	// merely imported: an import with no call site is what the first version
	// of this test accepted, and a mutation that dropped one entry from
	// managerMigrators while leaving its import in place stayed green. Three
	// of the 24 run from their own subsystem's boot block rather than the
	// central list, so this cannot be a whitelist of the central list — it
	// has to be every .Migrate selector in the binary.
	called := migratorsCalledByName()

	var orphans []string
	for importPath := range declaring {
		if called[importPath] {
			continue
		}
		orphans = append(orphans, importPath)
	}
	if len(orphans) == 0 {
		return
	}
	for _, o := range orphans {
		t.Errorf("%s declares a Migrate that nothing in cmd/opskeeper imports or calls.\n"+
			"  A migration that is not wired does not delay a feature, it removes one: "+
			"the tables it would create do not exist in any deployment, so every query "+
			"against them fails at runtime. Either wire it into managerMigrators() (or "+
			"the subsystem boot block that owns it), or delete the package — an unwired "+
			"migrator is worse than none, because the schema reads as delivered.", o)
	}
}

// migratorsCalledByName returns the import paths of the store packages that
// cmd/opskeeper calls .Migrate on directly, outside managerMigrators.
func migratorsCalledByName() map[string]bool {
	called := map[string]bool{}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), ".go") && !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return called
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			// Map this file's own import aliases so the selector in
			// `managerdataloopstore.Migrate` can be resolved to a path
			// rather than guessed from the alias.
			aliases := map[string]string{}
			for _, spec := range file.Imports {
				p, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					continue
				}
				name := p[strings.LastIndex(p, "/")+1:]
				if spec.Name != nil {
					name = spec.Name.Name
				}
				aliases[name] = p
			}
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Migrate" {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if p, ok := aliases[ident.Name]; ok {
					called[p] = true
				}
				return true
			})
		}
	}
	return called
}
