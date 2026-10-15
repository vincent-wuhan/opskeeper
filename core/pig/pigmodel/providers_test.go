package pigmodel

import (
	"context"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// stubProvider is a do-nothing ai.Provider: the registry tests care about
// resolution, not about a stream.
type stubProvider struct{ id string }

func (s stubProvider) ID() string { return s.id }

func (s stubProvider) Stream(context.Context, ai.TranscriptContext, ai.StreamOptions) (*ai.AssistantMessageEventStream, error) {
	return nil, nil
}

func (s stubProvider) Close() error { return nil }

func TestEveryKnownProviderHasAFactory(t *testing.T) {
	// The registry is the set of providers Resolve can build. If a provider
	// is known to the domain but has no factory, Resolve fails for it at
	// runtime — this makes that a build-time fact.
	for _, id := range domain.KnownProviders() {
		if _, ok := lookupProviderFactory(id); !ok {
			t.Errorf("provider %q is known to the domain but has no factory", id)
		}
	}
}

func TestRegisteredProvidersIsDerivedFromTheRegistry(t *testing.T) {
	got := RegisteredProviders()
	if len(got) == 0 {
		t.Fatal("RegisteredProviders returned nothing; the built-in factories must be registered")
	}
	for _, id := range got {
		if _, ok := lookupProviderFactory(id); !ok {
			t.Errorf("RegisteredProviders listed %q but no factory is registered", id)
		}
	}
}

func TestRegisterRefusesDuplicate(t *testing.T) {
	// A package must not silently take over a built-in provider's id.
	err := RegisterProviderFactory(domain.ProviderAnthropic, func(ProviderConfig, string, func(context.Context) (string, error)) ai.Provider {
		return stubProvider{id: "hijack"}
	})
	if err == nil {
		t.Fatal("registering over a built-in provider id must be refused")
	}
	// The built-in must survive the refused takeover.
	if f, ok := lookupProviderFactory(domain.ProviderAnthropic); !ok || f == nil {
		t.Fatal("the built-in anthropic factory must survive a refused registration")
	}
}

func TestRegisterRefusesEmptyAndNil(t *testing.T) {
	if err := RegisterProviderFactory("", func(ProviderConfig, string, func(context.Context) (string, error)) ai.Provider {
		return stubProvider{id: "x"}
	}); err == nil {
		t.Fatal("an empty provider id must be refused")
	}
	if err := RegisterProviderFactory(domain.ProviderID("brand-new"), nil); err == nil {
		t.Fatal("a nil factory must be refused")
	}
}

func TestLookupUnknownProviderIsAbsent(t *testing.T) {
	if _, ok := lookupProviderFactory(domain.ProviderID("does-not-exist")); ok {
		t.Fatal("an unregistered provider id must not resolve")
	}
}

func TestRegisterThenResolve(t *testing.T) {
	// A third-party provider is a registration, not a patch: after this,
	// the id resolves through the same lookup a built-in does.
	id := domain.ProviderID("third-party-test")
	if _, ok := lookupProviderFactory(id); ok {
		t.Skip("id already registered by another test")
	}
	err := RegisterProviderFactory(id, func(cfg ProviderConfig, providerID string, _ func(context.Context) (string, error)) ai.Provider {
		return stubProvider{id: providerID}
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	f, ok := lookupProviderFactory(id)
	if !ok {
		t.Fatal("the registered provider must resolve")
	}
	if got := f(ProviderConfig{}, string(id), nil); got == nil {
		t.Fatal("the factory must build a provider")
	}
}
