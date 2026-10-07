package main

// The gap decision 350 found, turned into a check.
//
// Five gates in this repository ask "is the tool present", "is the route
// audited", "is the method reachable", "is the package pinned", "is the
// mutating tool declared" — every one of them a correct question, and their
// union still not covering "is the thing ROADMAP.md says we shipped actually
// shipped". C.1 answered that question in the negative for the life of the
// product: a ☑ item, a BaseTool with a table and a store and eleven tests,
// and not one tool list this software has ever served contained it.
//
// The two halves of what makes it checkable are both here now. The wiring
// lives in toolRegistryWiring.apply, so a test can produce the same bag the
// binary produces; and the witnesses are named, so "shipped" is a claim with
// something to point at rather than a tick in a document.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/dbx"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/notify"
	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	manageraiopstools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/chat2query"
	managerbizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	devicebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/device"
	manageraiopsdata "github.com/vincent-wuhan/opskeeper/core/manager/data/aiops/store"
	manageralertdata "github.com/vincent-wuhan/opskeeper/core/manager/data/alert/store"
	managerapprovaldata "github.com/vincent-wuhan/opskeeper/core/manager/data/approval/store"
)

// roadmapWitness is what a delivered roadmap item is evidenced by.
//
// ManagerTools are asserted against the bag this binary hands the model;
// Packages are asserted to exist in the tree, and are for the items whose
// evidence is a node-side package rather than a control-plane tool. Both
// kinds are witnesses rather than assertions of quality: the check asks
// "is there something here that this item could be pointing at", which is
// the weakest question that would still have caught C.1.
type roadmapWitness struct {
	// Item is the ROADMAP.md identifier, e.g. "C.1".
	Item string
	// What is the item's own title, carried so a failing line names the
	// thing rather than a bare identifier.
	What string
	// ManagerTools are the control-plane tool names the item claims.
	ManagerTools []string
	// Packages are repository-relative paths the item claims.
	Packages []string
	// NodeTools are the names the node-side agent package contributes to
	// the model, asserted against the shipped extension's own inventory
	// rather than against the manager's bag. A diagnostic tool that runs
	// on the node and is invoked through the tunnel is not in the
	// manager's tool list, and pretending otherwise would make this table
	// lie about where each capability lives.
	NodeTools []string
}

// deliveredRoadmap is the witness table.
//
// It is hand-written on purpose. A table derived from ROADMAP.md would
// derive the requirement from the document being checked, and a checklist
// that reads itself is a checklist that passes when the document lies. The
// cost of writing it by hand is the cost of this file going stale, and the
// test below fails loudly when it does — which is the point: staleness has
// to be someone else's decision, not a silent pass.
var deliveredRoadmap = []roadmapWitness{
	{Item: "A.1", What: "investigator prompt as a causal traversal loop",
		ManagerTools: []string{"query_incidents", "correlate_incident"}},
	{Item: "A.2", What: "query_change_events BaseTool",
		ManagerTools: []string{alerting.ToolNameQueryChangeEvents}},
	{Item: "A.3", What: "edge-side change watcher",
		Packages: []string{"core/edge/changewatcher"}},
	{Item: "B.2", What: "network probes as first-class BaseTools",
		NodeTools: []string{"host_probe_tcp", "host_probe_http", "host_probe_dns"}},
	{Item: "B.3", What: "file, log, kernel diagnostics",
		NodeTools: []string{"host_dmesg", "host_grep_file", "host_lsof", "host_tail_file"}},
	{Item: "B.4", What: "cmdpolicy expansion",
		Packages: []string{"core/edge/cmdpolicy"}},
	{Item: "C.1", What: "LLM-generated PromQL / LogQL / TraceQL",
		ManagerTools: []string{chat2query.ToolNameChatToQuery}},
	{Item: "D.1", What: "specialist sub-agents",
		Packages: []string{"agents/specialist-network.md"}},
	{Item: "D.2", What: "critic loop",
		Packages: []string{"core/manager/biz/loop/critiqued_worker.go"}},
	{Item: "D.3", What: "eval / replay framework",
		Packages: []string{"core/harness", "cmd/opskeeper-eval"}},
	{Item: "D.4", What: "proposal and confirmation mediation",
		ManagerTools: []string{"apply_config_change"}},
	{Item: "I.1", What: "SSO / SAML / OIDC",
		Packages: []string{"core/manager/iam/biz/sso"}},
}

// roadmapItemRE matches a top-level roadmap entry that claims delivery. The
// two marks are ✓ (shipped) and ☑ (shipped and archived); ◐ and ◯ are
// partial and deliberately not covered, because a half-built item making
// promises is a different conversation from one that claims to be done.
var roadmapItemRE = regexp.MustCompile("(?m)^- \\*\\*([A-Z]\\.\\d+)\\*\\* \x60(?:\u2713|\u2611)\x60")

func roadmapDeliveredItems(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "ROADMAP.md"))
	if err != nil {
		t.Fatalf("read ROADMAP.md: %v", err)
	}
	var out []string
	for _, m := range roadmapItemRE.FindAllStringSubmatch(string(raw), -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatal("no delivered items parsed out of ROADMAP.md: the marker set drifted and this check " +
			"would pass by finding nothing")
	}
	return out
}

// The two directions, because they fail differently. A delivered item with
// no witness is an unbacked claim — the C.1 shape. A witness for an item the
// roadmap no longer claims is a table nobody has revisited, and it is the
// shape that makes a hand-written table rot.
func TestEveryDeliveredRoadmapItemHasAWitnessAndEveryWitnessIsStillClaimed(t *testing.T) {
	claimed := map[string]bool{}
	for _, id := range roadmapDeliveredItems(t) {
		claimed[id] = true
	}
	witnessed := map[string]bool{}
	for _, w := range deliveredRoadmap {
		if witnessed[w.Item] {
			t.Errorf("item %s has two witness rows; one claim needs one witness", w.Item)
		}
		witnessed[w.Item] = true
		if len(w.ManagerTools) == 0 && len(w.Packages) == 0 && len(w.NodeTools) == 0 {
			t.Errorf("item %s (%s) has a row that names nothing, so it witnesses nothing", w.Item, w.What)
		}
	}
	for id := range claimed {
		if !witnessed[id] {
			t.Errorf("ROADMAP.md marks %s delivered and nothing in this file says what it delivered; "+
				"either add a row naming the tool or the package, or take the mark off", id)
		}
	}
	for _, w := range deliveredRoadmap {
		if !claimed[w.Item] {
			t.Errorf("this file witnesses %s (%s) but ROADMAP.md no longer marks it delivered: "+
				"delete the row, or the next reader trusts it over the document", w.Item, w.What)
		}
	}
}

func TestEveryWitnessPackageIsInTheTree(t *testing.T) {
	for _, w := range deliveredRoadmap {
		for _, rel := range w.Packages {
			if _, err := os.Stat(filepath.Join("..", "..", rel)); err != nil {
				t.Errorf("item %s (%s) is witnessed by %s, which is not in the tree: %v",
					w.Item, w.What, rel, err)
			}
		}
	}
}

// The half that would have caught C.1 on the day. The bag is built by
// toolRegistryWiring.apply — the same call main() makes — so a tool that is
// wired in the binary and unwired in a test is not possible, and a tool that
// is unwired in the binary cannot pass here.
func TestEveryWitnessedManagerToolIsInTheBagThisBinaryServes(t *testing.T) {
	bag := buildTheRealToolBag(t)
	present := make(map[string]bool)
	for _, tool := range bag.AllTools() {
		if tool == nil {
			continue
		}
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		present[info.Name] = true
	}
	for _, w := range deliveredRoadmap {
		for _, name := range w.ManagerTools {
			if !present[name] {
				t.Errorf("item %s (%s) is witnessed by the tool %q, and the model is not handed it: "+
					"the bag holds %d tools and none is that one. A roadmap tick is a claim about "+
					"what a deployment serves", w.Item, w.What, name, len(present))
			}
		}
	}
}

// buildTheRealToolBag wires a registry the way main() does, against a
// throwaway sqlite database and a temporary pages directory, and returns the
// bag. It is the seam decision 350 added: before toolRegistryWiring existed,
// this function could not be written, and that is the whole reason C.1
// shipped as delivered without ever appearing.
func buildTheRealToolBag(t *testing.T) *manageraiopstools.ToolBag {
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
	wiring.apply(reg)
	return reg.BuildBaseTools()
}

// throwawayDB is a migrated sqlite database in a temp directory, opened and
// migrated through the same helpers main() uses. The chat_to_query template
// store and the alert channel store are both constructed against it here, so
// the wiring under test is the real one and not a set of nils shaped like it
// — a nil *gorm.DB would let NewQueryTemplateStore succeed and register the
// tool, which is precisely the kind of test-shaped wiring that lets a real
// deployment differ.
func throwawayDB(t *testing.T) *gorm.DB {
	t.Helper()
	log := newTestLogger(t)
	db, err := dbx.Open(config.DBConfig{
		Dialect: "sqlite",
		Path:    filepath.Join(t.TempDir(), "roadmap.db"),
	}, log)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := dbx.RunMigrations(db, log, manageralertdata.Migrate,
		managerapprovaldata.Migrate, manageraiopsdata.Migrate); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestEveryWitnessedNodeToolIsInTheShippedManifest is the node-side half of
// the same question. The manager bag test cannot answer it and this one
// cannot answer the manager's; each names the surface where the capability is
// actually reachable.
//
// The witness is the shipped manifest rather than the Go source or the
// package that generates it. The manifest is the artifact an edge node
// installs from, so a tool present in the code and absent from the manifest
// is a tool no node is ever offered — which is the failure mode worth
// catching, and the one the earlier source-level checks could not see.
func TestEveryWitnessedNodeToolIsInTheShippedManifest(t *testing.T) {
	const manifestRel = "../../plugins/pig-ops/opskeeper-sre-readonly/pig-ops.yaml"
	raw, err := os.ReadFile(manifestRel)
	if err != nil {
		t.Fatalf("the shipped agent manifest is what nodes install from; if it cannot be read, "+
			"this check cannot run and must not pass silently: %v", err)
	}
	var doc struct {
		Spec struct {
			Tools []struct {
				Name string `yaml:"name"`
			} `yaml:"tools"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", manifestRel, err)
	}
	present := make(map[string]bool, len(doc.Spec.Tools))
	for _, tool := range doc.Spec.Tools {
		present[tool.Name] = true
	}
	if len(present) == 0 {
		t.Fatalf("%s declares no tools, so nothing below is being checked", manifestRel)
	}
	for _, w := range deliveredRoadmap {
		for _, name := range w.NodeTools {
			if !present[name] {
				t.Errorf("item %s (%s) is witnessed by the node tool %q, and the manifest an edge "+
					"installs from does not offer it: it declares %d tools and none is that one",
					w.Item, w.What, name, len(present))
			}
		}
	}
}
