package agentteams

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `agentteams -> alert` edge was three types and two methods: a
// twenty-five-column entity, a six-field filter and a status constant, for a
// handler that reads two columns and asks one question. It was cut by moving
// the projection to domain.OpenAlert, narrowing the port to ListOpenAlerts,
// and letting alert.OpenAlertResolver satisfy it structurally.
//
// This file is the half of the cut that can be lost silently. The port
// compiles, the handler compiles, every test passes, and the edge is back
// the moment one import returns — with no behavioural difference anywhere,
// because the new method is a strict subset of what it replaced. Nothing
// about the product gets worse, so nothing else would notice.

// alertPackages are the packages this one must not import. Named
// explicitly rather than matched by a prefix: "alert" also appears in
// incidentcontrol, in alertbiz's own subpackages and in half a dozen
// unrelated names, and a prefix rule wide enough to be convenient is a rule
// that will eventually refuse a legitimate import and get deleted.
var alertPackages = []string{
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/alert",
	"github.com/vincent-wuhan/opskeeper/core/manager/model/alert",
	"github.com/vincent-wuhan/opskeeper/core/manager/service/alert",
	"github.com/vincent-wuhan/opskeeper/core/manager/data/alert/store",
}

func TestNoFileInThisPackageImportsTheAlertDomain(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	sawGoFile := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		sawGoFile = true
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			for _, forbidden := range alertPackages {
				if path == forbidden {
					t.Errorf("%s imports %s. The projection is domain.OpenAlert and the "+
						"port is declared in this file; an import of the alert domain here "+
						"is the edge coming back, and it comes back silently because "+
						"ListOpenAlerts is a strict subset of what it replaced", name, path)
				}
			}
		}
	}
	if !sawGoFile {
		t.Fatal("no Go files were examined, so this test is measuring nothing. A rename " +
			"or a move that empties this directory is not a way to make it green")
	}
}

// TestThePortStillAsksTheQuestionItWasCutFor is the other direction.
//
// Deleting the resolver, the port and the call site would leave the import
// check above perfectly green — the edge really would be gone. It would also
// leave a recovery closure that silently stops closing alert incidents, and
// nothing in that package would fail. So the port is pinned by shape: two
// methods, and the first one is the narrowed question, not the old one.
func TestThePortStillAsksTheQuestionItWasCutFor(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "http.go", nil, 0)
	if err != nil {
		t.Fatalf("parse http.go: %v", err)
	}

	var port *ast.InterfaceType
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "AlertIncidentResolver" {
				continue
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				t.Fatalf("AlertIncidentResolver is a %T, not an interface", ts.Type)
			}
			port = it
		}
	}
	if port == nil {
		t.Fatalf("http.go declares no AlertIncidentResolver interface, so a recovery " +
			"closure has nothing to call and the import check above is passing on a " +
			"package that no longer does the work")
	}

	methods := map[string]bool{}
	for _, field := range port.Methods.List {
		if len(field.Names) == 0 {
			continue
		}
		methods[field.Names[0].Name] = true
	}
	for _, required := range []string{"ListOpenAlerts", "SystemResolveIncident"} {
		if !methods[required] {
			t.Errorf("the port has no %s. It had %s, and a port that asks for "+
				"the whole entity is the edge this cut removed",
				required, map[string]string{
					"ListOpenAlerts":        "ListIncidents(ctx, filter) ([]*alertmodel.Incident, error)",
					"SystemResolveIncident": "SystemResolveIncident(ctx, dedupeKey, reason, occurredAt)",
				}[required])
		}
	}
	if methods["ListIncidents"] {
		t.Error("the port has ListIncidents again. That is the un-narrowed question, and " +
			"its return type is the twenty-five-column entity")
	}
}
