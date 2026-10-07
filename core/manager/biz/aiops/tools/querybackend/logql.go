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
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/logquery"
)

// ToolNameQueryLogQL is the stable wire name the LLM sees for the LogQL tool.
const ToolNameQueryLogQL = "query_logql"

// QueryLogQLDescription is the single-sentence description shown to the LLM.
// Phrased to push the model toward this tool whenever raw log inspection
// is needed beyond what the metric-side tools can express.
const QueryLogQLDescription = "Run a LogQL range query against Loki. " +
	"Use this to investigate log patterns, error counts, or pipe into per-edge filters. " +
	"Returns the raw Loki response (streams or matrix)."

// QueryLogQLSchema is the JSON Schema of the tool's argument object.
var QueryLogQLSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "LogQL expression. Example: \"{edge_id=\\\"1\\\"} |= \\\"error\\\"\"."
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
      "maximum": 5000,
      "description": "Max number of result rows (default 200)."
    },
    "direction": {
      "type": "string",
      "enum": ["backward", "forward"],
      "description": "Order of results, default \"backward\" (newest first)."
    }
  },
  "required": ["query"]
}`)

// QueryLogQLArgs is the typed form of QueryLogQLSchema.
type QueryLogQLArgs struct {
	Query     string `json:"query"`
	Start     string `json:"start,omitempty"`
	End       string `json:"end,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Direction string `json:"direction,omitempty"`
}

// queryLogqlCallTimeout caps how long a single dispatch may wait. It sits
// next to the tool rather than in toolcore because, unlike
// toolcore.QueryPromqlCallTimeout, only this one tool dispatches to Loki —
// there is no fourth caller reaching sideways for it.
//
// It is not the Prometheus timeout reused under another name. Loki is the
// backend that accepts an unbounded line count, and a 30s ceiling is what
// keeps a wide regex from holding an agent turn open past the point where
// the model would have given up anyway.
const queryLogqlCallTimeout = 30 * time.Second

// parseLogQLTime accepts what the log tool's own schema asks for: an
// RFC3339 stamp, the word "now", or a relative "now-2h" / "now+15m". Loki
// users write the relative form constantly and a tool that only understood
// RFC3339 would push all of them through a model round trip.
func parseLogQLTime(value string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "now") {
		return fallback, nil
	}
	if rest, ok := strings.CutPrefix(strings.ToLower(value), "now-"); ok {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return time.Time{}, err
		}
		return time.Now().Add(-d), nil
	}
	if rest, ok := strings.CutPrefix(strings.ToLower(value), "now+"); ok {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return time.Time{}, err
		}
		return time.Now().Add(d), nil
	}
	return time.Parse(time.RFC3339, value)
}

// RunQueryLogQL is the one implementation of query_logql. See
// RunQueryPromQL for why the two entry points share a body rather than
// keeping two copies in step.
//
// The window defaults to the last hour, and a caller who pins only "end"
// still gets a bounded one-hour window relative to that end rather than
// the default window measured from now — an end-pinned query with no start
// is almost always "up to that moment", and answering it with the wrong
// hour is worse than refusing.
func RunQueryLogQL(ctx context.Context, q toolcore.LogQuerier, args json.RawMessage) (json.RawMessage, error) {
	if q == nil {
		// Should not happen — when the querier is nil at NewRegistry the
		// tool is never registered. Defensive guard.
		return nil, fmt.Errorf("query_logql: log query client not configured")
	}
	var in QueryLogQLArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("query_logql: bad args: %w", err)
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, fmt.Errorf("query_logql: query required")
	}

	end := time.Now()
	start := end.Add(-time.Hour)
	if in.End != "" {
		t, err := parseLogQLTime(in.End, end)
		if err != nil {
			return nil, fmt.Errorf("query_logql: parse end: %w", err)
		}
		end = t
	}
	if in.Start != "" {
		t, err := parseLogQLTime(in.Start, start)
		if err != nil {
			return nil, fmt.Errorf("query_logql: parse start: %w", err)
		}
		start = t
	} else if in.End != "" {
		// User pinned end but not start — keep the 1h window relative to
		// the supplied end so the call is still bounded.
		start = end.Add(-time.Hour)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 200
	}
	direction := in.Direction
	if direction == "" {
		direction = "backward"
	}

	callCtx, cancel := context.WithTimeout(ctx, queryLogqlCallTimeout)
	defer cancel()

	res, err := q.QueryRange(callCtx, logquery.QueryRangeOptions{
		Query:     in.Query,
		Start:     start,
		End:       end,
		Limit:     limit,
		Direction: direction,
	})
	if err != nil {
		return nil, fmt.Errorf("query_logql: dispatch: %w", err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("query_logql: marshal response: %w", err)
	}
	return out, nil
}

// QueryLogQLTool is the BaseTool form of query_logql. It delegates to
// RunQueryLogQL.
type QueryLogQLTool struct {
	logQuery toolcore.LogQuerier
	log      *slog.Logger
}

// NewQueryLogQLTool builds the BaseTool variant.
func NewQueryLogQLTool(lq toolcore.LogQuerier, log *slog.Logger) *QueryLogQLTool {
	if log == nil {
		log = slog.Default()
	}
	return &QueryLogQLTool{logQuery: lq, log: log}
}

// queryLogQLWhenToUse — the canonical reverse-guard for log search.
// Explicit "do NOT use for ..." steers the model away from
// the metric / trace tools when the question is really about log content.
const queryLogQLWhenToUse = "When the user asks about log CONTENT — grep error / panic / fatal, see the line text " +
	"that explains why a service failed, or count log volume over time. " +
	"NOT for filesystem state, file names or file sizes (use a host_files skill). " +
	"NOT for metric trends like cpu/mem (use query_promql). " +
	"NOT for traces / span timelines (use query_traceql)."

// Info returns metadata. Class=read.
func (t *QueryLogQLTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameQueryLogQL,
		Description: QueryLogQLDescription,
		WhenToUse:   queryLogQLWhenToUse,
		Parameters:  QueryLogQLSchema,
		Class:       "read",
	}, nil
}

// InvokableRun runs the LogQL range query.
func (t *QueryLogQLTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	out, err := RunQueryLogQL(ctx, t.logQuery, json.RawMessage(argsJSON))
	if err != nil {
		return "", err
	}
	return string(out), nil
}
