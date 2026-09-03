// Package loop — llm_caller_test.go
//
// Test matrix for the LLMCaller abstraction (Design Doc §3.2):
//
//   - happy             — fake LLM returns valid JSON, schema passes.
//   - timeout           — fake LLM returns timeout error, wrapped.
//   - API 5xx           — fake LLM returns a 5xx, wrapped (transient).
//   - schema-invalid    — fake LLM returns JSON missing a required field.
//     Caller gets ErrSchemaInvalid wrapped.
//   - cost telemetry    — fake LLM returns canned token counts; verify
//     CallOutput.TokensIn/Out/CostUSD.
//
// Plus secondary coverage the spec calls out implicitly:
//
//   - retry exhausts       — fake LLM keeps failing transiently; after
//     MaxRetries+1 attempts we get the wrapped
//     "after N retries" error.
//   - permanent fail-fast  — fake LLM returns 4xx; we get the wrapped
//     error WITHOUT any retry.
//   - extract prose-wrapped JSON — markdown-fenced JSON is parsed.
//   - call without schema  — empty OutputSchema accepts arbitrary JSON.
//   - nil client panics    — guard clause on construction.
//
// The shared FakeLLMClient (loop/fake_llm_client.go) covers most cases
// and is shared with future phase worker + LLMJudge tests per Design
// Doc §9.1. The cost-telemetry test needs Usage injection, which the
// shared fake does not support (intentionally narrow surface), so it
// uses an inline fakeLLM.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

// fakeLLM is an in-test stub of llm.Client that supports Usage
// injection. Only the cost-telemetry test needs this; everywhere else
// we reuse the shared FakeLLMClient.
type fakeLLM struct {
	mu         sync.Mutex
	content    string
	usage      llm.Usage
	err        error
	calls      atomic.Int32
	lastPrompt string
}

func (f *fakeLLM) Chat(ctx context.Context, req llm.ChatReq) (*llm.ChatResp, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.lastPrompt = req.Messages[len(req.Messages)-1].Content
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &llm.ChatResp{
		Assistant: llm.Message{Role: "assistant", Content: f.content},
		Usage:     f.usage,
	}, nil
}

func (f *fakeLLM) callCount() int { return int(f.calls.Load()) }

func (f *fakeLLM) prompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPrompt
}

// standardTestSchema is the contract shape worker tests use to gate the
// fake LLM's output. Requires "summary" (string) + "severity" (string
// from a small enum) + "actions" (array of strings).
const standardTestSchema = `{
  "type": "object",
  "required": ["summary", "severity", "actions"],
  "properties": {
    "summary":  { "type": "string" },
    "severity": { "type": "string", "enum": ["low", "medium", "high"] },
    "actions":  { "type": "array", "items": { "type": "string" } }
  }
}`

// validJSONFor returns a JSON document that matches standardTestSchema.
func validJSONFor(summary, severity string, actions []string) string {
	b, _ := json.Marshal(map[string]any{
		"summary":  summary,
		"severity": severity,
		"actions":  actions,
	})
	return string(b)
}

// silentLogger drops all log output so the test runs are quiet and
// assertions on warn-level retries do not see spurious stderr noise.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestCall_Success — happy path: fake returns valid JSON, schema
// validates, CallOutput fields populate.
func TestCall_Success(t *testing.T) {
	t.Parallel()
	content := validJSONFor("investigated", "high", []string{"noop"})
	fc := NewFakeLLMClient()
	fc.SetResponse(0, content)

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	out, err := caller.Call(context.Background(), CallInput{
		Phase:        PhaseInvestigated,
		Prompt:       "probe prompt",
		SystemPrompt: "you are an investigator",
		OutputSchema: standardTestSchema,
	})
	if err != nil {
		t.Fatalf("Call returned err = %v, want nil", err)
	}
	if fc.CallCount() != 1 {
		t.Errorf("CallCount = %d, want 1 (no retry on happy path)", fc.CallCount())
	}
	if !json.Valid(out.Raw) {
		t.Errorf("CallOutput.Raw is not valid JSON: %s", out.Raw)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Raw, &parsed); err != nil {
		t.Fatalf("unmarshal Raw: %v", err)
	}
	if parsed["summary"] != "investigated" {
		t.Errorf("parsed[summary] = %v, want %q", parsed["summary"], "investigated")
	}
}

func TestCall_WithQwenNoThink_AppendsSoftSwitchOnce(t *testing.T) {
	fc := &fakeLLM{content: validJSONFor("investigated", "high", []string{"noop"})}
	caller := NewLLMCaller(fc, WithQwenNoThink(), WithLogger(silentLogger()))
	_, err := caller.Call(context.Background(), CallInput{
		Prompt:       "probe",
		OutputSchema: standardTestSchema,
	})
	if err != nil {
		t.Fatalf("Call returned err = %v, want nil", err)
	}
	if got := fc.prompt(); got != "probe\n/no_think" {
		t.Fatalf("prompt = %q, want appended soft switch", got)
	}
	_, err = caller.Call(context.Background(), CallInput{
		Prompt:       "probe /no_think",
		OutputSchema: standardTestSchema,
	})
	if err != nil {
		t.Fatalf("second Call returned err = %v, want nil", err)
	}
	if got := fc.prompt(); got != "probe /no_think" {
		t.Fatalf("second prompt = %q, want unchanged input", got)
	}
}

// TestCall_Timeout — fake returns a deadline-exceeded error; caller
// surfaces it wrapped. With default MaxRetries=1 we expect 2 calls
// (1 original + 1 retry), then a wrapped failure.
func TestCall_Timeout(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetError(0, errors.New("ChatCompletion: context deadline exceeded"))
	fc.SetError(1, errors.New("ChatCompletion: context deadline exceeded"))

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	_, err := caller.Call(context.Background(), CallInput{
		Phase:        PhaseDetected,
		Prompt:       "x",
		OutputSchema: standardTestSchema,
	})
	if err == nil {
		t.Fatal("Call returned err = nil, want timeout-wrapped error")
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want wrapped deadline/timeout", err)
	}
	if !strings.Contains(err.Error(), "after 1 retries") {
		t.Errorf("err = %v, want 'after 1 retries' in chain", err)
	}
	if got := fc.CallCount(); got != 2 {
		t.Errorf("CallCount = %d, want 2 (1 original + 1 retry)", got)
	}
}

// TestCall_APIServerError — fake returns a 5xx-shaped error. Treated
// as transient and retried.
func TestCall_APIServerError(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetError(0, errors.New("ChatCompletion: unexpected status 503 Service Unavailable"))
	fc.SetError(1, errors.New("ChatCompletion: unexpected status 503 Service Unavailable"))

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	_, err := caller.Call(context.Background(), CallInput{
		Phase:        PhaseCorrelated,
		Prompt:       "y",
		OutputSchema: standardTestSchema,
	})
	if err == nil {
		t.Fatal("Call returned err = nil, want 5xx-wrapped error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want wrapped 503", err)
	}
	if got := fc.CallCount(); got != 2 {
		t.Errorf("CallCount = %d, want 2 (1 original + 1 retry)", got)
	}
	if errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err wrapped ErrSchemaInvalid; that sentinel is reserved for schema failures")
	}
}

// TestCall_PermanentErrorFailsFast — fake returns a 4xx-shaped error.
// Treated as non-transient; NO retry.
func TestCall_PermanentErrorFailsFast(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetError(0, errors.New("ChatCompletion: unexpected status 401 Unauthorized"))

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	_, err := caller.Call(context.Background(), CallInput{
		Phase:        PhaseInvestigated,
		Prompt:       "z",
		OutputSchema: standardTestSchema,
	})
	if err == nil {
		t.Fatal("Call returned err = nil, want wrapped auth error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want wrapped 401", err)
	}
	if got := fc.CallCount(); got != 1 {
		t.Errorf("CallCount = %d, want 1 (4xx is permanent, no retry)", got)
	}
}

// TestCall_SchemaInvalid — fake returns JSON missing a required field.
// Caller gets ErrSchemaInvalid wrapped. No retry.
func TestCall_SchemaInvalid(t *testing.T) {
	t.Parallel()
	missingActions := `{"summary":"x","severity":"low"}` // "actions" missing
	fc := NewFakeLLMClient()
	fc.SetResponse(0, missingActions)

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	_, err := caller.Call(context.Background(), CallInput{
		Phase:        PhaseCritiqued,
		Prompt:       "critic this",
		OutputSchema: standardTestSchema,
	})
	if err == nil {
		t.Fatal("Call returned err = nil, want ErrSchemaInvalid")
	}
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err = %v, want errors.Is(err, ErrSchemaInvalid) true", err)
	}
	if !strings.Contains(err.Error(), "actions") {
		t.Errorf("err = %v, want mention of failing field 'actions'", err)
	}
	if got := fc.CallCount(); got != 1 {
		t.Errorf("CallCount = %d, want 1 (schema-invalid does NOT retry)", got)
	}
}

// TestCall_SchemaInvalidEnum — enum-bound field receives an out-of-
// band value. Caller still gets ErrSchemaInvalid.
func TestCall_SchemaInvalidEnum(t *testing.T) {
	t.Parallel()
	badEnum := `{"summary":"x","severity":"critical","actions":["a"]}`
	fc := NewFakeLLMClient()
	fc.SetResponse(0, badEnum)

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	_, err := caller.Call(context.Background(), CallInput{
		Phase:        PhasePostmortem,
		Prompt:       "postmortem",
		OutputSchema: standardTestSchema,
	})
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err = %v, want errors.Is(err, ErrSchemaInvalid)", err)
	}
	if !strings.Contains(err.Error(), "enum") {
		t.Errorf("err = %v, want mention of 'enum' in message", err)
	}
}

// TestCall_CostTelemetry — uses an inline fake (rather than the shared
// FakeLLMClient) because we need to inject Usage and verify
// CallOutput.TokensIn/Out/CostUSD reflect it. The shared fake has an
// intentionally narrow surface to keep LLMJudge tests stable; future
// change may extend it.
func TestCall_CostTelemetry(t *testing.T) {
	t.Parallel()
	content := validJSONFor("telemetry", "low", []string{"a"})
	fake := &fakeLLM{
		content: content,
		usage:   llm.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
	}

	wantCost := 0.42
	caller := NewLLMCaller(
		fake,
		WithLogger(silentLogger()),
		WithCostFunc(func(_ string, in, out int) float64 {
			if in != 1000 || out != 500 {
				t.Errorf("costFunc got in=%d out=%d, want 1000/500", in, out)
			}
			return wantCost
		}),
	)

	out, err := caller.Call(context.Background(), CallInput{
		Phase:        PhasePostmortem,
		Prompt:       "p",
		OutputSchema: standardTestSchema,
	})
	if err != nil {
		t.Fatalf("Call err: %v", err)
	}
	if out.TokensIn != 1000 {
		t.Errorf("TokensIn = %d, want 1000", out.TokensIn)
	}
	if out.TokensOut != 500 {
		t.Errorf("TokensOut = %d, want 500", out.TokensOut)
	}
	if out.CostUSD != wantCost {
		t.Errorf("CostUSD = %v, want %v (configured estimator)", out.CostUSD, wantCost)
	}
}

// TestCall_RetryExhaustion — fake keeps returning transient errors.
// With MaxRetries=2 (so 3 total tries) the caller returns a wrapped
// "after 2 retries" failure.
func TestCall_RetryExhaustion(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	for i := 0; i < 3; i++ {
		fc.SetError(i, errors.New("network: connection refused"))
	}

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	start := time.Now()
	_, err := caller.Call(context.Background(), CallInput{
		Phase:        PhaseDetected,
		Prompt:       "x",
		OutputSchema: standardTestSchema,
		MaxRetries:   2,
		// Keep backoff small so the test finishes quickly.
		TimeoutMs: 1000,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Call returned err = nil, want retry-exhaustion wrapped error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want underlying 'connection refused'", err)
	}
	if !strings.Contains(err.Error(), "after 2 retries") {
		t.Errorf("err = %v, want 'after 2 retries'", err)
	}
	if got := fc.CallCount(); got != 3 {
		t.Errorf("CallCount = %d, want 3 (1 + 2 retries)", got)
	}
	// 2 backoffs × 100ms minimum = ≥200ms. We don't enforce an upper
	// bound to keep the test stable across CI machines.
	if elapsed < 100*time.Millisecond {
		t.Errorf("elapsed = %v, want ≥ 100ms (backoff between retries)", elapsed)
	}
}

// TestCall_InvalidCallerSchema — schema string is malformed JSON. The
// caller wraps ErrSchemaUnparseable (NOT ErrSchemaInvalid) so
// operators can tell "worker's schema is wrong" from "model output is
// wrong". The chat call is never made.
func TestCall_InvalidCallerSchema(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetResponse(0, validJSONFor("ok", "low", []string{"a"}))

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	_, err := caller.Call(context.Background(), CallInput{
		Phase:        PhaseDetected,
		Prompt:       "x",
		OutputSchema: "{this is not json",
	})
	if !errors.Is(err, ErrSchemaUnparseable) {
		t.Errorf("err = %v, want errors.Is(err, ErrSchemaUnparseable)", err)
	}
	if errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err = %v wrapped ErrSchemaInvalid; expected ErrSchemaUnparseable only", err)
	}
	if got := fc.CallCount(); got != 0 {
		t.Errorf("CallCount = %d, want 0 (schema parse fails before chat)", got)
	}
}

// TestCall_ProseWrappedJSON — model wraps its JSON in markdown fences
// ("```json\n... \n```"). extractJSON should strip the fences and
// validate the inner document.
func TestCall_ProseWrappedJSON(t *testing.T) {
	t.Parallel()
	wrapped := "```json\n" + validJSONFor("fenced", "medium", []string{"x"}) + "\n```"
	fc := NewFakeLLMClient()
	fc.SetResponse(0, wrapped)

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	out, err := caller.Call(context.Background(), CallInput{
		Phase:        PhaseInvestigated,
		Prompt:       "x",
		OutputSchema: standardTestSchema,
	})
	if err != nil {
		t.Fatalf("Call err: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["severity"] != "medium" {
		t.Errorf("severity = %v, want medium", parsed["severity"])
	}
}

// TestCall_EmptySchema — OutputSchema is empty. LLMCaller skips
// validation entirely; arbitrary JSON is accepted (callers opt into
// the freedom explicitly).
func TestCall_EmptySchema(t *testing.T) {
	t.Parallel()
	arbitrary := `{"anything":true,"goes":42}`
	fc := NewFakeLLMClient()
	fc.SetResponse(0, arbitrary)

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	out, err := caller.Call(context.Background(), CallInput{
		Phase:  PhaseDetected,
		Prompt: "freeform",
	})
	if err != nil {
		t.Fatalf("Call err: %v", err)
	}
	if string(out.Raw) != arbitrary {
		t.Errorf("Raw = %s, want %s", out.Raw, arbitrary)
	}
}

// TestCall_NonJSONOutput — model returns prose with no JSON. Caller
// wraps ErrSchemaInvalid (contract violated: implicit JSON requirement).
func TestCall_NonJSONOutput(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetResponse(0, "Sorry, I cannot answer that.")

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	_, err := caller.Call(context.Background(), CallInput{
		Phase:        PhaseCorrelated,
		Prompt:       "x",
		OutputSchema: standardTestSchema,
	})
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err = %v, want errors.Is(err, ErrSchemaInvalid)", err)
	}
}

// TestNewLLMCaller_NilPanics — programming-error guard.
func TestNewLLMCaller_NilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("NewLLMCaller(nil) did not panic")
		}
	}()
	_ = NewLLMCaller(nil)
}

// TestSchemaParser_DisallowsUnknownFields — guards against silent typo
// in a worker contract schema ("require" vs "required").
func TestSchemaParser_DisallowsUnknownFields(t *testing.T) {
	_, err := parseSchema(`{"type":"object","required":["x"],"require":["x"]}`)
	if err == nil {
		t.Errorf("parseSchema accepted unknown field 'require'; want error")
	}
}

// TestSchemaParser_ArrayRequiresItems — defensive: an "array" schema
// must declare "items", otherwise per-element validation is undefined.
func TestSchemaParser_ArrayRequiresItems(t *testing.T) {
	if _, err := parseSchema(`{"type":"array"}`); err == nil {
		t.Errorf("parseSchema accepted array schema without items; want error")
	}
}

// TestIsTransient_Classifier — exhaustive coverage of the transient
// classifier so a future refactor does not silently flip a permanent
// error to transient (or vice versa).
func TestIsTransient_Classifier(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"DeadlineExceeded", context.DeadlineExceeded, true},
		{"Canceled", context.Canceled, false},
		{"deadline-substr", errors.New("rpc: deadline exceeded"), true},
		{"timeout-substr", errors.New("upstream timeout"), true},
		{"timedout-substr", errors.New("request timed out"), true},
		{"conn-refused", errors.New("dial tcp: connection refused"), true},
		{"conn-reset", errors.New("read tcp: connection reset by peer"), true},
		{"no-such-host", errors.New("dial: no such host"), true},
		{"broken-pipe", errors.New("write: broken pipe"), true},
		{"network-unreachable", errors.New("dial: network is unreachable"), true},
		{"500", errors.New("status 500 Internal Server Error"), true},
		{"502", errors.New("status 502 Bad Gateway"), true},
		{"503", errors.New("status 503 Service Unavailable"), true},
		{"504", errors.New("status 504 Gateway Timeout"), true},
		{"401", errors.New("status 401 Unauthorized"), false},
		{"403", errors.New("status 403 Forbidden"), false},
		{"429", errors.New("status 429 Too Many Requests"), false},
		{"budget", llm.ErrBudgetExceeded, false},
		{"no-api-key", llm.ErrNoAPIKey, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isTransient(tc.err)
			if got != tc.want {
				t.Errorf("isTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestExtractJSON — table-driven coverage for the prose-wrapping
// helper. Exercises markdown fences, prose preamble, and unbalanced
// inputs.
func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string // expected Raw (after Valid check)
		wantErr bool
	}{
		{
			name: "plain object",
			in:   `{"a":1,"b":"two"}`,
			want: `{"a":1,"b":"two"}`,
		},
		{
			name: "fenced-json",
			in:   "```json\n{\"a\":1}\n```",
			want: `{"a":1}`,
		},
		{
			name: "prose-preamble",
			in:   "Sure! Here it is: {\"a\":1,\"b\":[1,2,3]}",
			want: `{"a":1,"b":[1,2,3]}`,
		},
		{
			name: "array",
			in:   "[1,2,3]",
			want: "[1,2,3]",
		},
		{
			name:    "empty",
			in:      "",
			wantErr: true,
		},
		{
			name:    "no-json",
			in:      "no json here",
			wantErr: true,
		},
		{
			name:    "unbalanced",
			in:      `{"a":`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := extractJSON(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("extractJSON(%q) err = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractJSON(%q) err: %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Errorf("extractJSON(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestBackoffFor — bounded growth; capped at 3s.
func TestBackoffFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attempt int
		minD    time.Duration
		maxD    time.Duration
	}{
		{0, 0, 0},
		{1, 50 * time.Millisecond, 200 * time.Millisecond},
		{2, 200 * time.Millisecond, 500 * time.Millisecond},
		{3, 500 * time.Millisecond, 1200 * time.Millisecond},
		{10, 2 * time.Second, 3500 * time.Millisecond}, // cap-kick
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("attempt=%d", tc.attempt), func(t *testing.T) {
			t.Parallel()
			d := backoffFor(tc.attempt)
			if d < tc.minD || d > tc.maxD {
				t.Errorf("backoffFor(%d) = %v, want within [%v, %v]", tc.attempt, d, tc.minD, tc.maxD)
			}
		})
	}
}

// TestSchemaValidate_AllTypes — regression for the validator walking
// each supported JSON type.
func TestSchemaValidate_AllTypes(t *testing.T) {
	t.Parallel()
	schemaJSON := `{
		"type":"object",
		"required":["s","n","i","b","arr","obj","nilv"],
		"properties":{
			"s":    {"type":"string"},
			"n":    {"type":"number"},
			"i":    {"type":"integer"},
			"b":    {"type":"boolean"},
			"arr":  {"type":"array", "items":{"type":"string"}},
			"obj":  {"type":"object"},
			"nilv": {"type":"null"}
		}
	}`
	sch, err := parseSchema(schemaJSON)
	if err != nil {
		t.Fatalf("parseSchema: %v", err)
	}
	good := `{"s":"x","n":1.5,"i":3,"b":true,"arr":["a"],"obj":{},"nilv":null}`
	if err := sch.validate(json.RawMessage(good)); err != nil {
		t.Errorf("valid doc returned err: %v", err)
	}
	bad := `{"s":"x","n":"not-a-number","i":3,"b":true,"arr":["a"],"obj":{},"nilv":null}`
	if err := sch.validate(json.RawMessage(bad)); err == nil {
		t.Errorf("type-mismatch doc accepted; want validation error")
	}
	intMismatch := `{"s":"x","n":1,"i":3.5,"b":true,"arr":["a"],"obj":{},"nilv":null}`
	if err := sch.validate(json.RawMessage(intMismatch)); err == nil {
		t.Errorf("non-integer accepted as integer; want validation error")
	}
}
