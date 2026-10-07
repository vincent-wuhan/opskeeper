package llmgw

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// The two per-call ceilings are the ones a node cannot be trusted to raise.
// max_completion_tokens is the single number a node controls that directly
// buys tokens, and a provider call that never returns is the same problem in
// wall-clock terms: both hold the cluster's money and both hold the manager's
// connections. So both are clamped on the manager's side, and both refuse
// before the money moves.

// slowCompleter never answers on its own; it answers only when the gateway's
// deadline cancels it, which is the shape of the provider failure this exists
// for.
type slowCompleter struct {
	waited time.Duration
}

func (s *slowCompleter) Complete(ctx context.Context, _ pigmodel.Request) (*pigai.AssistantMessage, error) {
	start := time.Now()
	<-ctx.Done()
	s.waited = time.Since(start)
	return nil, ctx.Err()
}

// tunedCompleter reports what the gateway would have sent to the provider.
type tunedCompleter struct {
	maxTokens int
	called    bool
}

func (c *tunedCompleter) Complete(_ context.Context, req pigmodel.Request) (*pigai.AssistantMessage, error) {
	c.called = true
	var opts pigai.StreamOptions
	if req.Tune != nil {
		req.Tune(&opts)
	}
	c.maxTokens = opts.MaxTokens
	return &pigai.AssistantMessage{}, nil
}

func boundedHandler(t *testing.T, completer Completer, bounds CallBounds) *Handler {
	t.Helper()
	handler, err := NewHandler(Options{
		Auth:      &stubAuth{edges: map[string]uint64{"ak:sk": 1}},
		Completer: completer,
		Bounds:    bounds,
	})
	if err != nil {
		t.Fatalf("build the handler: %v", err)
	}
	return handler
}

// A node asking for more tokens than the operator allows gets the operator's
// number, not its own. Without the clamp the ceiling is a suggestion the node
// makes to itself, which is the same non-enforcement the whole host-side
// budget exists to avoid.
func TestANodeCannotBuyMoreTokensThanTheOperatorAllows(t *testing.T) {
	completer := &tunedCompleter{}
	handler := boundedHandler(t, completer, CallBounds{MaxOutputTokens: 4096})
	body := `{"model":"m","max_completion_tokens":200000,"messages":[{"role":"user","content":"hi"}]}`

	if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if completer.maxTokens != 4096 {
		t.Errorf("the provider was asked for %d output tokens, want the operator's 4096", completer.maxTokens)
	}
}

// The clamp is a ceiling, not a constant. A node that asks for less than the
// operator allows keeps its own number: a gateway that decided how big answers
// are would make the request field decorative and would silently overrule a
// caller that knows its own context window.
func TestANodeAskingForLessKeepsItsOwnCeiling(t *testing.T) {
	completer := &tunedCompleter{}
	handler := boundedHandler(t, completer, CallBounds{MaxOutputTokens: 4096})
	body := `{"model":"m","max_completion_tokens":512,"messages":[{"role":"user","content":"hi"}]}`

	if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if completer.maxTokens != 512 {
		t.Errorf("the provider was asked for %d output tokens, want the caller's 512", completer.maxTokens)
	}
}

// A node that sends no ceiling at all is the common case, and it is still
// bounded: "unset" means the registry's model configuration applies, and the
// operator's ceiling is about the cluster's money rather than about what the
// node remembered to include.
func TestAnAbsentCeilingStillGetsTheOperatorsCap(t *testing.T) {
	completer := &tunedCompleter{}
	handler := boundedHandler(t, completer, CallBounds{MaxOutputTokens: 4096})

	if rec := post(t, handler, "ak:sk", spendBody); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if completer.maxTokens != 4096 {
		t.Errorf("an unset ceiling reached the provider as %d, want the operator's 4096", completer.maxTokens)
	}
}

// No configured ceiling means the caller's own value stands untouched. An
// operator who set no bound gets exactly the previous behaviour, and a gateway
// that invented one would be spending an operator's decision on their behalf.
func TestAnUnconfiguredTokenCeilingChangesNothing(t *testing.T) {
	completer := &tunedCompleter{}
	handler := boundedHandler(t, completer, CallBounds{})
	body := `{"model":"m","max_completion_tokens":321,"messages":[{"role":"user","content":"hi"}]}`

	if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if completer.maxTokens != 321 {
		t.Errorf("the caller's %d was changed to %d with no ceiling configured", 321, completer.maxTokens)
	}
}

// A provider that never answers must be abandoned, and the refusal must be
// distinguishable from a provider error: a timed-out call is one the node
// should retry smaller, and 504 says "abandoned" where 500 says "the model
// failed" and a client retries both identically.
func TestAHungProviderIsAbandonedRatherThanWaitedOnForever(t *testing.T) {
	completer := &slowCompleter{}
	handler := boundedHandler(t, completer, CallBounds{ProviderTimeout: 20 * time.Millisecond})

	rec := post(t, handler, "ak:sk", spendBody)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("a hung provider was answered with %d, want 504; body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "timeout_error") {
		t.Errorf("the refusal is not an OpenAI timeout error object: %s", body)
	}
	if !strings.Contains(body, "smaller answer") {
		t.Errorf("the refusal does not tell the node what to do differently: %s", body)
	}
	if completer.waited > time.Second {
		t.Errorf("the provider was held for %s, so the deadline did not reach it", completer.waited)
	}
}

// The streaming path settles the same call and must abandon it the same way.
// Two call sites written separately is how one of them ends up answering a hung
// provider with a 500 while the other answers with a 504 — two failures for
// one problem, and a node's logs that disagree with themselves.
func TestAStreamIsAbandonedUnderTheSameDeadline(t *testing.T) {
	completer := &slowCompleter{}
	handler := boundedHandler(t, completer, CallBounds{ProviderTimeout: 20 * time.Millisecond})
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	rec := post(t, handler, "ak:sk", body)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("a hung provider on the streaming path was answered with %d, want 504; body %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "timeout_error") {
		t.Errorf("the streaming refusal is not the same timeout object: %s", rec.Body.String())
	}
}

// A provider that fails for its own reasons inside the window is that failure,
// not a timeout. Reporting a provider error as 504 would tell a node to retry
// smaller when the honest answer is "the model is down".
func TestAProviderErrorInsideTheWindowIsNotReportedAsATimeout(t *testing.T) {
	completer := &stubCompleter{err: context.DeadlineExceeded}
	handler := boundedHandler(t, completer, CallBounds{ProviderTimeout: time.Minute})

	rec := post(t, handler, "ak:sk", spendBody)
	if rec.Code == http.StatusGatewayTimeout {
		t.Fatalf("a provider error was reported as a gateway timeout: %s", rec.Body.String())
	}
}

// No configured timeout leaves the call unbounded, which is the deployment that
// set nothing — and it must not accidentally bound calls by a default nobody
// chose.
func TestAnUnconfiguredTimeoutDoesNotCutACallShort(t *testing.T) {
	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler := boundedHandler(t, completer, CallBounds{})

	if rec := post(t, handler, "ak:sk", spendBody); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
}

// The timeout error is a sentinel, not a string: an operator's alerting keys
// on it, and "upstream timeout" appearing in a log body is not the same fact.
func TestATimeoutCarriesTheManagerSentinel(t *testing.T) {
	bounds := CallBounds{ProviderTimeout: time.Millisecond}
	ctx, cancel := bounds.context(context.Background())
	defer cancel()
	<-ctx.Done()

	err := bounds.timeoutError(ctx, ctx.Err())
	if !strings.Contains(err.Error(), errs.ErrUpstreamTimeout.Error()) {
		t.Fatalf("the wrapped error does not name the sentinel: %s", err)
	}
	if errs.HTTPStatus(err) != http.StatusGatewayTimeout {
		t.Errorf("the sentinel maps to %d, want 504", errs.HTTPStatus(err))
	}
	// A provider error that arrives before the deadline keeps its own message.
	if got := bounds.timeoutError(context.Background(), context.Canceled); got == nil ||
		got.Error() != context.Canceled.Error() {
		t.Errorf("an error raised inside the window was rewritten as a timeout: %v", got)
	}
}
