package domain

// ProviderID names a configured LLM backend. The set is closed and matches
// the settings keys the control plane persists (`<provider>_api_key`,
// `<provider>_base_url`, `<provider>_models`, `<provider>_default_model`),
// so a provider id is simultaneously a storage key prefix and a wire value.
type ProviderID string

const (
	ProviderOpenAI    ProviderID = "openai"
	ProviderAnthropic ProviderID = "anthropic"
	ProviderZhipu     ProviderID = "zhipu"
	ProviderGemini    ProviderID = "gemini"
	ProviderDeepSeek  ProviderID = "deepseek"
	ProviderKimi      ProviderID = "kimi"
	// ProviderCustom is a generic OpenAI-compatible endpoint: vLLM,
	// Ollama, OpenRouter, LM Studio, Together, Groq, or a self-hosted
	// gateway. It has no default endpoint, so a base URL is mandatory
	// before it can be selected.
	ProviderCustom ProviderID = "custom"
)

// KnownProviders lists every provider the control plane can configure, in a
// stable order. Order is alphabetical by id and must stay stable: the
// settings UI and the default-provider fallback both depend on it.
func KnownProviders() []ProviderID {
	return []ProviderID{
		ProviderAnthropic,
		ProviderCustom,
		ProviderDeepSeek,
		ProviderGemini,
		ProviderKimi,
		ProviderOpenAI,
		ProviderZhipu,
	}
}

// IsKnown reports whether p is a provider the control plane can configure.
func (p ProviderID) IsKnown() bool {
	for _, k := range KnownProviders() {
		if k == p {
			return true
		}
	}
	return false
}

// RequiresBaseURL reports whether the provider cannot be used without an
// explicit endpoint. Only the custom provider is in this class.
func (p ProviderID) RequiresBaseURL() bool { return p == ProviderCustom }

// ModelRef names one model within a provider. Provider is carried
// alongside the slug because model slugs are only unique per provider, and
// the SPA model picker round-trips both.
type ModelRef struct {
	Provider ProviderID `json:"provider" yaml:"provider"`
	Model    string     `json:"model" yaml:"model"`
}

// IsZero reports whether the ref names no model.
func (m ModelRef) IsZero() bool { return m.Provider == "" && m.Model == "" }

// Valid reports whether the ref names a known provider and a non-empty
// model slug.
func (m ModelRef) Valid() bool { return m.Provider.IsKnown() && m.Model != "" }

// String renders the ref for logs. It never includes credentials.
func (m ModelRef) String() string {
	if m.Model == "" {
		return string(m.Provider)
	}
	return string(m.Provider) + "/" + m.Model
}

// ModelSelection is the per-turn model choice a caller may pin. A zero
// Provider or Model means "use the cluster default", which the host
// resolves at call time so a settings change takes effect without a restart.
type ModelSelection struct {
	Provider ProviderID `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model    string     `json:"model,omitempty" yaml:"model,omitempty"`
	// ThinkingLevel is a provider-neutral reasoning effort. Empty means the
	// provider default. Implementations clamp it to what the model supports.
	ThinkingLevel string `json:"thinking_level,omitempty" yaml:"thinking_level,omitempty"`
}

// Ref returns the pinned ModelRef, or the zero ref when the caller asked
// for the cluster default.
func (s ModelSelection) Ref() ModelRef {
	return ModelRef{Provider: s.Provider, Model: s.Model}
}

// Pinned reports whether the caller named a specific provider and model.
// A partially-filled selection (provider without model, or vice versa) is
// not pinned and resolves to the cluster default for the missing half.
func (s ModelSelection) Pinned() bool { return s.Provider != "" && s.Model != "" }
