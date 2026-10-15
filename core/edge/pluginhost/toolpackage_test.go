package pluginhost

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
)

// repoRoot locates the repository root by its tracked markers. It uses the
// same helper the rest of the tree does rather than probing for go.work:
// go.work is gitignored, so a probe for it would pass here and fail (or skip
// green) on a clean clone and in CI.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, ok := reporoot.Find(dir, 8)
	if !ok {
		t.Fatalf("no repository root above %s", dir)
	}
	return root
}

// mustPolicy is the default deployment profile's policy, or a failure —
// a test that silently ran with a zero policy would refuse everything and
// pass for the wrong reason.
func mustPolicy(t *testing.T) kernel.Policy {
	t.Helper()
	pol, err := EdgePolicyFor(DefaultProfile)
	if err != nil {
		t.Fatalf("default profile policy: %v", err)
	}
	return pol
}

func toolManifest(name string) string {
	return "apiVersion: opskeeper.io/v1\n" +
		"kind: Plugin\n" +
		"metadata:\n  name: " + name + "\n  version: 1.0.0\n" +
		"spec:\n" +
		"  targets: [edge]\n" +
		"  safety_level: L1\n" +
		"  capabilities: [read]\n" +
		"  tools:\n" +
		"    - {name: probe_one, class: read}\n" +
		"    - {name: probe_two, class: read}\n" +
		"  audit: {emits: true, mutates: false}\n" +
		"  approval: {required: false}\n" +
		"  install: {strategy: rolling}\n"
}

func TestDeclaredToolsActivateWithoutAnActivator(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "toolpkg", toolManifest("toolpkg"))

	sink := &fakeSink{}
	h := New(base, mustPolicy(t), sink, nil)
	// No activator registered: the package's declared tools are the
	// contribution, so it activates rather than reporting NoActivator.
	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Activated) != 1 || rep.Activated[0] != "toolpkg" {
		t.Fatalf("Activated = %v; want [toolpkg] (a tool package must activate without an activator)", rep.Activated)
	}
	tools := h.Registry().All(kernel.KindTool)
	if len(tools) != 2 {
		t.Fatalf("KindTool entries = %d; want 2", len(tools))
	}
	if tools[0].Name != "probe_one" || tools[1].Name != "probe_two" {
		t.Fatalf("tool names = %q,%q; want probe_one,probe_two (sorted)", tools[0].Name, tools[1].Name)
	}
	if tools[0].Plugin != "toolpkg" {
		t.Fatalf("tool plugin = %q; want toolpkg", tools[0].Plugin)
	}
	// The declaration carries the class, not an implementation.
	decl, ok := tools[0].Impl.(kernel.ToolDeclaration)
	if !ok {
		t.Fatalf("tool impl = %T; want kernel.ToolDeclaration", tools[0].Impl)
	}
	if decl.Class != domain.ClassRead {
		t.Fatalf("declared class = %q; want read", decl.Class)
	}
	// The tool package has no collector, so the sink stays empty.
	if len(sink.registered) != 0 {
		t.Fatalf("sink registered %v; a tool package contributes no collector", sink.registered)
	}
}

func TestPackageWithNeitherActivatorNorToolsIsReported(t *testing.T) {
	base := t.TempDir()
	// A manifest with no tools and no registered activator.
	writePackage(t, base, "empty", "apiVersion: opskeeper.io/v1\nkind: Plugin\n"+
		"metadata:\n  name: empty\n  version: 1.0.0\nspec:\n  targets: [edge]\n"+
		"  safety_level: L1\n  capabilities: [read]\n  audit: {emits: true, mutates: false}\n"+
		"  approval: {required: false}\n  install: {strategy: rolling}\n")

	h := New(base, mustPolicy(t), &fakeSink{}, nil)
	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.NoActivator) != 1 || rep.NoActivator[0] != "empty" {
		t.Fatalf("NoActivator = %v; want [empty]", rep.NoActivator)
	}
}

// TestTheShippedPackageSetIsExactlyWhatTheProfileComposes is the integration
// check, and it is driven by the repository's own policy vocabulary rather
// than by a policy this package invented: each deployment profile states a
// ceiling, a radius and a granted set, and the package set that set exists
// for. A node's active set should be exactly that — which is a claim that
// can only be made by loading the real packages and letting the real gate
// decide.
func TestTheShippedPackageSetIsExactlyWhatTheProfileComposes(t *testing.T) {
	shipped := filepath.Join(repoRoot(t), "plugins", "pig-ops")

	for _, name := range []pluginmanifest.ProfileName{
		pluginmanifest.ProfileFinance,
		pluginmanifest.ProfileSaaS,
	} {
		t.Run(string(name), func(t *testing.T) {
			pol, err := EdgePolicyFor(name)
			if err != nil {
				t.Fatalf("policy: %v", err)
			}
			h := New(shipped, pol, &fakeSink{}, nil)
			rep, err := h.Load(context.Background())
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			profile, err := pluginmanifest.ProfileForName(string(name))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{}
			for _, pkg := range profile.Composes {
				want[pkg] = true
			}
			if len(rep.Activated) != len(want) {
				t.Fatalf("Activated = %v; profile %q composes %v", rep.Activated, name, profile.Composes)
			}
			for _, pkg := range rep.Activated {
				if !want[pkg] {
					t.Fatalf("package %q activated but profile %q does not compose it", pkg, name)
				}
			}
			// Everything the profile does not compose is refused, and the
			// refusal is reported rather than silently skipped — a package
			// nobody chose is different from a package nobody may run.
			for pkg, reason := range rep.Refused {
				if want[pkg] {
					t.Fatalf("package %q is composed by profile %q but was refused: %s", pkg, name, reason)
				}
			}

			// The tool surface is exactly what the composed packages
			// declare, read from the manifests rather than written down.
			manifests, err := pluginmanifest.LoadAll(shipped)
			if err != nil {
				t.Fatal(err)
			}
			wantTools := 0
			for _, m := range manifests {
				if want[m.Name()] {
					wantTools += len(m.Manifest.Spec.Tools)
				}
			}
			got := h.Registry().All(kernel.KindTool)
			if len(got) != wantTools {
				t.Fatalf("KindTool entries = %d; the composed packages declare %d", len(got), wantTools)
			}
			if inv := h.Inventory(); inv.KindCounts[string(kernel.KindTool)] != len(got) {
				t.Fatalf("inventory tool count disagrees with the registry: %d vs %d",
					inv.KindCounts[string(kernel.KindTool)], len(got))
			}
		})
	}
}

// TestAnUnknownProfileRefusesEverything pins the fail-closed edge of a
// misconfigured node: a profile name that does not exist is an error, and the
// zero policy it produces admits nothing.
func TestAnUnknownProfileRefusesEverything(t *testing.T) {
	pol, err := EdgePolicyFor(pluginmanifest.ProfileName("no-such-profile"))
	if err == nil {
		t.Fatal("an unknown profile name must be an error")
	}
	if pol.MaxSafetyLevel != "" {
		t.Fatalf("a failed profile lookup must yield the zero policy, got %+v", pol)
	}

	base := t.TempDir()
	writePackage(t, base, "toolpkg", toolManifest("toolpkg"))
	h := New(base, pol, &fakeSink{}, nil)
	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Activated) != 0 {
		t.Fatalf("a node with no policy admitted %v; it must admit nothing", rep.Activated)
	}
}
