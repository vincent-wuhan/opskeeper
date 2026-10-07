package querybackend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/toolcore"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tracequery"
)

// ToolNameQueryTraceQL is the stable wire name the LLM sees for the TraceQL
// tool.
const ToolNameQueryTraceQL = "query_traceql"

// QueryTraceQLDescription is the single-sentence description shown to the
// LLM. Phrased to point the model at trace search whenever metrics + logs
// don't pin down which request is slow.
const QueryTraceQLDescription = "Run a TraceQL search against Tempo. " +
	"Use this to find traces by service / operation / latency / status. " +
	"Returns trace summaries (id, service, root span name, duration, span count)."

// QueryTraceQLSchema is the JSON Schema of the tool's argument object.
// query may be empty when service/operation/duration filters are supplied —
// Tempo's tag-mode search handles that case.
var QueryTraceQLSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "TraceQL expression. May be empty if service/operation/duration filters are given."
    },
    "service": {
      "type": "string",
      "description": "Filter by resource.service.name (tag mode)."
    },
    "operation": {
      "type": "string",
      "description": "Filter by span name (tag mode)."
    },
    "start": {
      "type": "string",
      "description": "RFC3339 start time. Defaults to now-1h."
    },
    "end": {
      "type": "string",
      "description": "RFC3339 end time. Defaults to now."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 1000,
      "description": "Max trace summaries to return (default 50)."
    },
    "min_duration": {
      "type": "string",
      "description": "Minimum trace duration as a Go duration string (e.g. \"100ms\", \"2s\")."
    },
    "max_duration": {
      "type": "string",
      "description": "Maximum trace duration as a Go duration string."
    }
  }
}`)

// QueryTraceQLArgs is the typed form of QueryTraceQLSchema.
type QueryTraceQLArgs struct {
	Query       string `json:"query,omitempty"`
	Service     string `json:"service,omitempty"`
	Operation   string `json:"operation,omitempty"`
	Start       string `json:"start,omitempty"`
	End         string `json:"end,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	MinDuration string `json:"min_duration,omitempty"`
	MaxDuration string `json:"max_duration,omitempty"`
}

// queryTraceqlCallTimeout caps how long a single dispatch may wait.
//
// Unlike the log window, which accepts a relative "now-2h", the trace
// window is strict RFC3339. That is not an oversight: a relative stamp
// means something different at the moment it is resolved than at the
// moment the operator said it, and for an incident window the operator
// is usually reading a time off a dashboard. Accepting "now-15m" here
// would let two calls that a human considers the same window land in
// different ones.
const queryTraceqlCallTimeout = 30 * time.Second

// RunQueryTraceQL is the one implementation of query_traceql. See
// RunQueryPromQL for why the two entry points share a body.
//
// The one filter requirement is the interesting part. An unfiltered Tempo
// search is not a slow query, it is an unbounded one: it walks the whole
// trace store and returns page after page of summaries the model will
// never read, at real cost. So at least one of query / service /
// operation / min_duration / max_duration must be present, and saying so
// costs the model one argument.
func RunQueryTraceQL(ctx context.Context, q toolcore.TraceQuerier, args json.RawMessage) (json.RawMessage, error) {
	if q == nil {
		// Should not happen — when the querier is nil at NewRegistry the
		// tool is never registered. Defensive guard.
		return nil, fmt.Errorf("query_traceql: trace query client not configured")
	}
	var in QueryTraceQLArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("query_traceql: bad args: %w", err)
	}

	// Require *some* filter — either a TraceQL query, a tag (service /
	// operation), or a duration bound. An unfiltered Tempo search is too
	// expensive to dump on the LLM by accident.
	if strings.TrimSpace(in.Query) == "" &&
		strings.TrimSpace(in.Service) == "" &&
		strings.TrimSpace(in.Operation) == "" &&
		strings.TrimSpace(in.MinDuration) == "" &&
		strings.TrimSpace(in.MaxDuration) == "" {
		return nil, fmt.Errorf("query_traceql: at least one of query/service/operation/min_duration/max_duration required")
	}

	end := time.Now()
	start := end.Add(-time.Hour)
	if in.End != "" {
		t, err := time.Parse(time.RFC3339, in.End)
		if err != nil {
			return nil, fmt.Errorf("query_traceql: parse end: %w", err)
		}
		end = t
	}
	if in.Start != "" {
		t, err := time.Parse(time.RFC3339, in.Start)
		if err != nil {
			return nil, fmt.Errorf("query_traceql: parse start: %w", err)
		}
		start = t
	} else if in.End != "" {
		start = end.Add(-time.Hour)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}

	var minDur, maxDur time.Duration
	if in.MinDuration != "" {
		d, err := time.ParseDuration(in.MinDuration)
		if err != nil {
			return nil, fmt.Errorf("query_traceql: parse min_duration: %w", err)
		}
		minDur = d
	}
	if in.MaxDuration != "" {
		d, err := time.ParseDuration(in.MaxDuration)
		if err != nil {
			return nil, fmt.Errorf("query_traceql: parse max_duration: %w", err)
		}
		maxDur = d
	}

	tags := map[string]string{}
	if in.Service != "" {
		tags["service.name"] = in.Service
	}
	if in.Operation != "" {
		tags["name"] = in.Operation
	}
	// Tempo's SearchTraces ignores Tags when Query is set — that's the
	// desired precedence. We forward both and let the client choose.
	if len(tags) == 0 {
		tags = nil
	}

	callCtx, cancel := context.WithTimeout(ctx, queryTraceqlCallTimeout)
	defer cancel()

	res, err := q.SearchTraces(callCtx, tracequery.SearchOptions{
		Query:       in.Query,
		Tags:        tags,
		Limit:       limit,
		Start:       start,
		End:         end,
		MinDuration: minDur,
		MaxDuration: maxDur,
	})
	if err != nil {
		return nil, fmt.Errorf("query_traceql: dispatch: %w", err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("query_traceql: marshal response: %w", err)
	}
	return out, nil
}

// QueryTraceQLTool is the BaseTool form of query_traceql. It delegates to
// RunQueryTraceQL.
type QueryTraceQLTool struct {
	traceQuery toolcore.TraceQuerier
	log        *slog.Logger
}

// NewQueryTraceQLTool builds the BaseTool variant.
func NewQueryTraceQLTool(tq toolcore.TraceQuerier, log *slog.Logger) *QueryTraceQLTool {
	if log == nil {
		log = slog.Default()
	}
	return &QueryTraceQLTool{traceQuery: tq, log: log}
}

// queryTraceQLWhenToUse — reverse-guard against picking trace search for
// log/metric questions.
const queryTraceQLWhenToUse = "When the user wants TRACES — span chains across services, latency outliers, " +
	"specific trace IDs, or 'which call took 5 seconds'. " +
	"NOT for log lines (use query_logql), NOT for metric trends (use query_promql), " +
	"NOT for live host stats (use get_host_load). " +
	"At least one filter (query / service / operation / duration) is required — Tempo unfiltered search is too expensive."

// Info returns metadata. Class=read.
func (t *QueryTraceQLTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameQueryTraceQL,
		Description: QueryTraceQLDescription,
		WhenToUse:   queryTraceQLWhenToUse,
		Parameters:  QueryTraceQLSchema,
		Class:       "read",
	}, nil
}

// InvokableRun runs the TraceQL search.
func (t *QueryTraceQLTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	out, err := RunQueryTraceQL(ctx, t.traceQuery, json.RawMessage(argsJSON))
	if err != nil {
		return "", err
	}
	return string(out), nil
}
