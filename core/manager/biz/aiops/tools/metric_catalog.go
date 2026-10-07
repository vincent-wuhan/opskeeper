// metric_catalog.go — the node-side entry point for list_metric_catalog,
// after the tool moved to the metriccatalog cluster.
//
// Ten lines: build a Runner, call Run. The other clusters' shims are the
// same shape for the same reason — a Registry method is welded to its
// receiver, and this is the path a node-side pig agent reaches through an
// upcall rather than through the BaseTool bag.
package tools

import (
	"context"
	"encoding/json"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/metriccatalog"
)

func (r *Registry) executeListMetricCatalog(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	runner := metriccatalog.Runner{
		PromQuery: r.promQuery,
		Log:       r.log,
	}
	out, err := runner.Run(ctx, args)
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{ResultJSON: out}, nil
}
