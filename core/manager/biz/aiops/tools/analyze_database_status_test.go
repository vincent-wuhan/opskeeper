package tools

import (
	"context"
	"encoding/json"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/database"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
)

type fakePluginConfigLister struct {
	rows map[uint64][]edgebiz.PluginRow
	err  error
}

func (f fakePluginConfigLister) ListForUI(_ context.Context, edgeID uint64) ([]edgebiz.PluginRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[edgeID], nil
}

type fakeDatabaseProm struct {
	mu      sync.Mutex
	results map[string]*promquery.InstantResult
	errs    map[string]error
	exprs   []string
}

func (f *fakeDatabaseProm) Query(_ context.Context, expr string, _ time.Time) (*promquery.InstantResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exprs = append(f.exprs, expr)
	if err := f.errs[expr]; err != nil {
		return nil, err
	}
	if res := f.results[expr]; res != nil {
		return res, nil
	}
	return promVector(), nil
}

func (f *fakeDatabaseProm) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) (*promquery.InstantResult, error) {
	return f.Query(context.Background(), expr, end)
}

func (f *fakeDatabaseProm) saw(part string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, expr := range f.exprs {
		if strings.Contains(expr, part) {
			return true
		}
	}
	return false
}

type promTestValue struct {
	metric map[string]string
	value  string
}

func promVector(items ...promTestValue) *promquery.InstantResult {
	rows := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		rows = append(rows, map[string]interface{}{
			"metric": item.metric,
			"value":  []interface{}{float64(1700000000), item.value},
		})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		panic(err)
	}
	return &promquery.InstantResult{ResultType: "vector", Result: raw}
}

func TestAnalyzeDatabaseStatus_RegisteredWhenPromEdgesAndPluginConfigsPresent(t *testing.T) {
	deviceID := uint64(42)
	uc := edgebiz.NewUsecase(newFakeEdgeRepo(&edgemodel.Edge{ID: 1, Name: "node-a", DeviceID: &deviceID}), nil, nil, slog.Default())
	reg := NewRegistry(&fakeCaller{}, uc, nil, &fakeDatabaseProm{}, nil, nil, nil, slog.Default())

	if containsName(schemaNames(reg.Schemas()), database.ToolNameAnalyzeDatabaseStatus) {
		t.Fatalf("%s should not register before plugin config lister is wired", database.ToolNameAnalyzeDatabaseStatus)
	}
	if containsToolName(t, reg.BuildBaseTools().AllTools(), database.ToolNameAnalyzeDatabaseStatus) {
		t.Fatalf("%s should not be in BaseTool bag before plugin config lister is wired", database.ToolNameAnalyzeDatabaseStatus)
	}

	reg.SetPluginConfigLister(fakePluginConfigLister{rows: map[uint64][]edgebiz.PluginRow{}})
	if !containsName(schemaNames(reg.Schemas()), database.ToolNameAnalyzeDatabaseStatus) {
		t.Fatalf("%s not registered: %v", database.ToolNameAnalyzeDatabaseStatus, schemaNames(reg.Schemas()))
	}
	if !containsToolName(t, reg.BuildBaseTools().AllTools(), database.ToolNameAnalyzeDatabaseStatus) {
		t.Fatalf("%s missing from BaseTool bag", database.ToolNameAnalyzeDatabaseStatus)
	}
}

func TestListDatabaseSources_RegisteredWhenEdgesAndPluginConfigsPresent(t *testing.T) {
	deviceID := uint64(9)
	uc := edgebiz.NewUsecase(newFakeEdgeRepo(&edgemodel.Edge{ID: 3, Name: "node-c", DeviceID: &deviceID}), nil, nil, slog.Default())
	reg := NewRegistry(&fakeCaller{}, uc, nil, nil, nil, nil, nil, slog.Default())
	reg.SetPluginConfigLister(fakePluginConfigLister{rows: map[uint64][]edgebiz.PluginRow{}})

	if !containsName(schemaNames(reg.Schemas()), database.ToolNameListDatabaseSources) {
		t.Fatalf("%s not registered: %v", database.ToolNameListDatabaseSources, schemaNames(reg.Schemas()))
	}
	if !containsToolName(t, reg.BuildBaseTools().AllTools(), database.ToolNameListDatabaseSources) {
		t.Fatalf("%s missing from BaseTool bag", database.ToolNameListDatabaseSources)
	}
}
