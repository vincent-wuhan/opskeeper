// Package vmquery turns a PromQL expression and a time window into the query
// parameters VictoriaMetrics' Prometheus-compatible HTTP API expects.
//
// The package exists as its own unit because the one claim that has to be
// true — "asking VictoriaMetrics is the same as asking Prometheus" — is a claim
// about bytes on the wire, and the only honest way to test a claim about bytes
// is to compare bytes. Everything here is parameter construction: no HTTP, no
// client, no decoding. The client in internal/vmclient assembles a URL from
// what this returns; promquery.Client builds its own. The contract test drives
// both against one recording server and compares the raw RequestURIs.
//
// Two things are deliberately absent. There is no MetricsQL support:
// VictoriaMetrics serves a superset of PromQL, and accepting that superset
// would mean a query that is valid on one backend and silently answered
// differently on the other — see the function whitelist below. And there is no
// expression rewriting: the expression goes out exactly as it came in, because
// a rewritten expression is a different question than the one that was asked.
package vmquery

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// QueryPromqlCallTimeout mirrors toolcore.QueryPromqlCallTimeout in
// core/manager/biz/aiops/tools/toolcore.
//
// It is copied rather than imported because this module may not import
// core/manager — that prohibition is what keeps a third-party package from
// dragging the control plane in. A copied constant is a constant that can
// drift, so the root module asserts the two are equal; if either side changes
// without the other, the build goes red instead of one backend quietly getting
// a different query window than the other.
const QueryPromqlCallTimeout = 30 * time.Second

// Paths on the VictoriaMetrics Prometheus-compatible API. Identical to the
// Prometheus ones by design; if VictoriaMetrics ever moves them, this is the
// single place that has to change and the contract test is what says so.
const (
	PathInstantQuery = "/api/v1/query"
	PathRangeQuery   = "/api/v1/query_range"
)

// ErrUnsupportedFunctionPrefix marks a rejection caused by an expression using
// a function that standard PromQL does not define. It appears in the error
// string the plan names, and it is also the prefix callers can match on rather
// than parsing the rest of the message.
const ErrUnsupportedFunctionPrefix = "vm_unsupported_function"

// ErrVM marks every error this package produces, so a caller can tell "the
// expression is wrong" apart from "the arguments are wrong".
const ErrVM = "vmquery: "

// UnsupportedFunctionError reports a MetricsQL-only (or otherwise non-PromQL)
// function in an expression.
type UnsupportedFunctionError struct {
	// Function is the offending name, as it appeared in the expression.
	Function string
	// Expr is the expression it was found in.
	Expr string
}

func (e *UnsupportedFunctionError) Error() string {
	return fmt.Sprintf("%s%s: function %q is not part of the PromQL function set; "+
		"VictoriaMetrics extends PromQL with MetricsQL functions whose results OpsKeeper "+
		"does not evaluate, so the query is refused rather than answered differently "+
		"(expression: %s)", ErrVM, ErrUnsupportedFunctionPrefix, e.Function, e.Expr)
}

// InstantParams builds the query parameters for an instant query at ts.
//
// The parameter set and its encoding have to match promquery.Client exactly,
// including the choice not to send `time` when ts is the zero time: Prometheus
// and VictoriaMetrics both read an absent `time` as "now", and sending
// "0001-01-01" instead would pin the query to a moment nobody asked for.
func InstantParams(expr string, ts time.Time) (url.Values, error) {
	if err := ValidateExpr(expr); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("query", expr)
	if !ts.IsZero() {
		q.Set("time", formatSeconds(ts))
	}
	return q, nil
}

// RangeParams builds the query parameters for a range query over [start, end]
// sampled every step.
//
// The argument checks come first and their wording mirrors promquery.Client's,
// because the difference between "this client rejected the call" and "the
// backend rejected the expression" is one an operator reads in a log line.
func RangeParams(expr string, start, end time.Time, step time.Duration) (url.Values, error) {
	if step <= 0 {
		return nil, fmt.Errorf("%sstep must be > 0, got %v", ErrVM, step)
	}
	if !end.After(start) {
		return nil, fmt.Errorf("%send (%v) must be after start (%v)", ErrVM, end, start)
	}
	if err := ValidateExpr(expr); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("query", expr)
	q.Set("start", formatSeconds(start))
	q.Set("end", formatSeconds(end))
	q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	return q, nil
}

// formatSeconds renders a timestamp the way the Prometheus HTTP API wants it:
// seconds since the epoch with no trailing zeros, so the encoding is stable and
// two clients given the same instant produce the same string.
func formatSeconds(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', -1, 64)
}

// identifierChar is what PromQL allows in a function name: letters, digits and
// underscore, not starting with a digit. Anything else means the token was not
// a function name — a metric name, an operator like `by`, or the inside of a
// string literal.
var identifierChar = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// standardFunctions is the PromQL function set.
//
// This is a whitelist, not a blacklist, and the direction matters. A blacklist
// has to enumerate every MetricsQL extension to stay current, and one it misses
// is a query that silently means something else. A whitelist only has to
// enumerate PromQL, which is finite and versioned. A function added to PromQL
// after this table was written is refused — visibly, with an error naming the
// function — rather than accepted on a guess.
//
// `histogram_quantile` is here because it is standard PromQL, despite also being
// the function most often mis-attributed to MetricsQL. The rollup functions
// (`range_median` and friends) are not: they are MetricsQL-only and refused.
var standardFunctions = map[string]struct{}{
	// Aggregations over a range.
	"avg_over_time": {}, "changes": {}, "count_over_time": {}, "delta": {},
	"deriv": {}, "holt_winters": {}, "idelta": {}, "increase": {},
	"irate": {}, "last_over_time": {}, "mad_over_time": {},
	"max_over_time": {}, "min_over_time": {}, "present_over_time": {},
	"rate": {}, "resets": {}, "stddev_over_time": {}, "stdvar_over_time": {},
	"sum_over_time": {}, "quantile_over_time": {},
	"predict_linear": {}, "time": {},
	// Instant aggregations.
	"absent": {}, "avg": {}, "count": {}, "count_values": {}, "group": {},
	"max": {}, "min": {}, "quantile": {}, "stddev": {}, "stdvar": {}, "sum": {},
	"topk": {}, "bottomk": {}, "limitk": {}, "sort": {}, "sort_desc": {},
	// Numeric and vector helpers.
	"abs": {}, "ceil": {}, "clamp": {}, "clamp_max": {}, "clamp_min": {},
	"exp": {}, "floor": {}, "ln": {}, "log2": {}, "log10": {}, "round": {},
	"sgn": {}, "sqrt": {}, "scalar": {}, "vector": {},
	// Timestamp helpers.
	"days_in_month": {}, "day_of_month": {}, "day_of_week": {}, "day_of_year": {},
	"hour": {}, "minute": {}, "month": {}, "year": {},
	// Label and metric helpers.
	"label_join": {}, "label_replace": {}, "absent_over_time": {},
	"timestamp": {}, "pi": {},
	// Histogram helpers.
	"histogram_avg": {}, "histogram_count": {}, "histogram_fraction": {},
	"histogram_quantile": {}, "histogram_stddev": {}, "histogram_stdvar": {},
	"histogram_sum": {},
}

// bracketedKeywords are PromQL keywords that may sit directly against a
// bracket without being a function call. `on(...)` and `ignoring(...)` name a
// label list for a vector match; `group_left(...)` / `group_right(...)` name
// the "many" side of one; `by(...)` / `without(...)` name an aggregation's
// grouping labels.
//
// They are listed rather than detected structurally because detecting them
// structurally means parsing, and the alternative to parsing is a list. This
// list is a list of things that are NOT function calls, so it can be checked
// against PromQL rather than against MetricsQL — the opposite of the blacklist
// the whitelist above exists to avoid.
var bracketedKeywords = map[string]struct{}{
	"on": {}, "ignoring": {}, "group_left": {}, "group_right": {},
	"by": {}, "without": {},
}

// ValidateExpr rejects an expression that uses a function outside the standard
// PromQL set.
//
// Function names are found by tokenising rather than by parsing. A full PromQL
// parser is a large dependency whose failure modes are their own problem, and
// the conservative direction is available: an expression that cannot be read as
// a function call is either valid PromQL or an expression the backend will
// reject on its own with a better message than this package could produce.
func ValidateExpr(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return fmt.Errorf("%sexpression is empty", ErrVM)
	}
	for _, tok := range tokenize(expr) {
		if !tok.called || !identifierChar.MatchString(tok.text) {
			continue
		}
		if _, kw := bracketedKeywords[tok.text]; kw {
			continue
		}
		if _, ok := standardFunctions[tok.text]; !ok {
			return &UnsupportedFunctionError{Function: tok.text, Expr: expr}
		}
	}
	return nil
}

// token is one lexical token: an identifier run, a string literal, or a single
// punctuation character.
type token struct {
	text string
	// called reports that the token was immediately followed by '(' with no
	// intervening space, i.e. it was used as a function name.
	called bool
}

// tokenize splits an expression for ValidateExpr. A '(' that directly follows
// an identifier marks that identifier as a call; a '(' preceded by anything
// else does not, which keeps a metric named `foo(` in a label matcher from
// being read as a function.
func tokenize(expr string) []token {
	var out []token
	var lit rune
	var inLit bool
	var buf strings.Builder

	flush := func() {
		if buf.Len() > 0 {
			out = append(out, token{text: buf.String()})
			buf.Reset()
		}
	}

	for _, r := range expr {
		switch {
		case inLit:
			buf.WriteRune(r)
			if r == lit {
				out = append(out, token{text: buf.String()})
				buf.Reset()
				inLit = false
			}
		case r == '"' || r == '\'' || r == '`':
			flush()
			lit, inLit = r, true
			buf.WriteRune(r)
		case isIdentRune(r):
			buf.WriteRune(r)
		case r == '(':
			if buf.Len() > 0 {
				out = append(out, token{text: buf.String(), called: true})
				buf.Reset()
			}
			out = append(out, token{text: "("})
		default:
			flush()
			out = append(out, token{text: string(r)})
		}
	}
	flush()
	return out
}

func isIdentRune(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}
