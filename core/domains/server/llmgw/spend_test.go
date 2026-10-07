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

// The three tests below are the money tests, and they exist because the
// gateway is the only path from a fleet of agents to a paid provider account.
// A node cannot be trusted to bound its own spend — a compromised node is
// exactly the case the centralized credential was introduced for — so every
// bound has to hold on the manager's side of the wire, where the node cannot
// reach around it.

// stubBudget is the cluster cap, reduced to two recorded calls.
type stubBudget struct {
	allowErr  error
	checked   []int
	recorded  []int
	recordErr error
}

func (b *stubBudget) Check(_ context.Context, estPromptTokens int) error {
	b.checked = append(b.checked, estPromptTokens)
	return b.allowErr
}

func (b *stubBudget) Record(_ context.Context, tokens int) error {
	b.recorded = append(b.recorded, tokens)
	return b.recordErr
}

// meteredReply is a reply that reported what it cost.
func meteredReply(input, output, total int) *pigai.AssistantMessage {
	msg := pigai.AssistantMessage{}
	msg.Content = append(msg.Content, pigai.TextContent{Text: "the disk is full"})
	msg.Usage = pigai.Usage{Input: input, Output: output, TotalTokens: total}
	return &msg
}

func gatedHandler(t *testing.T, auth EdgeAuthenticator, completer Completer, budget Budget, limiter Limiter) *Handler {
	t.Helper()
	handler, err := NewHandler(Options{Auth: auth, Completer: completer, Budget: budget, Limiter: limiter})
	if err != nil {
		t.Fatalf("build the handler: %v", err)
	}
	return handler
}

const spendBody = `{"model":"m","messages":[{"role":"user","content":"why is the disk full"}]}`

// A cap that is consulted after the provider has been called is a report, not
// a cap. The refusal has to happen before the money moves, and it has to
// arrive as 429 — the status that tells a provider client to back off rather
// than the 400 that says the request was malformed, which is what this
// endpoint's own error switch used to answer for every sentinel it did not
// know.
func TestTheClusterCapRefusesBeforeTheProviderIsTouched(t *testing.T) {
	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	budget := &stubBudget{allowErr: errs.ErrBudgetExceeded}
	handler := gatedHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 9}}, completer, budget, nil)

	rec := post(t, handler, "ak:sk", spendBody)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status %d, want 429; the body is %s", rec.Code, rec.Body.String())
	}
	if completer.got.Messages != nil {
		t.Error("the provider was called for a request the cap had already refused")
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Errorf("the refusal is not in the shape a provider client parses: %s", rec.Body.String())
	}
}

// The other half of a cap: a check that is never followed by a record is a
// tripwire, and every call after the first would pass it. Both request shapes
// are covered, because a stream bills the same tokens as a buffered reply and
// a budget that counted only the buffered ones would be bypassed by asking
// for a stream.
func TestASettledCallIsChargedToTheClusterCapOnBothPaths(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "buffered"
		if streaming {
			name = "streamed"
		}
		t.Run(name, func(t *testing.T) {
			budget := &stubBudget{}
			completer := &stubCompleter{reply: meteredReply(1200, 300, 1500)}
			handler := gatedHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 9}}, completer, budget, nil)

			body := spendBody
			if streaming {
				body = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200; the body is %s", rec.Code, rec.Body.String())
			}

			if len(budget.checked) != 1 {
				t.Fatalf("the cap was consulted %d times, want once", len(budget.checked))
			}
			if len(budget.recorded) != 1 || budget.recorded[0] != 1500 {
				t.Errorf("recorded %v, want one charge of 1500 tokens", budget.recorded)
			}
		})
	}
}

// A provider that reports no usage cannot be charged a number it never gave,
// and the ledger must not be invented from a guess. The log line carries
// usage_reported=false so an operator can see that a provider is running
// unaccounted, which is a fact worth having and is not the same as a zero.
func TestAProviderThatReportedNothingIsChargedNothingAndSaysSo(t *testing.T) {
	budget := &stubBudget{}
	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler := gatedHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 9}}, completer, budget, nil)

	if rec := post(t, handler, "ak:sk", spendBody); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if len(budget.recorded) != 0 {
		t.Errorf("recorded %v; a provider that reported no usage must not be billed a guess", budget.recorded)
	}
}

// One node in a pathological loop must not be able to spend the fleet's
// share, and the refusal has to name the node and the rate: a 429 with no
// reason is indistinguishable from a provider outage in the agent's logs, and
// an operator debugging the wrong one loses the incident.
func TestOneRunawayNodeCannotSpendTheFleetShare(t *testing.T) {
	limiter := NewLimiter(2)
	auth := &stubAuth{edges: map[string]uint64{"ak-sick:sk": 1, "ak-healthy:sk": 2}}
	handler := gatedHandler(t, auth, &stubCompleter{reply: &pigai.AssistantMessage{}}, nil, limiter)

	for i := 0; i < 2; i++ {
		if rec := post(t, handler, "ak-sick:sk", spendBody); rec.Code != http.StatusOK {
			t.Fatalf("call %d: status %d, want 200", i+1, rec.Code)
		}
	}
	rec := post(t, handler, "ak-sick:sk", spendBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("the third call in a 2/minute window was served: status %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "node 1") || !strings.Contains(body, "2 model requests per minute") {
		t.Errorf("the refusal does not name the node and the rate: %s", body)
	}

	// The healthy node is a different bucket. A fleet-wide throttle would
	// answer an incident by refusing the nodes that are not the problem.
	if rec := post(t, handler, "ak-healthy:sk", spendBody); rec.Code != http.StatusOK {
		t.Errorf("a second node was refused: status %d, body %s", rec.Code, rec.Body.String())
	}
}

// The sweep that keeps a long-running manager from accumulating a bucket per
// edge id it has ever seen. A decommissioned node's state must stop counting,
// or a fleet that churns accumulates entries for ever.
//
// The assertion is on the map rather than on an allowance, because the clock
// this test injects drives the sweep and not the token refill: x/time/rate
// keeps its own, and a test claiming an idle node's bucket had refilled would
// be asserting something the injected clock never touched.
func TestIdleBucketsAreSweptAndTheClockIsInjected(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := newEdgeLimiter(1, func() time.Time { return now })

	if allowed, _ := limiter.Allow(context.Background(), 5); !allowed {
		t.Fatal("a node's first request was refused")
	}
	if got := len(limiter.buckets); got != 1 {
		t.Fatalf("%d buckets for one node, want 1", got)
	}

	// Past the TTL, past the sweep interval, and by a different node — which
	// is the trigger. The idle node's bucket is gone; the new one's exists.
	now = now.Add(11 * time.Minute)
	if allowed, _ := limiter.Allow(context.Background(), 6); !allowed {
		t.Fatal("a different node's first request was refused")
	}
	if _, still := limiter.buckets[5]; still {
		t.Error("a node idle for over the TTL still has a bucket; a churning fleet grows one for ever")
	}
	if _, fresh := limiter.buckets[6]; !fresh {
		t.Error("the requesting node's own bucket was swept")
	}
}

// "Not configured" has to mean "no limit", not "no requests". An operator who
// sets the rate to zero is saying they would rather find out what an unbounded
// fleet costs, and a deployment that reads it as a fleet that cannot diagnose
// anything is a misreading of one integer.
func TestAnUnconfiguredRateDisablesTheGateInsteadOfRefusingEverything(t *testing.T) {
	if NewLimiter(0) != nil || NewLimiter(-1) != nil {
		t.Fatal("a non-positive rate built a limiter; it must build none, and it must be an " +
			"untyped nil so the handler's own nil check is the one that sees it")
	}
	// Exercised through the handler, because that is where the nil is met: a
	// deployment that set the rate to zero must serve traffic, not panic.
	auth := &stubAuth{edges: map[string]uint64{"ak:sk": 1}}
	handler := gatedHandler(t, auth, &stubCompleter{reply: &pigai.AssistantMessage{}}, nil, NewLimiter(0))
	for i := 0; i < 5; i++ {
		if rec := post(t, handler, "ak:sk", spendBody); rec.Code != http.StatusOK {
			t.Fatalf("call %d was refused with no limit configured: status %d", i+1, rec.Code)
		}
	}
}

// A node whose agent has a bug would otherwise lock itself out of the cluster
// by sending malformed requests: each one would be refused, and if the refusal
// cost the node its allowance the node would be out of budget before it had
// spent anything. The gates run after the request is understood, and this is
// the test that keeps them there.
func TestAMalformedRequestDoesNotConsumeTheNodesAllowance(t *testing.T) {
	limiter := NewLimiter(1)
	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler := gatedHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 3}}, completer, nil, limiter)

	if rec := post(t, handler, "ak:sk", `{"model":"m","messages":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for an empty transcript", rec.Code)
	}
	if rec := post(t, handler, "ak:sk", spendBody); rec.Code != http.StatusOK {
		t.Errorf("the one legitimate call in the window was refused: status %d, body %s",
			rec.Code, rec.Body.String())
	}
}

// max_completion_tokens is the only output bound a caller can state, and
// before this it was parsed and thrown away: an agent that asked for a 4k
// answer got whatever the provider felt like writing, which is both an
// overrun of the request and an unpredictable line item on the operator's
// bill.
func TestTheCallersOutputCeilingReachesTheProvider(t *testing.T) {
	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 4}}, completer)

	if rec := post(t, handler, "ak:sk",
		`{"model":"m","max_completion_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if completer.got.Tune == nil {
		t.Fatal("the request carried an output ceiling and no adjustment reached the model call")
	}
	opts := pigai.StreamOptions{MaxTokens: 1 << 20}
	completer.got.Tune(&opts)
	if opts.MaxTokens != 4096 {
		t.Errorf("MaxTokens is %d, want the caller's 4096", opts.MaxTokens)
	}

	// An absent ceiling is not a ceiling of zero: the registry's own model
	// configuration is what decides, and the gateway must not overwrite it
	// with a number nobody asked for.
	completer.got = pigmodel.Request{}
	if rec := post(t, handler, "ak:sk", spendBody); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if completer.got.Tune != nil {
		t.Error("a request with no max_completion_tokens still overrode the model configuration")
	}
}

// A negative ceiling is a caller that misread the field, not a caller asking
// for a short answer, and it is refused where it can still name the reason.
func TestANegativeOutputCeilingIsRefused(t *testing.T) {
	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 4}}, completer)

	rec := post(t, handler, "ak:sk",
		`{"model":"m","max_completion_tokens":-1,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400; the body is %s", rec.Code, rec.Body.String())
	}
	if completer.got.Messages != nil {
		t.Error("a request with a nonsense ceiling reached the provider")
	}
}

// The gateway is assembled in main from a config value, and the constant that
// documents the recommended rate lives in this package. If the two drift, the
// documented default is not the effective one and nobody finds out from the
// code — the test that compares them lives in the root module, where both are
// importable. This asserts the constant is at least the shape main passes.
func TestTheRecommendedRateIsUsableByTheWiring(t *testing.T) {
	if DefaultEdgeRequestsPerMinute <= 0 {
		t.Fatal("the recommended per-node rate is not positive; a deployment copying it " +
			"would build no limiter at all and believe it had one")
	}
	limiter := NewLimiter(DefaultEdgeRequestsPerMinute)
	if limiter == nil {
		t.Fatal("NewLimiter rejected the recommended rate")
	}
	if allowed, _ := limiter.Allow(context.Background(), 1); !allowed {
		t.Error("the recommended rate refuses a node's very first request")
	}
}

// Compile-time proof the seams are what main believes they are: a nil
// limiter and a nil budget are both legal configurations, and both have to
// read as "not configured" rather than as a crash on the first request.
func TestTheGateSeamsTolerateBeingUnwired(t *testing.T) {
	handler := gatedHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 1}},
		&stubCompleter{reply: &pigai.AssistantMessage{}}, nil, nil)

	if rec := post(t, handler, "ak:sk", spendBody); rec.Code != http.StatusOK {
		t.Errorf("a deployment with neither gate refused a call: status %d, body %s",
			rec.Code, rec.Body.String())
	}
}
