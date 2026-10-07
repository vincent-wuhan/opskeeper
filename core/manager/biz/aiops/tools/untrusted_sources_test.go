package tools

import (
	"context"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/correlate"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/host"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/querybackend"
	"log/slog"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/decorators"
	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promptguard"
)

// stubUntrustedTool is a BaseTool with a name we choose and a body we choose,
// so the marking pass can be tested without standing up a real backend.
type stubUntrustedTool struct {
	name string
	out  string
}

func (s stubUntrustedTool) Info(context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{Name: s.name, Class: "read"}, nil
}

func (s stubUntrustedTool) InvokableRun(context.Context, string, ...basetool.InvokeOption) (string, error) {
	return s.out, nil
}

// TestTheTableHasNoBlankOrDuplicateRows: the table is the readable statement
// of what is marked, so a typo or a doubled row must not pass silently. A
// duplicated name would also make the intent ambiguous for a reviewer.
func TestTheTableHasNoBlankOrDuplicateRows(t *testing.T) {
	seen := make(map[string]bool, len(untrustedOutputs))
	for i, e := range untrustedOutputs {
		if e.name == "" {
			t.Fatalf("row %d has an empty name", i)
		}
		if !e.kind.Valid() {
			t.Fatalf("row %d (%s) has kind %q, which is not a declared kind", i, e.name, e.kind)
		}
		if seen[e.name] {
			t.Fatalf("row %d repeats %q", i, e.name)
		}
		seen[e.name] = true
	}
}

// TestLookupAgreesWithTheTable: untrustedKindOf is the only reader, and it
// must return exactly the rows the table declares.
func TestLookupAgreesWithTheTable(t *testing.T) {
	for _, e := range untrustedOutputs {
		kind, ok := untrustedKindOf(e.name)
		if !ok || kind != e.kind {
			t.Fatalf("untrustedKindOf(%q) = %q, %v; want %q, true", e.name, kind, ok, e.kind)
		}
	}
	if _, ok := untrustedKindOf("not_a_tool_in_the_table"); ok {
		t.Fatal("a name that is not in the table resolved to a kind")
	}
}

// TestMarkUntrustedOutputsWrapsExactlyTheTable: one name from the table gets
// fenced with its declared kind, a name outside the table is left byte-for-byte
// alone.
func TestMarkUntrustedOutputsWrapsExactlyTheTable(t *testing.T) {
	f := promptguard.NewFencer()
	in := []basetool.BaseTool{
		stubUntrustedTool{name: querybackend.ToolNameQueryLogQL, out: "level=error msg=boom"},
		stubUntrustedTool{name: "query_promql", out: "42"},
	}
	out := markUntrustedOutputs(in, f)

	if _, ok := out[0].(*decorators.UntrustedOutput); !ok {
		t.Fatalf("the table's tool was not wrapped: %T", out[0])
	}
	if _, ok := out[1].(*decorators.UntrustedOutput); ok {
		t.Fatal("a tool outside the table was wrapped")
	}

	fenced, err := out[0].InvokableRun(context.Background(), "{}")
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	env, ok := promptguard.Parse(fenced)
	if !ok {
		t.Fatalf("the marked tool's result is not fenced:\n%s", fenced)
	}
	if env.Kind != promptguard.KindLog || env.Origin != querybackend.ToolNameQueryLogQL {
		t.Fatalf("envelope = %+v, want kind=log origin=%s", env, querybackend.ToolNameQueryLogQL)
	}
	if env.Body != "level=error msg=boom" {
		t.Fatalf("body = %q, want the tool's output unchanged", env.Body)
	}

	plain, err := out[1].InvokableRun(context.Background(), "{}")
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if plain != "42" {
		t.Fatalf("an unmarked tool's output changed: %q", plain)
	}
}

// TestMarkUntrustedOutputsCopiesRatherThanMutates: BuildBaseTools passes the
// slice it just built, but a caller that keeps its own slice must not find it
// rewritten underneath it.
func TestMarkUntrustedOutputsCopiesRatherThanMutates(t *testing.T) {
	in := []basetool.BaseTool{stubUntrustedTool{name: ToolNameBash, out: "x"}}
	_ = markUntrustedOutputs(in, promptguard.NewFencer())
	if _, ok := in[0].(*decorators.UntrustedOutput); ok {
		t.Fatal("the caller's slice was mutated in place")
	}
}

// TestEveryNameInTheTableIsFencedInTheShippedBag walks the bag production
// assembles and checks both directions: nothing the table names is left
// unmarked, and nothing marked is missing from the table. The second direction
// is what keeps the review list honest — a tool marked by a call somewhere
// else would be a fence a reviewer cannot see here.
func TestEveryNameInTheTableIsFencedInTheShippedBag(t *testing.T) {
	uc := edgebiz.NewUsecase(newFakeEdgeRepo(), nil, nil, slog.Default())
	dc := &fakeDevicesForToolBag{}
	reg := NewRegistry(&fakeCaller{}, uc, dc.usecase(), &fakePromQuerier{}, &fakeLogQuerier{}, &fakeTraceQuerier{}, &fakeAlertUC{}, slog.Default())
	reg.SetPluginConfigLister(fakePluginConfigLister{})

	bag := reg.BuildBaseTools()
	bag = host.AppendHostFilesTools(bag, &fakeCaller{}, uc, dc.usecase(), slog.Default())

	marked := map[string]bool{}
	for _, tool := range bag.AllTools() {
		info, err := tool.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		_, inTable := untrustedKindOf(info.Name)
		_, wrapped := tool.(*decorators.UntrustedOutput)
		switch {
		case inTable && !wrapped:
			t.Errorf("%q is in the untrusted table but the shipped tool is not fenced", info.Name)
		case !inTable && wrapped:
			t.Errorf("%q is fenced but is not in the untrusted table", info.Name)
		}
		if inTable {
			marked[info.Name] = true
		}
	}

	// A bag assembled with every dependency present must actually exercise
	// the table; otherwise this test passes by having nothing to check.
	for _, name := range []string{
		querybackend.ToolNameQueryLogQL, querybackend.ToolNameQueryTraceQL,
		alerting.ToolNameQueryIncidents, alerting.ToolNameGetIncidentDetail, alerting.ToolNameQueryAlertRules, correlate.ToolNameCorrelateIncident,
		host.ToolNameFindLargeFiles, host.ToolNameDuSummary, host.ToolNameStatFile,
	} {
		if !marked[name] {
			t.Errorf("%q must be present and fenced in the fully-wired bag", name)
		}
	}
}

// TestTheShippedBagFencesRatherThanJustWraps proves the wrapper on the
// production path is the fencing one, not some other decorator that happens to
// sit on the same tool.
func TestTheShippedBagFencesRatherThanJustWraps(t *testing.T) {
	uc := edgebiz.NewUsecase(newFakeEdgeRepo(), nil, nil, slog.Default())
	reg := NewRegistry(&fakeCaller{}, uc, nil, &fakePromQuerier{}, &fakeLogQuerier{}, &fakeTraceQuerier{}, nil, slog.Default())

	var found bool
	for _, tool := range reg.BuildBaseTools().AllTools() {
		info, err := tool.Info(context.Background())
		if err != nil || info == nil || info.Name != querybackend.ToolNameQueryLogQL {
			continue
		}
		found = true
		out, err := tool.InvokableRun(context.Background(), `{"query":"{job=\"x\"}"}`)
		if err != nil {
			t.Fatalf("%s: %v", info.Name, err)
		}
		env, ok := promptguard.Parse(out)
		if !ok {
			t.Fatalf("%s's result is not fenced:\n%s", info.Name, out)
		}
		if env.Origin != querybackend.ToolNameQueryLogQL || env.Kind != promptguard.KindLog {
			t.Fatalf("envelope = %+v, want origin=%s kind=log", env, querybackend.ToolNameQueryLogQL)
		}
	}
	if !found {
		t.Fatal("query_logql was not in the bag")
	}
}
