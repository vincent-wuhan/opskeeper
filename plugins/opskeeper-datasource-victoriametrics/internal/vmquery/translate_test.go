package vmquery

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestInstantParamsCarriesTheExpressionAndTheInstant(t *testing.T) {
	ts := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	q, err := InstantParams(`up{job="api"}`, ts)
	if err != nil {
		t.Fatalf("InstantParams: %v", err)
	}
	if got := q.Get("query"); got != `up{job="api"}` {
		t.Errorf("query = %q, want the expression verbatim", got)
	}
	if got := q.Get("time"); got != "1772366400" {
		t.Errorf("time = %q, want seconds since epoch with no trailing zeros", got)
	}
}

// A zero instant means "now" on the backend. Sending the zero time's own
// seconds would pin the query to year 1 and look like a working query returning
// nothing, which is a much worse failure than an absent parameter.
func TestInstantParamsOmitsTimeForTheZeroInstant(t *testing.T) {
	q, err := InstantParams("up", time.Time{})
	if err != nil {
		t.Fatalf("InstantParams: %v", err)
	}
	if _, present := q["time"]; present {
		t.Errorf("time was sent for a zero instant: %q", q.Get("time"))
	}
}

func TestRangeParamsCarriesTheWindow(t *testing.T) {
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Minute)
	q, err := RangeParams("rate(http_requests_total[5m])", start, end, 30*time.Second)
	if err != nil {
		t.Fatalf("RangeParams: %v", err)
	}
	want := url.Values{
		"query": {"rate(http_requests_total[5m])"},
		"start": {"1772366400"},
		"end":   {"1772368200"},
		"step":  {"30"},
	}
	if got := q.Encode(); got != want.Encode() {
		t.Errorf("encoded params\n got %s\nwant %s", got, want.Encode())
	}
}

func TestRangeParamsRejectsAnImpossibleWindow(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		start, end  time.Time
		step        time.Duration
		wantMessage string
	}{
		{"zero step", at, at.Add(time.Minute), 0, "step must be > 0"},
		{"negative step", at, at.Add(time.Minute), -time.Second, "step must be > 0"},
		{"end before start", at, at.Add(-time.Second), time.Second, "must be after start"},
		{"empty window", at, at, time.Second, "must be after start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RangeParams("up", tc.start, tc.end, tc.step)
			if err == nil {
				t.Fatalf("RangeParams accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMessage)
			}
		})
	}
}

// The arguments are checked before the expression, so a caller that passed
// both a bad window and a bad expression is told the window is the problem.
func TestRangeParamsChecksTheWindowBeforeTheExpression(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	_, err := RangeParams("union(up, node_load1)", at, at, time.Second)
	if err == nil || !strings.Contains(err.Error(), "must be after start") {
		t.Fatalf("err = %v, want the window complaint first", err)
	}
}

func TestValidateExprAcceptsStandardPromQL(t *testing.T) {
	for _, expr := range []string{
		"up",
		"up{job=\"api\", instance=~\".*\"}",
		"rate(http_requests_total{code=~\"5..\"}[5m])",
		`histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
		"avg_over_time(node_load1[15m]) / ignoring(instance) group_left avg_over_time(node_load1[15m])",
		"sum by(job) (up)",
		"node_load1 * on(instance) group_right(node_load5)",
		"sum without (instance) (node_load1)",
		"absent(up{job=\"gone\"})",
		`label_replace(up, "host", "$1", "instance", "(.*):.*")`,
		"time() - process_start_time_seconds",
		// A '(' inside a string literal is not a call.
		`up{job="rate(weird)"}`,
		// A metric name followed by '(' is a label matcher, not a call.
		`sum by (job) (up)`,
	} {
		t.Run(expr, func(t *testing.T) {
			if err := ValidateExpr(expr); err != nil {
				t.Errorf("ValidateExpr(%q) = %v, want nil", expr, err)
			}
		})
	}
}

// A MetricsQL function that VictoriaMetrics would happily evaluate is refused,
// because OpsKeeper cannot promise that the number it gets back answers the
// question it asked. Silently accepting it is the failure mode this test
// exists to prevent.
func TestValidateExprRefusesMetricsQLOnlyFunctions(t *testing.T) {
	for _, expr := range []string{
		`union(up, node_load1)`,
		`interpolate(node_load1)`,
		`range_median(node_load1[5m])`,
		`range_quantile(0.9, node_load1[5m])`,
		`range_first(node_load1[5m])`,
		`rollup_1m(rate(http_requests_total[5m]))`,
		`default_zero(node_load1)`,
		// Nested inside an otherwise ordinary expression.
		`sum(rate(http_requests_total[5m])) / on() union(up, up)`,
	} {
		t.Run(expr, func(t *testing.T) {
			err := ValidateExpr(expr)
			if err == nil {
				t.Fatalf("ValidateExpr(%q) = nil, want a refusal", expr)
			}
			var ufe *UnsupportedFunctionError
			if !errors.As(err, &ufe) {
				t.Fatalf("err = %T (%v), want *UnsupportedFunctionError", err, err)
			}
			if ufe.Function == "" {
				t.Error("refusal names no function")
			}
			if !strings.Contains(err.Error(), ErrUnsupportedFunctionPrefix) {
				t.Errorf("error %q does not carry %q", err, ErrUnsupportedFunctionPrefix)
			}
		})
	}
}

// histogram_quantile is standard PromQL and is also the function most often
// mis-attributed to MetricsQL. Refusing it would break the queries the plan's
// integration templates actually use.
func TestValidateExprKeepsHistogramQuantile(t *testing.T) {
	expr := `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`
	if err := ValidateExpr(expr); err != nil {
		t.Errorf("ValidateExpr(%q) = %v, want nil", expr, err)
	}
}

func TestValidateExprRefusesAnEmptyExpression(t *testing.T) {
	for _, expr := range []string{"", "   ", "\t\n"} {
		if err := ValidateExpr(expr); err == nil {
			t.Errorf("ValidateExpr(%q) = nil, want a refusal", expr)
		}
	}
}

// A space between the name and the bracket is not a call, so this is not
// rejected for being an unknown function. The backend, which does parse the
// expression, is the right place to answer it.
func TestValidateExprIgnoresNamesNotTouchingABracket(t *testing.T) {
	if err := ValidateExpr("up offset union"); err != nil {
		t.Errorf("ValidateExpr = %v, want nil for a bare metric name", err)
	}
}
