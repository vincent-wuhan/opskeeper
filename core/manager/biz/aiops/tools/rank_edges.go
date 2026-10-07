package tools

// This file is the upcall half of rank_edges. The batch BaseTool lives in
// tools/topology, and the metric vocabulary, the argument types and the
// Prometheus decoding are all declared there.
//
// It stays in this package because it is a method on Registry, and
// Registry is the upcall dispatch surface.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/topology"
)

// executeRankEdges builds a topk()/bottomk() PromQL, runs it through the
// PromQuerier, and decorates each row with the edge's friendly name.
func (r *Registry) executeRankEdges(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	if r.promQuery == nil {
		return ExecuteResult{}, fmt.Errorf("rank_edges: prom query client not configured")
	}
	if r.edges == nil {
		return ExecuteResult{}, fmt.Errorf("rank_edges: edge usecase not configured")
	}

	var in topology.RankEdgesArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ExecuteResult{}, fmt.Errorf("rank_edges: bad args: %w", err)
	}
	if in.By == "" {
		return ExecuteResult{}, fmt.Errorf("rank_edges: by required")
	}
	base, label, ok := topology.RankMetricExpr(in.By)
	if !ok {
		return ExecuteResult{}, fmt.Errorf("rank_edges: unsupported by=%q", in.By)
	}
	if in.Limit <= 0 {
		in.Limit = 5
	}
	if in.Limit > 50 {
		in.Limit = 50
	}
	op := "topk"
	switch in.Direction {
	case "", "top":
		op = "topk"
	case "bottom":
		op = "bottomk"
	default:
		return ExecuteResult{}, fmt.Errorf("rank_edges: direction must be top or bottom (got %q)", in.Direction)
	}
	expr := fmt.Sprintf(`%s(%d, %s)`, op, in.Limit, base)

	end := time.Now()
	start := end.Add(-5 * time.Minute)
	step := 30 * time.Second

	callCtx, cancel := context.WithTimeout(ctx, topology.RankEdgesCallTimeout)
	defer cancel()
	res, err := r.promQuery.QueryRange(callCtx, expr, start, end, step)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("rank_edges: dispatch: %w", err)
	}

	rows, err := topology.DecodeRankSeries(res, label)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("rank_edges: decode: %w", err)
	}

	// Decorate with edge name. List with a wide limit and build a small
	// id->name map; cheaper than per-row GetByID for typical fleets.
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
		"results":   rows,
		"metric":    label,
		"by":        in.By,
		"direction": op,
	})
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("rank_edges: marshal: %w", err)
	}
	return ExecuteResult{ResultJSON: out}, nil
}
