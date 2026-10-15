package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// webSearchFunc searches one provider.
//
// The two endpoint arguments are passed to every provider even though only
// Tavily and Brave use them, so that a provider is a value in a map rather
// than a case in a switch. A provider that ignores them ignores them
// explicitly; the signature is uniform so the registry can hold them
// together.
type webSearchFunc func(
	s *webSearchSkill,
	ctx context.Context,
	client *http.Client,
	tavilyURL, braveURL string,
	resolver WebSearchConfigResolver,
	p webSearchParams,
) (json.RawMessage, error)

// webSearchProviders is the provider registry.
//
// Before this map, the provider set was a switch in the skill's Run and a
// literal list in the tool schema — two places that had to agree and nothing
// kept them agreeing, and no way for a package to add a provider. Now the
// switch is a lookup and the schema's enum is derived from the same map, so
// a provider a package registers is offered to the model and dispatchable in
// one step.
var (
	webSearchMu        sync.RWMutex
	webSearchProviders = map[string]webSearchFunc{
		providerSearxng: func(s *webSearchSkill, ctx context.Context, client *http.Client, _, _ string, resolver WebSearchConfigResolver, p webSearchParams) (json.RawMessage, error) {
			return s.searchSearxng(ctx, client, resolver, p)
		},
		providerTavily: func(s *webSearchSkill, ctx context.Context, client *http.Client, tavilyURL, _ string, resolver WebSearchConfigResolver, p webSearchParams) (json.RawMessage, error) {
			return s.searchTavily(ctx, client, tavilyURL, resolver, p)
		},
		providerBrave: func(s *webSearchSkill, ctx context.Context, client *http.Client, _, braveURL string, resolver WebSearchConfigResolver, p webSearchParams) (json.RawMessage, error) {
			return s.searchBrave(ctx, client, braveURL, resolver, p)
		},
	}
)

// RegisterWebSearchProvider registers a provider by name.
//
// It refuses a duplicate rather than replacing, and an empty name, for the
// same reason the microkernel registry does: a silent replace is how one
// provider takes over another's name.
func RegisterWebSearchProvider(name string, f webSearchFunc) error {
	if name == "" {
		return fmt.Errorf("web_search: provider name is empty")
	}
	if f == nil {
		return fmt.Errorf("web_search: provider %q has no implementation", name)
	}
	webSearchMu.Lock()
	defer webSearchMu.Unlock()
	if _, ok := webSearchProviders[name]; ok {
		return fmt.Errorf("web_search: provider %q is already registered", name)
	}
	webSearchProviders[name] = f
	return nil
}

// lookupWebSearchProvider returns the implementation for name.
func lookupWebSearchProvider(name string) (webSearchFunc, bool) {
	webSearchMu.RLock()
	defer webSearchMu.RUnlock()
	f, ok := webSearchProviders[name]
	return f, ok
}

// RegisteredWebSearchProviders returns the registered names, sorted. It is
// what the tool schema's enum is built from, so the model is offered exactly
// the providers that can actually run.
func RegisteredWebSearchProviders() []string {
	webSearchMu.RLock()
	defer webSearchMu.RUnlock()
	out := make([]string, 0, len(webSearchProviders))
	for name := range webSearchProviders {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
