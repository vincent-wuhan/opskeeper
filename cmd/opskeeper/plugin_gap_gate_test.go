package main

// The reconciliation gate.
//
// A node package declares the tools it offers its agent in pig-ops.yaml.
// Every name in that list is a promise: the agent was handed the schema,
// so the agent will call it, and the call has to go somewhere.
//
// There are two places it can go. The control plane can serve it over the
// upcall channel — either from the closure registry or, now, from the
// BaseTool bridge. Or the node can serve it itself as a local skill, in
// which case the question never leaves the machine.
//
// A name on neither route was offered to a model that cannot reach it. The
// agent calls it, the call fails, and the failure is indistinguishable
// from the tool being broken. That is the whole class of bug this file
// exists to catch.
//
// Both name lists are BUILT, never written:
//
//   - the upcall-serviceable names come from the real registry and the
//     real bridge, constructed by the same wiring main() runs;
//   - the node-local names come from the skill registry the node serves
//     from, which is the real registry and not a transcription of it.
//
// A hand-written list here would be the same defect the gate is about:
// two places that each look right and disagree. The negative case below
// is what makes the gate meaningful — it is run for real, in a
// subprocess, and it is required to fail.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	manageraiopstools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	aiopstoolsdec "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/decorators"
)

// gapGatePackages are the node packages the gate covers.
//
// The other shipped packages are not in scope, and the reason is worth
// stating so the omission does not read as an oversight. The repair
// package declares a conversation-bound draft_config_change, which is
// served by the control plane's own conversation rather than by a node's
// upcall; the middleware package declares adapter tools that exist only
// where an operator configured a DSN. Neither shape is "a node asked for
// something the control plane could have served", so neither is the
// question this gate asks. Widen the list when a package joins the
// upcall story — and check that its declaration is upcall-shaped first.
var gapGatePackages = []string{
	"opskeeper-sre-observability",
	"opskeeper-sre-readonly",
}

// gapGateManifestNames reads the tool names one package declares.
//
// It goes through the real manifest loader rather than a YAML struct
// defined here, because the loader is what validates the declaration and
// a gate that parsed the file differently could disagree with the product
// about what the package declares.
func gapGateManifestNames(t *testing.T, pkg string) []string {
	t.Helper()
	root := filepath.Join("..", "..", "plugins", "pig-ops", pkg)
	plugin, err := pluginmanifest.Load(root)
	if err != nil {
		t.Fatalf("load the %s manifest: %v", pkg, err)
	}
	names := plugin.Manifest.Spec.Tools.Names()
	if len(names) == 0 {
		t.Fatalf("%s declares no tools, so the gate below is checking nothing", pkg)
	}
	return names
}

// gapGateNodeLocalNames lists the tools a node serves from its own
// registry.
//
// The source is the process-wide skill registry, which is where the
// node's edge runtime looks a tool up. Listing them by hand would be
// exactly the kind of list that drifts from the code it describes.
func gapGateNodeLocalNames() map[string]bool {
	out := map[string]bool{}
	for _, executor := range skill.All() {
		out[executor.Metadata().Key] = true
	}
	return out
}

// gapGateUpcallNames lists the tools a node's agent can reach over the
// upcall channel.
//
// The two sources are the two surfaces the channel dispatches across: the
// closure registry and the BaseTool bridge. A name served by neither is
// not serviceable, and saying so is the whole point.
func gapGateUpcallNames(t *testing.T, reg *manageraiopstools.Registry, bridge *baseToolUpcall) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, schema := range reg.Schemas() {
		out[schema.Name] = true
	}
	for _, name := range bridge.serviceableNames() {
		out[name] = true
	}
	return out
}

// gapGateUnserved returns the declared tool names that nobody serves.
//
// This is the gate, and it is deliberately a function returning a value
// rather than a test that fails: the negative case has to be able to run
// it and watch it fail without taking the parent test down with it.
func gapGateUnserved(declared []string, upcall, local map[string]bool) []string {
	var out []string
	for _, name := range declared {
		if upcall[name] || local[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// gapGateError renders the gate's failure.
//
// The message names the tools rather than counting them: a count tells a
// reader that something is wrong, and a list tells them what.
func gapGateError(pkg string, unserved []string) error {
	if len(unserved) == 0 {
		return nil
	}
	return fmt.Errorf(
		"package %s declares %d tool(s) that neither the upcall channel nor a node-local skill serves: %s",
		pkg, len(unserved), strings.Join(unserved, ", "))
}

// gapGateBridge builds the bridge exactly as main() does.
//
// It is a separate call from the one in main() only because a test cannot
// call into main(); the sequence — build the bag from the wired registry,
// wrap each tool in the decorator chain, expose the read-class ones — is
// the same one, and the bag it is handed is the bag main() hands it.
func gapGateBridge(t *testing.T, reg *manageraiopstools.Registry, bag *manageraiopstools.ToolBag) *baseToolUpcall {
	t.Helper()
	return newBaseToolUpcall(bag, aiopstoolsdec.Deps{
		Timeout:    15 * time.Second,
		Limiter:    aiopstoolsdec.NewTokenBucketLimiter(0),
		Registerer: prometheus.NewRegistry(),
	}, newTestLogger(t))
}

// The gate, on a deployment that has the bridge.
//
// Every tool either package declares has to be serviceable: over the
// upcall channel, or by the node itself. A name that is neither is a tool
// the agent was promised and cannot call.
func TestEveryDeclaredNodeToolIsServableOverTheUpcallChannelOrALocalSkill(t *testing.T) {
	reg, bag := buildTheRealToolRegistry(t)
	bridge := gapGateBridge(t, reg, bag)
	upcall := gapGateUpcallNames(t, reg, bridge)
	local := gapGateNodeLocalNames()

	for _, pkg := range gapGatePackages {
		declared := gapGateManifestNames(t, pkg)
		unserved := gapGateUnserved(declared, upcall, local)
		if err := gapGateError(pkg, unserved); err != nil {
			t.Errorf("%v", err)
		}
	}
}

// The gate, on a deployment that does NOT have the bridge.
//
// This is the negative case, and it is the one that says the gate is
// worth anything: without the BaseTool bridge the six tools that exist
// only as BaseTools are declared, offered and unserviceable, and the gate
// must say so.
//
// It runs as a subprocess so that the thing being observed is the gate
// failing, not an assertion about a helper returning an error. The child
// process runs the same gate against a deployment with no bridge and is
// required to exit non-zero.
func TestTheGapGateIsRedWhenTheBaseToolBridgeIsNotWired(t *testing.T) {
	const negativeEnv = "OPSKEEPER_GAP_GATE_NEGATIVE"

	if os.Getenv(negativeEnv) == "1" {
		// This process IS the negative case. Build the deployment the
		// way main() builds it, take the bridge away, and run the gate.
		reg, _ := buildTheRealToolRegistry(t)
		upcall := gapGateUpcallNames(t, reg, nil)
		local := gapGateNodeLocalNames()

		for _, pkg := range gapGatePackages {
			declared := gapGateManifestNames(t, pkg)
			unserved := gapGateUnserved(declared, upcall, local)
			if err := gapGateError(pkg, unserved); err != nil {
				t.Errorf("%v", err)
			}
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestTheGapGateIsRedWhenTheBaseToolBridgeIsNotWired")
	cmd.Env = append(os.Environ(), negativeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the gap gate passed on a deployment whose upcall channel carries no BaseTool "+
			"surface, so it cannot fail and cannot catch anything:\n%s", out)
	}
	// The failure has to name the tools, or it is a failure nobody can
	// act on.
	for _, want := range []string{
		"grep_source", "list_repo_sources", "query_change_events", "read_source",
		"expand_topology", "find_topology_node",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the negative run failed without naming %q, so the failure does not say "+
				"which tools were unserved:\n%s", want, out)
		}
	}
}

// The negative case, read from the same code path, so that the exact set
// of unserved tools is a fact the repository states rather than something
// a reviewer has to infer from a subprocess log.
//
// This is the second half of the negative case and it is the half that
// pins the SCOPE of the gap: the tools that are unserved without the
// bridge are exactly the six that exist only as BaseTools, and nothing
// else. If a seventh name ever appears here, the gate's negative case
// has widened and the scope of this slice has changed with it.
func TestTheUnservedSetWithoutTheBridgeIsExactlyTheBaseToolOnlyTools(t *testing.T) {
	reg, bag := buildTheRealToolRegistry(t)
	local := gapGateNodeLocalNames()

	// With the bridge: nothing is unserved.
	wired := gapGateUpcallNames(t, reg, gapGateBridge(t, reg, bag))
	for _, pkg := range gapGatePackages {
		declared := gapGateManifestNames(t, pkg)
		if unserved := gapGateUnserved(declared, wired, local); len(unserved) > 0 {
			t.Errorf("%s: the bridge is wired and %d tool(s) are still unserved: %s",
				pkg, len(unserved), strings.Join(unserved, ", "))
		}
	}

	// Without the bridge: the closure registry is all that is left.
	unwired := gapGateUpcallNames(t, reg, nil)
	var got []string
	for _, pkg := range gapGatePackages {
		declared := gapGateManifestNames(t, pkg)
		got = append(got, gapGateUnserved(declared, unwired, local)...)
	}
	sort.Strings(got)

	want := []string{
		"expand_topology",
		"find_topology_node",
		"grep_source",
		"list_repo_sources",
		"query_change_events",
		"read_source",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the unserved set without the bridge is %v, and it is expected to be %v.\n"+
			"A name here that is not a BaseTool-only tool means the closure registry lost a tool "+
			"the gate depends on; a name here that IS in the list but missing means the bridge "+
			"does not serve a tool the packages declare", got, want)
	}
}
