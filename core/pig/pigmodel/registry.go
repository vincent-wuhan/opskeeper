package pigmodel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// Errors a caller distinguishes. The distinction matters: ErrNotConfigured
// means "do not retry, the operator has not set this up", while a transport
// error means "retry may help".
var (
	// ErrNotConfigured means the provider or model has no usable settings.
	ErrNotConfigured = errors.New("pigmodel: provider not configured")
	// ErrUnknownProvider means the id is not one this build knows.
	ErrUnknownProvider = errors.New("pigmodel: unknown provider")
	// ErrNoFallback means a selection was incomplete and no cluster default
	// is set, so there is nothing to resolve to.
	ErrNoFallback = errors.New("pigmodel: no default provider configured")
	// ErrModelNotOffered means the provider is configured but does not
	// offer the requested slug.
	ErrModelNotOffered = errors.New("pigmodel: model not offered by provider")
)

// Registry resolves a model selection to a PiG provider and model.
//
// Providers are cached per id: constructing one is cheap but not free, and a
// provider holds an HTTP client that should be reused across turns. The
// cache key is the provider id alone, not the model, because a PiG provider
// serves every model in its family and the model is a per-call field.
//
// Credentials are deliberately not cached. A provider is built with a
// GetAPIKey closure that re-reads settings on every request, so rotating a
// key in the admin UI takes effect on the next call without rebuilding the
// provider and without the old key lingering in a cached struct.
type Registry struct {
	src SettingsSource

	mu       sync.RWMutex
	cache    map[domain.ProviderID]ai.Provider
	observer CallObserver
}

// CallObserver is told about every provider invocation the registry makes,
// including the ones that fail.
//
// It exists because the provider id and the model id are only known here —
// they are the output of resolution, and every caller above this layer sees
// a settled reply or an error and nothing in between. The observer is a
// hook rather than a direct prometheus call for the same reason the rest of
// this package takes no dependency on a metrics library: a node agent and a
// manager run the same registry, and only one of them has a /metrics
// endpoint. The host that has one passes prom.ObserveLLMCall.
type CallObserver func(provider, model, status string, seconds float64, inputTokens, outputTokens int)

// RegistryOption configures a Registry at construction.
type RegistryOption func(*Registry)

// WithCallObserver installs the observer Complete reports through.
func WithCallObserver(fn CallObserver) RegistryOption {
	return func(r *Registry) { r.observer = fn }
}

// NewRegistry returns a Registry over src. src must be non-nil.
func NewRegistry(src SettingsSource, opts ...RegistryOption) *Registry {
	r := &Registry{src: src, cache: make(map[domain.ProviderID]ai.Provider)}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// observe reports one provider invocation, if anybody is listening.
//
// A nil observer is the normal case for a node agent and for every test
// that does not care about telemetry, so the call is a nil check and
// nothing else. The status vocabulary is ok | error: a provider that
// refuses a request has still consumed an attempt, and a metrics series
// that only counts successes is how a failing provider stays invisible
// until an operator reads a log.
func (r *Registry) observe(model *ai.Model, status string, seconds float64, inputTokens, outputTokens int) {
	if r.observer == nil || model == nil {
		return
	}
	r.observer(model.Provider.ID(), model.ID, status, seconds, inputTokens, outputTokens)
}

// Resolve turns a possibly-partial selection into a concrete model ref.
//
// Resolution order:
//
//  1. A fully pinned selection (provider and model) is taken as-is and only
//     validated against the provider's offered list.
//  2. A provider without a model uses that provider's default model.
//  3. A model without a provider resolves the provider by scanning the
//     configured providers for one that offers the slug. The scan order is
//     KnownProviders order, so the result is deterministic rather than
//     dependent on map iteration.
//  4. An empty selection uses the cluster default provider and its default
//     model.
func (r *Registry) Resolve(ctx context.Context, sel domain.ModelSelection) (domain.ProviderID, string, error) {
	switch {
	case sel.Provider != "" && sel.Model != "":
		cfg, ok := r.src.ProviderConfig(ctx, sel.Provider)
		if !ok {
			return "", "", fmt.Errorf("%w: %s", ErrUnknownProvider, sel.Provider)
		}
		if !cfg.HasModel(sel.Model) {
			return "", "", fmt.Errorf("%w: %s/%s", ErrModelNotOffered, sel.Provider, sel.Model)
		}
		return sel.Provider, sel.Model, nil

	case sel.Provider != "":
		cfg, ok := r.src.ProviderConfig(ctx, sel.Provider)
		if !ok {
			return "", "", fmt.Errorf("%w: %s", ErrUnknownProvider, sel.Provider)
		}
		if !cfg.Configured() {
			return "", "", fmt.Errorf("%w: %s", ErrNotConfigured, sel.Provider)
		}
		return sel.Provider, cfg.DefaultModel, nil

	case sel.Model != "":
		for _, id := range domain.KnownProviders() {
			cfg, ok := r.src.ProviderConfig(ctx, id)
			if !ok || !cfg.Configured() {
				continue
			}
			if cfg.HasModel(sel.Model) {
				return id, sel.Model, nil
			}
		}
		return "", "", fmt.Errorf("%w: %s", ErrModelNotOffered, sel.Model)

	default:
		id, ok := r.src.DefaultProvider(ctx)
		if !ok {
			return "", "", ErrNoFallback
		}
		cfg, ok := r.src.ProviderConfig(ctx, id)
		if !ok || !cfg.Configured() {
			return "", "", fmt.Errorf("%w: default %s", ErrNotConfigured, id)
		}
		return id, cfg.DefaultModel, nil
	}
}

// Model resolves a selection and returns a PiG model bound to a cached
// provider, together with the stream options that carry the per-request
// API key.
//
// The returned options carry the key rather than the provider, so a key
// rotation between a long-lived provider's construction and this call is
// still observed.
func (r *Registry) Model(ctx context.Context, sel domain.ModelSelection) (*ai.Model, ai.StreamOptions, error) {
	id, slug, err := r.Resolve(ctx, sel)
	if err != nil {
		return nil, ai.StreamOptions{}, err
	}
	cfg, _ := r.src.ProviderConfig(ctx, id)

	provider, err := r.provider(ctx, id, cfg)
	if err != nil {
		return nil, ai.StreamOptions{}, err
	}

	opts := ai.StreamOptions{
		APIKey: cfg.APIKey,
		// SessionID is the caller's opaque cache key. It must never be a
		// user or tenant identifier: providers use it to key their prompt
		// cache and it is echoed on the wire.
		SessionID: string(id),
	}
	if lvl := sel.ThinkingLevel; lvl != "" {
		if parsed, ok := parseThinkingLevel(lvl); ok {
			opts.Thinking = parsed
		}
		// An unrecognised level is ignored rather than rejected: a stale
		// console sending a level this build predates must still get an
		// answer, just at the provider's default effort.
	}

	model := &ai.Model{
		ID:               slug,
		DisplayName:      slug,
		Provider:         provider,
		ProviderMeta:     ai.ProviderMetadata{ProviderID: string(id), BaseURL: normalizeBaseURL(cfg.BaseURL)},
		Capabilities:     capabilitiesFor(cfg, slug),
		SamplingParams:   map[string]any{},
		PromptCache:      nil,
		InputLimits:      nil,
		ThinkingLevelMap: ai.ThinkingLevelMap{},
	}
	return model, opts, nil
}

// capabilitiesFor derives what the model can do from the slug. OpsKeeper's
// settings carry no capability metadata, so the mapping is deliberately
// conservative: a model is assumed to support tool use (every OpsKeeper
// model must, or the agent loop cannot run) and images are assumed off
// unless the slug says otherwise.
func capabilitiesFor(cfg ProviderConfig, slug string) ai.ModelCapabilities {
	return ai.ModelCapabilities{
		SupportsToolUse: true,
		SupportsImages:  false,
		MaxThinking:     ai.ThinkingOff,
		ContextWindow:   0, // unknown: providers apply their own default
		MaxOutputTokens: 0,
	}
}

// provider returns a cached provider for id, building it on first use.
func (r *Registry) provider(ctx context.Context, id domain.ProviderID, cfg ProviderConfig) (ai.Provider, error) {
	if p, ok := r.cached(id); ok {
		return p, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check: another goroutine may have built it while we waited.
	if p, ok := r.cache[id]; ok {
		return p, nil
	}

	// The closure re-reads settings on every call so a key rotation is
	// observed without rebuilding the provider.
	getKey := func(context.Context) (string, error) {
		live, ok := r.src.ProviderConfig(context.WithoutCancel(ctx), id)
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrNotConfigured, id)
		}
		return live.APIKey, nil
	}
	base := normalizeBaseURL(cfg.BaseURL)
	providerID := string(id)

	var p ai.Provider
	switch id {
	case domain.ProviderAnthropic:
		p = ai.NewAnthropicProvider(ai.AnthropicConfig{
			APIKey:     cfg.APIKey,
			Model:      cfg.DefaultModel,
			ProviderID: providerID,
			BaseURL:    base,
		})
	case domain.ProviderGemini:
		p = ai.NewGoogleProvider(ai.GoogleConfig{
			APIKey:     cfg.APIKey,
			Model:      cfg.DefaultModel,
			ProviderID: providerID,
			BaseURL:    geminiBaseURL(base),
		})
	case domain.ProviderOpenAI, domain.ProviderCustom, domain.ProviderZhipu,
		domain.ProviderDeepSeek, domain.ProviderKimi:
		// These five share the OpenAI wire shape. Compat carries the
		// per-endpoint quirks Pi already knows about: whether to send
		// max_tokens or max_completion_tokens, which endpoints accept
		// reasoning_effort, and which need a non-standard auth path.
		p = ai.NewOpenAIProvider(ai.OpenAIConfig{
			BaseURL:    base,
			Model:      cfg.DefaultModel,
			ProviderID: providerID,
			Compat:     ai.DetectCompat(providerID, base),
			GetAPIKey:  getKey,
		})
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, id)
	}

	if p == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotConfigured, id)
	}
	r.cache[id] = p
	return p, nil
}

func (r *Registry) cached(id domain.ProviderID) (ai.Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.cache[id]
	return p, ok
}

// Invalidate drops a cached provider so the next call rebuilds it. A host
// calls it when an operator edits a provider's base URL, which is baked
// into the provider and cannot be picked up by a credential re-read alone.
func (r *Registry) Invalidate(id domain.ProviderID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, id)
}

// Available reports whether a selection resolves to a configured provider.
// Callers use it to distinguish "not set up" from "set up but failing", so
// the former is not retried.
func (r *Registry) Available(ctx context.Context, sel domain.ModelSelection) bool {
	_, _, err := r.Resolve(ctx, sel)
	return err == nil
}

// Close releases every cached provider's connections. The host calls it on
// shutdown so a reload does not leak transports.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	for id, p := range r.cache {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("pigmodel: close %s: %w", id, err)
		}
	}
	r.cache = make(map[domain.ProviderID]ai.Provider)
	return firstErr
}

// parseThinkingLevel maps a settings string onto PiG's reasoning enum. An
// unrecognised value reports false so the caller can ignore it.
func parseThinkingLevel(raw string) (ai.ThinkingLevel, bool) {
	switch ai.ThinkingLevel(raw) {
	case ai.ThinkingOff:
		// ai.ThinkingNone is an alias for ThinkingOff and cannot appear as
		// a separate case.
		return ai.ThinkingOff, true
	case ai.ThinkingMinimal:
		return ai.ThinkingMinimal, true
	case ai.ThinkingLow:
		return ai.ThinkingLow, true
	case ai.ThinkingMedium:
		return ai.ThinkingMedium, true
	case ai.ThinkingHigh:
		return ai.ThinkingHigh, true
	case ai.ThinkingXHigh:
		return ai.ThinkingXHigh, true
	default:
		return ai.ThinkingOff, false
	}
}

// geminiBaseURL makes the operator's Gemini base URL usable by PiG's native
// Google provider.
//
// PiG builds "{BaseURL}/{APIVersion}/models/{model}:streamGenerateContent"
// and, because it treats an explicit non-empty BaseURL as the operator
// saying "I know what I am doing", it then leaves APIVersion empty unless
// the base URL already spells the version out. So a base URL of
// "https://generativelanguage.googleapis.com" yields a request to
// "/models/...", which 404s — and 404s only in production, because every
// unit test points the provider at a URL it wrote itself.
//
// OpsKeeper's settings and env defaults historically carried the
// OpenAI-compatible endpoint (".../v1beta/openai"), which is a different API
// surface entirely: the native provider would post a Gemini-shaped body to
// an OpenAI-shaped route. Rather than depend on what an operator pasted,
// this normalises to the native endpoint by appending the default version
// path when the base URL does not already end in one.
//
// A base URL that already carries a version path is left exactly as written:
// a proxy or a pinned API version is a deliberate choice, and rewriting it
// would be the same class of bug in the other direction.
func geminiBaseURL(raw string) string {
	if raw == "" {
		// Empty lets PiG apply its own default (generativelanguage.googleapis.com
		// + v1beta), which is correct and needs no help from here.
		return ""
	}
	trimmed := strings.TrimRight(raw, "/")
	// The settings default and the env bootstrap both shipped the
	// OpenAI-compatible surface (".../v1beta/openai"). That path is a
	// different API, so posting a Gemini-shaped body there cannot work;
	// dropping the marker turns it back into the native endpoint the
	// provider actually speaks.
	trimmed = strings.TrimRight(strings.TrimSuffix(trimmed, "/openai"), "/")
	if trimmed == "" {
		return ""
	}
	if hasAPIVersionSuffix(trimmed) {
		return trimmed
	}
	return trimmed + "/v1beta"
}

// hasAPIVersionSuffix reports whether a base URL already names an API
// version as its last path segment.
func hasAPIVersionSuffix(base string) bool {
	last := base
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		last = base[i+1:]
	}
	// "v1", "v1beta", "v1alpha" — the shape Google's API versions take.
	if len(last) < 2 || last[0] != 'v' {
		return false
	}
	rest := last[1:]
	if rest[0] < '0' || rest[0] > '9' {
		return false
	}
	for _, r := range rest[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
