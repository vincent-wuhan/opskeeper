package domain

import (
	"context"
	"reflect"
	"testing"
)

// This file holds the one property that separates a real cut from a rename.
//
// The Grafana service used to hold `*setting.Service` by name. Declaring
// `SettingStore` and then keeping the concrete type in the field would have
// been the same dependency wearing an interface's clothes — a shape this
// repository has now found five times, and the reason these tests exist
// rather than a comment.
//
// What makes it a boundary is not the interface keyword. It is that the port's
// signature names nothing the producer owns: `*setting.Service` satisfies it
// structurally, and so does a five-line fake, and neither of those facts
// requires the setting package to be in the build graph. A port widened to
// take a `setting.Key` or return a `setting.Value` would satisfy the producer
// just as well while putting the dependency straight back, and it would
// compile, and it would go green in every other gate here.
//
// So the assertion is on the signature's own types, not on who satisfies it.

var errType = reflect.TypeOf((*error)(nil)).Elem()
var ctxType = reflect.TypeOf((*context.Context)(nil)).Elem()

// portNamesNothingButBuiltins fails on any type in a port's signature that
// belongs to a domain rather than to the language or to the context.
func portNamesNothingButBuiltins(t *testing.T, port reflect.Type) {
	t.Helper()
	for i := 0; i < port.NumMethod(); i++ {
		m := port.Method(i)
		in := m.Type.In(0)
		if in.Kind() != reflect.Interface || in != ctxType {
			t.Errorf("port %s: method %s takes %v as its receiver; a port method's first "+
				"argument is the context and nothing else", port, m.Name, in)
		}
		for j := 1; j < m.Type.NumIn(); j++ {
			assertBuiltinOrContext(t, port, m.Name+" argument", m.Type.In(j))
		}
		for j := 0; j < m.Type.NumOut(); j++ {
			assertBuiltinOrContext(t, port, m.Name+" result", m.Type.Out(j))
		}
	}
}

func assertBuiltinOrContext(t *testing.T, port reflect.Type, where string, typ reflect.Type) {
	t.Helper()
	switch {
	case typ == errType, typ == ctxType:
		return
	case typ.Kind() == reflect.String, typ.Kind() == reflect.Bool,
		typ.Kind() == reflect.Int, typ.Kind() == reflect.Int64,
		typ.Kind() == reflect.Float64:
		return
	case typ.Kind() == reflect.Slice && typ.Elem().Kind() == reflect.String:
		return
	case typ.Kind() == reflect.Map && typ.Key().Kind() == reflect.String:
		return
	case typ.Kind() == reflect.Pointer && typ.Elem().Kind() == reflect.Struct:
		// A *struct result is the shape decision 254 and 257 both had to
		// remove: one named type crossing a boundary is a relocation
		// waiting to happen, and it is allowed here only so that a future
		// reviewer has to delete this case deliberately rather than
		// discover it in a diff.
		t.Errorf("port %s: a %s is a named struct crossing the boundary (%s); return the "+
			"columns the consumer reads, or a scalar", port, where, typ)
		return
	}
	t.Errorf("port %s: %s is %v, which is neither a builtin nor context.Context; a port that "+
		"names a domain's own type is a package dependency with an interface in front of it",
		port, where, typ)
}

func TestTheSettingPortNamesNothingButBuiltins(t *testing.T) {
	t.Parallel()
	portNamesNothingButBuiltins(t, reflect.TypeOf((*SettingStore)(nil)).Elem())
}

// The vocabulary moved because a second domain has to name it, and the reason
// it moved rather than being copied is that a copy is a second word with the
// same spelling. These assertions are the cheap half of that: they fail if
// somebody re-declares a constant here, and they fail if somebody changes one
// of these strings while leaving an alias behind that still says the old one.
func TestTheMovedVocabularyKeepsTheValuesItsAliasesPromise(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ got, want string }{
		{SettingCategoryProm, "prom"},
		{SettingCategoryGrafana, "grafana"},
		{SettingKeyPromQueryURL, "query_url"},
		{SettingKeyPromRemoteWriteURL, "remote_write_url"},
		{SettingKeyPromBearerToken, "bearer_token"},
		{SettingKeyPromBasicUser, "basic_user"},
		{SettingKeyPromBasicPassword, "basic_password"},
		{SettingKeyPromTLSInsecure, "tls_insecure"},
		{SettingKeyPromTLSCAPEM, "tls_ca_pem"},
		{SettingKeyGrafanaRootURL, "root_url"},
		{SettingKeyGrafanaSAToken, "sa_token"},
		{SettingKeyGrafanaAPIKey, "api_key"},
		{SettingKeyGrafanaOrgID, "org_id"},
	} {
		if c.got != c.want {
			t.Errorf("a moved setting constant reads %q where the settings table has %q; "+
				"every alias left behind in model/setting now points at a value nothing reads", c.got, c.want)
		}
	}
}

// Two of these must not collide, and the one that would hurt most is a key
// reused across categories — the table is keyed on the pair, so a shared
// spelling is legal and a shared *meaning* is not.
func TestTheGrafanaAndPromKeysAreDistinctWithinTheirCategory(t *testing.T) {
	t.Parallel()
	grafana := map[string]bool{}
	for _, k := range []string{
		SettingKeyGrafanaRootURL, SettingKeyGrafanaSAToken,
		SettingKeyGrafanaAPIKey, SettingKeyGrafanaOrgID,
	} {
		if grafana[k] {
			t.Errorf("two grafana keys read %q", k)
		}
		grafana[k] = true
	}
	prom := map[string]bool{}
	for _, k := range []string{
		SettingKeyPromQueryURL, SettingKeyPromRemoteWriteURL, SettingKeyPromBearerToken,
		SettingKeyPromBasicUser, SettingKeyPromBasicPassword,
		SettingKeyPromTLSInsecure, SettingKeyPromTLSCAPEM,
	} {
		if prom[k] {
			t.Errorf("two prom keys read %q", k)
		}
		prom[k] = true
	}
}
