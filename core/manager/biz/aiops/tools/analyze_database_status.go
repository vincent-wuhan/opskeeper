// analyze_database_status.go — the node-side entry points for the two
// database tools, after their implementation moved to the database
// cluster.
//
// Two Registry methods stayed behind for the same reason the other five
// clusters' methods did: a method cannot be lifted off its receiver. What
// makes this the shortest such shim in the package is that both runners
// behind these methods are plain structs with exported fields — there was
// no panel mechanism welded to Registry, nothing to unpick. Ten lines each
// and the whole 2683-line implementation belongs to the cluster.
//
// These are not vestigial. The node-side pig agent reaches
// analyze_database_status as an upcall through Registry, not through the
// BaseTool bag, so deleting them would delete a production path.
package tools

import (
	"context"
	"encoding/json"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/database"
)

func (r *Registry) executeListDatabaseSources(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	runner := database.DatabaseSourceInventoryRunner{
		Edges:         r.edges,
		Devices:       r.devices,
		PluginConfigs: r.pluginConfigs,
		Log:           r.log,
	}
	out, err := runner.Run(ctx, args)
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{ResultJSON: out}, nil
}

func (r *Registry) executeAnalyzeDatabaseStatus(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	runner := database.DatabaseStatusRunner{
		PromQuery:     r.promQuery,
		Edges:         r.edges,
		Devices:       r.devices,
		PluginConfigs: r.pluginConfigs,
		Log:           r.log,
	}
	out, err := runner.Run(ctx, args)
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{ResultJSON: out}, nil
}
