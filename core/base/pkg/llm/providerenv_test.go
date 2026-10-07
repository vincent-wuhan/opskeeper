package llm

import (
	"reflect"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
)

func TestAnUnknownOrUnconfiguredProviderIsNotBuilt(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Anthropic.APIKey = "sk-real"

	// "custom" is a provider the router understands but the environment
	// does not configure. Asking for it must be a miss, not an entry with
	// an empty key that the router would then silently drop.
	if _, ok := ProviderConfigFor(ProviderCustom, cfg); ok {
		t.Error("the custom provider was built from environment settings it does not have")
	}
	if _, ok := ProviderConfigFor("nonesuch", cfg); ok {
		t.Error("an unknown provider id was built")
	}
	// A provider with a real key next to one without: the absence of the
	// second is a fact the caller needs, not a silent omission.
	if _, ok := ProviderConfigFor(ProviderZhipu, cfg); ok {
		t.Error("a provider with no API key was reported as configured")
	}
	if _, ok := ProviderConfigFor(ProviderAnthropic, cfg); !ok {
		t.Error("the one configured provider was reported as unconfigured")
	}
	if _, ok := ProviderConfigFor(ProviderAnthropic, nil); ok {
		t.Error("a nil config produced a provider")
	}
}

func TestDefaultsAreFilledInAndOverridesWin(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Anthropic.APIKey = "sk-real"
	cfg.LLM.DeepSeek.APIKey = "sk-real"
	cfg.LLM.DeepSeek.BaseURL = "https://proxy.internal/deepseek/v1"
	cfg.LLM.DeepSeek.Model = "deepseek-v4"

	deepseek, ok := ProviderConfigFor(ProviderDeepSeek, cfg)
	if !ok {
		t.Fatal("deepseek was not built")
	}
	// The default base URL is what makes a provider work with no
	// configuration at all; an override is how a proxy is used instead.
	if deepseek.BaseURL != "https://proxy.internal/deepseek/v1" {
		t.Errorf("base url = %q, want the configured override", deepseek.BaseURL)
	}
	if deepseek.Model != "deepseek-v4" {
		t.Errorf("model = %q, want the configured override", deepseek.Model)
	}

	// anthropic has no base URL set, so the default must appear.
	anthropic, ok := ProviderConfigFor(ProviderAnthropic, cfg)
	if !ok {
		t.Fatal("anthropic was not built")
	}
	if anthropic.BaseURL != "https://api.anthropic.com/v1" {
		t.Errorf("base url = %q, want the baked-in default", anthropic.BaseURL)
	}
	if anthropic.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want the baked-in default", anthropic.Model)
	}
	if anthropic.Label == "" || anthropic.ID != ProviderAnthropic {
		t.Errorf("entry = %+v, want an identified, labelled provider", anthropic)
	}
}

func TestTheCatalogOrderIsStable(t *testing.T) {
	// The list is presented to a human in a selector, and a list that
	// reshuffles between runs makes two runs incomparable at a glance.
	want := []string{ProviderAnthropic, ProviderDeepSeek, ProviderGemini, ProviderKimi, ProviderOpenAI, ProviderZhipu}
	for range 5 {
		if got := ProviderIDs(); !reflect.DeepEqual(got, want) {
			t.Fatalf("ProviderIDs() = %v, want %v", got, want)
		}
	}
}

func TestOnlyConfiguredProvidersReachTheCatalog(t *testing.T) {
	cfg := &config.Config{}
	cfg.OpenAI.APIKey = "sk-openai"
	cfg.LLM.Kimi.APIKey = "sk-kimi"

	got := ProviderConfigsFrom(cfg)
	if len(got) != 2 {
		t.Fatalf("catalog has %d entries (%+v), want 2", len(got), got)
	}
	// Ordered by id, not by which env var happened to be read first.
	if got[0].ID != ProviderKimi || got[1].ID != ProviderOpenAI {
		t.Errorf("catalog order = %s, %s; want kimi, openai", got[0].ID, got[1].ID)
	}
	// OpenAI carries the deployment's known-good model list so a selector
	// has something to offer; the configured model comes first.
	if len(got[1].Models) == 0 || got[1].Models[0] == "" {
		t.Errorf("openai model list = %v, want the configured model first", got[1].Models)
	}
	for i, m := range got[1].Models {
		for _, seen := range got[1].Models[:i] {
			if seen == m {
				t.Errorf("openai model list %v repeats %q", got[1].Models, m)
			}
		}
	}
}
