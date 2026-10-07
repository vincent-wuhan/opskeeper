package decorators

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

// governedFake is the minimal tool these tests need. It exists rather than
// reusing fakeTool because governance tests never touch timeouts, audit, or
// options — a smaller double keeps the failure readable when one of them
// breaks.
type governedFake struct {
	name   string
	class  string
	result string
	err    error
	calls  int32
}

func (f *governedFake) Info(context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{Name: f.name, Class: f.class}, nil
}

func (f *governedFake) InvokableRun(context.Context, string, ...basetool.InvokeOption) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.result, nil
}

// TestGovernanceMemoizesIdenticalReadCalls pins the identical-call cache: a
// byte-identical repeat of a read tool must not reach the tool again. If this
// regressed, a ReAct loop could re-run an expensive SSH probe or PromQL query
// on every iteration and exhaust the turn budget on work already done.
func TestGovernanceMemoizesIdenticalReadCalls(t *testing.T) {
	t.Parallel()
	inner := &governedFake{name: "query_promql", class: "read", result: `{"v":1}`}
	tool := NewGovernance().Wrap(inner)
	ctx := context.Background()

	first, err := tool.InvokableRun(ctx, `{"q":"up"}`)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := tool.InvokableRun(ctx, `{"q":"up"}`)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first != second || first != `{"v":1}` {
		t.Fatalf("results = %q, %q; want both %q", first, second, `{"v":1}`)
	}
	if inner.calls != 1 {
		t.Fatalf("identical read calls executed %d times, want 1", inner.calls)
	}
	if _, err := tool.InvokableRun(ctx, `{"q":"down"}`); err != nil {
		t.Fatalf("distinct call: %v", err)
	}
	if inner.calls != 2 {
		t.Fatalf("distinct args executed %d times, want 2 (the memo must key on args, not just the tool)", inner.calls)
	}
}

// TestGovernanceNeverMemoizesWriteTools pins that a mutating tool always
// executes. The review/approval flow is built around seeing every call; a
// cached "success" would report work that never happened, and a cached
// refusal would keep a repaired approval from being retried.
func TestGovernanceNeverMemoizesWriteTools(t *testing.T) {
	t.Parallel()
	for _, class := range []string{"write", "destructive"} {
		class := class
		t.Run(class, func(t *testing.T) {
			t.Parallel()
			inner := &governedFake{name: "host_restart_service", class: class, result: `{"ok":true}`}
			tool := NewGovernance().Wrap(inner)
			ctx := context.Background()
			_, _ = tool.InvokableRun(ctx, `{"svc":"nginx"}`)
			_, _ = tool.InvokableRun(ctx, `{"svc":"nginx"}`)
			if inner.calls != 2 {
				t.Fatalf("%s tool executed %d times, want 2 (mutation must not be memoized)", class, inner.calls)
			}
		})
	}
}

// TestGovernanceDoesNotMemoizeErrors pins that a failed read stays retryable.
// Pinning a transient failure for the rest of the run would turn one flaky
// probe into a fact the model cannot recheck.
func TestGovernanceDoesNotMemoizeErrors(t *testing.T) {
	t.Parallel()
	inner := &governedFake{name: "query_x", class: "read", err: errors.New("boom")}
	tool := NewGovernance().Wrap(inner)
	ctx := context.Background()
	_, _ = tool.InvokableRun(ctx, `{"q":"a"}`)
	_, _ = tool.InvokableRun(ctx, `{"q":"a"}`)
	if inner.calls != 2 {
		t.Fatalf("errored read executed %d times, want 2 (failures stay retryable)", inner.calls)
	}
}

// TestGovernanceCapsDistinctArgLoops pins the per-tool execution cap. This is
// the failure mode the memo cannot catch: the model calling one tool many
// times with slightly different arguments (query_promql per device, per
// mountpoint) without converging. Past the cap the tool must stop executing
// and hand back a directive to answer from what was already gathered.
func TestGovernanceCapsDistinctArgLoops(t *testing.T) {
	t.Parallel()
	inner := &governedFake{name: "query_x", class: "read", result: `{"v":1}`}
	tool := NewGovernance().Wrap(inner)
	ctx := context.Background()

	for i := 0; i < MaxToolCallsPerRun; i++ {
		out, err := tool.InvokableRun(ctx, fmt.Sprintf(`{"q":"m%d"}`, i))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if strings.Contains(out, "call_budget_exceeded") {
			t.Fatalf("call %d refused before the cap of %d", i, MaxToolCallsPerRun)
		}
	}
	if inner.calls != MaxToolCallsPerRun {
		t.Fatalf("executions = %d, want %d", inner.calls, MaxToolCallsPerRun)
	}

	out, err := tool.InvokableRun(ctx, `{"q":"over"}`)
	if err != nil {
		t.Fatalf("over-cap call returned an error; the refusal must be a tool RESULT: %v", err)
	}
	if !strings.Contains(out, "call_budget_exceeded") || !strings.Contains(out, "final_answer_required") {
		t.Fatalf("over-cap result = %q, want a call_budget_exceeded directive", out)
	}
	if inner.calls != MaxToolCallsPerRun {
		t.Fatalf("over-cap call executed anyway: %d executions", inner.calls)
	}

	// The refusal must be valid JSON: the model reads it as data, and a
	// truncated or escaped-wrong envelope would show up as a parse failure in
	// the transcript instead of an instruction.
	var probe map[string]any
	if err := json.Unmarshal([]byte(out), &probe); err != nil {
		t.Fatalf("over-cap result is not valid JSON: %v (%q)", err, out)
	}
	if probe["scope"] != "current_user_turn" {
		t.Fatalf("scope = %v, want current_user_turn so the model knows the cap expires", probe["scope"])
	}
}

// TestGovernanceQueryPromQLRefusalNudgesAggregation pins the specialised
// refusal text. query_promql is the one tool where "answer now" is not
// enough advice: the model needs to be told the *shape* of a better next
// query, or a later turn repeats the same per-device fan-out.
func TestGovernanceQueryPromQLRefusalNudgesAggregation(t *testing.T) {
	t.Parallel()
	inner := &governedFake{name: "query_promql", class: "read", result: `{"v":1}`}
	tool := NewGovernance().Wrap(inner)
	ctx := context.Background()
	for i := 0; i < MaxToolCallsPerRun; i++ {
		_, _ = tool.InvokableRun(ctx, fmt.Sprintf(`{"q":"m%d"}`, i))
	}
	out, _ := tool.InvokableRun(ctx, `{"q":"over"}`)
	if !strings.Contains(out, "aggregated PromQL") {
		t.Fatalf("query_promql refusal = %q, want the aggregation nudge", out)
	}
}

// TestGovernanceDraftConfigChangeCapIsOne pins the one-draft-per-turn rule.
// A confirmable draft is a proposal the operator is about to read; allowing a
// second one in the same turn would show them two competing proposals for one
// question.
func TestGovernanceDraftConfigChangeCapIsOne(t *testing.T) {
	t.Parallel()
	confirmable := `{"kind":"config_draft","draft_hash":"abc123"}`
	inner := &governedFake{name: "draft_config_change", class: "write", result: confirmable}
	tool := NewGovernance().Wrap(inner)
	ctx := context.Background()

	// The governor gates metric rules behind list_metric_catalog, so satisfy
	// that first with a sibling under the same governance.
	gov := NewGovernance()
	catalog := &governedFake{name: "list_metric_catalog", class: "read", result: `{"metrics":[]}`}
	_, _ = gov.Wrap(catalog).InvokableRun(ctx, `{}`)
	tool = gov.Wrap(inner)

	if MaxCallsForTool("draft_config_change") != 1 {
		t.Fatalf("draft cap = %d, want 1", MaxCallsForTool("draft_config_change"))
	}
	if _, err := tool.InvokableRun(ctx, `{"domain":"alert_rule"}`); err != nil {
		t.Fatalf("first draft: %v", err)
	}
	out, err := tool.InvokableRun(ctx, `{"domain":"alert_rule"}`)
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	if !strings.Contains(out, "call_budget_exceeded") {
		t.Fatalf("second confirmable draft = %q, want a budget refusal", out)
	}
}

// TestGovernanceValidationFailureDoesNotConsumeDraftCap pins the deliberate
// exception to the failure-counts-toward-the-cap rule. A model repairing a
// malformed draft produces several failures in one turn; counting them would
// spend the single-draft budget on the mistakes and make the corrected draft
// impossible.
func TestGovernanceValidationFailureDoesNotConsumeDraftCap(t *testing.T) {
	t.Parallel()
	inner := &governedFake{
		name:  "draft_config_change",
		class: "write",
		err:   errors.New("config_validation_failed: bad field"),
	}
	gov := NewGovernance()
	catalog := &governedFake{name: "list_metric_catalog", class: "read", result: `{"metrics":[]}`}
	_, _ = gov.Wrap(catalog).InvokableRun(context.Background(), `{}`)
	tool := gov.Wrap(inner)

	for i := 0; i < 5; i++ {
		if _, err := tool.InvokableRun(context.Background(), `{"domain":"alert_rule"}`); err == nil {
			t.Fatalf("iteration %d: expected the inner validation error to surface", i)
		}
	}
	if inner.calls != 5 {
		t.Fatalf("validation attempts = %d, want 5 (failures must not consume the draft cap)", inner.calls)
	}
}

// TestGovernanceMetricRuleRequiresCatalogFirst pins the preflight: a
// metric-based alert rule drafted without consulting the metric catalog is
// refused before it executes, so the operator is never shown a rule built on
// guessed metric names.
func TestGovernanceMetricRuleRequiresCatalogFirst(t *testing.T) {
	t.Parallel()
	draft := &governedFake{name: "draft_config_change", class: "write",
		result: `{"kind":"config_draft","draft_hash":"h"}`}
	tool := NewGovernance().Wrap(draft)

	out, err := tool.InvokableRun(context.Background(), `{"domain":"alert_rule","rule":{"kind":"metric_threshold"}}`)
	if err != nil {
		t.Fatalf("preflight refusal must be a tool result, not an error: %v", err)
	}
	if !strings.Contains(out, "metric_catalog_required") {
		t.Fatalf("refusal = %q, want metric_catalog_required", out)
	}
	if draft.calls != 0 {
		t.Fatalf("the draft executed %d times; the preflight must refuse before execution", draft.calls)
	}
}

// TestGovernanceLogRuleSkipsCatalogPreflight pins that the preflight is not
// applied where it makes no sense: a log-based rule has no metric names to
// verify, so demanding the catalog would block a valid draft forever.
func TestGovernanceLogRuleSkipsCatalogPreflight(t *testing.T) {
	t.Parallel()
	draft := &governedFake{name: "draft_config_change", class: "write",
		result: `{"kind":"config_draft","draft_hash":"h"}`}
	tool := NewGovernance().Wrap(draft)

	out, err := tool.InvokableRun(context.Background(), `{"domain":"alert_rule","rule":{"kind":"log_match"}}`)
	if err != nil {
		t.Fatalf("log rule draft: %v", err)
	}
	if strings.Contains(out, "metric_catalog_required") {
		t.Fatalf("log rule was gated on the metric catalog: %q", out)
	}
	if draft.calls != 1 {
		t.Fatalf("log rule executed %d times, want 1", draft.calls)
	}
}

// TestGovernanceAllowsMetricRuleAfterCatalog pins the other half of the
// preflight: once the catalog HAS been consulted in this run, a metric rule
// must go through. A gate that never opens is a tool that cannot be used.
func TestGovernanceAllowsMetricRuleAfterCatalog(t *testing.T) {
	t.Parallel()
	gov := NewGovernance()
	catalog := &governedFake{name: "list_metric_catalog", class: "read", result: `{"metrics":["up"]}`}
	if _, err := gov.Wrap(catalog).InvokableRun(context.Background(), `{}`); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	draft := &governedFake{name: "draft_config_change", class: "write",
		result: `{"kind":"config_draft","draft_hash":"h"}`}
	out, err := gov.Wrap(draft).InvokableRun(context.Background(), `{"domain":"alert_rule","rule":{"kind":"metric_threshold"}}`)
	if err != nil {
		t.Fatalf("draft after catalog: %v", err)
	}
	if strings.Contains(out, "metric_catalog_required") {
		t.Fatalf("draft still gated after the catalog ran: %q", out)
	}
}

// TestGovernanceSharesMemoAcrossTheBag pins that one Governance wraps a whole
// bag with ONE memo. The draft preflight depends on it: the catalog call and
// the draft call are different tools, and a per-tool memo would leave the
// preflight unable to see that the catalog had already run.
func TestGovernanceSharesMemoAcrossTheBag(t *testing.T) {
	t.Parallel()
	gov := NewGovernance()
	catalog := &governedFake{name: "list_metric_catalog", class: "read", result: `{"metrics":["up"]}`}
	draft := &governedFake{name: "draft_config_change", class: "write",
		result: `{"kind":"config_draft","draft_hash":"h"}`}
	bag := gov.WrapAll([]basetool.BaseTool{catalog, draft})
	if len(bag) != 2 {
		t.Fatalf("bag size = %d, want 2", len(bag))
	}
	if _, err := bag[0].InvokableRun(context.Background(), `{}`); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	out, err := bag[1].InvokableRun(context.Background(), `{"domain":"alert_rule","rule":{"kind":"metric_threshold"}}`)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if strings.Contains(out, "metric_catalog_required") {
		t.Fatalf("draft could not see the catalog call from its sibling; the memo is not shared: %q", out)
	}
}

// TestGovernanceWrapAllSkipsNil pins that a sparse bag (a skill filter that
// blanks slots rather than compacting) does not produce a nil entry that
// panics when the kernel invokes it.
func TestGovernanceWrapAllSkipsNil(t *testing.T) {
	t.Parallel()
	keep := &governedFake{name: "query_x", class: "read", result: `{"v":1}`}
	bag := NewGovernance().WrapAll([]basetool.BaseTool{nil, keep, nil})
	if len(bag) != 1 {
		t.Fatalf("bag size = %d, want 1 (nil entries dropped)", len(bag))
	}
	if _, err := bag[0].InvokableRun(context.Background(), `{}`); err != nil {
		t.Fatalf("invoke: %v", err)
	}
}

// TestGovernanceZeroValueIsDisabled pins the fail-open direction. A host that
// never builds a Governance holds a nil pointer; wrapping with it must return
// the tool untouched rather than panicking or capping it at zero calls.
// Governance makes a run cheaper and more convergent — it does not make it
// safe — so its absence must degrade to "ungoverned", never to "unusable".
func TestGovernanceZeroValueIsDisabled(t *testing.T) {
	t.Parallel()
	inner := &governedFake{name: "query_x", class: "read", result: `{"v":1}`}

	var nilGov *Governance
	if got := nilGov.Wrap(inner); got != basetool.BaseTool(inner) {
		t.Fatalf("nil Governance returned %#v, want the inner tool unchanged", got)
	}
	empty := &Governance{}
	if got := empty.Wrap(inner); got != basetool.BaseTool(inner) {
		t.Fatalf("zero Governance returned %#v, want the inner tool unchanged", got)
	}
	// And the pass-through must actually be callable.
	tool := empty.Wrap(inner)
	if _, err := tool.InvokableRun(context.Background(), `{}`); err != nil {
		t.Fatalf("pass-through invoke: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("pass-through executed %d times, want 1", inner.calls)
	}
}

// TestGovernanceInfoIsPassedThroughUntouched pins that governance does not
// rewrite the schema. The tool's declared description, when-to-use, and
// parameters are what the model reasons about; a decorator that reshaped them
// would change tool selection without any test that names it.
func TestGovernanceInfoIsPassedThroughUntouched(t *testing.T) {
	t.Parallel()
	inner := &governedFake{name: "query_x", class: "read", result: `{"v":1}`}
	got, err := NewGovernance().Wrap(inner).Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if got.Name != "query_x" || got.Class != "read" {
		t.Fatalf("Info = %#v, want the inner tool's metadata verbatim", got)
	}
}

// TestGovernanceInstancesDoNotShareState pins per-run isolation. The tool bag
// itself is built once at process start and reused by every request, so the
// memo MUST be owned by a Governance created per run — not by the tool bag.
// If a Governance (or its memo) were hoisted to boot time, a read result
// fetched for one user's session would be served from cache to the next
// user's, which is a cross-tenant data leak rather than a performance bug.
func TestGovernanceInstancesDoNotShareState(t *testing.T) {
	t.Parallel()
	inner := &governedFake{name: "query_promql", class: "read", result: `{"v":1}`}
	ctx := context.Background()

	runOne := NewGovernance().Wrap(inner)
	runTwo := NewGovernance().Wrap(inner)

	if _, err := runOne.InvokableRun(ctx, `{"q":"up"}`); err != nil {
		t.Fatalf("run one: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("executions after run one = %d, want 1", inner.calls)
	}
	// The identical call in a DIFFERENT run must re-execute: the two runs do
	// not share a cache.
	if _, err := runTwo.InvokableRun(ctx, `{"q":"up"}`); err != nil {
		t.Fatalf("run two: %v", err)
	}
	if inner.calls != 2 {
		t.Fatalf("executions after run two = %d, want 2 (memo leaked across runs)", inner.calls)
	}
}

// TestGovernanceDoesNotMutateTheBagItWraps pins that wrapping is
// non-destructive. The boot-time bag is shared; if WrapAll rewrote entries in
// place, a later run would find its tools pre-wrapped by an earlier run's
// Governance and would silently inherit that run's memo.
func TestGovernanceDoesNotMutateTheBagItWraps(t *testing.T) {
	t.Parallel()
	inner := &governedFake{name: "query_x", class: "read", result: `{"v":1}`}
	bag := []basetool.BaseTool{inner}

	wrapped := NewGovernance().WrapAll(bag)
	if len(bag) != 1 || bag[0] != basetool.BaseTool(inner) {
		t.Fatalf("WrapAll rewrote the caller's slice: %#v", bag)
	}
	if wrapped[0] == basetool.BaseTool(inner) {
		t.Fatalf("WrapAll returned the bare tool; nothing is governed")
	}
	// A second wrap of the SAME bag yields an independent wrapper.
	second := NewGovernance().WrapAll(bag)
	if second[0] == wrapped[0] {
		t.Fatalf("two WrapAll calls returned the same wrapper; state would be shared")
	}
}
