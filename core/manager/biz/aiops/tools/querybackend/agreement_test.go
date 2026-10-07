package querybackend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/logquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tracequery"
)

// These tests exist because for a long time each of these tools had two
// implementations — the Registry method and the BaseTool — that were nearly
// line-for-line identical and were held in step by a comment saying "mirrors".
// No test checked that they agreed. That is the same gap the correlate cluster
// had, where the two copies drifted and the Registry one shipped a
// "truncated":{} the BaseTool one omitted.
//
// The bodies are now shared, so agreement is structural rather than
// maintained. These tests are here to fail loudly the day someone gives one
// entry point its own argument handling again — which is the change that would
// reintroduce the drift, and the change that looks harmless.

// agreeProm / agreeLog / agreeTrace return a querier that answers with a fixed
// response. Fixed matters: the two entry points are called in turn, and if the
// fake echoed the resolved time window their answers would differ by
// microseconds for reasons that have nothing to do with what is under test.
func agreeProm() *fakePromQuerier {
	return &fakePromQuerier{resp: &promquery.InstantResult{
		ResultType: "matrix",
		Result:     json.RawMessage(`[{"metric":{"__name__":"up"},"values":[[1,"1"]]}]`),
	}}
}

func agreeLog() *fakeLogQuerier {
	return &fakeLogQuerier{resp: &logquery.QueryRangeResult{
		ResultType: "streams",
		Result:     json.RawMessage(`[{"stream":{"edge_id":"1"},"values":[["1700000000000000000","oops"]]}]`),
	}}
}

func agreeTrace() *fakeTraceQuerier {
	return &fakeTraceQuerier{resp: &tracequery.SearchResult{
		Traces: json.RawMessage(`[]`),
	}}
}

func TestPromQL_EntryPointsAgree(t *testing.T) {
	raw := `{"expr":"up","lookback_seconds":600}`

	fromRun, err := RunQueryPromQL(context.Background(), agreeProm(), json.RawMessage(raw))
	if err != nil {
		t.Fatalf("RunQueryPromQL: %v", err)
	}
	fromTool, err := NewQueryPromQLTool(agreeProm(), nil).InvokableRun(context.Background(), raw)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if string(fromRun) != fromTool {
		t.Errorf("entry points disagree:\n run  = %s\n tool = %s", fromRun, fromTool)
	}
}

func TestLogQL_EntryPointsAgree(t *testing.T) {
	raw := `{"query":"{a=\"b\"}","limit":50,"direction":"forward"}`

	fromRun, err := RunQueryLogQL(context.Background(), agreeLog(), json.RawMessage(raw))
	if err != nil {
		t.Fatalf("RunQueryLogQL: %v", err)
	}
	fromTool, err := NewQueryLogQLTool(agreeLog(), nil).InvokableRun(context.Background(), raw)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if string(fromRun) != fromTool {
		t.Errorf("entry points disagree:\n run  = %s\n tool = %s", fromRun, fromTool)
	}
}

func TestTraceQL_EntryPointsAgree(t *testing.T) {
	raw := `{"service":"web","operation":"GET /api","max_duration":"5s"}`

	fromRun, err := RunQueryTraceQL(context.Background(), agreeTrace(), json.RawMessage(raw))
	if err != nil {
		t.Fatalf("RunQueryTraceQL: %v", err)
	}
	fromTool, err := NewQueryTraceQLTool(agreeTrace(), nil).InvokableRun(context.Background(), raw)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if string(fromRun) != fromTool {
		t.Errorf("entry points disagree:\n run  = %s\n tool = %s", fromRun, fromTool)
	}
}

// TestEntryPointsRejectTheSameBadArgs is the other half of the invariant: the
// two paths must fail together. A guard that exists on only one of them is how
// a missing argument becomes a clean error on the bag path and a panic on the
// node-side upcall path.
func TestEntryPointsRejectTheSameBadArgs(t *testing.T) {
	raw := `{}` // carries no required argument for any of the three

	if _, err := RunQueryPromQL(context.Background(), agreeProm(), json.RawMessage(raw)); err == nil {
		t.Error("RunQueryPromQL accepted args with no expr")
	}
	if _, err := NewQueryPromQLTool(agreeProm(), nil).InvokableRun(context.Background(), raw); err == nil {
		t.Error("QueryPromQLTool accepted args with no expr")
	}

	if _, err := RunQueryLogQL(context.Background(), agreeLog(), json.RawMessage(raw)); err == nil {
		t.Error("RunQueryLogQL accepted args with no query")
	}
	if _, err := NewQueryLogQLTool(agreeLog(), nil).InvokableRun(context.Background(), raw); err == nil {
		t.Error("QueryLogQLTool accepted args with no query")
	}

	if _, err := RunQueryTraceQL(context.Background(), agreeTrace(), json.RawMessage(raw)); err == nil {
		t.Error("RunQueryTraceQL accepted args with no filter")
	}
	if _, err := NewQueryTraceQLTool(agreeTrace(), nil).InvokableRun(context.Background(), raw); err == nil {
		t.Error("QueryTraceQLTool accepted args with no filter")
	}
}

// TestNilQuerierRejectedByBothPaths pins the nil guard on both sides. The tools
// package registers these tools only when the querier is non-nil, so the nil
// case is unreachable in production — which is exactly why it needs a test: a
// guard that exists on only one path is a panic waiting for the wiring someone
// adds later.
func TestNilQuerierRejectedByBothPaths(t *testing.T) {
	ctx := context.Background()

	if _, err := RunQueryPromQL(ctx, nil, json.RawMessage(`{"expr":"up"}`)); err == nil {
		t.Error("RunQueryPromQL accepted a nil querier")
	}
	if _, err := NewQueryPromQLTool(nil, nil).InvokableRun(ctx, `{"expr":"up"}`); err == nil {
		t.Error("QueryPromQLTool accepted a nil querier")
	}

	if _, err := RunQueryLogQL(ctx, nil, json.RawMessage(`{"query":"{a=\"b\"}"}`)); err == nil {
		t.Error("RunQueryLogQL accepted a nil querier")
	}
	if _, err := NewQueryLogQLTool(nil, nil).InvokableRun(ctx, `{"query":"{a=\"b\"}"}`); err == nil {
		t.Error("QueryLogQLTool accepted a nil querier")
	}

	if _, err := RunQueryTraceQL(ctx, nil, json.RawMessage(`{"service":"web"}`)); err == nil {
		t.Error("RunQueryTraceQL accepted a nil querier")
	}
	if _, err := NewQueryTraceQLTool(nil, nil).InvokableRun(ctx, `{"service":"web"}`); err == nil {
		t.Error("QueryTraceQLTool accepted a nil querier")
	}
}

// TestDispatchErrorWrappedOnBothPaths checks the backend's own message reaches
// the caller on both paths — the property an operator debugs with when a query
// fails at 3am and needs to know whether Prometheus or the proxy said no.
func TestDispatchErrorWrappedOnBothPaths(t *testing.T) {
	boom := errors.New("backend 5xx")
	ctx := context.Background()
	raw := `{"expr":"up"}`

	_, runErr := RunQueryPromQL(ctx, &fakePromQuerier{err: boom}, json.RawMessage(raw))
	_, toolErr := NewQueryPromQLTool(&fakePromQuerier{err: boom}, nil).InvokableRun(ctx, raw)
	assertWraps(t, "query_promql run", runErr)
	assertWraps(t, "query_promql tool", toolErr)

	lraw := `{"query":"{a=\"b\"}"}`
	_, runErr = RunQueryLogQL(ctx, &fakeLogQuerier{err: boom}, json.RawMessage(lraw))
	_, toolErr = NewQueryLogQLTool(&fakeLogQuerier{err: boom}, nil).InvokableRun(ctx, lraw)
	assertWraps(t, "query_logql run", runErr)
	assertWraps(t, "query_logql tool", toolErr)

	traw := `{"service":"web"}`
	_, runErr = RunQueryTraceQL(ctx, &fakeTraceQuerier{err: boom}, json.RawMessage(traw))
	_, toolErr = NewQueryTraceQLTool(&fakeTraceQuerier{err: boom}, nil).InvokableRun(ctx, traw)
	assertWraps(t, "query_traceql run", runErr)
	assertWraps(t, "query_traceql tool", toolErr)
}

func assertWraps(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected an error, got nil", label)
		return
	}
	if !strings.Contains(err.Error(), "backend 5xx") {
		t.Errorf("%s: error = %v, want it to carry the backend message", label, err)
	}
}
