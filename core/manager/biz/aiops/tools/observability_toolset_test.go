package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/database"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/host"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/metriccatalog"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/querybackend"
	"go/format"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

// The node's observability and middleware toolset, generated from this
// registry.
//
// Twelve tools, most of them with schemas long enough that transcribing
// them by hand is not an option and retyping a range bound is not a
// mistake anybody should make on purpose. So the file the agent reads is
// produced from the live Info() of the tools that will actually run it,
// and the test below fails the moment the two drift apart.
//
// The direction of the copy matters. The registry is the authority — it is
// what executes the call, so its schema is the one that has to be right —
// and the extension is the copy. A hand-maintained copy would drift the
// moment a tool gained a parameter, and it would drift silently, because
// the model would be calling a schema the executor no longer parses and
// the resulting error reads like bad input rather than like staleness.
//
// Regenerate with:
//
//	OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/biz/aiops/tools/ -run Toolset
//
// after changing a schema. Then run scripts/sync-pig-ops.sh to copy the
// extension into the package.

// observabilityTools is the inventory this toolset exposes, in the order
// it is written to the file.
//
// The list is written out rather than discovered, because "every read tool
// in the registry" is a much larger and much more dangerous set: it
// includes tools that answer questions about the whole fleet, and a
// toolset is a review surface. A name appears here because somebody
// decided this node may ask it, and adding one is the same kind of decision
// as adding a line to a package manifest.
//
// Order is by name, for the same reason the manifests are sorted: a review
// is a diff more often than it is a reading.
func observabilityTools() []observabilityEntry {
	return []observabilityEntry{
		{database.ToolNameAnalyzeDatabaseStatus, "analyze_database_status",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := database.NewAnalyzeDatabaseStatusTool(nil, nil, nil, nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{ToolNameGetEdgeSummary, "get_edge_summary",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := NewGetEdgeSummaryTool(nil, nil, nil, nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{host.ToolNameGetHostLoad, "get_host_load",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := host.NewGetHostLoadTool(nil, nil, nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{ToolNameGrepSource, "grep_source",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := NewGrepSourceTool(nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{database.ToolNameListDatabaseSources, "list_database_sources",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := database.NewListDatabaseSourcesTool(nil, nil, nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{metriccatalog.ToolNameListMetricCatalog, "list_metric_catalog",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := metriccatalog.NewListMetricCatalogTool(nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{ToolNameListRepoSources, "list_repo_sources",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := NewListRepoSourcesTool(nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{alerting.ToolNameQueryChangeEvents, "query_change_events",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := alerting.NewQueryChangeEventsTool(nil, nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{querybackend.ToolNameQueryLogQL, "query_logql",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := querybackend.NewQueryLogQLTool(nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{querybackend.ToolNameQueryPromQL, "query_promql",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := querybackend.NewQueryPromQLTool(nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{querybackend.ToolNameQueryTraceQL, "query_traceql",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := querybackend.NewQueryTraceQLTool(nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
		{ToolNameReadSource, "read_source",
			func(t *testing.T, ctx context.Context) basetoolInfo {
				info, err := NewReadSourceTool(nil, nil).Info(ctx)
				return infoOf(t, info, err)
			}},
	}
}

// basetoolInfo is the part of a tool's Info() this toolset needs, copied
// out so the generator and the assertions do not each re-derive it.
type basetoolInfo struct {
	Name        string
	Description string
	Parameters  []byte
	Class       string
}

func infoOf(t *testing.T, info *basetool.ToolInfo, err error) basetoolInfo {
	t.Helper()
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	return basetoolInfo{
		Name:        info.Name,
		Description: info.Description,
		Parameters:  append([]byte(nil), info.Parameters...),
		Class:       info.Class,
	}
}

// observabilityEntry pairs a wire name with the tool that serves it.
type observabilityEntry struct {
	// constName is the manager's own constant. Asserting it equals the
	// wire name the tool reports is the check that stops a rename on
	// either side from splitting the pair.
	constName string
	wireName  string
	load      func(*testing.T, context.Context) basetoolInfo
}

// toolsetPath locates the generated file.
//
// It walks up looking for go.work rather than counting directories. The
// count is what would silently rot: this test moved once already, and a
// wrong number here does not fail — it regenerates the file somewhere
// nobody reads, or reads a stale copy from the wrong tree and passes.
func toolsetPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source file")
	}
	dir := filepath.Dir(file)
	// The shared tracked markers, not go.work: a clean clone has none, and
	// this test failed outright there rather than skipping.
	root, ok := reporoot.Find(dir, 12)
	if !ok {
		t.Fatalf("could not locate the repository root from %s", dir)
	}
	return filepath.Join(root, "core", "pig", "extensions", "opskeeper-sre-observability", "tools.go")
}

// renderToolset builds the file body from the live registry.
func renderToolset(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	entries := observabilityTools()

	names := make([]string, len(entries))
	byName := make(map[string]basetoolInfo, len(entries))
	for i, e := range entries {
		info := e.load(t, ctx)
		if info.Name != e.wireName {
			t.Errorf("%s is %q at the registry but the toolset calls it %q; "+
				"the node would offer a name the control plane does not serve",
				e.constName, info.Name, e.wireName)
		}
		if info.Class != "read" {
			t.Errorf("%s is %q in this registry; the observability package is L1 and "+
				"every tool in it must be a read", e.wireName, info.Class)
		}
		names[i] = info.Name
		byName[info.Name] = info
	}
	sort.Strings(names)
	for i := 1; i < len(names); i++ {
		if names[i] == names[i-1] {
			t.Fatalf("tool %q is listed twice", names[i])
		}
	}

	var b []byte
	add := func(format string, args ...any) {
		b = append(b, []byte(fmt.Sprintf(format, args...))...)
	}

	add("%s\n", toolsetHeader)
	for _, name := range names {
		info := byName[name]
		add("\t{\n")
		add("\t\tName:        %s,\n", strconv.Quote(name))
		add("\t\tLabel:       %s,\n", strconv.Quote(name))
		add("\t\tDescription: %s,\n", strconv.Quote(info.Description))
		add("\t\tParameters: `%s`,\n", string(info.Parameters))
		add("\t},\n")
	}
	add("}\n\n")
	add("// ToolNames returns the inventory in order, for the host-side drift\n")
	add("// check and for diagnostics.\n")
	add("func ToolNames() []string {\n")
	add("\tout := make([]string, 0, len(tools))\n")
	add("\tfor _, t := range tools {\n")
	add("\t\tout = append(out, t.Name)\n")
	add("\t}\n")
	add("\treturn out\n")
	add("}\n")

	// gofmt the result rather than hand-formatting the template. The
	// template has to care about the Go syntax, and indenting a nested
	// table by hand in a string literal is a place a diff would look
	// plausible and be wrong. Formatting here also means the file a node
	// builds is gofmt-clean, so a reviewer reading the diff is reading
	// the tools and not the whitespace.
	//
	// A formatting failure is fatal rather than skipped: this file is
	// produced, not authored, so malformed output is a bug in the
	// generator above, and shipping it unformatted would hide that.
	pretty, err := format.Source(b)
	if err != nil {
		t.Fatalf("the generated toolset is not valid Go (%v); the generator above is wrong, not the input", err)
	}
	return string(pretty)
}

const toolsetHeader = `package opskeeperobservability

// GENERATED FILE — do not edit.
//
// Produced from the live Info() of the tools in
// core/manager/biz/aiops/tools by that package's tests. The registry
// is the authority: it is what executes the call, so its schema is the one
// that has to be correct, and this file is the copy the node's agent reads.
//
// Regenerate with:
//
//	OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/biz/aiops/tools/ -run Toolset
//
// then run scripts/sync-pig-ops.sh to copy the extension into the package.
// TestTheObservabilityToolsetMatchesTheRegistry fails if this file and the
// registry disagree, so editing it by hand fails a test rather than
// shipping a menu the executor cannot parse.

// toolSpec is one tool this package contributes to the agent.
//
// The table is data, not code: every entry is the same shape and every
// entry does the same thing, which is hand the call to the host. Nothing
// here interprets an argument or touches a system, because everything that
// does lives in the control plane, where it is permissioned, covered and
// — for the mutating tools elsewhere in this repository — reachable only
// through a reviewer a human can see.
//
// These tools are all served by an upcall. The observability stack and the
// database sources are the manager's: the Prometheus and Loki endpoints and
// the registered repositories are configured there, and a subprocess on a
// node has no path to any of them. Inventing one would be a second,
// unaudited route into the control plane.
type toolSpec struct {
	// Name is what the model calls. It is also the name the host looks
	// up, so it is one string end to end.
	Name string
	// Label is the human-readable name the console shows.
	Label string
	// Description is what the model reads to decide whether to call this.
	Description string
	// Parameters is the tool's JSON Schema, copied from the registry.
	Parameters string
}

// tools is the whole inventory, sorted by name.
var tools = []toolSpec{
`

// TestTheObservabilityToolsetMatchesTheRegistry is the drift check.
//
// A stale copy here is not a stale file. The model calls the schema in
// this table; the control plane parses the schema in the registry. When
// they disagree the call fails with an argument error, which reads like
// the model made a mistake rather than like the menu is out of date — and
// that is a failure mode nobody debugs, because the model's behaviour
// looks wrong and the model's behaviour is not what is wrong.
func TestTheObservabilityToolsetMatchesTheRegistry(t *testing.T) {
	path := toolsetPath(t)
	want := renderToolset(t)

	if os.Getenv("OPSKEEPER_UPDATE_TOOLSET") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatalf("regenerate the toolset: %v", err)
		}
		t.Logf("regenerated %s (%d bytes)", path, len(want))
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the generated toolset: %v\n"+
			"regenerate it with OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/biz/aiops/tools/ -run Toolset",
			err)
	}
	if string(got) != want {
		t.Errorf("the generated toolset has drifted from the registry.\n"+
			"  file:     %s (sha256 %s)\n"+
			"  registry: %s (sha256 %s)\n"+
			"Regenerate it with OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/biz/aiops/tools/ -run Toolset",
			path, digestOf(got), "", digestOf([]byte(want)))
	}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}
