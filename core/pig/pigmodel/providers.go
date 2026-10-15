package pigmodel

import (
	"context"
	"fmt"
	"sync"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// ProviderFactory builds a PiG provider for one provider id.
//
// It is the microkernel's KindProvider extension point expressed in PiG's
// vocabulary: a package that ships a provider registers a factory here, and
// Resolve can then resolve that provider without a line of the registry
// changing. The three arguments are what a factory needs and nothing more —
// the resolved config, the id it is building for (PiG carries it for
// diagnostics), and the key closure that re-reads the credential on every
// call so a rotation lands without a restart.
type ProviderFactory func(
	cfg ProviderConfig,
	providerID string,
	getKey func(context.Context) (string, error),
) ai.Provider

// providerFactories is the provider registry.
//
// Before this map, adding a provider meant editing a switch inside
// provider(). That switch was the last place in the model layer where the
// set of providers was a property of the source file rather than of a
// registration — so a provider shipped by a package, or a self-hosted
// gateway that speaks a shape nobody anticipated, had no way in short of a
// patch.
//
// The five OpenAI-wire ids share one factory on purpose: they differ in
// what the endpoint accepts, which ai.DetectCompat derives from the id and
// base URL, not in how the provider is built.
var (
	providerMu        sync.RWMutex
	providerFactories = map[domain.ProviderID]ProviderFactory{
		domain.ProviderAnthropic: newAnthropicProvider,
		domain.ProviderGemini:    newGeminiProvider,
		domain.ProviderOpenAI:    newOpenAICompatProvider,
		domain.ProviderCustom:    newOpenAICompatProvider,
		domain.ProviderZhipu:     newOpenAICompatProvider,
		domain.ProviderDeepSeek:  newOpenAICompatProvider,
		domain.ProviderKimi:      newOpenAICompatProvider,
	}
)

// RegisterProviderFactory registers a provider builder for id.
//
// It refuses a duplicate rather than replacing, for the same reason the
// kernel registry does: a silent replace is how one provider takes over
// another's id, and a caller that keeps the id it always used would never
// see the implementation underneath change. A package that means to replace
// a built-in says so by name, not by arriving first.
func RegisterProviderFactory(id domain.ProviderID, f ProviderFactory) error {
	if id == "" {
		return fmt.Errorf("pigmodel: provider id is empty")
	}
	if f == nil {
		return fmt.Errorf("pigmodel: provider factory for %q is nil", id)
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	if _, ok := providerFactories[id]; ok {
		return fmt.Errorf("pigmodel: provider factory for %q is already registered", id)
	}
	providerFactories[id] = f
	return nil
}

// lookupProviderFactory returns the factory for id.
func lookupProviderFactory(id domain.ProviderID) (ProviderFactory, bool) {
	providerMu.RLock()
	defer providerMu.RUnlock()
	f, ok := providerFactories[id]
	return f, ok
}

// RegisteredProviders returns the ids a factory is registered for, in
// domain order. It is what a diagnostics page lists, and it is derived from
// the registry rather than hand-maintained so it cannot drift from what
// Resolve will actually build.
func RegisteredProviders() []domain.ProviderID {
	providerMu.RLock()
	defer providerMu.RUnlock()
	out := make([]domain.ProviderID, 0, len(providerFactories))
	for _, id := range domain.KnownProviders() {
		if _, ok := providerFactories[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// ── first-party factories ───────────────────────────────────────────────────

func newAnthropicProvider(cfg ProviderConfig, providerID string, _ func(context.Context) (string, error)) ai.Provider {
	return ai.NewAnthropicProvider(ai.AnthropicConfig{
		APIKey:     cfg.APIKey,
		Model:      cfg.DefaultModel,
		ProviderID: providerID,
		BaseURL:    normalizeBaseURL(cfg.BaseURL),
	})
}

func newGeminiProvider(cfg ProviderConfig, providerID string, _ func(context.Context) (string, error)) ai.Provider {
	return ai.NewGoogleProvider(ai.GoogleConfig{
		APIKey:     cfg.APIKey,
		Model:      cfg.DefaultModel,
		ProviderID: providerID,
		BaseURL:    geminiBaseURL(normalizeBaseURL(cfg.BaseURL)),
	})
}

func newOpenAICompatProvider(cfg ProviderConfig, providerID string, getKey func(context.Context) (string, error)) ai.Provider {
	base := normalizeBaseURL(cfg.BaseURL)
	// Compat carries the per-endpoint quirks Pi already knows about: whether
	// to send max_tokens or max_completion_tokens, which endpoints accept
	// reasoning_effort, and which need a non-standard auth path.
	return ai.NewOpenAIProvider(ai.OpenAIConfig{
		BaseURL:    base,
		Model:      cfg.DefaultModel,
		ProviderID: providerID,
		Compat:     ai.DetectCompat(providerID, base),
		GetAPIKey:  getKey,
	})
}
