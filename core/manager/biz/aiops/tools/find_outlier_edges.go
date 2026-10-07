package tools

// This file is the upcall half of find_outlier_edges. The batch BaseTool
// that the in-process agent loop presents lives in tools/topology; the
// wire name, the schema and the PromQL both come from there, so the two
// halves cannot answer to different tools.
//
// It stays in this package because it is a method on Registry, and
// Registry is the upcall dispatch surface.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/topology"
	"time"
)

// executeFindOutlierEdges builds a z-score PromQL of the shape
//
//	(per_edge_metric - on() group_left avg(per_edge_metric)) /
//	    on() group_left stddev(per_edge_metric)
//
// then filters > sigma. The result is decorated with edge names like
// rank_edges does.
func (r *Registry) executeFindOutlierEdges(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	if r.promQuery == nil {
		return ExecuteResult{}, fmt.Errorf("find_outlier_edges: prom query client not configured")
	}
	if r.edges == nil {
		return ExecuteResult{}, fmt.Errorf("find_outlier_edges: edge usecase not configured")
	}

	var in topology.FindOutlierEdgesArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ExecuteResult{}, fmt.Errorf("find_outlier_edges: bad args: %w", err)
	}
	// Whitelist cpu/mem/disk explicitly. rankMetricExpr also accepts
	// load/composite (for rank_edges), but z-score outlier detection on
	// those is not in scope — fail fast with a clear message instead of
	// relying on rankMetricExpr's ok-bool.
	switch in.Metric {
	case "cpu", "mem", "disk":
		// ok
	default:
		return ExecuteResult{}, fmt.Errorf("find_outlier_edges: metric must be cpu, mem or disk; got %q", in.Metric)
	}
	base, label, _ := topology.RankMetricExpr(in.Metric)
	if in.Sigma <= 0 {
		in.Sigma = 2
	}
	if in.Sigma > 10 {
		in.Sigma = 10
	}

	// `on() group_left` broadcasts the scalar avg/stddev across every
	// per-edge sample. Without it Prom drops the whole vector because
	// label sets don't match.
	expr := fmt.Sprintf(
		`((%s) - on() group_left() avg(%s)) / on() group_left() stddev(%s) > %g`,
		base, base, base, in.Sigma,
	)

	end := time.Now()
	start := end.Add(-5 * time.Minute)
	step := 30 * time.Second

	callCtx, cancel := context.WithTimeout(ctx, topology.OutlierCallTimeout)
	defer cancel()
	res, err := r.promQuery.QueryRange(callCtx, expr, start, end, step)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("find_outlier_edges: dispatch: %w", err)
	}

	rankRows, err := topology.DecodeRankSeries(res, label)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("find_outlier_edges: decode: %w", err)
	}
	rows := make([]topology.OutlierEdgeRow, 0, len(rankRows))
	for _, rr := range rankRows {
		rows = append(rows, topology.OutlierEdgeRow{
			EdgeID: rr.EdgeID,
			ZScore: rr.Value,
			Metric: rr.Metric,
		})
	}

	// Decorate with edge name.
	edges, err := r.edges.ListCatalog(callCtx, domain.EdgeFilter{Limit: 500})
	if err == nil {
		nameByID := make(map[uint64]string, len(edges))
		for _, e := range edges {
			nameByID[e.ID] = e.Name
		}
		for i := range rows {
			if n, ok := nameByID[rows[i].EdgeID]; ok {
				rows[i].EdgeName = n
			}
		}
	}

	out, err := json.Marshal(map[string]any{
		"outliers": rows,
		"sigma":    in.Sigma,
		"metric":   in.Metric,
	})
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("find_outlier_edges: marshal: %w", err)
	}
	return ExecuteResult{ResultJSON: out}, nil
}
