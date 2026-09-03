package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
)

// Validator enforces the safety rules for chat_to_query output:
//
//  1. syntax: expression must parse (live Prom/Log/Trace roundtrip)
//  2. cardinality: no `=~".+"` on high-cardinality labels, no
//     full-table scans with no metric selector
//  3. duration: range query suffix (e.g. `[60d]`) capped at 30d
//  4. risk: low / medium / high bucketing based on AST-ish patterns
//
// Live syntax validation is optional and gated behind a LiveChecker
// (production wires this to promquery.Client.InstantQuery; tests pass
// nil and rely on the regex layer alone).
type Validator struct {
	live LiveChecker
}

// LiveChecker is the optional live-syntax-validation hook. It must be
// safe to call from the chat_to_query tool path (i.e. respect ctx
// cancellation). nil means "regex-only" — acceptable for tests and
// for the first rollout where we want every chat_to_query result
// checked by a human before going to production.
type LiveChecker interface {
	CheckPromQL(ctx context.Context, expr string) error
	CheckLogQL(ctx context.Context, expr string) error
	CheckTraceQL(ctx context.Context, expr string) error
}

// NewValidator wires the validator.
func NewValidator(live LiveChecker) *Validator { return &Validator{live: live} }

// ValidationResult is the output of Validate. Risk mirrors the LLM
// risk semantics so the chat_to_query tool can show it on the UI.
type ValidationResult struct {
	Risk   string // low | medium | high
	Reason string // human-readable explanation of the risk bucketing
}

// maxQueryLen caps the expression length so a runaway LLM can't fill
// the promql parser with garbage.
const maxQueryLen = 2000

// maxRangeDays is the upper bound on the `[Nd]` / `[Nh]` suffix in a
// range vector selector. 30d matches the long-tail retention of
// cluster Prometheus instances.
const maxRangeDays = 30

// highCardinalityLabels are labels the validator rejects in any
// regex match (`=~"..."`) because they'd explode the result set.
var highCardinalityLabels = []string{
	"instance", "pod", "pod_name", "container", "node_id",
	"device_id", "uuid", "trace_id", "span_id", "ip",
}

// forbiddenPatterns are regexes that, if matched anywhere in the
// expression, indicate a query we never want to run automatically.
// Each carries a human-readable reason shown on the UI.
var forbiddenPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	// `count by (...) ({__name__=~".+"})` — no metric, full table scan.
	{regexp.MustCompile(`\(\s*\{[^{}]*__name__\s*=\s*~\s*"\.\+"`), "无 metric 选择器, 全表扫, 拒绝"},
	// Empty-label selector with regex wildcard.
	{regexp.MustCompile(`=\s*~\s*"\.\*\?"`), "label regex 通配符, 拒绝"},
	// High-cardinality label on a regex matcher.
	{regexp.MustCompile(`(\b(instance|pod|pod_name|container|node_id|device_id|uuid|trace_id|span_id|ip)\b\s*=\s*~)`), "高基数 label 用 regex, 拒绝"},
}

// dangerousCallPatterns marks calls that, combined with a long range,
// escalate risk to "high". Used after the duration check below.
var dangerousCalls = []string{"topk", "bottomk", "count_values", "quantile"}

// rangeSuffixRE captures the `[Nd|Nh|Nm|Ns]` range suffix on a
// vector selector so we can apply the maxRangeDays cap.
var rangeSuffixRE = regexp.MustCompile(`\[(\d+)(d|h|m|s)\]`)

// Validate runs the full pipeline. Returns (result, nil) on success,
// (nil, err) on reject. err messages are designed to be surfaced to
// the LLM (and the user) verbatim — they're the "why we said no".
func (v *Validator) Validate(ctx context.Context, signal, expr string) (*ValidationResult, error) {
	if signal != aiops.QueryTemplateSignalPromQL &&
		signal != aiops.QueryTemplateSignalLogQL &&
		signal != aiops.QueryTemplateSignalTraceQL {
		return nil, fmt.Errorf("validator: unknown signal %q", signal)
	}
	if expr == "" {
		return nil, fmt.Errorf("validator: empty expression")
	}
	if len(expr) > maxQueryLen {
		return nil, fmt.Errorf("validator: expression too long (%d > %d chars)", len(expr), maxQueryLen)
	}

	// Layer 1: structural forbidden patterns.
	for _, fp := range forbiddenPatterns {
		if fp.re.MatchString(expr) {
			return nil, fmt.Errorf("validator: %s", fp.reason)
		}
	}

	// Layer 2: duration cap. We only enforce for PromQL — LogQL /
	// TraceQL don't use the same `[Nd]` syntax.
	if signal == aiops.QueryTemplateSignalPromQL {
		if err := v.checkRangeDuration(expr); err != nil {
			return nil, err
		}
	}

	// Layer 3: high-cardinality label regex sanity (defense in depth
	// even though forbiddenPatterns above already catches the common
	// form — this layer allows tunability via the slice).
	low := strings.ToLower(expr)
	for _, lbl := range highCardinalityLabels {
		// `=~"..."` near the label — substring match is intentionally
		// loose; we'd rather false-positive than let a `instance=~"web.*"`
		// through.
		if strings.Contains(low, lbl+"=~") {
			return nil, fmt.Errorf("validator: regex on high-cardinality label %q rejected", lbl)
		}
	}

	// Layer 4 (optional): live syntax check via the production query
	// client. nil LiveChecker → skip. The chat_to_query tool only
	// wires a real checker once we have a test environment; the regex
	// layers above are sufficient for the first rollout.
	if v.live != nil {
		var err error
		switch signal {
		case aiops.QueryTemplateSignalPromQL:
			err = v.live.CheckPromQL(ctx, expr)
		case aiops.QueryTemplateSignalLogQL:
			err = v.live.CheckLogQL(ctx, expr)
		case aiops.QueryTemplateSignalTraceQL:
			err = v.live.CheckTraceQL(ctx, expr)
		}
		if err != nil {
			return nil, fmt.Errorf("validator: live syntax check failed: %w", err)
		}
	}

	return v.classifyRisk(expr), nil
}

// checkRangeDuration rejects any `[Nd|h|m|s]` suffix where the value
// exceeds maxRangeDays. Stacked subqueries (`[5m][1h]`) all have to
// pass independently — we scan every match.
func (v *Validator) checkRangeDuration(expr string) error {
	maxDur := time.Duration(maxRangeDays) * 24 * time.Hour
	for _, m := range rangeSuffixRE.FindAllStringSubmatch(expr, -1) {
		if len(m) < 3 {
			continue
		}
		n, err := parseRangeUnit(m[1], m[2])
		if err != nil {
			return fmt.Errorf("validator: cannot parse range %q: %w", m[0], err)
		}
		if n > maxDur {
			return fmt.Errorf("validator: range %q exceeds %d-day cap", m[0], maxRangeDays)
		}
	}
	return nil
}

// classifyRisk buckets the expression by shape. The buckets are
// intentionally conservative — "low" must be safe to auto-execute
// without human confirmation; "medium" and "high" warrant UI gating.
func (v *Validator) classifyRisk(expr string) *ValidationResult {
	low := strings.ToLower(expr)
	hasTopk := false
	for _, c := range dangerousCalls {
		if strings.Contains(low, c+"(") {
			hasTopk = true
			break
		}
	}
	hasLongRange := false
	for _, m := range rangeSuffixRE.FindAllStringSubmatch(expr, -1) {
		if len(m) < 3 {
			continue
		}
		d, err := parseRangeUnit(m[1], m[2])
		if err == nil && d >= time.Hour {
			hasLongRange = true
			break
		}
	}

	switch {
	case hasTopk && hasLongRange:
		return &ValidationResult{Risk: aiops.QueryTemplateRiskHigh, Reason: "topk/bottomk + ≥1h range"}
	case hasTopk || hasLongRange:
		return &ValidationResult{Risk: aiops.QueryTemplateRiskMedium, Reason: "topk 或 ≥1h range"}
	default:
		return &ValidationResult{Risk: aiops.QueryTemplateRiskLow, Reason: "单 metric, 短 range"}
	}
}

// parseRangeUnit converts the `[N d|h|m|s]` capture into a
// time.Duration. time.ParseDuration doesn't accept "d" for days, so
// we handle it inline. Negative or zero values are normalized to 0
// (caller treats 0 as "no range" and lets it through).
func parseRangeUnit(nStr, unit string) (time.Duration, error) {
	n := 0
	if _, err := fmt.Sscanf(nStr, "%d", &n); err != nil {
		return 0, err
	}
	switch unit {
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	case "h":
		return time.Duration(n) * time.Hour, nil
	case "m":
		return time.Duration(n) * time.Minute, nil
	case "s":
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("unknown unit %q", unit)
}
