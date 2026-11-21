package tools

import (
	"context"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/host"
	"log/slog"
	"testing"
	"time"

	audit "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/chat2query"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/configchange"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/recovery"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/topology"
	devicebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/device"
	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
)

func TestBuildBaseTools_NilGatingMatchesNewRegistry(t *testing.T) {
	uc := edgebiz.NewUsecase(newFakeEdgeRepo(), nil, nil, slog.Default())

	// Minimal — only edges + caller. NewRegistry registers exactly:
	// get_host_load, get_process_list, query_devices, get_topology,
	// get_edge_summary → 5 tools.
	regMin := NewRegistry(&fakeCaller{}, uc, nil, nil, nil, nil, nil, slog.Default())
	closureNames := schemaNames(regMin.Schemas())

	bag := regMin.BuildBaseTools()
	baseNames := toolInfoNames(t, bag.AllTools())

	for _, n := range closureNames {
		if !containsName(baseNames, n) {
			t.Errorf("BuildBaseTools is missing closure-side tool %q (closure=%v base=%v)", n, closureNames, baseNames)
		}
	}
}

func TestBuildBaseTools_FullSetWithAllDeps(t *testing.T) {
	uc := edgebiz.NewUsecase(newFakeEdgeRepo(), nil, nil, slog.Default())
	pq := &fakePromQuerier{}
	lq := &fakeLogQuerier{}
	tq := &fakeTraceQuerier{}
	au := &fakeAlertUC{}

	reg := NewRegistry(&fakeCaller{}, uc, nil, pq, lq, tq, au, slog.Default())
	reg.SetPluginConfigLister(fakePluginConfigLister{})
	closureNames := schemaNames(reg.Schemas())

	bag := reg.BuildBaseTools()
	// Filter out ToolSearch — it's registered unconditionally by
	// BuildBaseTools as an extras-slot tool and has no
	// counterpart in the closure-style Schemas() output.
	baseNames := filterOutTool(toolInfoNames(t, bag.AllTools()), ToolSearchToolName)

	for _, n := range closureNames {
		if !containsName(baseNames, n) {
			t.Errorf("BuildBaseTools missing %q (closure=%v base=%v)", n, closureNames, baseNames)
		}
	}
	for _, n := range baseNames {
		if !containsName(closureNames, n) {
			t.Errorf("BuildBaseTools has extra %q not in closure path (base=%v closure=%v)", n, baseNames, closureNames)
		}
	}
}

// TestBuildBaseTools_GraphKernelToolBagCount mirrors the production
// wiring path that OPSKEEPER_AGENT_KERNEL=graph follows in main.go:
//
//  1. NewRegistry with all four signal sources wired (mirrors a
//     fully-configured Prom + Loki + Tempo + alert deployment).
//  2. BuildBaseTools to materialise the BaseTool slice.
//  3. AppendHostFilesTools to add find_large_files / du_summary /
//     stat_file (PR-8). devices is required for host_files; we
//     construct a minimal usecase with nil repo so the helper's
//     non-nil check passes.
//
// The point of the test is twofold: it gates regression on the
// total tool count when adding/removing tools AND it gives the
// PR-9 reviewer a visible Logf line so they can see exactly what
// the LLM is exposed to in production.
func TestBuildBaseTools_GraphKernelToolBagCount(t *testing.T) {
	uc := edgebiz.NewUsecase(newFakeEdgeRepo(), nil, nil, slog.Default())
	dc := &fakeDevicesForToolBag{}
	pq := &fakePromQuerier{}
	lq := &fakeLogQuerier{}
	tq := &fakeTraceQuerier{}
	au := &fakeAlertUC{}

	reg := NewRegistry(&fakeCaller{}, uc, dc.usecase(), pq, lq, tq, au, slog.Default())
	reg.SetPluginConfigLister(fakePluginConfigLister{})
	bag := reg.BuildBaseTools()
	bag = host.AppendHostFilesTools(bag, &fakeCaller{}, uc, dc.usecase(), slog.Default())

	names := toolInfoNames(t, bag.AllTools())
	t.Logf("graph kernel toolBag (count=%d): %v", len(names), names)

	// Sanity: host_files trio MUST be present once AppendHostFilesTools
	// has been called with non-nil deps.
	for _, want := range []string{host.ToolNameFindLargeFiles, host.ToolNameDuSummary, host.ToolNameStatFile} {
		if !containsName(names, want) {
			t.Errorf("toolBag missing %q after host.AppendHostFilesTools", want)
		}
	}
}

// fakeDevicesForToolBag stands in for a *devicebiz.Usecase so
// AppendHostFilesTools' non-nil check passes. The host_files tool
// only needs a non-nil reference at construction; the actual
// resolver path is exercised by host_files_basetool_test.go.
type fakeDevicesForToolBag struct{}

func (fakeDevicesForToolBag) usecase() *devicebiz.Usecase {
	// devicebiz.NewUsecase accepts (Repo, EdgeDeviceRepo, Logger).
	// Passing nil triplet is fine for AppendHostFilesTools'
	// non-nil-pointer check; the resolver will return 0 for any
	// lookup but we never invoke a tool here.
	return devicebiz.NewUsecase(nil, nil, slog.Default())
}

func toolInfoNames(t *testing.T, tools []basetool.BaseTool) []string {
	t.Helper()
	out := make([]string, 0, len(tools))
	for _, x := range tools {
		info, err := x.Info(context.Background())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		out = append(out, info.Name)
	}
	return out
}

// filterOutTool removes a single name from the slice, preserving order.
// Used by tests that compare BuildBaseTools' output against the closure
// path — the new ToolSearch entry has no closure-side counterpart so we
// strip it before the diff.
func filterOutTool(names []string, drop string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}

// TestBaseToolCandidateNamesCoverEveryUnconditionalBuild is the drift
// check on baseToolCandidateNames.
//
// The list is what KnownToolNames hands the node upcall bridge, and the
// bridge uses it to tell "this product has no such tool" from "this
// deployment did not wire its dependency". A name BuildBaseTools can
// produce but the list omits would make a nil-gated absence read as
// "no such tool" — exactly the mis-phrasing the list exists to prevent.
// A name the list carries but nothing produces is a stale entry claiming
// a capability the product does not have.
//
// The check is run with every optional dependency wired, because that is
// the construction that produces the largest bag: anything the bag can
// ever hold has to be on the list. The nil-gated direction is not
// separately asserted here — the list is a vocabulary, and a vocabulary
// entry whose tool a deployment cannot serve is the *normal* case the
// bridge handles.
func TestBaseToolCandidateNamesCoverEveryUnconditionalBuild(t *testing.T) {
	uc := edgebiz.NewUsecase(newFakeEdgeRepo(), nil, nil, slog.Default())
	dc := &fakeDevicesForToolBag{}

	reg := NewRegistry(&fakeCaller{}, uc, dc.usecase(), &fakePromQuerier{}, &fakeLogQuerier{}, &fakeTraceQuerier{}, &fakeAlertUC{}, slog.Default())
	reg.SetPluginConfigLister(fakePluginConfigLister{})
	reg.SetKnowledgeSearcher(codeKnowledge{})
	reg.SetAuditLister(fakeAuditListerForCandidates{})
	reg.SetEdgeChangeLister(fakeAuditListerForCandidates{})
	reg.SetTopologyGraph(fakeTopologyGraphForCandidates{})
	reg.SetConfigManager(fakeConfigManagerForCandidates{})
	bag := reg.BuildBaseTools()

	candidates := map[string]bool{}
	for _, n := range baseToolCandidateNames {
		if candidates[n] {
			t.Errorf("baseToolCandidateNames lists %q twice", n)
		}
		candidates[n] = true
	}
	for _, tool := range bag.AllTools() {
		info, err := tool.Info(context.Background())
		if err != nil || info == nil {
			t.Fatalf("Info failed while enumerating the bag: %v", err)
		}
		if !candidates[info.Name] {
			t.Errorf("BuildBaseTools produces %q but baseToolCandidateNames does not list it; "+
				"a deployment missing its dependency would hear \"no such tool\" for a tool this product has",
				info.Name)
		}
	}
	// The coordinator trio, the mutating repair tools, the shell and the
	// notification tools are all gated on setters this construction does
	// not call (spawner, recovery audit, proposers, IM sender, page
	// store), so their names could silently drop off the list without the
	// loop above noticing. They are pinned directly, by their constants,
	// so removing one is a build error here rather than a phrasing bug in
	// a later slice.
	for _, n := range []string{
		AgentToolName, SendMessageToolName, TaskStopToolName,
		ToolNameRestartService, recovery.ToolNameRecoveryExecute,
		ToolNameBash, ToolNameCloudBash, ToolNameSendIMMessage, ToolNameServePage,
		chat2query.ToolNameChatToQuery,
	} {
		if !candidates[n] {
			t.Errorf("baseToolCandidateNames does not list %q; it is produced only under setters this test does not wire, so nothing else would catch its absence", n)
		}
	}
}

// fakeAuditListerForCandidates satisfies both alerting.AuditLister and
// alerting.EdgeChangeLister for the candidate-coverage test above. Both
// return empty and neither is called — the test only needs the setters to
// be non-nil so query_change_events enters the bag.
type fakeAuditListerForCandidates struct{}

func (fakeAuditListerForCandidates) ListChanges(context.Context, time.Time, time.Time, string, string, int) ([]audit.ChangeRow, error) {
	return nil, nil
}

func (fakeAuditListerForCandidates) ListChangeWindow(context.Context, time.Time, time.Time, string, int) ([]domain.ChangeEvent, error) {
	return nil, nil
}

// fakeTopologyGraphForCandidates satisfies topology.Graph.
type fakeTopologyGraphForCandidates struct{}

func (fakeTopologyGraphForCandidates) GetNode(context.Context, uint64) (*topology.Node, error) { return nil, nil }
func (fakeTopologyGraphForCandidates) ListNodes(context.Context, topology.NodeListFilter) ([]*topology.Node, int64, error) {
	return nil, 0, nil
}
func (fakeTopologyGraphForCandidates) ListRelations(context.Context, topology.RelationListFilter) ([]*topology.Relation, int64, error) {
	return nil, 0, nil
}
func (fakeTopologyGraphForCandidates) ListRelationTypes(context.Context) ([]*topology.RelationType, error) {
	return nil, nil
}

// fakeConfigManagerForCandidates satisfies configchange.ConfigManager.
type fakeConfigManagerForCandidates struct{}

func (fakeConfigManagerForCandidates) DraftAlertRuleConfig(context.Context, configchange.ConfigCaller, configchange.AlertRuleConfigArgs) (*configchange.ConfigDraft, error) {
	return nil, nil
}

func (fakeConfigManagerForCandidates) ApplyAlertRuleConfig(context.Context, configchange.ConfigCaller, configchange.AlertRuleApplyArgs) (*configchange.ConfigApplyResult, error) {
	return nil, nil
}
