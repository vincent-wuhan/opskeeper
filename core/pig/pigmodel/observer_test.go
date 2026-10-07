package pigmodel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The registry is the only layer that knows which provider and which model
// a request resolved to, and it is the only layer that sees a failed call
// at all -- a caller above it sees a settled reply or an error and nothing
// in between. So an LLM cost / latency / token series can only be fed from
// here, and for the life of this metric nothing fed it: the three series
// opskeeper_llm_calls_total, opskeeper_llm_call_duration_seconds and
// opskeeper_llm_router_tokens_total were registered, exported, and never
// incremented, and the manager's LLM spend dashboard read zero for every
// provider for the life of the deployment.

type observedCall struct {
	provider, model, status string
	seconds                 float64
	in, out                 int
}

// serveOpenAI stands up the OpenAI-shaped fixture the smoke gate already
// uses and returns its base URL.
func serveOpenAI(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(openAISSE))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTheObserverSeesAProviderCallWithItsTokens(t *testing.T) {
	base := serveOpenAI(t)
	id := domain.ProviderOpenAI
	cfg := configuredProvider(id, base)
	src := NewStaticSource(map[domain.ProviderID]ProviderConfig{id: cfg}, id)

	var got []observedCall
	reg := NewRegistry(src, WithCallObserver(func(p, m, status string, s float64, in, out int) {
		got = append(got, observedCall{p, m, status, s, in, out})
	}))

	if _, err := reg.Complete(context.Background(), Request{
		Selection: domain.ModelSelection{Provider: id, Model: cfg.DefaultModel},
		Messages:  []ai.Message{UserTurn("hi")},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("observer calls = %d, want 1: a turn the operator is billed for has to appear once", len(got))
	}
	if got[0].provider != string(id) {
		t.Errorf("provider = %q, want %q", got[0].provider, id)
	}
	if got[0].model != cfg.DefaultModel {
		t.Errorf("model = %q, want %q: a token series that cannot be split by model cannot be priced", got[0].model, cfg.DefaultModel)
	}
	if got[0].status != "ok" {
		t.Errorf("status = %q, want ok", got[0].status)
	}
	// openAISSE reports prompt_tokens:3 completion_tokens:1.
	if got[0].in != 3 || got[0].out != 1 {
		t.Errorf("tokens = %d in / %d out, want 3 / 1: the usage block is right there in the fixture", got[0].in, got[0].out)
	}
	if got[0].seconds <= 0 {
		t.Errorf("seconds = %v, want > 0", got[0].seconds)
	}
}

// A registry with no observer is the node agent's registry and every test
// that does not care about telemetry, so the hook must be optional rather
// than a required argument every construction site has to supply.
func TestACompletionWithNoObserverIsNotAnError(t *testing.T) {
	base := serveOpenAI(t)
	id := domain.ProviderOpenAI
	cfg := configuredProvider(id, base)
	reg := NewRegistry(NewStaticSource(map[domain.ProviderID]ProviderConfig{id: cfg}, id))
	if _, err := reg.Complete(context.Background(), Request{
		Selection: domain.ModelSelection{Provider: id, Model: cfg.DefaultModel},
		Messages:  []ai.Message{UserTurn("hi")},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// The half that makes the series worth having. A counter that only records
// successes reads as healthy while a provider is refusing every request,
// which is the failure an operator is least likely to notice.
func TestAFailedProviderCallIsObservedAsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	id := domain.ProviderOpenAI
	cfg := configuredProvider(id, srv.URL)

	var got []observedCall
	reg := NewRegistry(NewStaticSource(map[domain.ProviderID]ProviderConfig{id: cfg}, id),
		WithCallObserver(func(p, m, status string, s float64, in, out int) {
			got = append(got, observedCall{p, m, status, s, in, out})
		}))

	if _, err := reg.Complete(context.Background(), Request{
		Selection: domain.ModelSelection{Provider: id, Model: cfg.DefaultModel},
		Messages:  []ai.Message{UserTurn("hi")},
	}); err == nil {
		t.Fatal("a 401 was expected to fail the completion")
	}
	if len(got) != 1 {
		t.Fatalf("observer calls = %d, want 1: a refused call consumed an attempt and an operator's patience", len(got))
	}
	if got[0].status != "error" {
		t.Errorf("status = %q, want error", got[0].status)
	}
	if got[0].in != 0 || got[0].out != 0 {
		t.Errorf("tokens = %d / %d, want 0 / 0: a call that never produced a reply has no usage to bill", got[0].in, got[0].out)
	}
}

// The clock is read after the stream settles, not before it is opened, so
// the duration a provider is credited with includes the time it spent
// streaming rather than only the time it spent accepting the request.
func TestTheObservedDurationCoversStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(openAISSE))
	}))
	defer srv.Close()

	id := domain.ProviderOpenAI
	cfg := configuredProvider(id, srv.URL)
	var got []observedCall
	reg := NewRegistry(NewStaticSource(map[domain.ProviderID]ProviderConfig{id: cfg}, id),
		WithCallObserver(func(p, m, status string, s float64, in, out int) {
			got = append(got, observedCall{p, m, status, s, in, out})
		}))
	if _, err := reg.Complete(context.Background(), Request{
		Selection: domain.ModelSelection{Provider: id, Model: cfg.DefaultModel},
		Messages:  []ai.Message{UserTurn("hi")},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("observer calls = %d, want 1", len(got))
	}
	if got[0].seconds < 0.02 {
		t.Errorf("seconds = %v, want the ~30ms the provider held the turn: a duration that stops at the "+
			"request being accepted understates every slow provider as a fast one", got[0].seconds)
	}
}

// The three lies Settle names in its own comment, and the one that was
// checked nowhere: a stream that settles into an error rather than into a
// reply. ResultContext hands that value back with a nil error, so before
// this a 401 from any of the seven providers reached every caller as a
// successful, empty assistant turn -- and a caller's schema check then
// blames the prompt for a key that expired an hour ago.
func TestAProviderThatRefusesTheTurnIsAnErrorNotAnEmptyReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	defer srv.Close()

	id := domain.ProviderOpenAI
	cfg := configuredProvider(id, srv.URL)
	reg := NewRegistry(NewStaticSource(map[domain.ProviderID]ProviderConfig{id: cfg}, id))
	_, err := reg.Complete(context.Background(), Request{
		Selection: domain.ModelSelection{Provider: id, Model: cfg.DefaultModel},
		Messages:  []ai.Message{UserTurn("hi")},
	})
	if err == nil {
		t.Fatal("a refused turn was reported as a successful one")
	}
	for _, want := range []string{string(id), cfg.DefaultModel} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q: an operator reading this has to know which key to rotate", err, want)
		}
	}
}

// The narrow half, tested on the value rather than through a stream: a
// turn that finished because it is stopping to call a tool, because it ran
// out of output budget, or because it finished cleanly is a normal turn.
// The check keys on the two reason values that mean "nothing was produced",
// so treating every non-stop reason as a failure would turn a tool-calling
// agent into one that reports every step as an error.
func TestAToolCallingOrTruncatedTurnIsNotMistakenForAFailure(t *testing.T) {
	for _, reason := range []ai.StopReason{"toolUse", "length", "stop", ""} {
		if err := ProviderTurnError(&ai.AssistantMessage{StopReason: reason}); err != nil {
			t.Errorf("stop reason %q was reported as a provider failure: %v", reason, err)
		}
	}
}

func TestAProviderTurnThatProducedNothingIsAnError(t *testing.T) {
	for _, reason := range []ai.StopReason{ai.StopReasonError, ai.StopReasonAborted} {
		err := ProviderTurnError(&ai.AssistantMessage{
			StopReason: reason, Provider: "openai", Model: "gpt-4o", ErrorMessage: "rate limited",
		})
		if err == nil {
			t.Fatalf("stop reason %q was accepted as a reply", reason)
		}
		if !strings.Contains(err.Error(), "rate limited") {
			t.Errorf("error = %q, want it to carry the provider's own words: the operator cannot act on a "+
				"sentence that does not say what the provider said", err)
		}
	}
}
