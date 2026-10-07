// governance.go holds the per-run tool governance that used to live inside
// the eino tool adapter: the identical-call memo, the per-tool execution
// cap, and the draft_config_change metric-catalog preflight.
//
// Why it moved here. Those three rules are not eino's business — they are
// statements about how a tool bag behaves within one agent run, and they
// must hold identically whether the run is driven by the eino ReAct graph or
// by PiG's agent kernel. Leaving them in graph/tool_adapter.go meant that a
// kernel swap would silently drop them, and the work they prevent (a ReAct
// loop re-running an expensive PromQL probe until the iteration budget is
// gone) is exactly what a kernel swap puts at risk. As a decorator the rules
// ride on basetool.BaseTool, which every kernel already consumes.
//
// Ordering: this decorator sits OUTSIDE every other decorator. The memo must
// observe a byte-identical repeat before the timeout/audit/metric chain does
// its work, or a cached call still pays a timeout check and still writes an
// audit row for a call that never executed. It is applied by the bag builder,
// not by Wrap, so a caller that wants raw tools still gets them.
package decorators

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

// toolMemo is a per-run cache of identical (read-tool, args) calls.
//
// One memo is shared by every tool in one bag, and the bag is built per
// request, so the cache lifetime is exactly one agent run. Within that run an
// identical call (same tool, byte-identical args) cannot yield new
// information and is almost always a loop artifact; returning the prior
// result skips re-executing an expensive tool (SSH probe, PromQL,
// LLM-backed query_translate) and keeps an identical-call loop from burning
// the iteration budget on real work. Only Class=="read" tools are memoized —
// write/destructive tools never touch this path, so the review/mutation flow
// is unaffected.
type toolMemo struct {
	mu     sync.Mutex
	m      map[string]string // (tool\x00args) -> result, identical-call cache
	counts map[string]int    // tool name -> distinct executions this run
	last   map[string]string // tool name -> most recent successful result this run
}

func newToolMemo() *toolMemo {
	return &toolMemo{m: make(map[string]string), counts: make(map[string]int), last: make(map[string]string)}
}

func (t *toolMemo) get(k string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[k]
	return v, ok
}

func (t *toolMemo) put(k, v string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[k] = v
}

// count returns how many real executions of the named tool have happened
// this run; bump records one more.
func (t *toolMemo) count(name string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[name]
}

func (t *toolMemo) bump(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[name]++
}

func (t *toolMemo) putLast(name, result string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last[name] = result
}

func (t *toolMemo) lastResult(name string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	result, ok := t.last[name]
	return result, ok
}

const (
	toolNameDraftConfigChange = "draft_config_change"
	toolNameListMetricCatalog = "list_metric_catalog"
	toolNameQueryPromQL       = "query_promql"
)

// MaxToolCallsPerRun caps how many times any one tool may EXECUTE within a
// single agent run. Identical-arg repeats are served from the memo and don't
// count; this catches the other failure mode — the model calling the same
// tool many times with slightly different args (e.g. query_promql across a
// dozen metrics, query_alert_rules over and over) without converging. Past
// the cap the tool returns a "synthesize now" directive instead of running,
// which forces the agent to answer from what it already gathered. Generous
// enough that normal multi-step investigation isn't clipped.
const MaxToolCallsPerRun = 30

// MaxCallsForTool reports the per-run execution limit for a tool name.
//
// Exported because the limit is a prompt contract, not an implementation
// detail: the refusal text tells the model "that is the per-tool limit for
// this turn", so any caller that needs to agree with that sentence — a test,
// a docs generator, a second kernel — must be able to read the same number
// rather than restate it.
func MaxCallsForTool(name string) int {
	switch name {
	case toolNameDraftConfigChange:
		// Only a confirmable config_draft increments this counter;
		// config_validation_failed remains retryable so the model can repair
		// a draft in the same user turn.
		return 1
	default:
		return MaxToolCallsPerRun
	}
}

func countFailedToolCall(name string) bool {
	switch name {
	case toolNameDraftConfigChange:
		return false
	default:
		return true
	}
}

func countSuccessfulToolCall(name, result string) bool {
	if name != toolNameDraftConfigChange {
		return true
	}
	var raw struct {
		Kind      string `json:"kind"`
		DraftHash string `json:"draft_hash"`
	}
	if err := json.Unmarshal([]byte(result), &raw); err != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(raw.Kind), "config_draft") &&
		strings.TrimSpace(raw.DraftHash) != ""
}

type draftConfigChangeGateArgs struct {
	Domain string `json:"domain"`
	Rule   struct {
		Kind       string                 `json:"kind"`
		Conditions []struct{}             `json:"conditions"`
		Spec       map[string]interface{} `json:"spec"`
	} `json:"rule"`
}

func draftConfigChangeNeedsMetricCatalog(argumentsInJSON string) bool {
	var in draftConfigChangeGateArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &in); err != nil {
		return false
	}
	domain := strings.ToLower(strings.TrimSpace(in.Domain))
	if domain != "" && domain != "alert_rule" {
		return false
	}
	if len(in.Rule.Conditions) > 0 {
		return true
	}
	kind := strings.ToLower(strings.TrimSpace(in.Rule.Kind))
	switch kind {
	case "metric_threshold", "metric_raw", "metric_anomaly", "metric_forecast", "metric_burn_rate",
		"trace_latency", "trace_error_rate":
		return true
	case "log_match", "log_volume":
		return false
	case "":
		return len(in.Rule.Spec) > 0
	default:
		return false
	}
}

// metricCatalogRequiredResult refuses a metric-based alert-rule draft that
// never consulted the metric catalog in this run. The refusal is a tool
// RESULT, not a Go error, so the model reads it as data and repairs itself in
// the same turn.
func metricCatalogRequiredResult() string {
	return toolResultJSON(map[string]interface{}{
		"status":      "blocked",
		"error":       "metric_catalog_required",
		"instruction": "Metric-based alert-rule drafts must call list_metric_catalog once earlier in this same user turn. Use the returned metric names and sample_labels to build the rule, then call draft_config_change again.",
	})
}

func toolResultJSON(fields map[string]interface{}) string {
	b, err := json.Marshal(fields)
	if err != nil {
		return `{"status":"blocked","error":"tool_result_marshal_failed"}`
	}
	return string(b)
}

// ToolBudgetExceededResult is the synthetic tool result returned once a tool
// hits its per-run cap. It is exported because it is the prompt contract with
// the model, not an implementation detail: BOTH kernels must hand back this
// exact envelope, or the model learns the cap exists from one kernel and not
// the other.
//
// Shaped like a normal JSON tool result so the LLM reads it as data and
// (re)directs to answering.
func ToolBudgetExceededResult(name string, n int) string {
	instruction := fmt.Sprintf("TERMINAL TOOL BUDGET RESULT. You have already called %q %d times in the current user turn — that is the per-tool limit for this turn only and it expires on the next user message. Your NEXT assistant message MUST be the final answer. Do NOT call this tool again. Do NOT call any substitute tool just to continue the same line of investigation. Answer from the results already gathered; if they're insufficient, state exactly what signal is missing.", name, n)
	if name == toolNameQueryPromQL {
		instruction = fmt.Sprintf("TERMINAL TOOL BUDGET RESULT. You have already called %q %d times in the current user turn. Stop issuing one PromQL call per device/metric/mountpoint. Your NEXT assistant message MUST be the final answer from gathered data; if the data is insufficient, say which single aggregated PromQL expression should be run next time using sum/topk and by(device_id, mountpoint, fstype). Do NOT call another tool in this turn.", name, n)
	}
	b, err := json.Marshal(struct {
		Status      string `json:"status"`
		Tool        string `json:"tool"`
		Calls       int    `json:"calls"`
		Scope       string `json:"scope"`
		FinalAnswer bool   `json:"final_answer_required"`
		Instruction string `json:"instruction"`
	}{
		Status:      "call_budget_exceeded",
		Tool:        name,
		Calls:       n,
		Scope:       "current_user_turn",
		FinalAnswer: true,
		Instruction: instruction,
	})
	if err != nil {
		return fmt.Sprintf(`{"status":"call_budget_exceeded","scope":"current_user_turn","instruction":%q}`, instruction)
	}
	return string(b)
}

// Governance applies the per-run tool rules to a bag.
//
// The zero value is a WORKING, DISABLED governance: a host that never calls
// NewGovernance gets tools passed through untouched. That direction is
// deliberate — a nil *Governance is what a caller who forgot to initialise
// one holds, and failing that case closed (by panicking or by silently
// capping every tool at zero) would break a deployment for a configuration
// mistake. Governance makes a run cheaper and more convergent; it does not
// make it safe, so its absence must not be fatal.
type Governance struct {
	memo *toolMemo
}

// NewGovernance returns an enabled Governance with a fresh per-run memo.
// Build one per run and wrap the whole bag with it.
func NewGovernance() *Governance {
	return &Governance{memo: newToolMemo()}
}

// Wrap returns inner under this governance. A nil receiver or nil inner
// returns inner unchanged, so callers can chain without a nil check.
func (g *Governance) Wrap(inner basetool.BaseTool) basetool.BaseTool {
	if g == nil || g.memo == nil || inner == nil {
		return inner
	}
	return &governedTool{inner: inner, memo: g.memo}
}

// WrapAll wraps every non-nil tool in a bag, sharing one memo across all of
// them. Sharing is the point: the per-tool cap is per tool, but the memo
// exists so a repeat ANYWHERE in the bag is caught, and a per-tool memo would
// miss a read tool whose result another tool already fetched.
func (g *Governance) WrapAll(tools []basetool.BaseTool) []basetool.BaseTool {
	out := make([]basetool.BaseTool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		out = append(out, g.Wrap(t))
	}
	return out
}

// governedTool is the basetool.BaseTool half of the rules. Keeping it a
// plain decorator — rather than a hook the kernels call — is what makes the
// rules kernel-independent: any loop that invokes a BaseTool gets them.
type governedTool struct {
	inner basetool.BaseTool
	memo  *toolMemo

	// infoOnce caches the name + read-ness used for the memo key and the
	// cap. A tool's metadata is fixed for the process lifetime, so
	// re-deriving it per call would put a JSON unmarshal on the hot path of
	// every invocation.
	infoOnce  sync.Once
	cacheName string
	cacheable bool // Class == "read"
}

func (g *governedTool) resolveInfo(ctx context.Context) {
	g.infoOnce.Do(func() {
		if info, err := g.inner.Info(ctx); err == nil && info != nil {
			g.cacheName = info.Name
			g.cacheable = info.Class == "read"
		}
	})
}

func (g *governedTool) Info(ctx context.Context) (*basetool.ToolInfo, error) {
	return g.inner.Info(ctx)
}

func (g *governedTool) InvokableRun(ctx context.Context, argsJSON string, opts ...basetool.InvokeOption) (string, error) {
	g.resolveInfo(ctx)

	// 1. Identical-call memo (read tools only): a byte-identical repeat
	//    returns the prior result without re-executing.
	var memoKey string
	if g.cacheable && g.cacheName != "" {
		memoKey = g.cacheName + "\x00" + argsJSON
		if cached, ok := g.memo.get(memoKey); ok {
			return cached, nil
		}
	}

	// 2. Per-tool execution cap (all tools): once a tool has run its limit
	//    this run, stop executing it and hand back a "synthesize now"
	//    directive. Catches the distinct-args repeat loop that the memo
	//    cannot. Checked only after a name resolved — a tool whose Info()
	//    failed has no cap identity and must still be callable, or an
	//    Info() bug would silently disable the tool.
	if g.cacheName != "" && g.memo.count(g.cacheName) >= MaxCallsForTool(g.cacheName) {
		return ToolBudgetExceededResult(g.cacheName, g.memo.count(g.cacheName)), nil
	}

	// 3. draft_config_change metric-catalog preflight.
	if blocked, ok := g.draftMetricCatalogPreflight(argsJSON); ok {
		return blocked, nil
	}

	out, err := g.inner.InvokableRun(ctx, argsJSON, opts...)
	if err != nil {
		// Count most failures toward the cap so a failing tool cannot be
		// hammered. draft_config_change is the exception: validation
		// failures are common while the model corrects structured config
		// args, and only a successful draft should consume the
		// one-draft-per-turn budget.
		if g.cacheName != "" && countFailedToolCall(g.cacheName) {
			g.memo.bump(g.cacheName)
		}
		// The error is NOT swallowed here. Re-shaping a Go error into a
		// tool-result envelope is the kernel adapter's job, because the
		// envelope's exact shape is what that kernel's loop needs in order
		// to treat a tool failure as data instead of aborting the run.
		return "", err
	}

	// Count this successful real execution toward the per-tool cap. A
	// config_validation_failed result from draft_config_change is a normal
	// repair signal, not a confirmable draft, so it remains retryable within
	// the same user turn. Only a real config_draft consumes the one-draft cap.
	if g.cacheName != "" {
		if countSuccessfulToolCall(g.cacheName, out) {
			g.memo.bump(g.cacheName)
		}
		g.memo.putLast(g.cacheName, out)
	}
	// Cache successful read-tool results only — a failed call stays
	// retryable (a transient error shouldn't be pinned for the whole run).
	if memoKey != "" {
		g.memo.put(memoKey, out)
	}
	return out, nil
}

func (g *governedTool) draftMetricCatalogPreflight(argumentsInJSON string) (string, bool) {
	if g.cacheName != toolNameDraftConfigChange {
		return "", false
	}
	if !draftConfigChangeNeedsMetricCatalog(argumentsInJSON) {
		return "", false
	}
	if _, ok := g.memo.lastResult(toolNameListMetricCatalog); !ok {
		return metricCatalogRequiredResult(), true
	}
	return "", false
}
