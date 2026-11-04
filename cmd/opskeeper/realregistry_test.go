package main

// The registry a deployment serves, built by a test.
//
// toolRegistryWiring exists so that "what the binary hands the model" is
// something a test can produce rather than something a test has to
// re-guess. This file is the fixture behind that: it fills every field of
// the wiring struct with a fake that satisfies the port, calls the same
// buildRegistry/apply pair main() calls, and hands back the registry and
// the bag it produced.
//
// There is one of these on purpose. Two wirings would mean two answers to
// "is this tool in the bag", and the one that is checked is the one nobody
// runs. The roadmap delivery check and the plugin gap gate both read the
// registry this file builds.

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/notify"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	manageraiopstools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	managerbizaiopstoolsdatabase "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/database"
	managerbizaiopstoolstopology "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/topology"
	managerbizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	devicebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/device"
	managerbizknowledge "github.com/vincent-wuhan/opskeeper/core/manager/biz/knowledge"
	manageralertdata "github.com/vincent-wuhan/opskeeper/core/manager/data/alert/store"
	managerapprovaldata "github.com/vincent-wuhan/opskeeper/core/manager/data/approval/store"
)

// buildTheRealToolRegistry wires the manager's tool registry the way main()
// does and returns it together with the BaseTool bag it produces.
//
// Both halves are needed by the checks that use this: the bag is the tool
// list a deployment hands the model, and the registry is the closure form
// of the same capabilities — the two surfaces the node's upcall channel
// dispatches across.
func buildTheRealToolRegistry(t *testing.T) (*manageraiopstools.Registry, *manageraiopstools.ToolBag) {
	t.Helper()
	db := throwawayDB(t)
	log := newTestLogger(t)
	approval := managerbizapproval.NewUsecase(managerapprovaldata.NewRepo(db), log)
	channels := manageralertdata.NewRepo(db)
	router := notify.NewFromConfig(config.NotificationConfig{}, log)

	wiring := toolRegistryWiring{
		Caller:  stubCaller{},
		Edges:   stubEdgeCatalog{},
		Devices: &devicebiz.Usecase{},
		Prom:    stubPromQuerier{},
		Logs:    stubLogQuerier{},
		Trace:   stubTraceQuerier{},
		Alerts:  stubAlertUsecase{},
		Log:     log,
	}
	reg := wiring.buildRegistry()
	wiring.Approval = approval
	wiring.Channels = channels
	wiring.Router = router
	wiring.DB = db
	wiring.LLM = stubCompleter{}
	wiring.PagesDir = t.TempDir()
	wiring.AuditLister = stubAuditLister{}
	wiring.EdgeChanges = stubEdgeChangeLister{}
	wiring.ConfigManager = stubConfigManager{}
	// The three seams below are the ones the node's upcall channel depends
	// on and the closure registry does not carry: the plugin-config
	// inventory (list_database_sources / analyze_database_status), the
	// business topology graph (expand_topology / find_topology_node) and
	// the code browser (list_repo_sources / read_source / grep_source).
	//
	// They are wired here because a deployment wires them: leaving them
	// out would produce a registry that is missing exactly the tools the
	// gap gate is about, and the gate would then be measuring the fixture
	// rather than the product.
	wiring.PluginConfig = stubPluginConfigLister{}
	wiring.TopologyGraph = stubTopologyGraph{}
	wiring.Knowledge = stubCodeBrowser{}
	wiring.apply(reg)
	return reg, reg.BuildBaseTools()
}

// stubPluginConfigLister satisfies database.PluginConfigLister — the seam
// the database inventory tools read their source list from.
type stubPluginConfigLister struct{}

func (stubPluginConfigLister) ListForUI(context.Context, uint64) ([]domain.PluginRow, error) {
	return nil, nil
}

// stubCodeBrowser satisfies both tools.KnowledgeSearcher and
// tools.CodeBrowser — the two ports the knowledge usecase serves at once.
//
// It has to satisfy both, and not just one, because BuildBaseTools
// registers the knowledge tool behind the first and the three code tools
// behind a type assertion to the second. A fake that satisfied only the
// first would silently drop list_repo_sources / read_source / grep_source
// from the bag, and the gap gate would report them as unserved for a
// reason that has nothing to do with the product.
type stubCodeBrowser struct{}

func (stubCodeBrowser) Search(context.Context, string, managerbizknowledge.SearchOptions) ([]managerbizknowledge.SearchHit, error) {
	return nil, nil
}

func (stubCodeBrowser) ListRepoSources(context.Context, string, string) (*managerbizknowledge.RepoSourceListing, error) {
	return &managerbizknowledge.RepoSourceListing{Entries: []managerbizknowledge.RepoSourceEntry{{
		Path:  "core/manager",
		IsDir: true,
	}}}, nil
}

func (stubCodeBrowser) ReadSource(context.Context, string, string, int, int) (*managerbizknowledge.SourceFile, error) {
	return &managerbizknowledge.SourceFile{Content: "package main\n"}, nil
}

func (stubCodeBrowser) GrepSource(context.Context, string, string, string, int) (*managerbizknowledge.GrepResult, error) {
	return &managerbizknowledge.GrepResult{Hits: []managerbizknowledge.GrepHit{{
		Path: "core/manager/main.go",
		Line: 42,
		Text: "func ResolveEdgeID()",
	}}}, nil
}

// stubTopologyGraph satisfies topology.Graph with a two-node graph, so that
// expand_topology has something to walk and find_topology_node has
// something to find.
//
// The graph is a shape rather than an empty list on purpose: an empty graph
// would let a tool that returns "there is nothing here" pass a check that
// only asks whether the call came back.
type stubTopologyGraph struct{}

func (stubTopologyGraph) GetNode(_ context.Context, id uint64) (*managerbizaiopstoolstopology.Node, error) {
	switch id {
	case 1:
		return &managerbizaiopstoolstopology.Node{ID: 1, Type: "service", Name: "checkout"}, nil
	case 2:
		return &managerbizaiopstoolstopology.Node{ID: 2, Type: "service", Name: "payment"}, nil
	default:
		return nil, nil
	}
}

func (stubTopologyGraph) ListNodes(_ context.Context, f managerbizaiopstoolstopology.NodeListFilter) ([]*managerbizaiopstoolstopology.Node, int64, error) {
	nodes := []*managerbizaiopstoolstopology.Node{
		{ID: 1, Type: "service", Name: "checkout"},
		{ID: 2, Type: "service", Name: "payment"},
	}
	if f.Type != "" {
		kept := nodes[:0]
		for _, n := range nodes {
			if n.Type == f.Type {
				kept = append(kept, n)
			}
		}
		nodes = kept
	}
	if f.Q != "" {
		kept := nodes[:0]
		for _, n := range nodes {
			if strings.Contains(n.Name, f.Q) {
				kept = append(kept, n)
			}
		}
		nodes = kept
	}
	return nodes, int64(len(nodes)), nil
}

func (stubTopologyGraph) ListRelations(context.Context, managerbizaiopstoolstopology.RelationListFilter) ([]*managerbizaiopstoolstopology.Relation, int64, error) {
	relations := []*managerbizaiopstoolstopology.Relation{
		{SrcID: 1, DstID: 2, Type: "depends_on"},
	}
	return relations, int64(len(relations)), nil
}

func (stubTopologyGraph) ListRelationTypes(context.Context) ([]*managerbizaiopstoolstopology.RelationType, error) {
	return []*managerbizaiopstoolstopology.RelationType{{
		Name:              "depends_on",
		SemanticsTag:      "runtime_dep",
		PropagatesFailure: true,
	}}, nil
}

// compile-time assertions: a fake that quietly stops satisfying a port
// would take the tool out of the bag, and the failure would read as a
// product gap rather than as a broken fake.
var (
	_ managerbizaiopstoolsdatabase.PluginConfigLister = stubPluginConfigLister{}
	_ manageraiopstools.KnowledgeSearcher              = stubCodeBrowser{}
	_ manageraiopstools.CodeBrowser                    = stubCodeBrowser{}
	_ managerbizaiopstoolstopology.Graph               = stubTopologyGraph{}
)
