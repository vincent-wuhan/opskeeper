package edge

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// TestEdgeCatalogIsTheMethodsTheRCAToolsActuallyCall is the assertion behind
// decision 283's claim that the cut narrowed anything.
//
// The claim is easy to make and easy to lose: an interface is a snapshot, and
// the next person who needs one more thing adds one more method. Nothing about
// the compiler objects, and the seam widens every time until it is the concrete
// type again with extra steps. The method set is pinned, and the assertion
// fails on a *removal* too — a port that shrank is a decision somebody has to
// make out loud rather than discover later.
func TestEdgeCatalogIsTheMethodsTheRCAToolsActuallyCall(t *testing.T) {
	typ := reflect.TypeOf((*domain.EdgeCatalog)(nil)).Elem()
	want := []string{"ListCatalog", "PluginHealth", "Presence", "PresenceByName"}
	got := make([]string, 0, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("domain.EdgeCatalog has methods %v, want %v", got, want)
	}
}

// TestNoEdgeCatalogSignatureNamesAManagerPackage is the assertion that this cut
// is a cut and not a rename.
//
// Before decision 283 every RCA tool held `*edgebiz.Usecase` outright, and two
// of them held a locally-declared interface whose return type was an edge GORM
// entity — a port whose signature reached past the port into the store. After
// the cut, everything reachable from these four signatures is declared in
// core/domain or the standard library. Checking the package path of every type
// reachable from a signature means moving a row to another manager package
// shows up as a diff instead of as a quiet re-widening.
//
// `map[string]interface{}` reaches PluginRow.Spec, which is deliberately
// unstructured: the set of plugins is a property of the node's installed
// binaries, and typing it would be the marketplace's schema leaking into the
// contract layer.
func TestNoEdgeCatalogSignatureNamesAManagerPackage(t *testing.T) {
	typ := reflect.TypeOf((*domain.EdgeCatalog)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		seen := map[reflect.Type]bool{}
		walkCatalogSignature(t, typ.String()+"."+m.Name, m.Type, seen)
	}
}

// walkCatalogSignature descends into everything a type can be built out of.
//
// The Func case is the whole test, and getting it wrong is a mistake this
// repository has already made once: an interface method's reflect.Type has
// Kind Func, so a switch that handled Ptr and Slice but not Func walked nothing
// at all and the assertion below it never fired (decision 281). The `seen` set
// is per method and guards against a cycle; it is keyed on the type, not on the
// path, so a struct reachable twice is still reported under the first path that
// reaches it.
func walkCatalogSignature(t *testing.T, where string, typ reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if typ == nil || seen[typ] {
		return
	}
	seen[typ] = true
	switch typ.Kind() {
	case reflect.Func:
		for i := 0; i < typ.NumIn(); i++ {
			walkCatalogSignature(t, where+" in", typ.In(i), seen)
		}
		for i := 0; i < typ.NumOut(); i++ {
			walkCatalogSignature(t, where+" out", typ.Out(i), seen)
		}
	case reflect.Interface, reflect.Struct:
		for i := 0; i < typ.NumMethod(); i++ {
			walkCatalogSignature(t, where+"."+typ.Method(i).Name, typ.Method(i).Type, seen)
		}
	case reflect.Slice, reflect.Array, reflect.Ptr, reflect.Map, reflect.Chan:
		walkCatalogSignature(t, where, typ.Elem(), seen)
	}
	if typ.PkgPath() == "" {
		return
	}
	if strings.HasPrefix(typ.PkgPath(), "github.com/vincent-wuhan/opskeeper/core/manager/") {
		t.Errorf("%s names %s.%s, which lives in core/manager; the catalog port is supposed to "+
			"be expressible in core/domain types and standard-library types", where, typ.PkgPath(), typ.Name())
	}
}
