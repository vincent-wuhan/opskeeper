package tools

// metric_catalog_test.go — is list_metric_catalog actually wired in?
//
// The rest of these tests moved to the metriccatalog cluster with the code
// they cover. This one is about the parent: it asks whether the tool
// reaches the schema table and the BaseTool bag, which is a question about
// the assembly and belongs where the assembly is.

import (
	"log/slog"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/metriccatalog"
	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
)

func TestRegistryRegistersMetricCatalogWithProm(t *testing.T) {
	uc := edgebiz.NewUsecase(newFakeEdgeRepo(), nil, nil, slog.Default())
	reg := NewRegistry(&fakeCaller{}, uc, nil, &fakePromQuerier{}, nil, nil, nil, slog.Default())
	if !containsName(schemaNames(reg.Schemas()), metriccatalog.ToolNameListMetricCatalog) {
		t.Fatalf("closure registry missing %q: %v", metriccatalog.ToolNameListMetricCatalog, schemaNames(reg.Schemas()))
	}
	if !containsName(toolInfoNames(t, reg.BuildBaseTools().AllTools()), metriccatalog.ToolNameListMetricCatalog) {
		t.Fatalf("BaseTool registry missing %q", metriccatalog.ToolNameListMetricCatalog)
	}
}
