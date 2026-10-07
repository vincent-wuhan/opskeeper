package pigprofile

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/coding/piglet"
)

// The shipped ops piglet is the first-party composition of the node agent:
// the read-only toolset, the observability toolset, the gate courier, and
// the skills that use them. It is what a developer runs to get the ops
// agent on a workstation, and it is the reference the node's own generated
// profile is read against — so the properties asserted here are the ones
// the two files are supposed to agree on, and they are asserted with PiG's
// own parser, resolver and scoping function rather than by reading the
// schema.
//
// The companion test lives in core/floor/pluginmanifest, and it is split
// across two modules because of this repository's own rules rather than by
// preference: the governance manifests are read by the root module, which
// may not import PiG, and this module may not import the control plane's
// packages. Each side asserts the half it can see, over the same file.
const opsPigletRel = "../../../plugins/pig-ops/opskeeper-sre-readonly/pig-opskeeper-ops.yaml"

// personas are the seven worker personas the plan names, plus the two
// skill documents that ship beside them. A composition that dropped one
// would still start and still answer questions — just without the
// procedure that knows what it is doing.
var personas = []string{
	"diagnose-readonly",
	"opskeeper-alerter",
	"opskeeper-critic",
	"opskeeper-investigator",
	"opskeeper-postmortem",
	"opskeeper-repairer",
	"opskeeper-reviewer",
	"opskeeper-verifier",
}

func parseShippedOpsPiglet(t *testing.T) *piglet.Piglet {
	t.Helper()
	path, err := filepath.Abs(opsPigletRel)
	if err != nil {
		t.Fatalf("resolve the piglet path: %v", err)
	}
	p, err := piglet.Parse(path)
	if err != nil {
		t.Fatalf("PiG refused the shipped ops piglet: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("PiG's own validation refused the shipped ops piglet: %v", err)
	}
	return p
}

func TestPiGAcknowledgesTheShippedOpsPiglet(t *testing.T) {
	p := parseShippedOpsPiglet(t)

	if p.Name != "opskeeper-ops" {
		t.Errorf("name = %q, want opskeeper-ops", p.Name)
	}
	// Omitted and empty are different, and only one of them is safe: an
	// omitted `tools` means PiG's stock built-ins, which include a shell.
	if p.BuiltinTools == nil {
		t.Fatal(`PiG read the piglet with no "tools" field; omitted means PiG's stock built-ins`)
	}
	if len(*p.BuiltinTools) != 0 {
		t.Errorf("PiG read tools = %v, want an empty list", *p.BuiltinTools)
	}
	if p.Discovery == nil || p.Discovery.Extensions == nil || p.Discovery.Skills == nil {
		t.Error("PiG did not read an empty discovery block; ambient skills would load unchecked")
	}
	if len(p.Extensions) == 0 || len(p.Skills) == 0 {
		t.Fatalf("the composition declares %d extensions and %d skills; it is supposed to be a composition",
			len(p.Extensions), len(p.Skills))
	}
}

// Every origin is a relative path into the plugin fleet. A rename on either
// side leaves a piglet that parses and then resolves nothing, which is why
// the resolution is done by PiG's resolver rather than by a glob here.
func TestTheShippedOpsPigletResolvesEveryOriginThroughPiG(t *testing.T) {
	p := parseShippedOpsPiglet(t)

	skills, skillErrs := piglet.ResolveSkills(p)
	if len(skillErrs) > 0 {
		t.Errorf("PiG could not resolve the piglet's skills: %v", skillErrs)
	}
	if len(skills) != len(p.Skills) {
		t.Errorf("resolved %d of %d declared skills", len(skills), len(p.Skills))
	}
	for _, s := range skills {
		info, err := os.Stat(s.Path)
		if err != nil {
			t.Errorf("skill %q resolves to %s, which does not exist: %v", s.Entry.Name, s.Path, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("skill %q resolves to %s, which is not a directory", s.Entry.Name, s.Path)
			continue
		}
		// One entry, one skill: a path holding several definitions is a
		// composition that resolves and then fails to bake.
		if _, err := os.Stat(filepath.Join(s.Path, "SKILL.md")); err != nil {
			t.Errorf("skill %q resolves to %s, which holds no SKILL.md", s.Entry.Name, s.Path)
		}
		if filepath.Base(s.Path) != s.Entry.Name {
			t.Errorf("skill %q resolves to %s; the entry name and the directory name must agree, "+
				"because a renamed directory is otherwise a silently different skill", s.Entry.Name, s.Path)
		}
	}

	exts, extErrs := piglet.ResolveExtensions(p)
	if len(extErrs) > 0 {
		t.Errorf("PiG could not resolve the piglet's extensions: %v", extErrs)
	}
	if len(exts) != len(p.Extensions) {
		t.Errorf("resolved %d of %d declared extensions", len(exts), len(p.Extensions))
	}
	for _, e := range exts {
		if _, err := os.Stat(filepath.Join(e.Path, "go.mod")); err != nil {
			t.Errorf("extension %q resolves to %s, which is not a buildable extension (no go.mod): %v",
				e.Entry.Name, e.Path, err)
		}
	}
}

// The scope of the composition is what the extensions register, and nothing
// else. Both directions matter: a tool the package registers and the piglet
// forgot is a capability that silently disappeared from the agent, and a
// name in the piglet that nothing registers is a promise the composition
// cannot keep.
func TestTheShippedOpsPigletScopesToExactlyWhatItsExtensionsRegister(t *testing.T) {
	p := parseShippedOpsPiglet(t)
	registered, declared := registeredTools(t, p)

	active := piglet.ScopeTools(p, registered)
	slices.Sort(active)

	if slices.Contains(active, "bash") {
		t.Error("bash survived the composition; this process holds the operator's credentials")
	}
	for _, name := range builtinNames {
		if slices.Contains(active, name) {
			t.Errorf("built-in %q survived the composition", name)
		}
	}
	if !slices.Equal(active, declared) {
		t.Errorf("active tools = %v, want exactly what the composition declares (%v)", active, declared)
	}
}

// registeredTools builds what a runtime would hand to ScopeTools: PiG's
// built-ins plus every tool the composed extensions register, attributed to
// the extension that registers it. The names are read out of the extension
// sources rather than written down here, so renaming a tool in a package is
// a failure in this test instead of a fixture that quietly stopped
// matching reality.
func registeredTools(t *testing.T, p *piglet.Piglet) (registered []piglet.ToolInfo, declared []string) {
	t.Helper()
	exts, errs := piglet.ResolveExtensions(p)
	if len(errs) > 0 {
		t.Fatalf("resolve extensions: %v", errs)
	}
	byName := make(map[string]string, len(exts))
	for _, e := range exts {
		byName[e.Entry.Name] = e.Path
	}
	for _, info := range builtinInfos() {
		registered = append(registered, info)
	}
	for _, entry := range p.Extensions {
		path, ok := byName[entry.Name]
		if !ok {
			t.Fatalf("extension %q did not resolve", entry.Name)
		}
		offered := stringLiteralsForField(t, filepath.Join(path, "tools.go"), "Name")
		if entry.Tools == nil {
			t.Fatalf("extension %q declares no tool list; an omitted list means every tool it registers", entry.Name)
		}
		if !slices.Equal(sortedCopy(*entry.Tools), sortedCopy(offered)) {
			t.Errorf("extension %q: the piglet allows %v, the extension registers %v",
				entry.Name, sortedCopy(*entry.Tools), sortedCopy(offered))
		}
		for _, name := range offered {
			registered = append(registered, piglet.ToolInfo{Name: name, Source: entry.Name})
		}
		declared = append(declared, *entry.Tools...)
	}
	slices.Sort(declared)
	return registered, declared
}

func builtinInfos() []piglet.ToolInfo {
	out := make([]piglet.ToolInfo, 0, len(builtinNames))
	for _, name := range builtinNames {
		out = append(out, piglet.ToolInfo{Name: name, Source: "builtin"})
	}
	return out
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// stringLiteralsForField collects every `Field: "literal"` in a Go file.
//
// It is a scrape of the packaged extension's own source, which is the only
// description of the tool inventory that lives next to the code that
// registers it. An extension with no such file contributes nothing, which
// is the right answer for the gate courier: it registers no tools, it only
// listens for tool_call.
func stringLiteralsForField(t *testing.T, path, field string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != field {
			return true
		}
		lit, ok := kv.Value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("%s: unquote %s: %v", path, lit.Value, err)
		}
		out = append(out, value)
		return true
	})
	slices.Sort(out)
	return slices.Compact(out)
}

// The skill set is the union of what the composed packages ship. A persona
// that was dropped here is a procedure the agent no longer follows, which
// shows up in production as an agent that improvises.
func TestTheShippedOpsPigletNamesEverySkillItsPackagesShip(t *testing.T) {
	p := parseShippedOpsPiglet(t)
	skills, errs := piglet.ResolveSkills(p)
	if len(errs) > 0 {
		t.Fatalf("resolve skills: %v", errs)
	}
	declared := make([]string, 0, len(skills))
	for _, s := range skills {
		declared = append(declared, s.Entry.Name)
	}
	slices.Sort(declared)

	shipped := shippedSkills(t, p)
	if !slices.Equal(declared, shipped) {
		t.Errorf("the piglet names %v but its packages ship %v", declared, shipped)
	}
	for _, persona := range personas {
		if !slices.Contains(declared, persona) {
			t.Errorf("the composition is missing the %q skill", persona)
		}
	}
}

// shippedSkills walks the `skills/` directory of the package the
// composition lives in.
//
// The walk is relative to the piglet file, which is also the anchor PiG
// resolves every origin against — so this reads the same directory the
// runtime does rather than a second list of names kept here.
func shippedSkills(t *testing.T, p *piglet.Piglet) []string {
	t.Helper()
	skillsDir := filepath.Join(filepath.Dir(opsPigletAbs(t)), "skills")
	ents, err := os.ReadDir(skillsDir)
	if err != nil {
		t.Fatalf("read %s: %v", skillsDir, err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

func opsPigletAbs(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(opsPigletRel)
	if err != nil {
		t.Fatalf("resolve the piglet path: %v", err)
	}
	return path
}

// TestAnOmittedToolListStillKeepsTheShell is the negative control for the
// scoping test above: without it, "the composition removed bash" could be
// true because ScopeTools returned nothing for a piglet it did not
// understand.
func TestAnOmittedToolListStillKeepsTheShell(t *testing.T) {
	p := parse(t, "name: control\n")
	active := piglet.ScopeTools(p, registered())
	if !slices.Contains(active, "bash") {
		t.Fatalf("a piglet with no tools field kept no built-ins either (active = %v); the scoping "+
			"test would then pass without proving anything", active)
	}
}

// TestPiGRefusesAnOriginThatEscapesThePigletAnchor is why the shipped
// composition lives inside the package it composes.
//
// PiG anchors a piglet's relative origins at the piglet's own directory and
// refuses one that escapes it, so "the piglet next to the fleet, naming its
// siblings" is not a composition PiG will load at all. A composition that
// needs resources from a second package therefore has to copy them in
// first — which is a fact about the loader, not a preference, and it is
// pinned here so that a future attempt to tidy the layout by moving the
// file up a directory fails in a test that says why.
func TestPiGRefusesAnOriginThatEscapesThePigletAnchor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "escaping.yaml")
	body := "name: escaping\nextensions:\n  - name: elsewhere\n    origins: [local:../elsewhere]\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := piglet.Parse(path)
	if err == nil {
		t.Fatal("PiG accepted an origin that escapes the piglet's directory; the shipped composition's " +
			"location is no longer forced by the loader, and this file's explanation is stale")
	}
	if !strings.Contains(err.Error(), "escapes the Piglet anchor") {
		t.Errorf("PiG refused the origin for a different reason (%v); this test is not pinning the rule "+
			"it claims to pin", err)
	}
}
