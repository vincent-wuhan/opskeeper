package toolcore

import (
	"context"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/logquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tracequery"
)

// The three queriers below are how a tool asks the observability stack
// something: Prometheus for numbers, Loki for logs, Tempo for traces.
//
// They are here rather than in query_promql.go, query_logql.go and
// query_traceql.go because far more files ask these backends questions
// than there are tools named after them. rank_edges, find_outlier_edges,
// correlate_incident, the metric catalog, the database analyzer,
// chat_to_query and their tests all hold one. When each interface lived
// next to the tool that happens to be called query_promql, every one of
// those files was reaching sideways into another cluster's file to name a
// type, and none of them could move without dragging the package along.
//
// The concrete *promquery.Client, *logquery.Client and *tracequery.Client
// satisfy these unchanged. The interfaces exist so a test can inject a
// fake without a live backend.
// QueryPromqlCallTimeout caps how long a single PromQL dispatch may wait.
//
// It lives here, beside the querier interface, rather than next to the
// tool that happens to be called query_promql. A timeout is a property of
// the backend a dispatch goes to, and four callers ask that backend
// questions: query_promql, the metric catalog, and the database analyzer.
// Declaring it in any one of them made the other three reach sideways
// into a file named after a tool they are not.
const QueryPromqlCallTimeout = 30 * time.Second

type PromQuerier interface {
	// QueryRange asks for a range of points at the given resolution.
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (*promquery.InstantResult, error)
	// Query is the instant form. correlate_incident uses it to grab a
	// single point-in-time vector for cpu_pct / mem_pct / up.
	Query(ctx context.Context, expr string, ts time.Time) (*promquery.InstantResult, error)
}

// LogQuerier is the narrow surface anything reading logs needs from the
// Loki client. *logquery.Client satisfies it.
type LogQuerier interface {
	QueryRange(ctx context.Context, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error)
}

// TraceQuerier is the narrow surface anything reading traces needs from
// the Tempo client. *tracequery.Client satisfies it.
type TraceQuerier interface {
	SearchTraces(ctx context.Context, opts tracequery.SearchOptions) (*tracequery.SearchResult, error)
}

// MaxQueryPromQLLookbackSeconds is the widest window a PromQL range query
// may cover. A model that asks for two years of history is asking for a
// response nobody can read, and the cost of trying is paid before the
// result is known.
const MaxQueryPromQLLookbackSeconds = 7 * 24 * 3600

// StepFor is the PromQL resolution a lookback window implies. Coarser
// windows get coarser steps on purpose: the point budget of the response
// is what the step controls, and a year of 15-second samples is a
// million points the model will never look at.
func StepFor(lookbackSeconds int) time.Duration {
	switch {
	case lookbackSeconds <= 300:
		return 15 * time.Second
	case lookbackSeconds <= 3600:
		return time.Minute
	case lookbackSeconds <= 6*3600:
		return 5 * time.Minute
	case lookbackSeconds <= 24*3600:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}
