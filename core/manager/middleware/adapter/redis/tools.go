// tools.go adapts the Diagnose category functions to the tool-handler
// signature.
//
// The two differ by more than a type: a category is asked for by name with a
// parameter bag, while a tool is called with a flat argument map. Keeping
// the diagnostic bodies single and wrapping them here means the console's
// Diagnose call and the agent's tool call cannot drift into two
// implementations of the same query.
package redis

import (
	"context"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
)

func runCategory(name string) func(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return func(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
		route, ok := diagnoseRoutes[name]
		if !ok {
			return nil, "", fmt.Errorf("redis: no diagnose category %q", name)
		}
		return route.run(ctx, a, adapter.DiagnoseQuery{Category: name, Params: args})
	}
}

func runInfo(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catServerInfo)(ctx, a, args)
}

func runDBSize(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	n, err := client.DBSize(ctx).Result()
	if err != nil {
		return nil, "", fmt.Errorf("redis: DBSIZE: %w", err)
	}
	return []map[string]any{{"keys": n}}, "", nil
}

func runClusterInfo(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catCluster)(ctx, a, args)
}

func runConfigGet(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catConfig)(ctx, a, args)
}

func runBigKeys(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catBigKeys)(ctx, a, args)
}

func runHotKeys(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catHotKeys)(ctx, a, args)
}

func runSlowLog(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catSlowLog)(ctx, a, args)
}

func runKeyspace(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catKeyspace)(ctx, a, args)
}

func runMemory(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catMemory)(ctx, a, args)
}

func runFragmentation(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catFragmentation)(ctx, a, args)
}

func runClients(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catClients)(ctx, a, args)
}

func runBlocked(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error) {
	return runCategory(catBlocked)(ctx, a, args)
}
