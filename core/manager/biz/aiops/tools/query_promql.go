// query_promql.go — the node-side entry point for query_promql, after the
// tool moved to the querybackend cluster.
//
// Ten lines, same shape as metric_catalog.go: a Registry method is welded to
// its receiver, and this is the path a node-side pig agent reaches through an
// upcall rather than through the BaseTool bag. What could move, and did, is
// everything it called — including the copy of that everything that used to
// sit in query_promql_basetool.go and had to be kept in step by a comment
// saying "mirrors". There is now one body, RunQueryPromQL, and this shim
// only hands it the querier the Registry was built with.
package tools

import (
	"context"
	"encoding/json"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/querybackend"
)

func (r *Registry) executeQueryPromQL(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	out, err := querybackend.RunQueryPromQL(ctx, r.promQuery, args)
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{ResultJSON: out}, nil
}
