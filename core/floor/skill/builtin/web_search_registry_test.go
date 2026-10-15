package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestRegisteredWebSearchProvidersCoversBuiltins(t *testing.T) {
	got := RegisteredWebSearchProviders()
	want := map[string]bool{providerSearxng: true, providerTavily: true, providerBrave: true}
	for _, name := range got {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("these built-in providers have no implementation: %v", want)
	}
}

func TestRegisterWebSearchProviderRefusesDuplicate(t *testing.T) {
	err := RegisterWebSearchProvider(providerBrave, func(*webSearchSkill, context.Context, *http.Client, string, string, WebSearchConfigResolver, webSearchParams) (json.RawMessage, error) {
		return nil, nil
	})
	if err == nil {
		t.Fatal("registering over a built-in provider must be refused")
	}
}

func TestRegisterWebSearchProviderRefusesEmptyAndNil(t *testing.T) {
	if err := RegisterWebSearchProvider("", func(*webSearchSkill, context.Context, *http.Client, string, string, WebSearchConfigResolver, webSearchParams) (json.RawMessage, error) {
		return nil, nil
	}); err == nil {
		t.Fatal("an empty provider name must be refused")
	}
	if err := RegisterWebSearchProvider("brand-new", nil); err == nil {
		t.Fatal("a nil provider implementation must be refused")
	}
}

func TestSchemaEnumIsDerivedFromTheRegistry(t *testing.T) {
	// The tool schema's provider enum must offer exactly the registered
	// providers, so the model is never shown a provider that has no
	// implementation. This is the check that the switch and the schema
	// cannot drift: they are now one map.
	s := NewWebSearch(WebSearchDeps{})
	md := s.Metadata()
	var enum []string
	for _, item := range md.Params {
		if item.Name == "provider" {
			enum = item.Param.Enum
		}
	}
	if enum == nil {
		t.Fatal("the tool schema must declare a `provider` parameter")
	}
	// The enum leads with the empty "system default" entry, then the names.
	if len(enum) == 0 || enum[0] != "" {
		t.Fatalf("enum = %v; the first entry must be the empty default", enum)
	}
	got := enum[1:]
	want := RegisteredWebSearchProviders()
	if len(got) != len(want) {
		t.Fatalf("enum lists %v but the registry holds %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("enum[%d] = %q; want %q (enum must be the registry, sorted)", i, got[i], want[i])
		}
	}
}

func TestRegisteredThenDispatchable(t *testing.T) {
	typ := "third-party-search-test"
	if _, ok := lookupWebSearchProvider(typ); ok {
		t.Skip("provider already registered by another test")
	}
	err := RegisterWebSearchProvider(typ, func(_ *webSearchSkill, _ context.Context, _ *http.Client, _, _ string, _ WebSearchConfigResolver, _ webSearchParams) (json.RawMessage, error) {
		return json.RawMessage(`{"provider":"third-party-search-test","results":[]}`), nil
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, ok := lookupWebSearchProvider(typ); !ok {
		t.Fatal("the registered provider must resolve")
	}
	// And it must now appear in the schema the model is shown.
	found := false
	for _, name := range RegisteredWebSearchProviders() {
		if name == typ {
			found = true
		}
	}
	if !found {
		t.Fatal("a registered provider must appear in the schema enum")
	}
}
