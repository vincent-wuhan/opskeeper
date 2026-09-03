package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/logquery"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tracequery"
)

// QueryExecutor is the production Executor implementation. It fans
// out to whichever query client matches the signal. Concrete clients
// are passed by the manager's main.go wiring; nil is OK for signals
// the operator hasn't enabled (in which case Run returns an error).
type QueryExecutor struct {
	Prom  PromQuerier
	Logs  LogQuerier
	Trace TraceQuerier
}

// Run executes the query against the appropriate backend and returns
// the raw JSON the backend sent back. The chat_to_query tool hands
// this blob to the LLM unchanged (mirrors the pattern in
// query_promql / query_logql — the model can decide how to render).
func (e *QueryExecutor) Run(ctx context.Context, signal, expr string, lookback time.Duration) (json.RawMessage, error) {
	switch signal {
	case aiops.QueryTemplateSignalPromQL:
		if e.Prom == nil {
			return nil, errExecutorMissing("prom")
		}
		end := time.Now()
		start := end.Add(-lookback)
		step := stepFor(int(lookback.Seconds()))
		res, err := e.Prom.QueryRange(ctx, expr, start, end, step)
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)
	case aiops.QueryTemplateSignalLogQL:
		if e.Logs == nil {
			return nil, errExecutorMissing("logs")
		}
		end := time.Now()
		start := end.Add(-lookback)
		res, err := e.Logs.QueryRange(ctx, logquery.QueryRangeOptions{Query: expr, Start: start, End: end, Limit: 1000})
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)
	case aiops.QueryTemplateSignalTraceQL:
		if e.Trace == nil {
			return nil, errExecutorMissing("trace")
		}
		res, err := e.Trace.SearchTraces(ctx, tracequery.SearchOptions{Query: expr, Start: time.Now().Add(-lookback), End: time.Now(), Limit: 100})
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)
	}
	return nil, errUnknownSignal(signal)
}

func errExecutorMissing(name string) error {
	return &execErr{name: name}
}

func errUnknownSignal(s string) error {
	return &execErr{name: "unknown signal: " + s}
}

type execErr struct{ name string }

func (e *execErr) Error() string { return "chat_to_query executor: " + e.name }
