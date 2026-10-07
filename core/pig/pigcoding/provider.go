package pigcoding

import (
	"fmt"
	"sort"
	"strings"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

// ProviderSpec is one OpsKeeper-configured provider, as the admin settings
// table holds it.
//
// It is deliberately the smallest shape that can be registered with PiG.
// OpsKeeper's settings rows carry exactly these fields, and anything this
// struct does not name cannot reach the model registry — which is the point:
// the registry is the trust boundary for what a model may be told exists.
type ProviderSpec struct {
	// ID is OpsKeeper's stable provider id: "openai", "zhipu", "custom".
	// It is the prefix OpsKeeper uses in its own model-selection vocabulary
	// and the name BuildModel is called with.
	ID string
	// APIKey is the credential. Empty means the provider is configured but
	// unauthenticated, and PiG will refuse a request against it rather than
	// sending an empty bearer token.
	APIKey string
	// BaseURL overrides the provider's default endpoint. Required for the
	// custom provider, optional elsewhere.
	BaseURL string
	// API is the wire format. OpsKeeper's providers are all
	// OpenAI-compatible except Anthropic, so this is usually
	// APIOpenAICompletions; an empty value lets PiG pick from the provider
	// id, which is the safer default when a new id is added to settings
	// before this switch learns about it.
	API ai.API
	// DefaultModel is the slug a request resolves to when the caller pins
	// no model.
	DefaultModel string
	// Models is every slug this provider offers, in display order. It is
	// what the console's model picker reads, and what PiG validates a
	// pinned slug against.
	Models []string
	// Insecure skips TLS verification. Opt-in only, for a self-signed
	// gateway on an internal network.
	Insecure bool
}

// RegisterProviders publishes OpsKeeper's provider configuration into PiG's
// model registry.
//
// This is the whole credential story, and it is worth being precise about
// why it works this way. PiG's own credential model is a file: auth.json
// under the agent directory, written by an interactive `pig login`. A
// server has no interactive login, its keys live in a database an operator
// edits from an admin page, and a key rotation has to take effect on the
// next request without a restart. So OpsKeeper does not use auth.json at
// all: it registers the key it read from its own settings table, and
// re-registers on every change. PiG resolves the credential through the
// registry it was handed, so an edit is visible to the next model request
// with no cache to expire and no restart.
//
// RegisterProviders is a full replace, not a merge. A provider that was
// removed from settings must stop resolving, and a merge would leave the
// last-known key registered forever for a provider nobody configured.
func RegisterProviders(rt *Runtime, specs []ProviderSpec) error {
	if rt == nil || rt.Services() == nil {
		return ErrClosed
	}
	registry := rt.Services().Registry().ModelRegistry

	// The catalogue is replaced wholesale, not merged, and that is the same
	// decision the registry makes below for a different reason. A provider
	// removed from the settings table must stop resolving, and a merge
	// would leave the last-known key registered forever for a provider
	// nobody configured.
	catalogue := make(map[string]struct{})

	// Sorted so a boot is deterministic: registration order feeds PiG's
	// dynamic provider ordering, which decides which provider a bare model
	// slug resolves to when more than one offers it. Map iteration would
	// make that answer differ between two otherwise identical processes.
	ordered := append([]ProviderSpec(nil), specs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	registered := make(map[string]struct{}, len(ordered))
	for _, spec := range ordered {
		cfg, err := providerConfig(spec)
		if err != nil {
			return fmt.Errorf("pigcoding: provider %s: %w", spec.ID, err)
		}
		// SetProvider rather than RegisterProvider: the former replaces a
		// registration in one committed change, so a model list that shrank
		// loses its removed entries instead of keeping them. The latter
		// merges and would leave a deleted model selectable.
		registry.SetProvider(spec.ID, cfg)
		registered[spec.ID] = struct{}{}
		for _, slug := range slugsOf(spec) {
			catalogue[spec.ID+"/"+slug] = struct{}{}
		}
	}

	rt.mu.Lock()
	rt.catalogue = catalogue
	rt.mu.Unlock()
	return nil
}

// slugsOf is every slug a provider offers, default first, de-duplicated.
//
// It mirrors the list providerConfig builds, and the two are kept in step
// by construction: both read the same spec, and a slug the registry knows
// but the catalogue does not is a model the console cannot offer, while one
// the catalogue knows but the registry does not is a model every request
// for it fails on.
func slugsOf(spec ProviderSpec) []string {
	seen := make(map[string]struct{}, len(spec.Models)+1)
	out := make([]string, 0, len(spec.Models)+1)
	for _, slug := range append([]string{spec.DefaultModel}, spec.Models...) {
		if slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		out = append(out, slug)
	}
	return out
}

// ForgetProviders removes providers that are no longer configured.
//
// It is separate from RegisterProviders because a removal is a different
// act with a different blast radius: adding a provider makes new model
// calls possible, while removing one has to actually stop them. A caller
// that wants the two to be atomic should hold the previous spec slice and
// diff it itself — the diff is host policy (which settings rows are
// authoritative), and PiG has no opinion about it.
func ForgetProviders(rt *Runtime, ids []string) {
	if rt == nil || rt.Services() == nil {
		return
	}
	registry := rt.Services().Registry().ModelRegistry
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, id := range ids {
		registry.SetProvider(id, extension.ProviderConfig{Name: id})
		for key := range rt.catalogue {
			if strings.HasPrefix(key, id+"/") {
				delete(rt.catalogue, key)
			}
		}
	}
}

// providerConfig renders one OpsKeeper provider as PiG's registration.
//
// The default model's context window and output budget are left at zero
// rather than guessed. PiG treats a zero limit as "the provider decides",
// which is the honest answer: OpsKeeper's settings table records which
// slugs exist, not how large their windows are, and inventing a number
// would silently truncate a long investigation on the one model an operator
// happens to be using.
func providerConfig(spec ProviderSpec) (extension.ProviderConfig, error) {
	if spec.ID == "" {
		return extension.ProviderConfig{}, fmt.Errorf("provider has no id")
	}
	api := spec.API
	if api == "" {
		api = defaultAPI(spec.ID)
	}

	models := make([]extension.ProviderModelConfig, 0, len(spec.Models)+1)
	seen := make(map[string]struct{}, len(spec.Models)+1)
	slugs := spec.Models
	if spec.DefaultModel != "" {
		// The default is prepended rather than appended so a catalog that
		// forgot to list it still offers it — and so the console's default
		// is the first thing a reader sees.
		slugs = append([]string{spec.DefaultModel}, spec.Models...)
	}
	for _, slug := range slugs {
		if slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		models = append(models, extension.ProviderModelConfig{
			ID:      slug,
			Name:    slug,
			API:     api,
			BaseURL: spec.BaseURL,
			Input:   []string{"text"},
		})
	}
	if len(models) == 0 {
		return extension.ProviderConfig{}, fmt.Errorf("no models configured")
	}

	return extension.ProviderConfig{
		Name:       spec.ID,
		BaseURL:    spec.BaseURL,
		APIKey:     spec.APIKey,
		API:        api,
		Insecure:   spec.Insecure,
		AuthHeader: spec.APIKey != "",
		Models:     models,
	}, nil
}

// defaultAPI maps an OpsKeeper provider id onto a wire format.
//
// The default is OpenAI-compatible, because that is what every provider in
// OpsKeeper's settings table actually speaks — including the ones with
// their own brand names, which are all OpenAI-compatible gateways. The two
// exceptions are the two that are not, and both have been exceptions since
// before this function existed; a third is the signal to add a case here
// rather than to start guessing per-request.
func defaultAPI(id string) ai.API {
	switch id {
	case "anthropic":
		return ai.APIAnthropicMessages
	default:
		return ai.APIOpenAICompletions
	}
}
