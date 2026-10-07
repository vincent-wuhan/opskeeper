package frontierbound

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestTheEdgePortsAreTheMethodsTheHandlersActuallyCall is the assertion behind
// decision 281's claim that the cut narrowed anything.
//
// The claim is easy to make and easy to lose: an interface is a snapshot, and
// the next person who needs one more thing adds one more method. Nothing about
// the compiler objects, and the seam gets wider every time until it is the
// concrete type again with extra steps. So the method sets are pinned, and the
// assertion fails on a *removal* too — a port that shrank is a decision
// somebody has to make out loud rather than discover later.
func TestTheEdgePortsAreTheMethodsTheHandlersActuallyCall(t *testing.T) {
	cases := []struct {
		port reflect.Type
		want []string
	}{
		{reflect.TypeOf((*EdgeAuthenticator)(nil)).Elem(), []string{"Authenticate"}},
		{reflect.TypeOf((*EdgeLifecycle)(nil)).Elem(), []string{"HandleHeartbeat", "HandleOffline", "HandleRegister", "RecordPluginHealth"}},
		{reflect.TypeOf((*ChangeEventIngestor)(nil)).Elem(), []string{"Ingest"}},
		{reflect.TypeOf((*PluginConfigFetcher)(nil)).Elem(), []string{"FetchForEdge"}},
	}
	for _, c := range cases {
		typ := c.port
		got := make([]string, 0, typ.NumMethod())
		for i := 0; i < typ.NumMethod(); i++ {
			got = append(got, typ.Method(i).Name)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s has methods %v, want %v", typ, got, c.want)
		}
	}
}

// TestNoEdgePortSignatureNamesAManagerPackage is the assertion that this cut is
// a cut and not a rename.
//
// After decision 281 the handler names the edge domain nowhere, and what is
// left in these signatures is core/floor's wire types, the standard library,
// and core/domain. A method that took `edgebiz.PluginHealth` would still
// satisfy a call site, still compile, and still be a package-shaped boundary —
// which is exactly the thing decisions 218 and 281 exist to remove. Checking
// the package path of every type reachable from a signature means moving a row
// to another manager package shows up as a diff instead of as a quiet
// re-widening.
//
// `map[string]interface{}` reaches PluginConfig.Spec, which is deliberately
// unstructured: it is the plugin author's own spec, and typing it would be the
// marketplace's schema leaking into the transport.
func TestNoEdgePortSignatureNamesAManagerPackage(t *testing.T) {
	const managerPrefix = "github.com/vincent-wuhan/opskeeper/core/manager/"
	for _, typ := range []reflect.Type{
		reflect.TypeOf((*EdgeAuthenticator)(nil)).Elem(),
		reflect.TypeOf((*EdgeLifecycle)(nil)).Elem(),
		reflect.TypeOf((*ChangeEventIngestor)(nil)).Elem(),
		reflect.TypeOf((*PluginConfigFetcher)(nil)).Elem(),
	} {
		for i := 0; i < typ.NumMethod(); i++ {
			m := typ.Method(i)
			seen := map[reflect.Type]bool{}
			walkSignature(t, typ.String()+"."+m.Name, m.Type, seen)
		}
	}
}

// walkSignature descends into everything a type can be built out of.
//
// The Func case is the whole test, and getting it wrong is what the first
// version of this file did: an interface method's reflect.Type has Kind Func,
// so a switch that handled Ptr and Slice but not Func walked nothing at all
// and the assertion below it never fired. Widening the forbidden prefix to all
// of core/ — a mutation that should have produced a dozen errors — produced
// none, and that is how the hole was found.
//
// The `seen` set is per method and guards against a type graph with a cycle;
// it is keyed on the type, not on the path, so a struct reachable twice is
// still reported under the first path that reaches it.
func walkSignature(t *testing.T, where string, typ reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if typ == nil || seen[typ] {
		return
	}
	seen[typ] = true
	switch typ.Kind() {
	case reflect.Func:
		for i := 0; i < typ.NumIn(); i++ {
			walkSignature(t, where+" in", typ.In(i), seen)
		}
		for i := 0; i < typ.NumOut(); i++ {
			walkSignature(t, where+" out", typ.Out(i), seen)
		}
	case reflect.Interface, reflect.Struct:
		for i := 0; i < typ.NumMethod(); i++ {
			walkSignature(t, where+"."+typ.Method(i).Name, typ.Method(i).Type, seen)
		}
	case reflect.Slice, reflect.Array, reflect.Ptr, reflect.Map, reflect.Chan:
		walkSignature(t, where, typ.Elem(), seen)
	}
	if typ.PkgPath() == "" {
		return
	}
	if strings.HasPrefix(typ.PkgPath(), "github.com/vincent-wuhan/opskeeper/core/manager/") {
		t.Errorf("%s names %s.%s, which lives in core/manager; the four ports are supposed to be "+
			"expressible in core/floor wire types, standard-library types and core/domain",
			where, typ.PkgPath(), typ.Name())
	}
}
