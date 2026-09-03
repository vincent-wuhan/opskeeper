package investigatorreal

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/logquery"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/promquery"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeMetricQuerier struct {
	err      error
	result   *promquery.InstantResult
	calls    atomic.Int32
	lastExpr atomic.Value
}

func (f *fakeMetricQuerier) QueryRange(_ context.Context, expr string, _, _ time.Time, _ time.Duration) (*promquery.InstantResult, error) {
	f.calls.Add(1)
	f.lastExpr.Store(expr)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type fakeLogQuerier struct {
	err    error
	result *logquery.QueryRangeResult
	calls  atomic.Int32
}

func (f *fakeLogQuerier) QueryRange(_ context.Context, _ logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func window() loop.TimeWindow {
	return loop.TimeWindow{
		Start: time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 15, 10, 5, 0, 0, time.UTC),
	}
}

func evidenceTools(evidence []loop.EvidenceItem) map[string]bool {
	tools := make(map[string]bool, len(evidence))
	for _, item := range evidence {
		tools[item.Tool] = true
	}
	return tools
}

func TestInvestigatorToolset_Investigate_MultiSourceSuccess(t *testing.T) {
	metrics := &fakeMetricQuerier{result: &promquery.InstantResult{
		ResultType: "vector",
		Result:     []byte(`[{"metric":{},"value":[1770000000,"42"]}]`),
	}}
	logs := &fakeLogQuerier{result: &logquery.QueryRangeResult{
		ResultType: "streams",
		Result:     []byte(`[{"stream":{},"values":[["1770000000000000000","error"]]},{"stream":{},"values":[["1770000001000000000","error"]]}]`),
	}}
	toolset := New(metrics, logs, quietLogger())

	evidence, err := toolset.Investigate(context.Background(), "pg", "ALERT-1", window())
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	tools := evidenceTools(evidence)
	if !tools["resource_alert"] || !tools["query_promql"] || !tools["query_logql"] {
		t.Fatalf("want all evidence tools, got %+v", tools)
	}
	if got := metrics.lastExpr.Load(); got != `max(pg_stat_activity_max_tx_duration_seconds{alert_id="ALERT-1"})` {
		t.Errorf("PromQL = %v", got)
	}
	if metrics.calls.Load() != 1 || logs.calls.Load() != 1 {
		t.Errorf("query counts: prom=%d log=%d", metrics.calls.Load(), logs.calls.Load())
	}
}

func TestInvestigatorToolset_Investigate_MetricFailureSoftFails(t *testing.T) {
	metrics := &fakeMetricQuerier{err: errors.New("prom unavailable")}
	logs := &fakeLogQuerier{result: &logquery.QueryRangeResult{ResultType: "streams", Result: []byte(`[]`)}}
	toolset := New(metrics, logs, quietLogger())

	evidence, err := toolset.Investigate(context.Background(), "redis", "ALERT-2", loop.TimeWindow{})
	if err != nil {
		t.Fatalf("Investigate should soft-fail: %v", err)
	}
	tools := evidenceTools(evidence)
	if !tools["resource_alert"] || !tools["query_logql"] || tools["query_promql"] {
		t.Fatalf("unexpected tools after partial failure: %+v", tools)
	}
}

func TestInvestigatorToolset_Investigate_AllFailuresReturnResourceAlert(t *testing.T) {
	toolset := New(
		&fakeMetricQuerier{err: errors.New("prom down")},
		&fakeLogQuerier{err: errors.New("loki down")},
		quietLogger(),
	)
	evidence, err := toolset.Investigate(context.Background(), "host", "ALERT-3", window())
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(evidence) != 1 || evidence[0].Tool != "resource_alert" {
		t.Fatalf("want resource_alert fallback, got %+v", evidence)
	}
}

func TestInvestigatorToolset_Investigate_ParameterValidation(t *testing.T) {
	toolset := New(&fakeMetricQuerier{}, nil, quietLogger())
	if _, err := toolset.Investigate(context.Background(), "", "A", window()); err == nil {
		t.Error("empty resourceType must fail")
	}
	if _, err := toolset.Investigate(context.Background(), "host", "", window()); err == nil {
		t.Error("empty alertID must fail")
	}
}

func TestInvestigatorToolset_Investigate_ConcurrentUse(t *testing.T) {
	toolset := New(&fakeMetricQuerier{result: &promquery.InstantResult{Result: []byte(`[]`)}}, nil, quietLogger())
	done := make(chan struct{})
	for index := 0; index < 20; index++ {
		go func(index int) {
			_, _ = toolset.Investigate(context.Background(), "pg", "ALERT", window())
			done <- struct{}{}
			_ = index
		}(index)
	}
	for index := 0; index < 20; index++ {
		<-done
	}
}

func TestSanitizeEvidence_RedactsSecrets(t *testing.T) {
	got := sanitizeEvidence([]byte(`{"password":"hunter2","authorization":"Bearer abc","safe":"ok"}`))
	if strings.Contains(got, "hunter2") || strings.Contains(got, "Bearer abc") {
		t.Fatalf("secret leaked: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") || !strings.Contains(got, `"safe":"ok"`) {
		t.Fatalf("unexpected redaction result: %s", got)
	}
}

func TestSanitizeEvidence_TruncatesLargePayload(t *testing.T) {
	raw := strings.Repeat("x", maxEvidencePayloadBytes+100)
	got := sanitizeEvidence([]byte(raw))
	if len(got) > maxEvidencePayloadBytes+len(`...[TRUNCATED]`) {
		t.Fatalf("payload not capped: %d", len(got))
	}
	if !strings.HasSuffix(got, `...[TRUNCATED]`) {
		t.Fatalf("truncation marker missing")
	}
}

func TestInvestigatorToolset_ListRemediationsWithEvidence_Thresholds(t *testing.T) {
	toolset := New(&fakeMetricQuerier{}, nil, quietLogger())
	cases := []struct {
		name         string
		resourceType string
		metric       string
		logCount     int
		wantAction   string
	}{
		{"pg long transaction", "pg", `[{"value":[1,"42"]}]`, 0, "pg.terminate_long_tx"},
		{"pg errors", "pg", `[]`, 1, "pg.kill_backend"},
		{"redis pressure", "redis", `[{"value":[1,"0.95"]}]`, 0, "redis.failover"},
		{"redis logs", "redis", `[]`, 1, "redis.client_kill"},
		{"k8s restarts", "k8s", `[{"value":[1,"0.2"]}]`, 0, "k8s.rolling_restart"},
		{"host cpu", "host", `[{"value":[1,"0.9"]}]`, 0, "host.restart_service"},
		{"mq backlog", "mq", `[{"value":[1,"1200"]}]`, 0, "mq.drain_queue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evidence := []loop.EvidenceItem{{Tool: "query_promql", Value: tc.metric}}
			if tc.logCount > 0 {
				evidence = append(evidence, loop.EvidenceItem{Tool: "query_logql", Count: tc.logCount})
			}
			options, err := toolset.ListRemediationsWithEvidence(context.Background(), tc.resourceType, "ALERT", evidence)
			if err != nil {
				t.Fatalf("strategy: %v", err)
			}
			found := 0
			seen := make(map[string]struct{})
			for _, option := range options {
				if option.Action == tc.wantAction {
					found++
				}
				if _, exists := seen[option.Action]; exists {
					t.Fatalf("duplicate action %q in %+v", option.Action, options)
				}
				seen[option.Action] = struct{}{}
			}
			if found != 1 {
				t.Fatalf("action %q missing in %+v", tc.wantAction, options)
			}
		})
	}
}

func TestInvestigatorToolset_ListRemediationsWithEvidence_BaselineAndSafety(t *testing.T) {
	toolset := New(&fakeMetricQuerier{}, nil, quietLogger())
	for _, resourceType := range []string{"pg", "redis", "k8s", "host", "mq"} {
		options, err := toolset.ListRemediationsWithEvidence(context.Background(), resourceType, "ALERT", nil)
		if err != nil {
			t.Fatalf("%s strategy: %v", resourceType, err)
		}
		var safe, mutating bool
		for _, option := range options {
			if option.Target == "" {
				t.Fatalf("%s action %q has empty target", resourceType, option.Action)
			}
			if option.AutoApprove && option.Risk != "safe" {
				t.Fatalf("%s unsafe auto approval: %+v", resourceType, option)
			}
			if option.Risk == "safe" {
				safe = true
			}
			if option.Risk == "mutating" {
				mutating = true
			}
		}
		if !safe || !mutating {
			t.Fatalf("%s baseline lacks safe/mutating pair: %+v", resourceType, options)
		}
	}
}

func TestInvestigatorToolset_ListRemediations_ValidationAndUnknown(t *testing.T) {
	toolset := New(&fakeMetricQuerier{}, nil, quietLogger())
	if _, err := toolset.ListRemediations(context.Background(), ""); err == nil {
		t.Error("empty resourceType must fail")
	}
	options, err := toolset.ListRemediations(context.Background(), "unknown")
	if err != nil {
		t.Fatalf("unknown resource should soft-fail: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("unknown resource should return empty options, got %+v", options)
	}
}
