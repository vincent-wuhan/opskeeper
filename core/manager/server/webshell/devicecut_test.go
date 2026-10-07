package webshell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The `webshell -> device` edge was two types and five methods, and none of
// the five were called on this side. The live code used exactly one thing
// from the device domain: a parameter it could not vary.
//
//	LookupEdgeForDevice(ctx, deviceID, t devicemodel.EdgeDeviceRelationType)
//
// with `Host` on the one call site. The enum has two values; this handler
// had one opinion about both of them. The test that stood next to it made
// the same point by asserting the value arriving was Host — a test of a fact
// the caller had no way to state differently.
//
// It was cut by deleting the parameter. The relation did not disappear; it
// stopped being a question the consumer was allowed to ask. The device
// domain already answered the two-argument question from the same junction
// table (biz/device/usecase.go: LookupEdgeForDevice hardcodes Host), so
// nothing about the answer changed — only who is entitled to choose it.
//
// This file is the half of that cut which can be lost silently. The port
// compiles, the handler compiles, every behavioural test stays green, and
// the edge is back the moment one import returns. Nothing about the product
// gets worse, so nothing else would notice.

// devicePackages are the packages this one must not import. Named
// explicitly rather than matched by a prefix: "device" also appears in
// edgemodel, in the device HTTP handler's own subpackages and in a handful of
// unrelated names, and a prefix rule wide enough to be convenient is a rule
// that will eventually refuse a legitimate import and get deleted.
var devicePackages = []string{
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/device",
	"github.com/vincent-wuhan/opskeeper/core/manager/model/device",
	"github.com/vincent-wuhan/opskeeper/core/manager/service/device",
	"github.com/vincent-wuhan/opskeeper/core/manager/data/device/store",
}

func TestNoFileInThisPackageImportsTheDeviceDomain(t *testing.T) {
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
			for _, forbidden := range devicePackages {
				if path == forbidden {
					t.Errorf("%s imports %s. Which edge owns a device *as its host* is the "+
						"only relation a shell can be opened against, and it is a fact about "+
						"the device domain rather than a choice this handler makes; an import "+
						"here is that choice coming back, and it comes back silently because "+
						"the narrowed question is a strict subset of the old one",
						name, path)
				}
			}
		}
	}
	if !sawGoFile {
		t.Fatal("no Go files were examined, so this test is measuring nothing. A rename " +
			"or a move that empties this directory is not a way to make it green")
	}
}

// TestTheDevicePortAsksTwoQuestionsAndNoMore pins the width of the cut on the
// interface itself.
//
// A count is asserted here for the same reason the edge-status one is: the
// alternative is a comment, and a comment is what the next person optimises
// away. But the arity is the load-bearing half, because arity is where the
// relation went. Widening the port back to three parameters puts
// devicemodel.EdgeDeviceRelationType back into a file in this package, and
// the import check above would catch it — one file, one import, one edit, and
// nobody deciding to. Two parameters cannot hold it, so this assertion is the
// one that makes the removal structural instead of conventional.
func TestTheDevicePortAsksTwoQuestionsAndNoMore(t *testing.T) {
	port := reflect.TypeOf((*DeviceLinks)(nil)).Elem()

	if port.NumMethod() != 1 {
		t.Fatalf("DeviceLinks has %d methods (%v); the cut left it exactly one question, "+
			"and a second one is how a caller starts choosing relations again",
			port.NumMethod(), methodNames(port))
	}
	method := port.Method(0)
	if method.Name != "LookupEdgeForDevice" {
		t.Errorf("DeviceLinks' one method is %s, want LookupEdgeForDevice", method.Name)
	}
	// reflect reports an interface method's type without the receiver, so
	// this is the declared arity directly. The third declared parameter was
	// `t devicemodel.EdgeDeviceRelationType`, and its removal is the entire
	// content of this cut.
	if got := method.Type.NumIn(); got != 2 {
		t.Errorf("LookupEdgeForDevice takes %d parameters, want 2. The third was the "+
			"relation type, which this handler passed as Host on its only call site — "+
			"a parameter that could not vary, and a parameter that cannot vary is a fact "+
			"wearing a parameter's clothes", got)
	}
	if got := method.Type.NumOut(); got != 2 {
		t.Errorf("LookupEdgeForDevice returns %d values, want 2 (edge id and error)", got)
	}
}

// TestThePortStillAsksTheQuestionItWasCutFor is the other direction.
//
// Deleting the port, its call site and resolveEdge would leave the two
// assertions above perfectly green — the edge really would be gone. It would
// also leave a device shell that cannot find its edge, so a handler that
// never asks is pinned by asking.
func TestThePortStillAsksTheQuestionItWasCutFor(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "http.go", nil, 0)
	if err != nil {
		t.Fatalf("parse http.go: %v", err)
	}

	sawInterface := false
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "DeviceLinks" {
				continue
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				t.Fatalf("DeviceLinks is a %T, not an interface", ts.Type)
			}
			sawInterface = true
			methods := map[string]int{}
			for _, field := range it.Methods.List {
				if len(field.Names) == 0 {
					continue
				}
				ft, ok := field.Type.(*ast.FuncType)
				if !ok {
					t.Fatalf("the port's %s is a %T, not a method signature",
						field.Names[0].Name, field.Type)
				}
				methods[field.Names[0].Name] = len(ft.Params.List)
			}
			if params, ok := methods["LookupEdgeForDevice"]; !ok {
				t.Errorf("the port has no LookupEdgeForDevice (%v), so a shell has nothing "+
					"to call and the arity check above is measuring an interface nobody uses",
					methods)
			} else if params != 2 {
				t.Errorf("LookupEdgeForDevice is declared with %d parameters, want 2", params)
			}
		}
	}
	if !sawInterface {
		t.Fatal("http.go declares no DeviceLinks interface, so a shell has nothing to call " +
			"and the arity check above is passing on a package that no longer does the work")
	}
}
