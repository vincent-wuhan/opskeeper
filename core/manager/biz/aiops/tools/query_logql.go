// query_logql.go — the node-side entry point for query_logql, after the
// tool moved to the querybackend cluster. See query_promql.go for why the
// method stayed and everything it called did not.
package tools

import (
	"context"
	"encoding/json"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/querybackend"
)

func (r *Registry) executeQueryLogQL(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	out, err := querybackend.RunQueryLogQL(ctx, r.logQuery, args)
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{ResultJSON: out}, nil
}
