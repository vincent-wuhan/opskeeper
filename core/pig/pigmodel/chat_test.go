package pigmodel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// These tests cover the shapes OpsKeeper owns now that the model request is
// PiG's own type. The transcript validation that used to live here —
// rejecting an orphaned tool result, an unknown role, a malformed schema —
// belongs to ai.NormalizeContext and is not re-tested; re-testing it would
// only assert that PiG still does what PiG does. What is left is the set of
// decisions this package makes, and each one was a place where a silent
// conversion bug would have looked like a model that had stopped calling
// tools.

// --- transcript ---------------------------------------------------------

// The transcript a turn sends is the transcript the host stored. A shape
// that survives construction but not normalisation is a shape the provider
// never sees, which is why this asserts the normalised output rather than the
// input.
func TestNewTranscriptRoundTripsTheOperationsShapes(t *testing.T) {
	t.Parallel()
	req := Request{Messages: []ai.Message{
		SystemTurn("you are an sre"),
		UserTurn("why is pg slow?"),
		AssistantTurn("checking locks", ai.ToolCall{
			ID: "c1", Name: "pg.lock_waits", Arguments: ai.JsonObject{"limit": float64(5)},
		}),
		ToolTurn("c1", "pg.lock_waits", "3 sessions waiting"),
	}}
	transcript, err := NewTranscript(req)
	if err != nil {
		t.Fatalf("NewTranscript: %v", err)
	}
	messages := transcript.Messages()
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(messages))
	}
	// The tool result must still be adjacent to the call it answers; a
	// strict provider rejects the whole request otherwise, and the error
	// names no message.
	last := messages[len(messages)-1]
	result, ok := last.(ai.ToolResultMessage)
	if !ok {
		t.Fatalf("last message = %T, want ai.ToolResultMessage", last)
	}
	if result.ToolCallID != "c1" {
		t.Errorf("tool call id = %q, want c1", result.ToolCallID)
	}
}

// A system turn passed as SystemPrompt and as a message is two system
// messages, which providers concatenate in an order nobody specifies. The
// constructor is documented to say so; this is the test that keeps the
// document true.
func TestNewTranscriptAcceptsEitherSystemTurnButNotBoth(t *testing.T) {
	t.Parallel()
	if _, err := NewTranscript(Request{
		SystemPrompt: "be terse",
		Messages:     []ai.Message{UserTurn("hi")},
	}); err != nil {
		t.Fatalf("SystemPrompt alone: %v", err)
	}
	if _, err := NewTranscript(Request{
		SystemPrompt: "be terse",
		Messages:     []ai.Message{SystemTurn("be terse"), UserTurn("hi")},
	}); err == nil {
		t.Error("both a SystemPrompt and a system message: want an error, got nil")
	}
}

// --- reading a reply ----------------------------------------------------

// Every caller reads a reply as two things: the text, and the calls. A
// multi-block assistant turn has to flatten to both without one caller
// re-deriving it differently from the next.
func TestReplyHelpersFlattenTextAndKeepToolCalls(t *testing.T) {
	t.Parallel()
	// The shape a streamed reply actually has: text arrives as deltas, and
	// the tool calls are interleaved with them rather than appended after.
	reply := ai.AssistantMessage{Content: []ai.AssistantContentBlock{
		ai.TextContent{Text: "checking "},
		ai.ToolCall{ID: "c1", Name: "get_host_load"},
		ai.TextContent{Text: "now"},
		ai.ToolCall{ID: "c2", Name: "query_promql", Arguments: ai.JsonObject{"q": "up"}},
	}}

	// Concatenated, not joined: an assistant message's text blocks are
	// streaming deltas of one utterance, so a separator would insert a
	// space or newline the model never wrote.
	if got := ReplyText(&reply); got != "checking now" {
		t.Errorf("ReplyText = %q, want the deltas concatenated with no separator", got)
	}
	calls := ReplyToolCalls(&reply)
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(calls))
	}
	if calls[0].ID != "c1" || calls[1].Name != "query_promql" {
		t.Errorf("calls = %+v, want them in transcript order", calls)
	}
}

// Thinking blocks are not text. Reading one as text would put a model's
// private reasoning into the operator's console bubble, which is both a leak
// of the provider's reasoning trace and a confusing thing to read.
func TestReplyTextDropsThinkingBlocks(t *testing.T) {
	t.Parallel()
	reply := AssistantTurn("the lock is held by session 42")
	reply.Content = append([]ai.AssistantContentBlock{
		ai.ThinkingContent{Thinking: "session 42 started at 09:12, before the batch job"},
	}, reply.Content...)
	if got := ReplyText(&reply); got != "the lock is held by session 42" {
		t.Errorf("ReplyText = %q, want the thinking block excluded", got)
	}
}

// A nil reply is a real case: a cancelled or aborted call. Both helpers have
// to survive it rather than panic in a metrics decorator.
func TestReplyHelpersToleratNil(t *testing.T) {
	t.Parallel()
	if got := ReplyText(nil); got != "" {
		t.Errorf("ReplyText(nil) = %q, want empty", got)
	}
	if got := ReplyToolCalls(nil); got != nil {
		t.Errorf("ReplyToolCalls(nil) = %v, want nil so it is a nil check", got)
	}
}

// A parameterless tool call marshals to the four bytes "null" if its
// argument set is rendered naively. The host stores this blob and replays it
// verbatim on the next turn, and a stored null argument set is a request the
// provider refuses — so the normalisation has to happen here, where the
// reason is known, rather than three turns later where it is not.
func TestAParameterlessToolCallDoesNotBecomeStoredNull(t *testing.T) {
	t.Parallel()
	if got := string(ArgumentsJSON(nil)); got != "{}" {
		t.Errorf("ArgumentsJSON(nil) = %q, want {}", got)
	}
	if got := string(ArgumentsJSON(ai.JsonObject{})); got != "{}" {
		t.Errorf("ArgumentsJSON(empty) = %q, want {}", got)
	}
}

// A real argument set must survive the round trip in meaning: the host
// replays this blob, so a re-encoded object that changed shape would change
// what the tool receives. A number in particular must not come back as a
// string, because the tool's schema says integer.
func TestToolCallArgumentsSurviveTheRoundTrip(t *testing.T) {
	t.Parallel()
	raw := ArgumentsJSON(ai.JsonObject{"q": "up", "step": float64(30)})
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
	if got["q"] != "up" {
		t.Errorf("q = %v, want up", got["q"])
	}
	if got["step"] != float64(30) {
		t.Errorf("step = %v (%T), want the number 30", got["step"], got["step"])
	}
}

// --- message text -------------------------------------------------------

// A tool result is content the model reads back, so a prompt that logs or
// asserts on it has to see the text rather than nothing. This is the case
// that made MessageText necessary: four message types, four different
// content unions, and no single generic in ai covers all of them.
func TestMessageTextRendersEveryMessageType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  ai.Message
		want string
	}{
		{"system", SystemTurn("be terse"), "be terse"},
		{"user", UserTurn("why is pg slow?"), "why is pg slow?"},
		{"assistant", AssistantTurn("session 42 holds the lock"), "session 42 holds the lock"},
		{"tool result", ToolTurn("c1", "pg.lock_waits", "3 sessions waiting"), "3 sessions waiting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MessageText(tc.msg); got != tc.want {
				t.Errorf("MessageText = %q, want %q", got, tc.want)
			}
		})
	}
	if got := MessageText(nil); got != "" {
		t.Errorf("MessageText(nil) = %q, want empty", got)
	}
}

// A multi-block user turn — a pasted log line next to a screenshot — must
// render its text and skip the image. Dropping the whole message because one
// block was not text is the failure this guards: a judge that received the
// evidence as image+text has to see the text.
func TestMessageTextKeepsTextAndSkipsImages(t *testing.T) {
	t.Parallel()
	msg := ai.UserMessage{Content: ai.UserContentBlocks{
		ai.TextContent{Text: "before"},
		ai.ImageContent{Data: "aGk=", MimeType: "image/png"},
		ai.TextContent{Text: "after"},
	}}
	if got := MessageText(msg); got != "before\nafter" {
		t.Errorf("MessageText = %q, want the two text blocks and not the image", got)
	}
}

// --- the accounting -----------------------------------------------------

// PiG reports input, output, and cache separately and carries a provider
// total that need not equal their sum. The budget ledger bills against the
// total, so recomputing it would drift from the invoice wherever cache or
// reasoning tokens are counted separately.
func TestUsageOfCarriesTheProviderTotalSeparately(t *testing.T) {
	t.Parallel()
	got := UsageOf(&ai.AssistantMessage{Usage: ai.Usage{
		Input:       11,
		Output:      4,
		CacheRead:   100,
		TotalTokens: 115,
	}})
	if got.InputTokens != 11 || got.OutputTokens != 4 {
		t.Fatalf("usage = %+v, want input 11 / output 4", got)
	}
	if got.CacheReadTokens != 100 {
		t.Errorf("cache read = %d, want 100 kept separate rather than folded into input", got.CacheReadTokens)
	}
	if got.ReportedTotal != 115 {
		t.Errorf("reported total = %d, want the provider's 115", got.ReportedTotal)
	}
	// The number the budget actually bills is the provider's.
	if got.Total() != 115 {
		t.Errorf("Total() = %d, want 115 rather than the sum 15", got.Total())
	}
}

// The other half: a provider that streams no total still produces a usable
// budget number rather than a zero that would read as "free".
func TestUsageOfFallsBackToTheSumWhenNoTotalIsReported(t *testing.T) {
	t.Parallel()
	got := UsageOf(&ai.AssistantMessage{Usage: ai.Usage{Input: 11, Output: 4}})
	if got.ReportedTotal != 0 {
		t.Errorf("reported total = %d, want 0 so the sum is used", got.ReportedTotal)
	}
	if got.Total() != 15 {
		t.Errorf("Total() = %d, want 15 when the provider reports no total", got.Total())
	}
}

// A message that streamed no usage at all is an honest zero, not a
// fabrication. A caller that budgets on this must be able to tell "the
// provider told us nothing" from "the request was free" — and the honest
// answer is that the fold reports what it was given.
func TestUsageOfIsZeroWhenTheProviderReportedNothing(t *testing.T) {
	t.Parallel()
	if got := UsageOf(&ai.AssistantMessage{}); got.Total() != 0 {
		t.Errorf("usage = %+v, want an honest zero", got)
	}
	if got := UsageOf(nil); got.Total() != 0 {
		t.Errorf("usage of nil = %+v, want an honest zero", got)
	}
}

// The cost is carried through as the provider priced it, labelled advisory
// wherever it is shown. It is stored anyway because "which model is
// expensive" is the question an operator actually asks, and token counts do
// not answer it across a catalogue with different per-token rates.
func TestUsageOfCarriesTheProvidersOwnCost(t *testing.T) {
	t.Parallel()
	got := UsageOf(&ai.AssistantMessage{Usage: ai.Usage{
		Input:  1000,
		Output: 500,
		Cost:   ai.UsageCost{Input: 0.01, Output: 0.02, Total: 0.03},
	}})
	if got.CostUSD != 0.03 {
		t.Errorf("cost = %v, want the provider's total 0.03", got.CostUSD)
	}
}

// --- run totals ---------------------------------------------------------

// --- the static source --------------------------------------------------

// The eval command and the contract tests have no settings table, so the
// static source is how they resolve a model. It has to behave like the
// catalog adapter in the two places they overlap, or a score produced
// offline would not be comparable with one the server produces.
func TestStaticSettingsPrefersTheNamedDefaultOnlyWhenConfigured(t *testing.T) {
	t.Parallel()
	src := StaticSettings([]ProviderConfig{
		{ID: "openai", APIKey: "k", DefaultModel: "gpt-4o"},
		{ID: "custom", BaseURL: "http://localhost:11434/v1", DefaultModel: "llama"},
	}, "custom")

	got, ok := src.DefaultProvider(context.Background())
	if !ok || got != "custom" {
		t.Errorf("default = %q ok=%v, want custom", got, ok)
	}

	// Naming a provider with no key falls through to the first configured
	// one rather than reporting a default that cannot serve a request.
	src = StaticSettings([]ProviderConfig{
		{ID: "openai", APIKey: "k", DefaultModel: "gpt-4o"},
		{ID: "zhipu", DefaultModel: "glm-4.7"},
	}, "zhipu")
	got, ok = src.DefaultProvider(context.Background())
	if !ok || got != "openai" {
		t.Errorf("default = %q ok=%v, want the first configured provider", got, ok)
	}
}

// A duplicated id would otherwise be resolved twice with the last one
// silently winning, which depends on nothing an operator can see.
func TestStaticSettingsDeduplicatesByID(t *testing.T) {
	t.Parallel()
	src := StaticSettings([]ProviderConfig{
		{ID: "openai", APIKey: "first", DefaultModel: "a"},
		{ID: "openai", APIKey: "second", DefaultModel: "b"},
	}, "")
	cfg, ok := src.ProviderConfig(context.Background(), "openai")
	if !ok {
		t.Fatal("openai not found")
	}
	if cfg.APIKey != "first" {
		t.Errorf("api key = %q, want the first registration to win", cfg.APIKey)
	}
}

// A provider the source does not carry reports absent rather than empty, and
// the registry treats the two differently: absent is not retried, empty is
// misconfigured.
func TestStaticSettingsReportsAnUnknownProviderAsAbsent(t *testing.T) {
	t.Parallel()
	src := StaticSettings(nil, "")
	if _, ok := src.ProviderConfig(context.Background(), "openai"); ok {
		t.Error("unknown provider reported present")
	}
	if _, ok := src.DefaultProvider(context.Background()); ok {
		t.Error("empty source reported a default")
	}
}

// A selection naming a provider the deployment never configured fails at
// resolution, not at the provider with a 401. The error has to name the
// model, because that is what the operator has to fix.
func TestResolutionFailureNamesTheModel(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(StaticSettings([]ProviderConfig{
		{ID: "openai", APIKey: "k", DefaultModel: "gpt-4o"},
	}, "openai"))
	_, _, err := reg.Resolve(context.Background(), domain.ModelSelection{Provider: "openai", Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, _, err = reg.Resolve(context.Background(), domain.ModelSelection{Provider: "openai", Model: "gpt-9-turbo"})
	if err == nil {
		t.Fatal("unknown model: want an error")
	}
	if !strings.Contains(err.Error(), "gpt-9-turbo") {
		t.Errorf("error = %v, want it to name the model the operator has to fix", err)
	}
}
