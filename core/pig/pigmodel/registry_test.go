package pigmodel

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

func testSource() map[domain.ProviderID]ProviderConfig {
	return map[domain.ProviderID]ProviderConfig{
		domain.ProviderOpenAI: {
			ID:           domain.ProviderOpenAI,
			APIKey:       "sk-openai",
			Models:       []string{"gpt-5", "gpt-5-mini"},
			DefaultModel: "gpt-5",
		},
		domain.ProviderAnthropic: {
			ID:           domain.ProviderAnthropic,
			APIKey:       "sk-anthropic",
			BaseURL:      "https://api.anthropic.com/",
			Models:       []string{"claude-opus-5"},
			DefaultModel: "claude-opus-5",
		},
		domain.ProviderKimi: {
			ID:           domain.ProviderKimi,
			APIKey:       "sk-kimi",
			BaseURL:      "https://api.moonshot.cn/v1",
			Models:       []string{"kimi-k2"},
			DefaultModel: "kimi-k2",
		},
	}
}

func newTestRegistry() *Registry {
	return NewRegistry(NewStaticSource(testSource(), domain.ProviderOpenAI))
}

func TestResolveFullyPinned(t *testing.T) {
	r := newTestRegistry()
	id, slug, err := r.Resolve(context.Background(), domain.ModelSelection{
		Provider: domain.ProviderOpenAI, Model: "gpt-5-mini",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != domain.ProviderOpenAI || slug != "gpt-5-mini" {
		t.Errorf("got %s/%s, want openai/gpt-5-mini", id, slug)
	}
}

func TestResolveProviderOnlyUsesProviderDefault(t *testing.T) {
	r := newTestRegistry()
	id, slug, err := r.Resolve(context.Background(), domain.ModelSelection{Provider: domain.ProviderKimi})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != domain.ProviderKimi || slug != "kimi-k2" {
		t.Errorf("got %s/%s, want kimi/kimi-k2", id, slug)
	}
}

func TestResolveModelOnlyScansInDeterministicOrder(t *testing.T) {
	r := newTestRegistry()
	// claude-opus-5 is offered only by anthropic.
	id, slug, err := r.Resolve(context.Background(), domain.ModelSelection{Model: "claude-opus-5"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != domain.ProviderAnthropic || slug != "claude-opus-5" {
		t.Errorf("got %s/%s, want anthropic/claude-opus-5", id, slug)
	}
}

func TestResolveEmptyUsesClusterDefault(t *testing.T) {
	r := newTestRegistry()
	id, slug, err := r.Resolve(context.Background(), domain.ModelSelection{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != domain.ProviderOpenAI || slug != "gpt-5" {
		t.Errorf("got %s/%s, want openai/gpt-5", id, slug)
	}
}

func TestResolveErrors(t *testing.T) {
	r := newTestRegistry()
	cases := []struct {
		name string
		sel  domain.ModelSelection
		want error
	}{
		{"unknown provider", domain.ModelSelection{Provider: "ollama", Model: "llama3"}, ErrUnknownProvider},
		{"model not offered", domain.ModelSelection{Provider: domain.ProviderOpenAI, Model: "claude-opus-5"}, ErrModelNotOffered},
		{"model offered nowhere", domain.ModelSelection{Model: "no-such-model"}, ErrModelNotOffered},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := r.Resolve(context.Background(), c.sel)
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestResolveNoFallbackIsDistinctError(t *testing.T) {
	// No default provider and an empty selection must report the missing
	// fallback, not a generic "not configured" — the caller's remedy
	// differs (set a default vs configure a provider).
	src := NewStaticSource(testSource(), "")
	r := NewRegistry(src)
	_, _, err := r.Resolve(context.Background(), domain.ModelSelection{})
	if !errors.Is(err, ErrNoFallback) {
		t.Errorf("err = %v, want %v", err, ErrNoFallback)
	}
}

func TestResolveUnconfiguredProvider(t *testing.T) {
	// Present in settings but without a default model: not usable.
	src := NewStaticSource(map[domain.ProviderID]ProviderConfig{
		domain.ProviderOpenAI: {ID: domain.ProviderOpenAI, APIKey: "k"},
	}, domain.ProviderOpenAI)
	r := NewRegistry(src)
	_, _, err := r.Resolve(context.Background(), domain.ModelSelection{Provider: domain.ProviderOpenAI})
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want %v", err, ErrNotConfigured)
	}
}

func TestConfiguredAcceptsKeylessLocalEndpoint(t *testing.T) {
	// A local vLLM endpoint needs no key but does need a URL.
	c := ProviderConfig{ID: domain.ProviderCustom, BaseURL: "http://127.0.0.1:8000/v1", DefaultModel: "qwen"}
	if !c.Configured() {
		t.Error("a keyless local endpoint with a URL should count as configured")
	}
	// Key but no URL and no key: not usable.
	c2 := ProviderConfig{ID: domain.ProviderCustom, DefaultModel: "qwen"}
	if c2.Configured() {
		t.Error("no key and no base URL should not count as configured")
	}
}

func TestHasModel(t *testing.T) {
	c := ProviderConfig{
		Models:       []string{"a", "b"},
		DefaultModel: "c",
	}
	for _, slug := range []string{"a", "b", "c"} {
		if !c.HasModel(slug) {
			t.Errorf("HasModel(%q) = false, want true", slug)
		}
	}
	for _, slug := range []string{"", "z"} {
		if c.HasModel(slug) {
			t.Errorf("HasModel(%q) = true, want false", slug)
		}
	}
}

func TestModelCarriesPerRequestKey(t *testing.T) {
	r := newTestRegistry()
	m, opts, err := r.Model(context.Background(), domain.ModelSelection{
		Provider: domain.ProviderOpenAI, Model: "gpt-5",
	})
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	if m.ID != "gpt-5" {
		t.Errorf("model ID = %q, want gpt-5", m.ID)
	}
	if m.Provider == nil {
		t.Fatal("model has no provider")
	}
	if !m.Capabilities.SupportsToolUse {
		t.Error("OpsKeeper models must advertise tool use or the agent loop cannot run")
	}
	// The key travels in the options, not baked into the cached provider,
	// so a rotation is observed without rebuilding the provider.
	if opts.APIKey != "sk-openai" {
		t.Errorf("opts.APIKey = %q, want the settings key", opts.APIKey)
	}
}

func TestModelPicksUpRotatedKey(t *testing.T) {
	// A provider is cached across calls; the key must not be. Rotating the
	// key in settings must be visible on the very next Model call.
	cfg := map[domain.ProviderID]ProviderConfig{
		domain.ProviderOpenAI: {
			ID: domain.ProviderOpenAI, APIKey: "old-key",
			Models: []string{"gpt-5"}, DefaultModel: "gpt-5",
		},
	}
	src := &mutableSource{providers: cfg, fallback: domain.ProviderOpenAI}
	r := NewRegistry(src)

	_, opts, err := r.Model(context.Background(), domain.ModelSelection{Provider: domain.ProviderOpenAI, Model: "gpt-5"})
	if err != nil {
		t.Fatalf("first Model: %v", err)
	}
	if opts.APIKey != "old-key" {
		t.Fatalf("first key = %q, want old-key", opts.APIKey)
	}

	src.providers[domain.ProviderOpenAI] = ProviderConfig{
		ID: domain.ProviderOpenAI, APIKey: "new-key",
		Models: []string{"gpt-5"}, DefaultModel: "gpt-5",
	}

	_, opts, err = r.Model(context.Background(), domain.ModelSelection{Provider: domain.ProviderOpenAI, Model: "gpt-5"})
	if err != nil {
		t.Fatalf("second Model: %v", err)
	}
	if opts.APIKey != "new-key" {
		t.Errorf("after rotation key = %q, want new-key", opts.APIKey)
	}
}

func TestAvailableDistinguishesUnconfiguredFromFailing(t *testing.T) {
	r := newTestRegistry()
	if !r.Available(context.Background(), domain.ModelSelection{Provider: domain.ProviderOpenAI, Model: "gpt-5"}) {
		t.Error("a configured provider should report available")
	}
	if r.Available(context.Background(), domain.ModelSelection{Provider: "ollama", Model: "x"}) {
		t.Error("an unknown provider must not report available")
	}
}

func TestUnknownThinkingLevelIsIgnoredNotRejected(t *testing.T) {
	r := newTestRegistry()
	// A stale console sending a level this build predates must still get an
	// answer, just at the provider's default effort.
	_, _, err := r.Model(context.Background(), domain.ModelSelection{
		Provider: domain.ProviderOpenAI, Model: "gpt-5", ThinkingLevel: "ludicrous",
	})
	if err != nil {
		t.Errorf("unknown thinking level should not fail the call: %v", err)
	}
}

func TestKnownThinkingLevelIsApplied(t *testing.T) {
	r := newTestRegistry()
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh"} {
		_, opts, err := r.Model(context.Background(), domain.ModelSelection{
			Provider: domain.ProviderOpenAI, Model: "gpt-5", ThinkingLevel: level,
		})
		if err != nil {
			t.Fatalf("Model(%s): %v", level, err)
		}
		if string(opts.Thinking) != level {
			t.Errorf("ThinkingLevel %q mapped to %q", level, opts.Thinking)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	r := newTestRegistry()
	if _, _, err := r.Model(context.Background(), domain.ModelSelection{Provider: domain.ProviderOpenAI, Model: "gpt-5"}); err != nil {
		t.Fatalf("Model: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
	// After Close the registry must still work: the next call rebuilds.
	if _, _, err := r.Model(context.Background(), domain.ModelSelection{Provider: domain.ProviderOpenAI, Model: "gpt-5"}); err != nil {
		t.Errorf("Model after Close: %v", err)
	}
}

func TestInvalidateForcesRebuild(t *testing.T) {
	r := newTestRegistry()
	if _, _, err := r.Model(context.Background(), domain.ModelSelection{Provider: domain.ProviderOpenAI, Model: "gpt-5"}); err != nil {
		t.Fatalf("Model: %v", err)
	}
	if _, ok := r.cached(domain.ProviderOpenAI); !ok {
		t.Fatal("provider should be cached after first use")
	}
	r.Invalidate(domain.ProviderOpenAI)
	if _, ok := r.cached(domain.ProviderOpenAI); ok {
		t.Error("Invalidate should drop the cached provider")
	}
}

// mutableSource is a SettingsSource whose settings can change between calls,
// standing in for the control plane's live settings table.
type mutableSource struct {
	providers map[domain.ProviderID]ProviderConfig
	fallback  domain.ProviderID
}

func (m *mutableSource) ProviderConfig(_ context.Context, id domain.ProviderID) (ProviderConfig, bool) {
	cfg, ok := m.providers[id]
	return cfg, ok
}

func (m *mutableSource) DefaultProvider(_ context.Context) (domain.ProviderID, bool) {
	if m.fallback == "" {
		return "", false
	}
	return m.fallback, true
}
