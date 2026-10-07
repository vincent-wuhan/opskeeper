package domain

import "testing"

func TestKnownProvidersIsSortedAndStable(t *testing.T) {
	got := KnownProviders()
	want := []ProviderID{
		ProviderAnthropic, ProviderCustom, ProviderDeepSeek,
		ProviderGemini, ProviderKimi, ProviderOpenAI, ProviderZhipu,
	}
	if len(got) != len(want) {
		t.Fatalf("len(KnownProviders()) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestProviderIsKnown(t *testing.T) {
	for _, p := range KnownProviders() {
		if !p.IsKnown() {
			t.Errorf("%q should be known", p)
		}
	}
	for _, p := range []ProviderID{"", "openai2", "OPENAI", "ollama"} {
		if p.IsKnown() {
			t.Errorf("%q should not be known", p)
		}
	}
}

func TestRequiresBaseURL(t *testing.T) {
	if !ProviderCustom.RequiresBaseURL() {
		t.Error("custom provider must require a base URL")
	}
	for _, p := range KnownProviders() {
		if p == ProviderCustom {
			continue
		}
		if p.RequiresBaseURL() {
			t.Errorf("%q should not require a base URL", p)
		}
	}
}

func TestModelRefValid(t *testing.T) {
	cases := []struct {
		name string
		ref  ModelRef
		want bool
	}{
		{"complete", ModelRef{ProviderOpenAI, "gpt-5"}, true},
		{"no model", ModelRef{ProviderOpenAI, ""}, false},
		{"no provider", ModelRef{"", "gpt-5"}, false},
		{"unknown provider", ModelRef{"ollama", "llama3"}, false},
		{"zero", ModelRef{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ref.Valid(); got != c.want {
				t.Errorf("Valid() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestModelRefIsZero(t *testing.T) {
	if !(ModelRef{}).IsZero() {
		t.Error("empty ref should be zero")
	}
	if (ModelRef{Provider: ProviderOpenAI}).IsZero() {
		t.Error("partially-filled ref is not zero")
	}
}

func TestModelRefStringOmitsEmptyModel(t *testing.T) {
	if got := (ModelRef{Provider: ProviderKimi, Model: "k2"}).String(); got != "kimi/k2" {
		t.Errorf("String() = %q, want %q", got, "kimi/k2")
	}
	if got := (ModelRef{Provider: ProviderKimi}).String(); got != "kimi" {
		t.Errorf("String() = %q, want %q", got, "kimi")
	}
}

func TestModelSelectionPinned(t *testing.T) {
	cases := []struct {
		name string
		sel  ModelSelection
		want bool
	}{
		{"both", ModelSelection{Provider: ProviderOpenAI, Model: "gpt-5"}, true},
		{"neither", ModelSelection{}, false},
		{"provider only", ModelSelection{Provider: ProviderOpenAI}, false},
		{"model only", ModelSelection{Model: "gpt-5"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sel.Pinned(); got != c.want {
				t.Errorf("Pinned() = %v, want %v", got, c.want)
			}
		})
	}
}
