package domain

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The four types here were pulled out of two business packages because three
// domains needed them and none of the three owned them. That makes this file
// the place where a cut can quietly undo itself: every type moved down here
// arrives with whatever it was next to, and the first thing anybody checks is
// whether the code still compiles — which it will.

// TestThisPackageImportsNothingButTheStandardLibrary is the guard for that.
//
// `core/domain` is imported by every bounded context in the control plane, so
// the moment it imports something, that something is no longer optional for
// any of them, and the dependency stops being a domain edge and becomes a
// floor-wide one. Nothing in the build would say so: the import graph would
// have a new entry, the entry would be legal, and `domaincheck` — which walks
// `core/manager` — would never see it, because the file is not under that
// prefix at all.
//
// This is the same hole the price column fell into three decisions ago, seen
// from the other side. The tool learned to count a closure (decision 237) and
// then still priced `marketplace -> pluginimport` at two types when it carried
// six. The general form of that mistake is "the shape you can see is smaller
// than the shape you will have", and this assertion is the cheap place to
// catch it for the one package whose whole value is being dependency-free.
func TestThisPackageImportsNothingButTheStandardLibrary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	var foreign []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, spec := range file.Imports {
			path, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				t.Fatalf("%s: unquote %s: %v", name, spec.Path.Value, uerr)
			}
			// A standard library path has no dot in its first segment. That
			// is a cruder rule than a list, and deliberately so: a list would
			// have to be edited every time something is added, and the next
			// person would not edit it.
			if strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
				foreign = append(foreign, name+": "+path)
			}
		}
	}
	sort.Strings(foreign)
	if len(foreign) != 0 {
		t.Errorf("core/domain imports %v.\nEvery domain in the control plane imports this "+
			"one, so a dependency here is not a domain edge that domaincheck can price — it "+
			"is a dependency on all of them at once, and domaincheck will not report it "+
			"because the file is not under core/manager. If a shape needs a name from "+
			"elsewhere, either it belongs to that package's consumers to declare, or the "+
			"shape is two shapes", foreign)
	}
}

// TestPluginImportReportCarriesEveryKeyTheImportEndpointReturns pins the wire
// contract. This struct is the body of POST /v1/marketplace/import, and the
// console parses every one of these keys — so a rename here is a broken page,
// not a refactor.
//
// The second half is the part that is easy to get wrong quietly: a field with
// no json tag serialises under its Go name, and a Go name is CamelCase while
// every other key in this object is snake_case. One such field and the console
// is handed `{"SourceManifest": ...}` where it expects `{"source_manifest": ...}`,
// and nothing in this repository can see it — the response still marshals, the
// route still returns 200, and the test above still passes.
func TestPluginImportReportCarriesEveryKeyTheImportEndpointReturns(t *testing.T) {
	want := []string{
		"agent_environments", "agents", "decisions", "description", "extensions",
		"kind", "mcp", "name", "prompts", "skills", "source_manifest", "themes",
		"version", "warnings",
	}
	typ := reflect.TypeOf(PluginImportReport{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag, tagged := field.Tag.Lookup("json")
		if !tagged {
			t.Errorf("PluginImportReport.%s has no json tag, so it marshals as %q. Every "+
				"other key in this object is snake_case; one CamelCase key is a console that "+
				"reads undefined and shows an empty report with no error anywhere",
				field.Name, field.Name)
			continue
		}
		got = append(got, strings.SplitN(tag, ",", 2)[0])
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("PluginImportReport marshals %v; POST /v1/marketplace/import has always "+
			"answered %v. This is the response body, not an internal shape", got, want)
	}
}

// TestTheImportReportNamesNoTypeFromAnotherPackage is the closure check, done
// on the thing rather than on the report about it.
//
// A `Report` is only movable if everything its fields are made of is movable
// too, and the two ways that fails are a field typed by a name in another
// domain, and a slice element typed by one. Both are invisible to `go build`
// and both are exactly what decision 241 had to fix — `Report.Kind` was a
// chatruntime.ContainerKind and `Report.Warnings` was a []chatruntime.LoadWarning
// before it, so the "two types" the price column quoted came with four more
// and the cut would have been the wrong cut if anybody had believed the price.
func TestTheImportReportNamesNoTypeFromAnotherPackage(t *testing.T) {
	// Any field type reached from here must be declared in this package or
	// built from the standard library. json.RawMessage and Targets are the
	// two shapes that are legitimately not declared here, and Targets IS
	// declared here — so the honest statement of the rule is: the reachable
	// set of field types stays inside this package.
	local := localTypeNames(t)
	var foreign []string
	typ := reflect.TypeOf(PluginImportReport{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		ft := field.Type
		if ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && !local[ft.Name()] {
			foreign = append(foreign, field.Name+" "+ft.Name())
		}
	}
	sort.Strings(foreign)
	if len(foreign) != 0 {
		t.Errorf("PluginImportReport reaches struct types this package does not declare: %v. "+
			"Every one of those is an import this report drags behind it, and the price "+
			"column counts types, not the closure behind them", foreign)
	}

	// Pinning that nothing reaches *out* is not enough on its own, and the
	// first version of this test was not enough on its own: rewriting
	// `Warnings []LoadWarning` as `Warnings []string` closes the closure
	// further than before — the import is gone AND the warnings are now
	// strings — and the check above is right to say nothing, because a string
	// reaches nothing.
	//
	// That is a silent change to a response body, so the three fields whose
	// element type is the whole point of them are named here instead. The
	// keys test already pins the names; this pins what is behind them.
	for field, want := range map[string]string{
		"Warnings":       "LoadWarning",
		"Decisions":      "PluginImportDecision",
		"SourceManifest": "PluginImportSourceManifest",
	} {
		f, ok := typ.FieldByName(field)
		if !ok {
			t.Errorf("PluginImportReport has no %s field; the keys test should have said so", field)
			continue
		}
		el := f.Type
		if el.Kind() == reflect.Slice {
			el = el.Elem()
		}
		if el.Name() != want {
			t.Errorf("PluginImportReport.%s is %s, want %s. The field is a list of findings "+
				"or decisions, and replacing the element with a bare string keeps the json "+
				"key while changing what the console is handed — which is the shape of "+
				"change no key-set assertion can see", field, el, want)
		}
	}
}

// localTypeNames is the set of type names declared in this package, read from
// the syntax tree rather than from a hand-written list. A hand-written list
// would be one more thing to remember to update, and the failure mode is the
// one this file exists to prevent: a type arrives, the list does not, and the
// closure guard reports a false violation that somebody "fixes" by deleting
// the guard.
func localTypeNames(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	names := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range file.Decls {
			gen, isGen := decl.(*ast.GenDecl)
			if !isGen || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				if ts, isType := spec.(*ast.TypeSpec); isType {
					names[ts.Name.Name] = true
				}
			}
		}
	}
	return names
}

// jsonRoundTripIsStable pins that the zero value marshals to an object rather
// than to null, which is the one thing a moved struct can get wrong without
// any key changing.
func TestAnEmptyImportReportIsAnObject(t *testing.T) {
	raw, err := json.Marshal(PluginImportReport{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) == "null" {
		t.Error("an empty PluginImportReport marshals as null; the import page would render " +
			"a report of nothing for a conversion that in fact decided four things")
	}
	if !strings.HasPrefix(string(raw), "{") {
		t.Errorf("an empty PluginImportReport marshals as %s, not an object", raw)
	}
}

// TestContainerSourceCarriesTheSixFieldsTheImporterWrites is the field-set
// guard for the projection that replaced a package model.
//
// The loader's real result also carries parsed `Skills` and `Agents` trees.
// The importer reads neither — it copies files by walking the directory — so
// every one of those fields is a column the port hands over for no reader, and
// a field added "for symmetry with the loader" is the same leak in a smaller
// dose. The set is pinned by name and by count, and adding one has to be argued
// for in the same commit.
func TestContainerSourceCarriesTheSixFieldsTheImporterWrites(t *testing.T) {
	typ := reflect.TypeOf(ContainerSource{})
	if typ.NumField() != 6 {
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, typ.Field(i).Name)
		}
		t.Fatalf("ContainerSource has %d fields %v, want the 6 the conversion report is "+
			"built from; the loader's parsed skill and agent trees are not among them and "+
			"must not become so", typ.NumField(), got)
	}
	want := map[string]bool{
		"Kind": true, "ID": true, "DisplayName": true,
		"Version": true, "Description": true, "Warnings": true,
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !want[name] {
			t.Errorf("ContainerSource.%s is not one of the six fields the report is built "+
				"from; if it was added on purpose, list it here in the same commit and say "+
				"which part of the conversion reads it", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("ContainerSource is missing %s, which the report is built from; dropping "+
			"it breaks a caller that compiles fine against a zero value", name)
	}
}

// TestContainerLoaderAnswersOneQuestion stops the port from accreting.
//
// The importer asks one thing about a directory. A second method is how a
// container loader turns into a package loader inside a port, and a package
// loader is a different seam with a different owner — the same accretion
// `EdgeQuery`'s own doc comment rules out for the edge ports.
func TestContainerLoaderAnswersOneQuestion(t *testing.T) {
	typ := reflect.TypeOf((*ContainerLoader)(nil)).Elem()
	if typ.NumMethod() != 1 {
		var got []string
		for i := 0; i < typ.NumMethod(); i++ {
			got = append(got, typ.Method(i).Name)
		}
		t.Fatalf("ContainerLoader has %d methods %v; it answers one question — what is in "+
			"this directory — and a second method is a package loader arriving through the "+
			"front door", typ.NumMethod(), got)
	}
	if name := typ.Method(0).Name; name != "LoadContainer" {
		t.Errorf("ContainerLoader's one method is %s, want LoadContainer", name)
	}
}

// TestContainerSourceCarriesNoDefaults pins the half of the contract that is
// easy to get wrong in the direction that looks helpful.
//
// The importer falls back to the directory name when a source states no id and
// to "0.0.0" when it states no version. Those are conversion policy — what
// this repository decides an incomplete package should be called — and a
// loader that applied them would be making that decision for the one domain
// that is allowed to. The port returns what the source said, including
// nothing.
func TestContainerSourceCarriesNoDefaults(t *testing.T) {
	typ := reflect.TypeOf(ContainerSource{})
	for _, field := range []string{"ID", "DisplayName", "Version", "Description"} {
		f, ok := typ.FieldByName(field)
		if !ok {
			t.Fatalf("ContainerSource has no %s field", field)
		}
		if f.Type.Kind() != reflect.String {
			t.Errorf("ContainerSource.%s is %s, want string; a defaulted field would be a "+
				"field whose zero value is a decision the converter has not made yet", field, f.Type)
		}
	}
	// The zero value is what a source that declared nothing produces, and it
	// has to be an honest "nothing", not a "0.0.0".
	var zero ContainerSource
	if zero.Version != "" {
		t.Errorf("the zero ContainerSource has Version %q; a default baked into the zero "+
			"value is a default the loader chose", zero.Version)
	}
	if zero.ID != "" {
		t.Errorf("the zero ContainerSource has ID %q, want empty", zero.ID)
	}
}
