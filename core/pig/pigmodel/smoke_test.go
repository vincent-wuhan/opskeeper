package pigmodel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The Phase B acceptance gate: "all seven providers smoke".
//
// The registry maps OpsKeeper's seven configured providers onto PiG's two
// wire families — five speak the OpenAI shape, one speaks Anthropic's, one
// speaks Google's. The gate is not "the registry returns a struct"; it is
// "a request built from this registry's settings reaches the right endpoint
// and comes back as an assistant message". A test that stopped at the
// struct would pass while the provider id, the base URL, or the auth header
// was wrong, and every one of those is a request that fails in production
// and not in CI.
//
// So each case below stands up an httptest server, points the provider at
// it through a ProviderConfig, and drives one real turn through the same
// `Provider.Stream` the kernel calls.

// openAISSE is a minimal but complete OpenAI-compatible stream: one text
// delta and a terminating chunk.
const openAISSE = `data: {"id":"chatcmpl-smoke","choices":[{"delta":{"content":"hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-smoke","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}

data: [DONE]

`

// sseFixture returns the response body for one provider family, plus the
// path suffix its request must hit.
type sseFixture struct {
	// path is the request path the provider must use. Asserting it is what
	// catches a base URL joined wrong — a failure that otherwise surfaces
	// as an opaque 404 mid-incident.
	path string
	body string
}

var providerFixtures = map[domain.ProviderID]sseFixture{
	domain.ProviderOpenAI:   {"/chat/completions", openAISSE},
	domain.ProviderCustom:   {"/chat/completions", openAISSE},
	domain.ProviderZhipu:    {"/chat/completions", openAISSE},
	domain.ProviderDeepSeek: {"/chat/completions", openAISSE},
	domain.ProviderKimi:     {"/chat/completions", openAISSE},
	domain.ProviderAnthropic: {"/v1/messages", "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
	domain.ProviderGemini: {"/v1beta/models/gemini-2.0:streamGenerateContent", "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n"},
}

// TestEveryProviderCompletesATurn is the gate itself.
func TestEveryProviderCompletesATurn(t *testing.T) {
	for _, id := range domain.KnownProviders() {
		t.Run(string(id), func(t *testing.T) {
			fixture, ok := providerFixtures[id]
			if !ok {
				t.Fatalf("no SSE fixture for provider %q; the smoke gate would silently skip it", id)
			}
			var hitPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hitPath = r.URL.Path
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(fixture.body))
			}))
			defer srv.Close()

			cfg := configuredProvider(id, srv.URL)
			src := NewStaticSource(map[domain.ProviderID]ProviderConfig{id: cfg}, id)
			reg := NewRegistry(src)

			model, opts, err := reg.Model(context.Background(), domain.ModelSelection{
				Provider: id, Model: cfg.DefaultModel,
			})
			if err != nil {
				t.Fatalf("Model: %v", err)
			}
			if model.Provider == nil {
				t.Fatal("model carries no provider")
			}
			if model.Provider.ID() != string(id) {
				t.Fatalf("model provider = %q, want %q", model.Provider.ID(), id)
			}

			msg := streamOneTurn(t, model, opts)
			if got := ai.ContentText(msg.Content); !strings.Contains(got, "hello") {
				t.Errorf("assistant text = %q, want it to carry the fixture text", got)
			}
			if got := stripVersion(hitPath); got != fixture.path {
				t.Errorf("request path = %q, want %q: a mis-joined base URL is a 404 mid-incident",
					got, fixture.path)
			}
		})
	}
}

// TestEveryProviderIsInTheSmokeTable is the drift guard.
//
// domain.KnownProviders is the list the settings UI offers; the fixture
// table is the list this gate covers. A provider added to one and not the
// other would ship with no smoke coverage and no failing test, which is how
// a gate quietly stops being one.
func TestEveryProviderIsInTheSmokeTable(t *testing.T) {
	for _, id := range domain.KnownProviders() {
		if _, ok := providerFixtures[id]; !ok {
			t.Errorf("provider %q has no smoke fixture; add one so the gate covers everything the UI offers", id)
		}
	}
	if len(providerFixtures) != len(domain.KnownProviders()) {
		t.Errorf("smoke table has %d entries, registry offers %d",
			len(providerFixtures), len(domain.KnownProviders()))
	}
}

// configuredProvider builds a ProviderConfig pointing one provider at a
// local URL.
func configuredProvider(id domain.ProviderID, baseURL string) ProviderConfig {
	return ProviderConfig{
		ID:           id,
		APIKey:       "sk-smoke",
		BaseURL:      baseURL,
		Models:       []string{smokeModel(id)},
		DefaultModel: smokeModel(id),
	}
}

// smokeModel is the slug each family's fixture answers for. The Google
// provider puts the model in the request path, so its slug is load-bearing
// for the path assertion; the others tolerate any slug.
func smokeModel(id domain.ProviderID) string {
	if id == domain.ProviderGemini {
		return "gemini-2.0"
	}
	if id == domain.ProviderAnthropic {
		return "claude-opus-5"
	}
	return "smoke-model"
}

// stripVersion removes any query string so the path comparison is about the
// route, not the credentials the provider attached to it.
func stripVersion(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// streamOneTurn drives one turn and returns the settled assistant message.
func streamOneTurn(t *testing.T, model *ai.Model, opts ai.StreamOptions) *ai.AssistantMessage {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := model.Provider.Stream(ctx, ai.NormalizeContext(ai.Context{
		Messages: []ai.Message{ai.UserMessage{Content: ai.UserText("ping"), Timestamp: 1}},
	}), opts)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Drain the queue first: Result blocks on a terminal event and does not
	// pull from the stream, so a caller that only called Result would hang.
	for range stream.Events(ctx) {
	}
	msg, err := stream.ResultContext(ctx)
	if err != nil {
		t.Fatalf("ResultContext: %v", err)
	}
	if msg == nil {
		t.Fatal("stream produced no assistant message")
	}
	if msg.StopReason == ai.StopReasonError {
		t.Fatalf("stream ended in error: %s", msg.ErrorMessage)
	}
	return msg
}

// TestGeminiBaseURLKeepsTheVersionPath pins the bug this gate found.
//
// PiG treats a non-empty BaseURL as authoritative and then leaves its own
// API version unset, so a base URL without a version path produces a request
// to "/models/..." — a 404 that no unit test saw because every fixture
// pointed at a URL the fixture wrote. The settings default also carried the
// OpenAI-compatible surface ("/v1beta/openai"), which is a different API.
func TestGeminiBaseURLKeepsTheVersionPath(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"bare host gains the default version", "https://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com/v1beta"},
		{"trailing slash is not doubled", "https://generativelanguage.googleapis.com/", "https://generativelanguage.googleapis.com/v1beta"},
		{"an explicit version is left alone", "https://proxy.internal/v1", "https://proxy.internal/v1"},
		{"an explicit beta version is left alone", "https://proxy.internal/v1beta", "https://proxy.internal/v1beta"},
		{"the openai-compat default is translated to the native endpoint", "https://generativelanguage.googleapis.com/v1beta/openai", "https://generativelanguage.googleapis.com/v1beta"},
		{"empty stays empty so PiG applies its own default", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := geminiBaseURL(tc.in); got != tc.want {
				t.Errorf("geminiBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
