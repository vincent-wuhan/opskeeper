package embedding

import (
	"fmt"
	"sort"
	"sync"
)

// ProviderFactory builds an Embedder from the resolved config.
//
// It is the extension point a package registers through: a self-hosted
// embedding server, a different ONNX runtime, a vendor shape nobody
// anticipated is a registration, not an edit to New.
type ProviderFactory func(cfg Config) (Embedder, error)

// providerFactories is the embedding provider registry.
//
// Before this map, the provider set was a switch in New and nothing could
// add one — a self-hosted embedding server that speaks the OpenAI shape had
// to masquerade as "openai", and a genuinely new shape had no way in. Now a
// provider is a registration, and the switch is a lookup.
//
// The three local spellings ("local", "fastembed", "onnx") share one
// factory on purpose: they name the same in-process implementation, and the
// aliases exist so an operator's existing configuration keeps working.
var (
	providerMu        sync.RWMutex
	providerFactories = map[string]ProviderFactory{
		"openai": openAIFactory,
		"local":  localFactory,
		// Aliases for the in-process ONNX implementation.
		"fastembed": localFactory,
		"onnx":      localFactory,
	}
)

// openAIFactory adapts newOpenAI to the registry's signature. It returns a
// typed-nil as interface(nil) on error so a caller's `embedder != nil` check
// behaves the way it did before the registry.
func openAIFactory(cfg Config) (Embedder, error) {
	e, err := newOpenAI(cfg)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// localFactory adapts newLocal to the registry's signature.
func localFactory(cfg Config) (Embedder, error) {
	e, err := newLocal(cfg)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// RegisterProvider registers an embedding provider by name.
//
// It refuses a duplicate rather than replacing, and an empty name, for the
// same reason the microkernel registry does: a silent replace is how one
// provider takes over another's name.
func RegisterProvider(name string, f ProviderFactory) error {
	if name == "" {
		return fmt.Errorf("embedding: provider name is empty")
	}
	if f == nil {
		return fmt.Errorf("embedding: provider %q has no implementation", name)
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	if _, ok := providerFactories[name]; ok {
		return fmt.Errorf("embedding: provider %q is already registered", name)
	}
	providerFactories[name] = f
	return nil
}

// lookupProvider returns the factory for name.
func lookupProvider(name string) (ProviderFactory, bool) {
	providerMu.RLock()
	defer providerMu.RUnlock()
	f, ok := providerFactories[name]
	return f, ok
}

// RegisteredProviders returns the registered names, sorted. It is what a
// diagnostics page lists, derived from the registry so it cannot drift from
// what New will actually build.
func RegisteredProviders() []string {
	providerMu.RLock()
	defer providerMu.RUnlock()
	out := make([]string, 0, len(providerFactories))
	for name := range providerFactories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
