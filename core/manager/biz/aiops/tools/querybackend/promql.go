package querybackend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/toolcore"
)

// ToolNameQueryPromQL is the stable wire name the LLM sees for the PromQL tool.
const ToolNameQueryPromQL = "query_promql"

// QueryPromQLDescription is the single-sentence description shown to the LLM.
// Phrased to push the model toward this tool whenever the host-load /
// process-list tools are too narrow.
const QueryPromQLDescription = "Run a PromQL range query against the cluster's Prometheus. " +
	"Use this when you need any host or container metric beyond the few host-level fields the basic tools return. " +
	"For fleet or multi-device questions, write one vectorized PromQL expression with by(device_id, ...) / regex selectors / topk instead of one query per device or metric. " +
	"Returns the raw Prom HTTP API response."

// QueryPromQLSchema is the JSON Schema of the tool's argument object.
var QueryPromQLSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "expr": {
      "type": "string",
      "description": "PromQL expression. Prefer one vectorized expression for multiple devices/labels. Example: \"avg by (device_id) (rate(node_cpu_seconds_total{mode!=\\\"idle\\\"}[5m]))\". For filesystem percent, combine numerator/denominator in one expression instead of separate used/size queries."
    },
    "lookback_seconds": {
      "type": "integer",
      "minimum": 60,
      "maximum": 604800,
      "description": "How far back to query in seconds (default 300 = 5 minutes; max 604800 = 7d). Use one 7d range query for weekly trends instead of repeating short lookbacks."
    }
  },
  "required": ["expr"]
}`)

// QueryPromQLArgs is the typed form of QueryPromQLSchema.
type QueryPromQLArgs struct {
	Expr            string `json:"expr"`
	LookbackSeconds int    `json:"lookback_seconds,omitempty"`
}

// RunQueryPromQL is the one implementation of query_promql.
//
// Both entry points call it: the node-side Registry shim in the tools package
// and the QueryPromQLTool below. The lookback default, the seven-day clamp,
// the step choice, the per-dispatch timeout, the nil-querier guard and the
// error strings all live here precisely so the two paths cannot disagree —
// they used to be separate copies kept in step by a comment.
//
// The window is deliberately edge-agnostic: DeviceID is never set, because
// this tool is not bound to a specific edge. Callers that need an audit row
// keyed to an edge get one from the decorator chain, not from here.
func RunQueryPromQL(ctx context.Context, q toolcore.PromQuerier, args json.RawMessage) (json.RawMessage, error) {
	if q == nil {
		// Should not happen — when the querier is nil at NewRegistry the
		// tool is never registered. Defensive guard.
		return nil, fmt.Errorf("query_promql: prom query client not configured")
	}
	var in QueryPromQLArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("query_promql: bad args: %w", err)
	}
	if in.Expr == "" {
		return nil, fmt.Errorf("query_promql: expr required")
	}
	if in.LookbackSeconds <= 0 {
		in.LookbackSeconds = 300 // 5 min
	}
	if in.LookbackSeconds > toolcore.MaxQueryPromQLLookbackSeconds {
		in.LookbackSeconds = toolcore.MaxQueryPromQLLookbackSeconds
	}

	end := time.Now()
	start := end.Add(-time.Duration(in.LookbackSeconds) * time.Second)
	step := toolcore.StepFor(in.LookbackSeconds)

	callCtx, cancel := context.WithTimeout(ctx, toolcore.QueryPromqlCallTimeout)
	defer cancel()

	res, err := q.QueryRange(callCtx, in.Expr, start, end, step)
	if err != nil {
		return nil, fmt.Errorf("query_promql: dispatch: %w", err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("query_promql: marshal response: %w", err)
	}
	return out, nil
}

// QueryPromQLTool is the BaseTool form of query_promql. It holds the
// querier on the struct rather than capturing it in a closure, and
// delegates to RunQueryPromQL.
type QueryPromQLTool struct {
	promQuery toolcore.PromQuerier
	log       *slog.Logger
}

// NewQueryPromQLTool builds a new BaseTool-shape query_promql tool. log
// may be nil (the tool degrades to slog.Default()).
func NewQueryPromQLTool(p toolcore.PromQuerier, log *slog.Logger) *QueryPromQLTool {
	if log == nil {
		log = slog.Default()
	}
	return &QueryPromQLTool{promQuery: p, log: log}
}

// queryPromQLWhenToUse is the routing hint shown to the LLM under a
// "When to use" header in the system prompt. It is kept distinct from
// the Description field so skill manifests can override one without
// rewriting the other.
const queryPromQLWhenToUse = "When the user asks about metric values, time-series trends, " +
	"per-edge resource usage, or anything that boils down to a Prometheus range query. " +
	"NOT for log content (use query_logql) or filesystem state (use host-level tools). " +
	"For fleet / multi-device / multi-mountpoint questions, prefer one PromQL call with by(device_id, ...) " +
	"or topk/ranking over repeated per-device queries. " +
	"Prefer query_promql over the narrower get_host_load / get_process_list when the " +
	"question spans more than one host or asks for derivatives / aggregates."

// Info returns the tool metadata. Info is pure (no I/O).
// The Class field marks this as "read" — query_promql never mutates
// state, so it is exempt from any future destructive-action gating.
func (t *QueryPromQLTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameQueryPromQL,
		Description: QueryPromQLDescription,
		WhenToUse:   queryPromQLWhenToUse,
		Parameters:  QueryPromQLSchema,
		Class:       "read",
	}, nil
}

// InvokableRun parses argsJSON, runs the PromQL range query, and returns
// the marshalled response as a string.
//
// opts are accepted but ignored: query_promql is tenant-agnostic and not
// edge-scoped (DeviceID stays nil in the audit row). The decorator chain
// still consumes them upstream (tenant_bind for ratelimit/audit keying).
func (t *QueryPromQLTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	out, err := RunQueryPromQL(ctx, t.promQuery, json.RawMessage(argsJSON))
	if err != nil {
		return "", err
	}
	return string(out), nil
}
