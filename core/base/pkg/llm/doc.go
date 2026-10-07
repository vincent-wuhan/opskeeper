// Package llm holds the control plane's model *configuration* vocabulary:
// which providers exist, what each one offers, and how spend is capped.
//
// # What this package used to be
//
// Until the PiG SDK was embedded directly, this package also contained a
// complete second LLM stack: a Message/ChatReq/ChatResp vocabulary, a
// hand-rolled OpenAI wire format, a provider router, and a PiG-backed
// implementation of all of it sitting behind one `Client` interface. Every
// model call in OpsKeeper went through that vocabulary and was translated
// into PiG's on the way out and back on the way in.
//
// That layer is gone. The reasons are worth keeping in the doc comment
// because the pull to re-add it is strong and the failure it causes is
// quiet:
//
//   - The translation is where a tool_call id is dropped. A dropped id does
//     not produce an error. It produces a model that stops calling tools,
//     and the only evidence is a line of conversion code that looks
//     correct.
//   - The second wire format meant every provider compatibility fix
//     (max_tokens vs max_completion_tokens, a provider's non-standard auth
//     path, its SSE framing) had to be made twice — once upstream and once
//     here — and the two drifted.
//   - The interface froze the vocabulary. A PiG release that added a
//     content block, a reasoning field or a stream option could not reach
//     any OpsKeeper call site, because none of them spoke PiG.
//
// What remains is exactly the part PiG has no opinion about: the set of
// providers a deployment has configured, and the budget policy that caps
// what those providers may spend. Configuration is a control-plane
// decision; the calls themselves belong to core/pig.
package llm

import "context"

// ProviderConfig is one provider's persisted configuration.
//
// It is the control plane's record. It is NOT the shape a model call
// takes: that is PiG's, reached through core/pig. Keeping the two apart is
// what lets an admin edit a key, a base URL or a model list without any
// model-call site changing.
type ProviderConfig struct {
	ID      string   // stable id: "openai" | "anthropic" | "zhipu" | "gemini"
	Label   string   // display name
	APIKey  string   // empty → provider not configured
	Model   string   // default model
	BaseURL string   // optional base URL override
	Models  []string // closed-set of allowed models for the UI selector
}

// Configured reports whether the provider can serve a request.
//
// An API key or a base URL is required, not a key alone: a local vLLM or a
// corporate gateway behind an internal proxy is a legitimate deployment
// that authenticates at the network layer, and refusing it would push
// operators into pasting a dummy key.
func (c ProviderConfig) Configured() bool {
	return c.Model != "" && (c.APIKey != "" || c.BaseURL != "")
}

// HasModel reports whether the provider offers slug.
//
// A provider whose operator left the model list empty offers only its
// default. Treating an empty list as "offers everything" is how a
// deployment ends up accepting a slug the deployment cannot price.
func (c ProviderConfig) HasModel(slug string) bool {
	if slug == "" {
		return false
	}
	if slug == c.Model {
		return true
	}
	for _, m := range c.Models {
		if m == slug {
			return true
		}
	}
	return false
}

// ProviderInfo is the subset of ProviderConfig safe to leak through the
// HTTP /v1/aiops/models endpoint. It is a separate type rather than a
// field-level json:"-" convention because a key that is one struct tag away
// from being exposed is a key that eventually is.
type ProviderInfo struct {
	ID     string
	Label  string
	Model  string
	Models []string
}

// Info projects a configuration onto its publicly visible half.
func (c ProviderConfig) Info() ProviderInfo {
	return ProviderInfo{ID: c.ID, Label: c.Label, Model: c.Model, Models: c.Models}
}

// ProvidersResolver supplies a fresh provider catalog at call time.
//
// The seam exists so admin-edited database rows flow into the control plane
// without a manager restart. The returned slice supersedes any
// constructor-time catalog; an empty slice falls back to the constructor's.
// defaultProvider, when non-empty and present, becomes the new default.
type ProvidersResolver interface {
	ResolveProviders(ctx context.Context) (providers []ProviderConfig, defaultProvider string, err error)
}
