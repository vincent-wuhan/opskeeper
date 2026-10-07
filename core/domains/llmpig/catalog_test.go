package llmpig

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/llm"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigcoding"
)

// stubCatalog is a fixed set of provider rows. It stands in for the settings
// table without a database, which is the point: every one of these cases is
// about the resolution rule, and none of them is about SQL.
type stubCatalog struct {
	providers []llm.ProviderConfig
	def       string
	err       error
}

func (s stubCatalog) ResolveProviders(context.Context) ([]llm.ProviderConfig, string, error) {
	return s.providers, s.def, s.err
}

var _ ProviderCatalog = stubCatalog{}

// A deployment that only ever configured DeepSeek is configured. The
// original bug this guards was a check that asked "is OpenAI present",
// which reported an entirely working cluster as unconfigured.
func TestCatalogViewAcceptsANonOpenAIProvider(t *testing.T) {
	view := NewCatalogView(nil, stubCatalog{providers: []llm.ProviderConfig{
		{ID: llm.ProviderDeepSeek, APIKey: "sk-test", Model: "deepseek-v4-flash"},
	}})

	providers := view.Providers()
	if len(providers) != 1 || providers[0].ID != llm.ProviderDeepSeek {
		t.Fatalf("Providers() = %+v, want just deepseek", providers)
	}
	if provider, model := view.Default(); provider != llm.ProviderDeepSeek || model != "deepseek-v4-flash" {
		t.Fatalf("Default() = (%q, %q), want (deepseek, deepseek-v4-flash)", provider, model)
	}
}

func TestCatalogViewReportsNothingWithoutProviders(t *testing.T) {
	view := NewCatalogView(nil, stubCatalog{})
	if got := view.Providers(); len(got) != 0 {
		t.Fatalf("Providers() = %+v, want empty", got)
	}
	if provider, model := view.Default(); provider != "" || model != "" {
		t.Fatalf("Default() = (%q, %q), want empty", provider, model)
	}
}

// An operator can name a default in the settings table and then not finish
// configuring it. The default has to fall through to a provider that can
// actually serve a request — otherwise every unpinned turn fails with
// "default x is not configured", which reads as an OpsKeeper bug rather than
// as the missing key it is.
func TestDefaultFallsBackWhenThePreferredProviderHasNoKey(t *testing.T) {
	view := NewCatalogView(nil, stubCatalog{
		providers: []llm.ProviderConfig{
			{ID: llm.ProviderOpenAI, APIKey: "", Model: "gpt-5.4"},
			{ID: llm.ProviderDeepSeek, APIKey: "sk-test", Model: "deepseek-v4-flash"},
		},
		def: llm.ProviderOpenAI,
	})
	if provider, _ := view.Default(); provider != llm.ProviderDeepSeek {
		t.Fatalf("Default() provider = %q, want the configured one rather than the keyless preferred", provider)
	}
}

// The same, with every provider keyless: there is no default, and reporting
// one would put a dead option in the console's model picker.
func TestDefaultIsEmptyWhenNothingIsConfigured(t *testing.T) {
	view := NewCatalogView(nil, stubCatalog{
		providers: []llm.ProviderConfig{
			{ID: llm.ProviderDeepSeek, APIKey: "", Model: "deepseek-v4-flash"},
		},
		def: llm.ProviderDeepSeek,
	})
	if provider, _ := view.Default(); provider != "" {
		t.Fatalf("Default() provider = %q, want empty", provider)
	}
}

// A provider the operator has a row for but no key for is not offered. The
// picker has no disabled state, and offering it means every selection of it
// ends in a 401 rather than in the "not configured" that tells them what to
// fix.
func TestProvidersOmitsKeylessRows(t *testing.T) {
	view := NewCatalogView(nil, stubCatalog{providers: []llm.ProviderConfig{
		{ID: llm.ProviderOpenAI, APIKey: "", Model: "gpt-5.4"},
		{ID: llm.ProviderZhipu, APIKey: "sk-test", Model: "glm-4.7"},
		{ID: llm.ProviderDeepSeek, APIKey: "sk-test", Model: "deepseek-v4-flash"},
	}})
	got := view.Providers()
	if len(got) != 2 {
		t.Fatalf("Providers() = %+v, want the two configured ones", got)
	}
	// Sorted by id so the list does not reshuffle between two requests that
	// read the same rows.
	if got[0].ID != llm.ProviderDeepSeek || got[1].ID != llm.ProviderZhipu {
		t.Errorf("Providers() order = %q,%q; want deepseek before zhipu", got[0].ID, got[1].ID)
	}
}

// An unreadable settings table is not "every provider was removed". Reporting
// it as an empty catalog is the fail-closed answer: the picker hides itself
// and a turn fails with a clear not-configured error, rather than the
// registry deciding the operator deleted their providers.
func TestCatalogViewFailsClosedOnAnUnreadableCatalog(t *testing.T) {
	view := NewCatalogView(nil, stubCatalog{err: context.DeadlineExceeded})
	if got := view.Providers(); len(got) != 0 {
		t.Fatalf("Providers() = %+v, want empty on a read failure", got)
	}
	if provider, _ := view.Default(); provider != "" {
		t.Fatalf("Default() provider = %q, want empty on a read failure", provider)
	}
}

// The view's whole reason to exist: the model the picker preselects is the
// model an unpinned turn resolves to.
//
// This is the end-to-end version — a real pigcoding.Runtime, a real publish,
// a real read back — because the two answers are produced by different code
// paths and only the wired version proves they agree. A test that called
// defaultProvider on both sides would pass even if Publish stopped setting
// the fallback, which is exactly the drift this guards.
func TestCatalogViewDefaultMatchesThePublishedTurnResolution(t *testing.T) {
	catalog := stubCatalog{
		providers: []llm.ProviderConfig{
			{ID: llm.ProviderOpenAI, APIKey: "sk-a", Model: "gpt-5.4"},
			{ID: llm.ProviderDeepSeek, APIKey: "sk-b", Model: "deepseek-v4-flash"},
		},
		def: llm.ProviderDeepSeek,
	}

	runtime, err := pigcoding.NewRuntime(pigcoding.RuntimeOptions{
		AgentDir: t.TempDir(),
		CWD:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	sync := NewSync(runtime, catalog, nil)
	if err := sync.Publish(context.Background()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if sync.Default() == "" {
		t.Fatal("Publish published no default provider")
	}

	view := NewCatalogView(sync, catalog)
	provider, model := view.Default()
	if string(provider) != string(sync.Default()) {
		t.Fatalf("picker default = %q, turn default = %q; the two must not diverge", provider, sync.Default())
	}
	if provider != llm.ProviderDeepSeek || model != "deepseek-v4-flash" {
		t.Fatalf("Default() = (%q, %q), want (deepseek, deepseek-v4-flash)", provider, model)
	}

	// The published default is also what an unpinned selection resolves to,
	// and the runtime really knows the model — otherwise the picker's
	// preselection names something BuildModel would reject.
	if _, err := runtime.BuildModel(Spec(string(sync.Default()), "deepseek-v4-flash")); err != nil {
		t.Fatalf("the published default does not resolve against the runtime: %v", err)
	}
}

// A publish against no runtime is a no-op and leaves the fallback empty, so
// an assembly that forgot to wire the runtime reports "no model" rather than
// quietly resolving against nothing. This is the fail-closed half of
// NewSync's contract, and it is the reason the boot path in main treats a
// publish failure as a warning: the deployment comes up, and every turn says
// "not configured" instead of the process refusing to start.
func TestSyncWithoutARuntimePublishesNothing(t *testing.T) {
	sync := NewSync(nil, stubCatalog{providers: []llm.ProviderConfig{
		{ID: llm.ProviderOpenAI, APIKey: "sk-a", Model: "gpt-5.4"},
	}}, nil)
	if err := sync.Publish(context.Background()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if sync.Default() != "" {
		t.Fatalf("Default() = %q, want empty with no runtime", sync.Default())
	}
	// A nil Sync is the fully-unwired case, and every method on it has to
	// answer rather than panic: the boot path calls Default() before it can
	// know whether wiring succeeded.
	var absent *Sync
	if absent.Default() != "" {
		t.Fatal("nil Sync reported a default")
	}
	if err := absent.Publish(context.Background()); err != nil {
		t.Fatalf("nil Sync Publish: %v", err)
	}
}

// A key rotation has to reach the runtime, not just the picker: the console
// showing a new model while every session still fails on the old credential
// is the split this guards.
func TestPublishRegistersEveryConfiguredProvider(t *testing.T) {
	runtime, err := pigcoding.NewRuntime(pigcoding.RuntimeOptions{
		AgentDir: t.TempDir(),
		CWD:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	sync := NewSync(runtime, stubCatalog{providers: []llm.ProviderConfig{
		{ID: llm.ProviderOpenAI, APIKey: "sk-a", Model: "gpt-5.4", Models: []string{"gpt-5.4", "gpt-5.5"}},
		{ID: llm.ProviderDeepSeek, APIKey: "sk-b", Model: "deepseek-v4-flash"},
		// An unconfigured row is skipped rather than registered empty, so a
		// request against it fails with "not configured" instead of a 401.
		{ID: llm.ProviderZhipu, APIKey: "", Model: "glm-4.7"},
	}}, nil)
	if err := sync.Publish(context.Background()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	catalogue := runtime.Catalogue()
	want := []string{
		"deepseek/deepseek-v4-flash",
		"openai/gpt-5.4",
		"openai/gpt-5.5",
	}
	if len(catalogue) != len(want) {
		t.Fatalf("catalogue = %v, want %v", catalogue, want)
	}
	for _, spec := range want {
		found := false
		for _, got := range catalogue {
			if got == spec {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("catalogue is missing %q: %v", spec, catalogue)
		}
	}
	for _, got := range catalogue {
		if strings.HasPrefix(got, "zhipu/") {
			t.Errorf("catalogue registered the keyless zhipu row as %q", got)
		}
	}
}
