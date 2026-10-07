package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The node's half of the vendor-credential property.
//
// The gate that reads LoadEdge's call closure lives in core/floor/config,
// because that is where the reads are. This half lives here, because what
// matters on this side is which door the node opens: a correct LoadEdge that
// nothing calls is the state the repository has already been wrong about
// twice (decision 192's federation row, decision 244's crystallisation
// wiring), and "the loader is safe" says nothing about whether the node
// uses it.
//
// So this asserts two things, both of which are checkable without running a
// node: the node calls the node loader, and the node never names the
// platform configuration type at all. The second is the stronger one — a
// process that cannot spell config.Config has no field to read a vendor key
// out of, which is a stronger guarantee than any runtime check.

// TestTheNodeOpensTheNodeLoaderAndNeverNamesThePlatformConfiguration is
// load-bearing in both directions: reintroducing config.Load() is red, and
// so is naming config.Config even without calling Load.
func TestTheNodeOpensTheNodeLoaderAndNeverNamesThePlatformConfiguration(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the node command's directory: %v", err)
	}

	sawLoader := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// The test files describe the node rather than being it; a test that
		// mentions config.Config in order to assert its absence is not a
		// violation of anything.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "config" {
				return true
			}
			switch sel.Sel.Name {
			case "LoadEdge":
				sawLoader = true
			case "Load":
				t.Errorf("%s calls config.Load(), which reads every model-vendor "+
					"API key, the admin password, the JWT secret and the database DSN "+
					"into a process that runs restart_service and a bash sandbox on a "+
					"customer host. The node's own door is config.LoadEdge()", name)
			case "Config":
				t.Errorf("%s names config.Config. A node has no use for the platform "+
					"configuration, and the type is the only thing standing between "+
					"this process and a vendor credential; a node that cannot spell it "+
					"cannot read a field out of it", name)
			}
			return true
		})
	}

	if !sawLoader {
		t.Fatalf("no file in the node command calls config.LoadEdge(), so the node " +
			"reads no configuration at all. That is a node that cannot be told where " +
			"the control plane is, and it is the other way this property can be lost — " +
			"not by widening the loader but by abandoning it")
	}
}
