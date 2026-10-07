package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// portSource is where the vocabulary actually lives. The constant list in
// log.go is a hand-written copy of the names in it, and a hand-written
// copy is exactly the kind of thing that rots quietly: a new action lands
// in the port, every existing caller still compiles, and this package's
// re-export simply stops offering it.
// The port moved out to the core/base module with the rest of the control
// plane's shared infrastructure (decision 221), so this path is three levels
// up and across rather than two levels up. It is read as a file rather than
// imported on purpose: the point of the test is that the re-export covers
// the vocabulary *in the source*, which an import would hide.
const portSource = "../../../base/pkg/audit/port.go"

// declaredConsts returns every constant name in an AST file mapped to the
// right-hand side it is bound to, reduced to the part that identifies it.
//
// Two forms matter here and the comparison needs both to line up: the port
// binds a name to a string literal, and this package binds it to a
// qualified name (ActionUserCreate = auditport.ActionUserCreate). Anything
// else — an iota block, a computed value — is recorded as "?" so it shows
// up in the diff instead of being silently skipped.
func declaredConsts(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					out[name.Name] = "?"
					continue
				}
				switch rhs := vs.Values[i].(type) {
				case *ast.BasicLit:
					if rhs.Kind == token.STRING {
						out[name.Name] = strings.Trim(rhs.Value, `"`)
						continue
					}
					out[name.Name] = "?"
				case *ast.SelectorExpr:
					out[name.Name] = rhs.Sel.Name
				case *ast.Ident:
					out[name.Name] = rhs.Name
				default:
					out[name.Name] = "?"
				}
			}
		}
	}
	return out
}

// TestTheReExportCoversTheWholeVocabulary holds the two lists in step in
// both directions.
//
// Forward (every port constant is re-exported) is the one that matters
// operationally: a caller reading auditmodel.Action* must see the same
// closed list the middleware and the port agree on.
//
// Backward (every re-export exists in the port) is what catches a rename.
// A constant left behind here would compile, would be handed to a handler,
// and would write an action string no filter in the UI knows about.
func TestTheReExportCoversTheWholeVocabulary(t *testing.T) {
	fset := token.NewFileSet()

	portFile, err := parser.ParseFile(fset, portSource, nil, 0)
	if err != nil {
		t.Fatalf("parse the port: %v", err)
	}
	port := declaredConsts(portFile)

	selfFile, err := parser.ParseFile(fset, "log.go", nil, 0)
	if err != nil {
		t.Fatalf("parse log.go: %v", err)
	}
	reexported := declaredConsts(selfFile)

	// The GORM entities share this package; only the Action*/Resource*/
	// Status* names are vocabulary.
	isVocabulary := func(name string) bool {
		return strings.HasPrefix(name, "Action") ||
			strings.HasPrefix(name, "Resource") ||
			strings.HasPrefix(name, "Status")
	}

	portNames := 0
	for name := range port {
		if !isVocabulary(name) {
			continue
		}
		portNames++
		if _, ok := reexported[name]; !ok {
			t.Errorf("pkg/audit declares %s but model/audit does not re-export it; "+
				"a caller in this module will not see the new action", name)
		}
	}
	if portNames == 0 {
		t.Fatal("no vocabulary constants were found in the port; the AST walk is broken")
	}

	reexportNames := 0
	for name := range reexported {
		if !isVocabulary(name) {
			continue
		}
		reexportNames++
		target := reexported[name]
		if target == "?" {
			t.Errorf("model/audit's %s is not a plain alias; the re-export has to be a "+
				"compile-time identity, not a copied string", name)
			continue
		}
		if _, ok := port[target]; !ok {
			t.Errorf("model/audit re-exports %s = auditport.%s, which pkg/audit no longer "+
				"declares; this name would reach the database as a value nothing else can produce",
				name, target)
		}
	}
	if reexportNames != portNames {
		t.Errorf("the port declares %d vocabulary constants and this file re-exports %d", portNames, reexportNames)
	}
}
