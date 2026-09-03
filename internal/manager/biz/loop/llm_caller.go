// Package loop — llm_caller.go
//
// LLMCaller is the shared LLM invocation seam for the 5 phase workers
// (detected / correlated / investigated / critiqued / postmortem).
// Each worker would otherwise re-implement the same five concerns:
//
//  1. Prompt rendering (system + user message pair).
//  2. Provider-agnostic LLM call (goes through internal/pkg/llm.Client).
//  3. JSON extraction from prose-wrapped model output.
//  4. Schema validation of the extracted JSON.
//  5. Cost / token / latency telemetry.
//
// Funnelling all five through one type keeps worker code short and
// prevents drift (e.g. one worker quietly skipping retry on transient
// errors while another silently double-charges a token budget).
//
// Phase workers MUST hold an LLMCaller, never an *llm.Client or any
// cloudwego/eino SDK API. The orchestrator wires a single LLMCaller
// at startup and shares it across all workers; bypassing the seam
// re-introduces the duplication this abstraction exists to remove.
//
// Spec: docs/superpowers/specs/2026-08-12-llm-worker-integration-design.md §3.
// Delta spec: openspec/changes/llm-worker-integration/specs/closed-loop-orchestrator/spec.md
//
//	"LLMCaller 共享抽象" Requirement.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

// LLMCaller is the unified entry point phase workers use to invoke the
// LLM. Phase workers MUST NOT call internal/pkg/llm or any
// cloudwego/eino SDK directly.
//
// Failure semantics (see Call for the full matrix):
//
//   - OutputSchema cannot be parsed            → ErrSchemaUnparseable (no retry).
//   - LLM call returns non-JSON output         → ErrSchemaInvalid (no retry).
//   - LLM call returns JSON that fails schema  → ErrSchemaInvalid (no retry).
//   - LLM call returns timeout / 5xx / network → retried MaxRetries times,
//     then returned wrapped. No ErrSchemaInvalid wrap.
//   - LLM call returns 4xx (auth, budget)      → returned wrapped, no retry.
type LLMCaller interface {
	Call(ctx context.Context, in CallInput) (CallOutput, error)
}

// CallInput is the per-phase input handed to LLMCaller.Call.
//
// Field defaults (zero-value behaviour):
//
//   - TimeoutMs  == 0 → defaultCallTimeoutMs (60s).
//   - MaxRetries == 0 → 1 (one retry on transient errors, two total tries).
//   - OutputSchema is required for the contract-driven phases; for freeform
//     JSON (alert dedup, postmortem) callers may pass "{}" to disable
//     structural validation but Cost / token telemetry still populates.
type CallInput struct {
	// Phase is the orchestrator phase the call belongs to. Used for log
	// tagging and conditional model / prompt selection in future
	// iterations; today it is purely informational.
	Phase Phase

	// Prompt is the user-turn message. The orchestrator hands each worker
	// its full upstream contract + plan + incident metadata here.
	Prompt string

	// SystemPrompt is the system-turn message. Optional; empty string is
	// allowed when the worker owns all instructions in Prompt.
	SystemPrompt string

	// OutputSchema is a JSON-Schema document (subset: type / required /
	// properties / items / enum). The LLM output is validated against it
	// and returned as CallOutput.Raw on success. Schema-invalid output
	// produces ErrSchemaInvalid.
	OutputSchema string

	// TimeoutMs caps the wall-clock per attempt. A value <= 0 falls back
	// to defaultCallTimeoutMs.
	TimeoutMs int

	// MaxRetries bounds retries on transient errors (timeout / 5xx /
	// network). Values < 0 are treated as 0; 0 falls back to the spec
	// default of 1; values > 1 are allowed for tests that need to
	// exhaust retries.
	MaxRetries int
}

// CallOutput captures the verified LLM response plus telemetry fields
// the SRE team needs to attribute cost / latency / token usage per phase.
type CallOutput struct {
	// Raw is the JSON document returned by the model, already validated
	// against OutputSchema. Callers MUST NOT mutate the bytes; they are
	// safe to unmarshal into a typed struct.
	Raw json.RawMessage

	// TokensIn / TokensOut reflect the Usage reported by internal/pkg/llm
	// on the call that produced Raw. Populated on success only.
	TokensIn  int
	TokensOut int

	// CostUSD is the dollar cost of the call as computed by the cost
	// estimator (configurable via WithCostFunc). Default estimator uses a
	// blended per-token rate; production must replace via config before
	// cost dashboards trust the number.
	CostUSD float64

	// LatencyMs is wall-clock from "Chat call start" to "schema validated",
	// in milliseconds. Retries are NOT folded into this number — each
	// attempt has its own LatencyMs visible only via instrumentation.
	LatencyMs int
}

// ErrSchemaInvalid is returned when the LLM output violates the
// OutputSchema contract or the LLM fails to return valid JSON at all.
//
// Callers MUST treat this as a non-retryable, "structured-output
// contract violated" signal. The model has already produced an answer
// that does not match the contract; re-running the same prompt is
// unlikely to recover on its own (a different prompt or model might,
// but that is the worker's responsibility, not the caller's).
//
// Use errors.Is(err, ErrSchemaInvalid) at the worker Verifier to keep
// the import-free comparison.
var ErrSchemaInvalid = errors.New("llm_caller: schema validation failed")

// ErrSchemaUnparseable is a more specific sentinel wrapped when the
// caller-supplied OutputSchema itself is malformed (the worker's
// programmer shipped a broken schema). Distinct from ErrSchemaInvalid
// so operators can tell "model output wrong" from "schema wrong".
var ErrSchemaUnparseable = errors.New("llm_caller: caller schema unparseable")

// defaultCallTimeoutMs is the per-attempt wall-clock budget when the
// caller leaves CallInput.TimeoutMs at 0. 60s is the project-wide
// closed-loop floor — long enough for the slowest reasoning models to
// finish a schema-rich turn, short enough that a stuck call does not
// stall the orchestrator control loop.
const defaultCallTimeoutMs = 60_000

// llmCaller is the production LLMCaller backed by an llm.Client from
// internal/pkg/llm. It is safe to share across goroutines: the wrapped
// llm.Client is already concurrency-safe, and llmCaller holds no
// mutable state of its own outside the constructor-supplied logger.
type llmCaller struct {
	client llm.Client
	// qwenNoThink appends Qwen3's soft switch to user prompts.
	qwenNoThink bool

	// costFunc is overridable for tests and future per-model pricing.
	costFunc func(model string, tokensIn, tokensOut int) float64

	log *slog.Logger
}

// Option configures the llmCaller at construction time. Phase workers
// never construct an llmCaller directly — the orchestrator / main.go
// does so, and workers receive a read-only LLMCaller.
type Option func(*llmCaller)

// WithCostFunc replaces the default cost estimator. Signature lets
// future per-model pricing look up the model string passed by ChatResp.
func WithCostFunc(fn func(model string, tokensIn, tokensOut int) float64) Option {
	return func(c *llmCaller) { c.costFunc = fn }
}

// WithLogger injects a structured logger. slog.Default() is used when
// no logger is supplied; this matches the rest of the loop package.
func WithLogger(log *slog.Logger) Option {
	return func(c *llmCaller) { c.log = log }
}

func WithQwenNoThink() Option {
	return func(c *llmCaller) { c.qwenNoThink = true }
}

// NewLLMCaller wires a LLMCaller backed by an llm.Client.
//
// The client MUST be non-nil; passing nil is a programming error and
// will panic on first Call. Construct the client via internal/pkg/llm
// .New(cfg, budget, reg) so workers stay decoupled from SDK choices.
func NewLLMCaller(client llm.Client, opts ...Option) LLMCaller {
	if client == nil {
		// Fail loudly — silently substituting a noop client would mask
		// misconfiguration until the first phase fails at runtime, and
		// by then the orchestrator cannot tell wiring from infra.
		panic("loop.NewLLMCaller: client is nil")
	}
	c := &llmCaller{
		client:   client,
		log:      slog.Default(),
		costFunc: defaultCost,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// defaultCost is the placeholder cost estimator used when no
// WithCostFunc is supplied. It applies a flat blended per-token rate
// chosen as a mid-band price for GPT-class production models. It is
// intentionally conservative for governance: ops dashboards MUST NOT
// publish these numbers to finance before a per-model pricing plugin
// is wired. Production code MUST either call WithCostFunc at
// construction or extend this function to read from cfg.
func defaultCost(_ string, tokensIn, tokensOut int) float64 {
	const perTokenUSD = 0.000015 / 1000.0 // blended $0.015 per 1k tokens
	return float64(tokensIn+tokensOut) * perTokenUSD
}

// Call implements LLMCaller. See LLMCaller doc comment for the full
// failure-semantics matrix; the short version is "transient → retry
// once, contract violation → ErrSchemaInvalid, everything else
// propagates wrapped".
func (c *llmCaller) Call(ctx context.Context, in CallInput) (CallOutput, error) {
	// Compile OutputSchema once so a malformed schema does not pay the
	// parse cost on every retry. Schema parse failure is a programmer
	// error: wrap ErrSchemaUnparseable (NOT ErrSchemaInvalid) so
	// operators can tell "bad worker" from "bad model output".
	schema, err := parseSchema(in.OutputSchema)
	if err != nil {
		return CallOutput{}, fmt.Errorf("%w: %v", ErrSchemaUnparseable, err)
	}

	maxRetries := in.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries == 0 {
		// Default per spec: 1 retry on transient errors (2 total tries).
		// Higher values are opt-in via CallInput.MaxRetries (tests use
		// this to drive the retry-exhaustion path).
		maxRetries = 1
	}

	timeoutMs := in.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = defaultCallTimeoutMs
	}
	userPrompt := in.Prompt
	if c.qwenNoThink && !strings.Contains(userPrompt, "/no_think") {
		userPrompt += "\n/no_think"
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Backoff between retries. Implementation lives in backoffFor.
			select {
			case <-time.After(backoffFor(attempt)):
			case <-ctx.Done():
				return CallOutput{}, fmt.Errorf("llm_caller: context canceled during backoff: %w", ctx.Err())
			}
			c.log.Warn(
				"llm_caller: retrying after transient error",
				slog.String("phase", string(in.Phase)),
				slog.Int("attempt", attempt),
				slog.Int("max_retries", maxRetries),
				slog.String("last_error", lastErrString(lastErr)),
			)
		}

		callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
		start := time.Now()
		resp, err := c.client.Chat(callCtx, llm.ChatReq{
			Messages: []llm.Message{
				{Role: "system", Content: in.SystemPrompt},
				{Role: "user", Content: userPrompt},
			},
		})
		cancel()
		if err != nil {
			lastErr = err
			if !isTransient(err) {
				// Permanent failure (auth, budget) — fail fast with no retry.
				return CallOutput{}, fmt.Errorf("llm_caller: chat failed: %w", err)
			}
			continue
		}

		// Parse model output to JSON. Failure here means the model did
		// not produce JSON; treat as schema-invalid (the contract was
		// implicit in the request for JSON).
		raw, perr := extractJSON(resp.Assistant.Content)
		if perr != nil {
			return CallOutput{}, fmt.Errorf("%w: %v", ErrSchemaInvalid, perr)
		}

		// Validate against the caller-supplied schema. Skip validation
		// entirely when OutputSchema is empty (freeform probes / tests).
		var verr error
		if schema != nil {
			verr = schema.validate(raw)
		}
		if verr != nil {
			// Schema-invalid → no retry. The model already produced a
			// structured-but-wrong answer; re-running the same prompt is
			// likely to repeat the mistake.
			return CallOutput{}, fmt.Errorf("%w: %v", ErrSchemaInvalid, verr)
		}

		// Success. Build telemetry.
		latencyMs := int(time.Since(start) / time.Millisecond)
		cost := c.costFunc("", resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		return CallOutput{
			Raw:       raw,
			TokensIn:  resp.Usage.PromptTokens,
			TokensOut: resp.Usage.CompletionTokens,
			CostUSD:   cost,
			LatencyMs: latencyMs,
		}, nil
	}

	// All retries exhausted. Wrap with attempt count so the worker
	// Verifier can include "after N retries" in its Reasons.
	return CallOutput{}, fmt.Errorf("llm_caller: chat failed after %d retries: %w", maxRetries, lastErr)
}

// isTransient decides whether an error is worth retrying. Only timeout,
// 5xx, and network-class failures qualify. Auth (401/403), budget
// (ErrBudgetExceeded), and decode failures fail fast so operators see a
// clear signal instead of a slow cascade of retries.
//
// The check is string-based on err.Error(): Go 1.13+ error wrapping
// can hide the underlying text, but the strings we care about (SDK
// HTTP errors, Go stdlib net errors) bubble up unwrapped in our
// observed failure modes. errors.Is(ctx.DeadlineExceeded) handles the
// context-deadline case which is the only wrap we explicitly chase.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		// Canceled is NOT transient — the caller asked us to give up.
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "deadline exceeded") || strings.Contains(s, "timeout") || strings.Contains(s, "timed out") {
		return true
	}
	if strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "network is unreachable") ||
		strings.Contains(s, "broken pipe") {
		return true
	}
	// HTTP 5xx; we do NOT match 4xx — those are caller / config issues.
	if strings.Contains(s, "status 5") ||
		strings.Contains(s, " 500 ") || strings.Contains(s, " 502 ") ||
		strings.Contains(s, " 503 ") || strings.Contains(s, " 504 ") {
		return true
	}
	return false
}

// backoffFor returns the wait between retry attempts. Doubled-ish
// exponential schedule (100ms × 3^(attempt-1)), capped at 3s to keep
// the worst-case retry wait bounded inside the per-phase 30s verifier
// deadline.
//
// Returns 0 on attempt<=0 so callers can safely invoke unconditionally.
func backoffFor(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	d := 100 * time.Millisecond
	for i := 1; i < attempt; i++ {
		d *= 3
		if d >= 3*time.Second {
			return 3 * time.Second
		}
	}
	return d
}

// extractJSON pulls the first JSON object/array out of the model's
// response. Models frequently wrap JSON in markdown fences ("```json
// ... ```") or lead with prose ("Here is the answer: {...}"). The
// caller wants the JSON; this helper isolates it.
//
// Returns json.RawMessage pointing into a fresh byte slice — safe
// to embed in CallOutput.Raw without aliasing the input string.
func extractJSON(content string) (json.RawMessage, error) {
	s := strings.TrimSpace(content)
	if s == "" {
		return nil, errors.New("empty LLM output")
	}
	// Strip leading ```json fence if present.
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		// Strip trailing fence.
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
	}
	s = strings.TrimSpace(s)
	// Locate the first '{' or '[' to skip any prose preamble.
	ob := strings.IndexAny(s, "{[")
	if ob < 0 {
		return nil, errors.New("no JSON object/array in LLM output")
	}
	s = s[ob:]
	end := scanJSONEnd(s)
	if end <= 0 {
		return nil, errors.New("unbalanced JSON in LLM output")
	}
	raw := json.RawMessage(s[:end])
	if !json.Valid(raw) {
		return nil, errors.New("extracted JSON failed validation")
	}
	return raw, nil
}

// scanJSONEnd returns the index just past the matching closing brace
// of the JSON document starting at offset 0 of s. Handles nested
// objects/arrays and quoted strings (with backslash escapes) but no
// other JSON quirks — sufficient for our LLM output shape.
func scanJSONEnd(s string) int {
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		r := s[i]
		switch {
		case esc:
			esc = false
		case inStr && r == '\\':
			esc = true
		case r == '"':
			inStr = !inStr
		case !inStr && (r == '{' || r == '['):
			depth++
		case !inStr && (r == '}' || r == ']'):
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return 0
}

// lastErrString coerces an error to its error message for structured
// logging; returns "<nil>" when err is nil.
func lastErrString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
