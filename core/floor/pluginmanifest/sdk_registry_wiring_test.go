package pluginmanifest

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	// The executors have to be registered for skill.Get to find them.
	_ "github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// The sdk's registration check, run against the packages this repository
// actually ships.
//
// sdk.Registry.Check exists so a plugin author can find out at build time
// that their manifest and their tool table disagree. A check that has only
// ever been run on hand-written fixtures in the sdk's own tests is a check
// nobody has proved works on a real package, and the failure mode is
// silent in both directions: an undeclared tool that the gate refuses on
// every call, or a declared tool nobody registers that is permitted and
// unreachable.
//
// So these tests build the registry from the shipped toolsets — read out
// of the sources the node builds — and run it against the shipped
// manifests. The positive case would pass even if Check returned nil
// unconditionally, so the negative controls below are the real assertions:
// each one breaks one of the three disagreement classes on real data and
// requires Check to name it.

// shippedExtension is one package's toolset, as the node builds it.
type shippedExtension struct {
	pkg  string
	ext  string
	root func(t *testing.T) string
}

func shippedExtensions(t *testing.T) []shippedExtension {
	t.Helper()
	return []shippedExtension{
		{pkg: readOnlyProfile, ext: readOnlyProfile, root: profileRoot},
		{pkg: observabilityProfile, ext: observabilityProfile, root: observabilityRoot},
		{pkg: repairProfile, ext: repairProfile, root: repairProfileRoot},
	}
}

// liveRegistry builds the registry the sdk would have if this package were
// an author running its own build check.
//
// The class is not read off the manifest, because that would make the
// class comparison in Check a tautology. It is read from wherever the node
// would get it at a call site: the executor's own metadata for a tool this
// node runs, and the manifest only for a tool that is a control-plane
// upcall — which the node has no executor for and therefore cannot
// classify independently.
//
// A tool that is neither declared nor executable on this node cannot be
// classified at all, and that is reported rather than guessed: there is no
// class to register it with, and inventing `read` would be the exact
// understatement the class check exists to catch.
func liveRegistry(t *testing.T, ext shippedExtension, m domain.PluginManifest) (*sdk.Registry, error) {
	t.Helper()
	declared := make(map[string]domain.ToolClass, len(m.Spec.Tools))
	for _, tool := range m.Spec.Tools {
		declared[tool.Name] = tool.Class
	}

	shipped := readShippedToolNamesFor(t, ext.root(t), ext.ext)
	if len(shipped) == 0 {
		return nil, fmt.Errorf("no tool names could be read out of %s", ext.ext)
	}

	reg := sdk.NewRegistry(ext.pkg)
	for name := range shipped {
		if exec, ok := skill.Get(name); ok {
			// The limits come from the executor's own metadata, for the
			// same reason the class does: reading them off the manifest
			// would make the comparison in Check a tautology. A ceiling
			// that only one of the two files has is a ceiling the author
			// believes is enforced and the node does not enforce.
			meta := exec.Metadata()
			if err := reg.RegisterWithLimits(name, classOfSkillForTest(meta.EffectiveClass()), meta.Limits); err != nil {
				return nil, err
			}
			continue
		}
		class, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf(
				"the toolset ships %q, the manifest does not declare it, and this node has no executor to classify it; "+
					"declare it in spec.tools so the class is a reviewed decision rather than a guess", name)
		}
		if err := reg.Register(name, class); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// TestTheShippedPackagesPassTheSdkRegistryCheck is the baseline: on the
// packages as they are, the SDK's own check finds nothing.
func TestTheShippedPackagesPassTheSdkRegistryCheck(t *testing.T) {
	checked := 0
	for _, ext := range shippedExtensions(t) {
		ext := ext
		t.Run(ext.pkg, func(t *testing.T) {
			p, err := Load(ext.root(t))
			if err != nil {
				t.Fatalf("Load %s: %v", ext.pkg, err)
			}
			reg, err := liveRegistry(t, ext, p.Manifest)
			if err != nil {
				t.Fatalf("%s: build the registry the way an author would: %v", ext.pkg, err)
			}
			checked++
			if err := reg.Check(p.Manifest); err != nil {
				t.Errorf("%s does not pass the sdk's own registration check: %v", ext.pkg, err)
			}
		})
	}
	if checked == 0 {
		t.Fatal("no shipped package was checked, so this test is vacuous")
	}
}

// TestTheSdkRegistryCheckCatchesAToolTheManifestNeverDeclares is the
// negative control for the direction the model experiences as a broken
// tool: the extension offers something, the allow-list does not name it,
// and every call is refused.
func TestTheSdkRegistryCheckCatchesAToolTheManifestNeverDeclares(t *testing.T) {
	ext := shippedExtensions(t)[0]
	p, err := Load(ext.root(t))
	if err != nil {
		t.Fatalf("Load %s: %v", ext.pkg, err)
	}
	reg, err := liveRegistry(t, ext, p.Manifest)
	if err != nil {
		t.Fatalf("build the registry: %v", err)
	}
	if err := reg.Register("host_undeclared_extra", domain.ClassRead); err != nil {
		t.Fatalf("register the planted tool: %v", err)
	}

	err = reg.Check(p.Manifest)
	if err == nil {
		t.Fatal("Check accepted a tool the manifest never declares; the gate would refuse every call to it")
	}
	if !strings.Contains(err.Error(), "host_undeclared_extra") {
		t.Errorf("Check refused for some other reason and did not name the planted tool: %v", err)
	}
	if !strings.Contains(err.Error(), "registered but not declared") {
		t.Errorf("Check named the tool but not the disagreement: %v", err)
	}
}

// TestTheSdkRegistryCheckCatchesAToolTheManifestDeclaresAndNothingOffers is
// the negative control for the review-integrity direction: the package was
// approved for a capability it never ships.
func TestTheSdkRegistryCheckCatchesAToolTheManifestDeclaresAndNothingOffers(t *testing.T) {
	ext := shippedExtensions(t)[0]
	p, err := Load(ext.root(t))
	if err != nil {
		t.Fatalf("Load %s: %v", ext.pkg, err)
	}

	// Drop one real tool from the registry, leaving it declared.
	shipped := readShippedToolNamesFor(t, ext.root(t), ext.ext)
	names := make([]string, 0, len(shipped))
	for name := range shipped {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no shipped tool could be dropped, so this control is vacuous")
	}
	// Deterministic rather than whichever name the map yielded first: a
	// control that drops a different tool on each run reports a different
	// message on each run, and the message is the assertion here.
	dropped := names[0]
	delete(shipped, dropped)

	reg := sdk.NewRegistry(ext.pkg)
	for name := range shipped {
		if exec, ok := skill.Get(name); ok {
			if err := reg.Register(name, classOfSkillForTest(exec.Metadata().EffectiveClass())); err != nil {
				t.Fatalf("register %q: %v", name, err)
			}
			continue
		}
		if err := reg.Register(name, domain.ClassRead); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}

	err = reg.Check(p.Manifest)
	if err == nil {
		t.Fatalf("Check accepted a manifest declaring %q that the package never registers; "+
			"the review approved a capability that cannot be called", dropped)
	}
	if !strings.Contains(err.Error(), dropped) {
		t.Errorf("Check refused for some other reason and did not name %q: %v", dropped, err)
	}
	if !strings.Contains(err.Error(), "never registers them") {
		t.Errorf("Check named the tool but not the disagreement: %v", err)
	}
}

// TestTheSdkRegistryCheckCatchesAnUnderstatedClassOnARealNodeTool is the
// negative control that matters most, because it is the only one where the
// consequence is a live change nobody approves.
//
// The class in the manifest is what the host's gate uses to decide whether
// a human is asked before the call runs. A package that registers a tool
// as write and declares it as read has not saved an approval round trip;
// it has changed a mutation into something the gate believes is safe.
// host_restart_service is the real tool used here rather than a fixture,
// because a fixture would only prove the sdk compares two strings.
func TestTheSdkRegistryCheckCatchesAnUnderstatedClassOnARealNodeTool(t *testing.T) {
	ext := shippedExtensions(t)[2] // the repair package
	p, err := Load(ext.root(t))
	if err != nil {
		t.Fatalf("Load %s: %v", ext.pkg, err)
	}

	const tool = "host_restart_service"
	exec, ok := skill.Get(tool)
	if !ok {
		t.Fatalf("%q is registered nowhere on this node, so this control cannot run", tool)
	}
	actual := classOfSkillForTest(exec.Metadata().EffectiveClass())
	if actual != domain.ClassWrite {
		t.Fatalf("this node classifies %q as %s; the control needs a write tool to understate", tool, actual)
	}

	reg, err := liveRegistry(t, ext, p.Manifest)
	if err != nil {
		t.Fatalf("build the registry: %v", err)
	}
	// Rewrite the manifest's claim to read, leaving the registration alone.
	rewritten := p.Manifest
	rewritten.Spec.Tools = append(domain.Tools(nil), p.Manifest.Spec.Tools...)
	for i, dt := range rewritten.Spec.Tools {
		if dt.Name == tool {
			rewritten.Spec.Tools[i].Class = domain.ClassRead
		}
	}

	err = reg.Check(rewritten)
	if err == nil {
		t.Fatalf("Check accepted %q declared read while the node classifies it write; "+
			"the gate reads the declaration, so this is an unattended restart", tool)
	}
	if !strings.Contains(err.Error(), tool) {
		t.Errorf("Check refused for some other reason and did not name %q: %v", tool, err)
	}
	if !strings.Contains(err.Error(), "declared") || !strings.Contains(err.Error(), "registered as") {
		t.Errorf("Check named the tool but not the class disagreement: %v", err)
	}
}

// TestTheCanonicalToolsetIsWhatThePackagedCopyOffers guards the assumption
// every test above makes: that readShippedToolNamesFor reads the file the
// node actually builds. It reads the packaged copy, which the sync script
// generates from the canonical source, and the drift test compares the two
// — so a name read here is a name the node sees.
func TestTheCanonicalToolsetIsWhatThePackagedCopyOffers(t *testing.T) {
	for _, ext := range shippedExtensions(t) {
		ext := ext
		t.Run(ext.pkg, func(t *testing.T) {
			// core/pig is the pkgRoot here: the helper joins
			// "extensions" onto it, and the canonical sources live at
			// core/pig/extensions/<ext>/tools.go.
			canonical := readShippedToolNamesFor(t,
				filepath.Join(repoRoot(t), "core", "pig"), ext.ext)
			packaged := readShippedToolNamesFor(t, ext.root(t), ext.ext)
			if len(canonical) == 0 || len(packaged) == 0 {
				t.Fatalf("%s: no tool names read (canonical %d, packaged %d)",
					ext.pkg, len(canonical), len(packaged))
			}
			if len(canonical) != len(packaged) {
				t.Errorf("%s: canonical offers %d tools, the packaged copy offers %d",
					ext.pkg, len(canonical), len(packaged))
			}
			for name := range canonical {
				if !packaged[name] {
					t.Errorf("%s: the canonical toolset offers %q and the packaged copy does not", ext.pkg, name)
				}
			}
		})
	}
}
