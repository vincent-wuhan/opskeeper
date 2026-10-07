package pluginmanifest

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The shipped ops piglet composes the read-only package into a runnable
// agent. Its security property is "everything this composition can do is a
// read", and the place that claim can be checked is here: the class of a
// tool lives in the governance manifest, which this package owns, and the
// module that may parse the piglet with PiG's own parser (core/pig) is not
// allowed to import this one.
//
// So the file is read twice, once on each side of the module boundary, and
// each side asserts what it can see. This half is the governance half: the
// manifest the node would admit is L1 read-only, and the composition names
// exactly the tools that manifest declares — no more (a tool the package
// never agreed to) and no fewer (a capability that silently vanished from
// the agent).
const opsPigletRel = "../../../plugins/pig-ops/opskeeper-sre-readonly/pig-opskeeper-ops.yaml"

// composition is the part of the piglet this test reads.
//
// It is deliberately not the whole schema and deliberately not strict: the
// piglet's shape is PiG's to define and is validated on the other side of
// the boundary by PiG's own parser. Reading two fields without validating
// the document is the honest description of what happens here.
type composition struct {
	Extensions []struct {
		Name    string   `yaml:"name"`
		Origins []string `yaml:"origins"`
		Tools   []string `yaml:"tools"`
	} `yaml:"extensions"`
	Skills []struct {
		Name    string   `yaml:"name"`
		Origins []string `yaml:"origins"`
	} `yaml:"skills"`
}

func readOpsPiglet(t *testing.T) (composition, string) {
	t.Helper()
	path, err := filepath.Abs(opsPigletRel)
	if err != nil {
		t.Fatalf("resolve the piglet path: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shipped ops piglet: %v", err)
	}
	var c composition
	// The decoder is not strict here on purpose: this test does not own the
	// piglet schema, and a field PiG adds tomorrow must not fail a
	// governance check.
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatalf("the shipped ops piglet is not valid YAML: %v", err)
	}
	if len(c.Extensions) == 0 {
		t.Fatal("the shipped ops piglet declares no extensions")
	}
	return c, filepath.Dir(path)
}

// TestTheShippedOpsPigletIsReadOnly is the claim the composition exists to
// make: the built-in ops agent can look and cannot touch.
func TestTheShippedOpsPigletIsReadOnly(t *testing.T) {
	c, dir := readOpsPiglet(t)

	composed := 0
	for _, ext := range c.Extensions {
		root, ok := packageRoot(dir, ext.Origins)
		if !ok {
			// The gate courier is an extension inside the same package, so
			// it resolves like any other; an origin that does not is a real
			// problem, not a special case.
			t.Errorf("extension %q: no local origin resolved to a package root (%v)", ext.Name, ext.Origins)
			continue
		}
		plugin, err := Load(root)
		if err != nil {
			t.Fatalf("load the package at %s: %v", root, err)
		}
		composed++

		// The package's own ceiling, which the host refuses to exceed.
		if got := plugin.Manifest.HighestCapability(); got != domain.ClassRead {
			t.Errorf("package %s declares capabilities %v (highest %q); a composition that is supposed "+
				"to start read-only must not include a package that can write",
				plugin.Name(), plugin.Manifest.Spec.Capabilities, got)
		}
		if got := plugin.Manifest.Spec.SafetyLevel; got != domain.SafetyL1 {
			t.Errorf("package %s is %s; the read-only composition is the L1 tier, and a package above "+
				"it belongs in a separate, deliberately installed composition", plugin.Name(), got)
		}
		// Every declared tool, not just the ones the piglet names: a tool
		// the package declares is a tool the host will allow, so the class
		// has to be read.
		for _, tool := range plugin.Manifest.Spec.Tools {
			if tool.Class != domain.ClassRead {
				t.Errorf("package %s declares tool %s as %q; this composition is read-only",
					plugin.Name(), tool.Name, tool.Class)
			}
		}
	}
	if composed != len(c.Extensions) {
		t.Errorf("checked %d of %d composed extensions", composed, len(c.Extensions))
	}
}

// TestTheShippedOpsPigletNamesExactlyWhatItsPackageDeclares pins the two
// lists together.
//
// A narrower composition is a legitimate thing to write in general — the
// per-extension list exists so a piglet can hold a capability back — so a
// failure here is not automatically a bug: it is a question about which of
// the two lists is now wrong. For this file the answer is that the built-in
// composition is the whole L1 toolset, so both directions are asserted.
func TestTheShippedOpsPigletNamesExactlyWhatItsPackageDeclares(t *testing.T) {
	c, dir := readOpsPiglet(t)

	for _, ext := range c.Extensions {
		root, ok := packageRoot(dir, ext.Origins)
		if !ok {
			t.Errorf("extension %q: no local origin resolved to a package root (%v)", ext.Name, ext.Origins)
			continue
		}
		plugin, err := Load(root)
		if err != nil {
			t.Fatalf("load the package at %s: %v", root, err)
		}
		// The piglet's entry is keyed by extension name; the manifest is
		// keyed by tool name. The gate courier is not a package tool, so it
		// contributes an empty list here and must not be compared against
		// the package's inventory.
		if len(ext.Tools) == 0 {
			continue
		}
		declared := make([]string, 0, len(plugin.Manifest.Spec.Tools))
		for _, tool := range plugin.Manifest.Spec.Tools {
			declared = append(declared, tool.Name)
		}
		missing, extra := toolsNotMatching(ext.Tools, declared)
		if len(missing) > 0 || len(extra) > 0 {
			t.Errorf("extension %q: the piglet does not name %v and names %v, which the package does not declare\n"+
				"(piglet: %v; package %s: %v)",
				ext.Name, missing, extra, sorted(ext.Tools), plugin.Name(), sorted(declared))
		}
	}
}

// toolsNotMatching reports what each list has that the other lacks.
func toolsNotMatching(listed, declared []string) (missing, extra []string) {
	for _, name := range declared {
		if !slices.Contains(listed, name) {
			missing = append(missing, name)
		}
	}
	for _, name := range listed {
		if !slices.Contains(declared, name) {
			extra = append(extra, name)
		}
	}
	return sorted(missing), sorted(extra)
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// packageRoot resolves the first local origin that points at
// `<root>/extensions/<name>` and returns `<root>`.
func packageRoot(pigletDir string, origins []string) (string, bool) {
	const prefix = "local:"
	for _, origin := range origins {
		if !slices.Contains([]string{prefix}, origin[:min(len(origin), len(prefix))]) {
			continue
		}
		extDir := filepath.Join(pigletDir, filepath.FromSlash(origin[len(prefix):]))
		root := filepath.Dir(filepath.Dir(extDir))
		if _, err := os.Stat(filepath.Join(root, ManifestFile)); err != nil {
			continue
		}
		return root, true
	}
	return "", false
}

// TestToolsNotMatchingCanSayNo is the control: a helper that always
// reported "no difference" would make the pinning test above pass forever.
func TestToolsNotMatchingCanSayNo(t *testing.T) {
	missing, extra := toolsNotMatching([]string{"a", "b"}, []string{"b", "c"})
	if !slices.Equal(missing, []string{"c"}) {
		t.Errorf("missing = %v, want [c] (declared but not listed)", missing)
	}
	if !slices.Equal(extra, []string{"a"}) {
		t.Errorf("extra = %v, want [a] (listed but not declared)", extra)
	}
	if m, e := toolsNotMatching([]string{"a"}, []string{"a"}); len(m) != 0 || len(e) != 0 {
		t.Errorf("equal lists reported %v / %v", m, e)
	}
}
