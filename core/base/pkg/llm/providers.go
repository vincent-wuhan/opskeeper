// Provider ids accepted by the settings-backed LLM stack. Keep in lockstep
// with the admin settings page (all OpenAI-compatible).
//
// These used to live next to the eino routing model, which no longer exists.
// They stay in the llm package because they are the vocabulary the whole
// manager shares — the settings resolver, the model picker catalog and the
// boot-time fallbacks all name providers with them.

package llm

import "errors"

const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
	ProviderZhipu     = "zhipu"
	ProviderGemini    = "gemini"
	ProviderDeepSeek  = "deepseek"
	ProviderKimi      = "kimi"
	// ProviderCustom is a generic OpenAI-compatible endpoint configured
	// entirely from settings (base_url + key + models). Routing is
	// id-agnostic, so it dispatches like any other provider.
	ProviderCustom = "custom"
)

// ErrUnknownProvider is returned when a caller names a provider that is not
// configured. It survives the eino routing model because the routing
// behaviour it describes does: a name outside the configured set has no
// transport to reach.
var ErrUnknownProvider = errors.New("llm: unknown provider")
