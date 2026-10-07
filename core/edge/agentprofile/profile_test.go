package agentprofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests check the file without a YAML library, because core/edge may
// not take one: the module's allowed set is core's contracts and the
// standard library, and a test is held to the same rule as production code
// so that a boundary does not quietly become negotiable in test files. The
// field reader below is therefore hand-written, and it is hand-written on
// purpose — the assertion it has to support is an *absent-versus-empty*
// distinction, and a typed struct would collapse exactly that.
//
//   tools: []      -> no PiG built-ins. This is the whole point of the file.
//   (no tools:)    -> PiG's normal built-ins, which on a node include bash.
//
// The proof that PiG itself accepts this file and reads it that way lives in
// core/pig/pigprofile, which is the one module allowed to know what PiG's
// API looks like this week.

// topLevel reads one column-0 key out of the profile.
//
// Comments and blank lines are skipped, and a key is only recognised at
// column 0 — which is what makes `tools:` inside the comment block
// indistinguishable from the real one, so the comments in Render must never
// use that spelling at column 0. It is a narrow reader on purpose: anything
// cleverer would be reimplementing a YAML parser badly.
func topLevel(t *testing.T, body, key string) (value string, found bool) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue // nested, so not the key being asked about
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok || name != key {
			continue
		}
		return strings.TrimSpace(rest), true
	}
	return "", false
}

// nested reads one level of indentation under a parent key. topLevel skips
// indented lines on purpose, which is right for a root key and wrong for the
// one place this file needs to look inside a block: the discovery scopes,
// where the whole profile turns on a nested list.
// nodePackages is the admitted set the tests below describe: two
// extensions, and the tools a review said each package may have on a node.
//
// The tests are about the profile's static half, and they pass this anyway.
// A profile for a node that admitted nothing is the degenerate case, and
// asserting only on it would let the shape every real node runs rot
// unnoticed — which is the failure this file was rewritten for.
var nodePackages = []Extension{
	{Name: "opskeeper-sre-readonly", Tools: []string{"get_topology", "host_probe_tcp"}},
	{Name: "opskeeper-sre-repair", Tools: []string{"host_restart_service"}},
}

func nested(t *testing.T, body, parent, key string) (value string, found bool) {
	t.Helper()
	inside := false
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indented := line[0] == ' ' || line[0] == '\t'
		name, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !indented {
			inside = name == parent
			continue
		}
		if inside && ok && name == key {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// extensionTools reads the tool list written under one `extensions` entry.
// The entries are list items rather than mappings keyed by name, so this
// cannot be nested(): there is no key line to hang the block off.
func extensionTools(rendered, name string) (string, bool) {
	lines := strings.Split(rendered, "\n")
	head := "  - name: " + name
	for i, line := range lines {
		if strings.TrimSpace(line) != strings.TrimSpace(head) {
			continue
		}
		for _, next := range lines[i+1:] {
			trimmed := strings.TrimSpace(next)
			if !strings.HasPrefix(trimmed, "tools:") {
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				return "", false
			}
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "tools:")), true
		}
	}
	return "", false
}

func TestTheProfileIsNamedForWhatItIs(t *testing.T) {
	name, found := topLevel(t, Render(nodePackages), "name")
	if !found {
		t.Fatal("the profile has no name; it is what an operator sees in the agent's own diagnostics")
	}
	if name != Name {
		t.Errorf("name = %q, want %q", name, Name)
	}
	desc, found := topLevel(t, Render(nodePackages), "description")
	if !found || len(strings.Trim(desc, `"`)) == 0 {
		t.Error("the profile has no description; a node agent that names nothing explains nothing in pig status")
	}
}

// TestTheProfileRemovesEveryBuiltinTool is the load-bearing assertion, and
// it is about presence rather than value.
func TestTheProfileRemovesEveryBuiltinTool(t *testing.T) {
	value, found := topLevel(t, Render(nodePackages), "tools")
	if !found {
		// The failure this guards is the entire reason the file exists.
		// An omitted `tools` means PiG's stock built-ins, and one of them
		// is a shell on a host running with the node's privileges.
		t.Fatal(`the profile has no "tools" field; omitted means PiG's stock built-ins, which ` +
			"include a shell, and the field must be present and empty")
	}
	if value != "[]" {
		t.Errorf("tools = %q, want \"[]\"; anything named here is a tool the model is offered on a node", value)
	}
}

// TestTheProfileKeepsTheNodeItsOwnPackages is the assertion that replaced
// one which asserted the opposite and was wrong.
//
// The old test wanted both discovery lists empty, so that a skill dropped
// into the agent's home directory could not become a tool the model may
// invoke. That goal is real. The mechanism was not: PiG resolves ONE scope
// list for the agent's own top-level directories and for the Packages in
// settings.json, and an empty list means "no scopes" rather than "no ambient
// sources" — so it switched off the node's own reviewed packages along with
// the ambient ones. A node ran with eighteen declared plugin tools and none
// of them reachable, and the suite was green throughout, because the test
// read the file's text and the file's text had said what the test wanted.
//
// What this asserts is the invariant that actually matters: the scope a
// node's packages are registered at is in the list, and the project-scope
// ambient surface is not. The project half is the old test's intent, still
// enforced, in the only form PiG offers.
func TestTheProfileKeepsTheNodeItsOwnPackages(t *testing.T) {
	rendered := Render(nodePackages)
	disc, found := topLevel(t, rendered, "discovery")
	if !found {
		t.Fatal("the profile has no discovery block. Under --piglet an absent block is not " +
			"\"discover everything\": PiG answers an absent block with an empty scope list " +
			"exactly as it answers an empty one, so omitting it loads no packages either")
	}
	_ = disc
	for _, kind := range []string{"extensions", "skills"} {
		value, found := nested(t, rendered, "discovery", kind)
		if !found {
			t.Errorf("discovery has no %s key; a profile whose protection depends on which "+
				"field a future editor happened to keep is not a boundary", kind)
			continue
		}
		if !strings.Contains(value, "user") {
			t.Errorf("discovery.%s = %q, which does not admit the user scope. A node's packages "+
				"are registered at that scope, so this profile would load none of them and the "+
				"agent would be offered the host's built-ins and nothing else", kind, value)
		}
		for _, ambient := range []string{"workspace", "project"} {
			if strings.Contains(value, ambient) {
				t.Errorf("discovery.%s names %q; a project-scope resource is one the model may "+
					"invoke without ever appearing in a manifest, and the node has no project scope "+
					"to review one in", kind, ambient)
			}
		}
	}
}

// TestTheProfileGrantsNothingItself keeps this file honest about what it
// is.
//
// "extensions" left this list when the profile learned to name them, and
// leaving it would have been the lazy thing to do in either direction. The
// reason it is not a grant is structural: a named extension is held to the
// list written next to it, so naming one can only subtract. The property
// that matters — that the list is the manifest's and not something wider —
// cannot be checked from here, because this package never sees a manifest.
// It is asserted where PiG's own scoping function is reachable, in
// core/pig/pigprofile.
//
// The fields still below are grants in PiG's sense: they add resources, or
// credentials, or a model, and a profile that carried any of them would be
// a second unreviewed way to shape what runs on a host.
func TestTheProfileGrantsNothingItself(t *testing.T) {
	for _, field := range []string{"skills", "packages", "secrets", "agentEnv", "model"} {
		if _, found := topLevel(t, Render(nodePackages), field); found {
			t.Errorf("the profile declares %q at the root; a profile that adds resources is a second "+
				"way to put tools on a node that never passed a manifest review", field)
		}
	}
}

// TestTheProfileNamesEachAdmittedExtensionAndItsReviewedTools is the
// property the extensions block exists for, asserted on the rendered text.
//
// The failure it guards is a package becoming more capable without anybody
// accepting the diff. If a tool reaches the model that no manifest declared,
// the node still refuses the call — the gate is the boundary — but the model
// has been handed a capability review never approved, and every transcript
// from then on is a transcript of an agent reaching for something it cannot
// have.
func TestTheProfileNamesEachAdmittedExtensionAndItsReviewedTools(t *testing.T) {
	rendered := Render(nodePackages)
	if _, found := topLevel(t, rendered, "extensions"); !found {
		t.Fatal("the profile names no extensions; PiG then treats every extension as " +
			"unconstrained, which is the state this file exists to leave")
	}
	for _, ext := range nodePackages {
		want := "  - name: " + ext.Name + "\n"
		if !strings.Contains(rendered, want) {
			t.Errorf("the profile does not name %q; its tools are then unconstrained", ext.Name)
			continue
		}
		tools, found := extensionTools(rendered, ext.Name)
		if !found {
			t.Errorf("extension %q is named with no tool list, which PiG reads as \"all of them\"", ext.Name)
			continue
		}
		for _, tool := range ext.Tools {
			if !strings.Contains(tools, tool) {
				t.Errorf("extension %q is not held to the reviewed tool %q (list: %s)", ext.Name, tool, tools)
			}
		}
	}
}

// TestANodeWithNoPackagesGetsNoExtensionsBlock keeps the degenerate case
// honest. An empty list is not the same as no block: `tools: []` on an
// extension means "this extension offers nothing", and writing that for
// extensions the node does not have would be a claim about code that is not
// loaded.
func TestANodeWithNoPackagesGetsNoExtensionsBlock(t *testing.T) {
	if _, found := topLevel(t, Render(nil), "extensions"); found {
		t.Error("a node that admitted no packages wrote an extensions block; " +
			"it is describing extensions it does not have")
	}
}

// TestTwoPackagesCannotClaimOneExtensionName covers the conflict Scopes
// refuses rather than resolves. Merging the two tool lists would produce a
// profile that looks like both packages were reviewed as one, and the agent
// would load whichever extension it reached first.
func TestTwoPackagesCannotClaimOneExtensionName(t *testing.T) {
	_, err := Scopes([]Extension{
		{Name: "opskeeper-sre-readonly", Tools: []string{"get_topology"}},
		{Name: "opskeeper-sre-readonly", Tools: []string{"host_restart_service"}},
	})
	if err == nil {
		t.Fatal("two extensions with one name were accepted; the node would load one of them " +
			"and this profile would describe both")
	}
}

func TestWriteLeavesAReadableProfileAndNothingElse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent", ".pig")
	path, err := Write(dir, nodePackages)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("Write wrote to %s, want it inside %s", path, dir)
	}
	if filepath.Base(path) != FileName {
		t.Errorf("Write named the file %q, want %q", filepath.Base(path), FileName)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the written profile: %v", err)
	}
	if string(body) != Render(nodePackages) {
		t.Error("the written profile differs from Render(nodePackages); a node's profile is a review surface " +
			"and a difference between what is reviewed and what runs defeats it")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the profile: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("mode = %o, want 640; the agent reads it as the user it runs as and nothing else on "+
			"the host has any business editing what runs here", perm)
	}

	// Nothing else is left behind. A staging file that survived a crash
	// would sit in the directory that decides what runs, and the next boot
	// would have to decide what to do with it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the config directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the config directory holds %v, want only %s", names, FileName)
	}
}

func TestWriteIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".pig")
	first, err := Write(dir, nodePackages)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	second, err := Write(dir, nodePackages)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if first != second {
		t.Errorf("Write returned %s then %s; the path has to be stable because it is passed to the "+
			"agent on every spawn", first, second)
	}
	body, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read the profile: %v", err)
	}
	if string(body) != Render(nodePackages) {
		t.Error("a second Write did not reproduce the same profile")
	}
}

func TestWriteRefusesAnEmptyDirectory(t *testing.T) {
	// A node with no agent directory has been misconfigured, and the
	// profile is not optional. Writing it somewhere else would start an
	// agent whose profile nobody can find.
	if path, err := Write("  ", nodePackages); err == nil {
		t.Errorf(`Write("  ", nodePackages) returned %s; an unset agent directory must stop the node`, path)
	}
}
