// Provider catalog construction from the environment-derived config.
//
// This is the single place that knows a provider's default base URL and
// its default model. It used to live in cmd/opskeeper's assembly layer,
// which meant anything else that needed a provider — the evaluation CLI,
// a migration script, a one-off diagnostic — had to either import cmd
// (impossible) or retype the table. Retyping it is the failure mode worth
// naming: a copy that adds a provider, or fixes a base URL, or changes a
// default model, produces a client that talks to a different endpoint than
// the server does, and the symptom is a judge scoring a different system
// than the one under test. Nothing announces that divergence.
//
// The catalog is therefore built here, once, and both binaries ask for it.
package llm

import (
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
)

// providerSpec is the static half of a catalog entry: everything that does
// not come from the environment.
type providerSpec struct {
	label      string
	defaultMdl string
	baseURL    string
	// models are appended to the configured model list. They are the models
	// the deployment is known to work with; an operator who configured
	// something else still gets their choice, they just do not get these
	// suggestions in a selector.
	models []string
}

// specs is keyed by provider id. A missing key means "no environment
// configuration for this provider" — a custom base URL, for instance, is
// configured per deployment and has no default worth baking in here.
var specs = map[string]providerSpec{
	ProviderOpenAI: {
		label:      "OpenAI",
		defaultMdl: "gpt-5.4",
		models:     []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini"},
	},
	ProviderAnthropic: {
		label:      "Anthropic",
		defaultMdl: "claude-sonnet-4-6",
		baseURL:    "https://api.anthropic.com/v1",
	},
	ProviderZhipu: {
		label:      "智谱 GLM",
		defaultMdl: "glm-4.7",
		baseURL:    "https://open.bigmodel.cn/api/paas/v4",
	},
	ProviderGemini: {
		label:      "Gemini",
		defaultMdl: "gemini-2.5-pro",
		// The OpenAI-compatible endpoint, not the native one. PiG's
		// geminiBaseURL normalisation strips the /openai suffix again
		// before the request goes out, so the value here is the
		// operator-facing form and the normaliser is the one that
		// decides the wire form.
		baseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
	},
	ProviderDeepSeek: {
		label:      "DeepSeek",
		defaultMdl: "deepseek-v4-flash",
		baseURL:    "https://api.deepseek.com/v1",
	},
	ProviderKimi: {
		label:      "Kimi",
		defaultMdl: "kimi-k2.6",
		baseURL:    "https://api.moonshot.cn/v1",
	},
}

// ProviderIDs is every provider the environment can configure, in a stable
// order. Callers that present a choice to a human use it so the list does
// not reshuffle between runs.
func ProviderIDs() []string {
	ids := make([]string, 0, len(specs))
	for id := range specs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ProviderConfigFor builds one provider's catalog entry.
//
// It reports false when the id is unknown or when the provider has no API
// key. An empty key is "not configured", not "configured and broken":
// NewMultiClient drops those entries, and a caller that wanted a specific
// provider needs to be able to tell the difference between "you gave me a
// name I do not have" and "you gave me a name with no credentials".
func ProviderConfigFor(id string, c *config.Config) (ProviderConfig, bool) {
	spec, ok := specs[id]
	if !ok || c == nil {
		return ProviderConfig{}, false
	}
	apiKey, model, baseURL, models := providerEnvOf(c, id)
	if strings.TrimSpace(apiKey) == "" {
		return ProviderConfig{}, false
	}
	return ProviderConfig{
		ID:      id,
		Label:   spec.label,
		APIKey:  apiKey,
		Model:   firstNonEmpty(model, spec.defaultMdl),
		BaseURL: firstNonEmpty(baseURL, spec.baseURL),
		Models:  dedupeModels(append([]string{model}, append(models, spec.models...)...)...),
	}, true
}

// ProviderConfigsFrom builds every configured provider, in id order.
func ProviderConfigsFrom(c *config.Config) []ProviderConfig {
	var out []ProviderConfig
	for _, id := range ProviderIDs() {
		if p, ok := ProviderConfigFor(id, c); ok {
			out = append(out, p)
		}
	}
	return out
}

// providerEnvOf reads one provider's environment-derived settings.
//
// It is a switch rather than a map because the config struct has a
// distinct typed entry per provider, and a map keyed by string would need
// interface{} boxing to reach them — turning a typo in a provider id into
// a runtime nil rather than a compile error.
func providerEnvOf(c *config.Config, id string) (apiKey, model, baseURL string, models []string) {
	switch id {
	case ProviderOpenAI:
		return c.OpenAI.APIKey, c.OpenAI.Model, c.OpenAI.BaseURL, nil
	case ProviderAnthropic:
		return c.LLM.Anthropic.APIKey, c.LLM.Anthropic.Model, c.LLM.Anthropic.BaseURL, c.LLM.Anthropic.Models
	case ProviderZhipu:
		return c.LLM.Zhipu.APIKey, c.LLM.Zhipu.Model, c.LLM.Zhipu.BaseURL, c.LLM.Zhipu.Models
	case ProviderGemini:
		return c.LLM.Gemini.APIKey, c.LLM.Gemini.Model, c.LLM.Gemini.BaseURL, c.LLM.Gemini.Models
	case ProviderDeepSeek:
		return c.LLM.DeepSeek.APIKey, c.LLM.DeepSeek.Model, c.LLM.DeepSeek.BaseURL, c.LLM.DeepSeek.Models
	case ProviderKimi:
		return c.LLM.Kimi.APIKey, c.LLM.Kimi.Model, c.LLM.Kimi.BaseURL, c.LLM.Kimi.Models
	}
	return "", "", "", nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func dedupeModels(vals ...string) []string {
	seen := make(map[string]struct{}, len(vals))
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
