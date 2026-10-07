package pigprofile

import (
	"slices"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/coding/piglet"

	"github.com/vincent-wuhan/opskeeper/core/edge/agentprofile"
)

// The node agent runs as `pig --mode rpc --piglet <this file>` with the
// node's privileges. PiG's stock built-ins include bash, powershell, edit
// and write, so "does this profile actually remove them" is a security
// question, not a configuration one — and it is answered here by PiG's own
// parser and PiG's own tool-scoping function rather than by a reading of
// the schema documentation.
//
// Every positive assertion is paired with a negative control, because the
// failure mode of this kind of test is a test that passes for the wrong
// reason: an empty result from ScopeTools proves nothing if ScopeTools
// returns nothing for every input.

// builtinNames is PiG's built-in tool list, as a snapshot.
//
// PiG exposes it through an internal package, which this module may not
// import, so the names are written down here and then *checked* against
// PiG's own validator below. That check matters: a name that stopped being
// a built-in would make these tests quietly weaker, and a validator that
// accepted anything would make the check meaningless, so both directions
// are asserted.
//
// It is worth being clear that production safety does not depend on this
// list being complete. The profile says `tools: []`, which PiG reads as
// "no built-ins" whatever they happen to be, so a tool PiG adds tomorrow is
// excluded by the profile without this file changing. The list is the test
// instrument, not the mechanism.
var builtinNames = []string{
	"read", "bash", "powershell", "edit", "write", "grep", "find", "ls",
}

// dangerousBuiltins are named separately because they are the ones whose
// survival is the actual finding, and a failure message that says "bash
// survived" is worth more to whoever reads it at 3am than a list diff.
var dangerousBuiltins = []string{"bash", "powershell", "edit", "write"}

// extensionTools is what the admitted plugin packages register, under the
// extension names those packages actually use. Inventing fixture names here
// would let the test pass while the packages renamed themselves.
//
// The gate courier is absent on purpose. It registers no tools at all — it
// only listens for tool_call — so it never appears in what the runtime
// hands ScopeTools. An earlier version of this file gave it a fictional
// tool to stand in for "loads but contributes nothing", and the test
// immediately failed on it: PiG keeps every tool of an extension the
// profile does not name, including one that does not exist. That failure is
// the invariant working, and the honest way to record it is to model the
// courier as what it is.
var extensionTools = []piglet.ToolInfo{
	{Name: "host_probe_tcp", Source: "opskeeper-sre-readonly"},
	{Name: "get_topology", Source: "opskeeper-sre-readonly"},
	{Name: "query_promql", Source: "opskeeper-sre-observability"},
	{Name: "host_restart_service", Source: "opskeeper-sre-repair"},
}


// admittedExtensions is the node profile's own view of the fixture above:
// the same tools, grouped by the extension that registers them.
//
// Deriving one from the other is the point. A profile written by hand here
// would be a second, independent statement of what the packages ship, and
// the two could disagree without anything noticing — which is the shape of
// the bug this file exists to catch, reproduced in the test that was
// supposed to prevent it.
func admittedExtensions() []agentprofile.Extension {
	bySource := map[string][]string{}
	for _, info := range extensionTools {
		bySource[info.Source] = append(bySource[info.Source], info.Name)
	}
	out := make([]agentprofile.Extension, 0, len(bySource))
	for name, tools := range bySource {
		out = append(out, agentprofile.Extension{Name: name, Tools: tools})
	}
	return out
}

func parse(t *testing.T, body string) *piglet.Piglet {
	t.Helper()
	p, err := piglet.ParseBytes([]byte(body))
	if err != nil {
		t.Fatalf("PiG refused the profile: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("PiG's own validation refused the profile: %v", err)
	}
	return p
}

// registered is what a node's agent would have loaded: PiG's built-ins
// plus the tools the admitted extensions register.
func registered() []piglet.ToolInfo {
	infos := make([]piglet.ToolInfo, 0, len(builtinNames)+len(extensionTools))
	for _, name := range builtinNames {
		infos = append(infos, piglet.ToolInfo{Name: name, Source: "builtin"})
	}
	return append(infos, extensionTools...)
}

func TestPiGAcknowledgesTheNodeProfile(t *testing.T) {
	p := parse(t, agentprofile.Render(admittedExtensions()))

	if p.BuiltinTools == nil {
		// Omitted and empty are different and only one of them is safe.
		// This catches a refactor turning the field into a pointer that is
		// left nil, which is the one edit that would put a shell back on
		// every node in the fleet without failing anything else.
		t.Fatal(`PiG read the profile with no "tools" field; omitted means PiG's stock built-ins`)
	}
	if len(*p.BuiltinTools) != 0 {
		t.Errorf("PiG read tools = %v, want an empty list", *p.BuiltinTools)
	}
	if p.Discovery == nil || p.Discovery.Extensions == nil || p.Discovery.Skills == nil {
		t.Error("PiG did not read an empty discovery block; ambient skills would load unchecked")
	}
}

// TestTheProfileRemovesTheShellFromTheMenu is the assertion the profile
// exists for, evaluated with PiG's own scoping function.
func TestTheProfileRemovesTheShellFromTheMenu(t *testing.T) {
	p := parse(t, agentprofile.Render(admittedExtensions()))
	active := piglet.ScopeTools(p, registered())

	for _, name := range dangerousBuiltins {
		if slices.Contains(active, name) {
			t.Errorf("%q survived the profile; the node agent runs with the node's privileges, so "+
				"this is arbitrary code execution offered to the model", name)
		}
	}
	for _, name := range builtinNames {
		if slices.Contains(active, name) {
			t.Errorf("built-in %q survived the profile", name)
		}
	}

	// The plugins this node admitted are still exactly what the model can
	// call. A profile that subtracted the extensions too would leave a node
	// with a correct-looking file and an agent that cannot do its job —
	// which is the failure this half of the test exists to prevent,
	// because the obvious "fix" for the first half is a broader list.
	want := make([]string, 0, len(extensionTools))
	for _, info := range extensionTools {
		want = append(want, info.Name)
	}
	slices.Sort(want)
	for _, tool := range want {
		if !slices.Contains(active, tool) {
			t.Errorf("plugin tool %q was removed by the profile; the profile subtracts the host's "+
				"own built-ins and must not touch what the admitted packages offer", tool)
		}
	}
	slices.Sort(active)
	if !slices.Equal(active, want) {
		t.Errorf("active tools = %v, want exactly %v; the profile is a subtraction and this says it "+
			"subtracted something else too", active, want)
	}
}

// TestAnOmittedToolListKeepsTheShell is the negative control for the test
// above. Without it, "the profile removed bash" could be true because
// ScopeTools returned nothing for a profile it did not understand.
func TestAnOmittedToolListKeepsTheShell(t *testing.T) {
	p := parse(t, "name: control\n")
	active := piglet.ScopeTools(p, registered())
	if !slices.Contains(active, "bash") {
		t.Fatalf("a profile with no tools field kept no built-ins either (active = %v); the real "+
			"test above would then pass without proving anything", active)
	}
}

// TestANonEmptyToolListIsHonoured is the second control: it shows the empty
// list in the real profile is what does the removing, rather than some
// other field quietly producing the same result.
func TestANonEmptyToolListIsHonoured(t *testing.T) {
	p := parse(t, "name: control\ntools: [read]\n")
	active := piglet.ScopeTools(p, registered())
	if !slices.Contains(active, "read") {
		t.Errorf(`a profile naming "read" did not keep it (active = %v)`, active)
	}
	if slices.Contains(active, "bash") {
		t.Errorf(`a profile naming only "read" also kept bash (active = %v)`, active)
	}
}

// TestEveryBuiltinNameHereIsARealPiGBuiltin checks the snapshot against
// PiG's own validator, which is the only public way to ask "is this a
// built-in tool name" from outside the module that defines them.
func TestEveryBuiltinNameHereIsARealPiGBuiltin(t *testing.T) {
	for _, name := range builtinNames {
		if _, err := piglet.ParseBytes([]byte("name: control\ntools: [" + name + "]\n")); err != nil {
			t.Errorf("PiG no longer recognises %q as a built-in tool (%v); the profile still removes "+
				"it, but this test has stopped being able to say so", name, err)
		}
	}
}

// TestTheBuiltinCheckIsNotVacuous is the control on the check above. A
// validator that accepted any string would make the previous test pass
// forever while proving nothing, so the failure direction is asserted too.
func TestTheBuiltinCheckIsNotVacuous(t *testing.T) {
	_, err := piglet.ParseBytes([]byte("name: control\ntools: [not-a-pig-builtin]\n"))
	if err == nil {
		t.Fatal("PiG accepted a made-up tool name; the built-in check above cannot mean anything")
	}
	// v0.4.0 reworded the refusal from "unknown built-in tool" to
	// "is not a built-in tool"; the direction of the check is what matters,
	// so pin the phrase this version actually uses.
	if !strings.Contains(err.Error(), "is not a built-in tool") {
		t.Errorf("PiG refused %q for a reason other than the tool name (%v), so the check above is "+
			"not testing what it claims to test", "not-a-pig-builtin", err)
	}
}
