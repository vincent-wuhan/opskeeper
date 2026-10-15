package pluginhost

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
)

// writePackage writes a package directory with a pig-ops.yaml into base.
func writePackage(t *testing.T, base, name, manifest string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pig-ops.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func manifestYAML(name, target, level string) string {
	return "apiVersion: opskeeper.io/v1\n" +
		"kind: Plugin\n" +
		"metadata:\n  name: " + name + "\n  version: 1.0.0\n" +
		"spec:\n" +
		"  targets: [" + target + "]\n" +
		"  safety_level: " + level + "\n" +
		"  capabilities: [read]\n" +
		"  audit: {emits: true, mutates: false}\n" +
		"  approval: {required: false}\n" +
		"  install: {strategy: rolling}\n"
}

// contrib registers one datasource so the test can see activation happened.
type contrib struct{ name string }

func (c contrib) PluginName() string { return c.name }
func (c contrib) Register(r kernel.Registrar) error {
	return r.Register(kernel.KindDatasource, "probe-"+c.name, struct{}{})
}

func managerPolicy() kernel.Policy {
	return kernel.Policy{
		Target:         domain.TargetManager,
		MaxSafetyLevel: domain.SafetyL1,
		MaxBlastRadius: domain.RadiusSingleNS,
	}
}

func TestLoadActivatesManagerPackages(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "good", manifestYAML("good", "manager", "L1"))

	h := New(base, managerPolicy(), nil)
	h.RegisterActivator("good", func() kernel.Contribution { return contrib{name: "good"} })

	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rep.Activated) != 1 || rep.Activated[0] != "good" {
		t.Fatalf("Activated = %v; want [good]", rep.Activated)
	}
	if h.Registry().Len() != 1 {
		t.Fatalf("registry has %d entries; want 1", h.Registry().Len())
	}
}

func TestLoadSkipsOtherTargets(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "edge-only", manifestYAML("edge-only", "edge", "L1"))

	h := New(base, managerPolicy(), nil)
	h.RegisterActivator("edge-only", func() kernel.Contribution { return contrib{name: "edge-only"} })

	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0] != "edge-only" {
		t.Fatalf("Skipped = %v; want [edge-only]", rep.Skipped)
	}
	if h.Registry().Len() != 0 {
		t.Fatal("an edge-targeted package must not activate on the manager host")
	}
}

func TestLoadRefusesAboveCeiling(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "big", manifestYAML("big", "manager", "L3"))

	h := New(base, managerPolicy(), nil)
	h.RegisterActivator("big", func() kernel.Contribution { return contrib{name: "big"} })

	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rep.Refused["big"]; !ok {
		t.Fatalf("an L3 package must be refused by an L1 host; report = %+v", rep)
	}
	if h.Registry().Len() != 0 {
		t.Fatal("a refused package must register nothing")
	}
}

func TestLoadReportsPackagesWithNoActivator(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "uncompiled", manifestYAML("uncompiled", "manager", "L1"))

	h := New(base, managerPolicy(), nil)
	// No activator registered: the package is admitted but this build was
	// not compiled to run it.
	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.NoActivator) != 1 || rep.NoActivator[0] != "uncompiled" {
		t.Fatalf("NoActivator = %v; want [uncompiled]", rep.NoActivator)
	}
	if len(rep.Activated) != 0 {
		t.Fatal("a package with no activator must not be reported as activated")
	}
}

func TestOneBadPackageDoesNotStopTheOthers(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "big", manifestYAML("big", "manager", "L3")) // refused
	writePackage(t, base, "good", manifestYAML("good", "manager", "L1"))
	writePackage(t, base, "edge", manifestYAML("edge", "edge", "L1")) // skipped

	h := New(base, managerPolicy(), nil)
	h.RegisterActivator("big", func() kernel.Contribution { return contrib{name: "big"} })
	h.RegisterActivator("good", func() kernel.Contribution { return contrib{name: "good"} })

	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Activated) != 1 || rep.Activated[0] != "good" {
		t.Fatalf("Activated = %v; want [good] — one refusal must not stop the rest", rep.Activated)
	}
	if _, ok := rep.Refused["big"]; !ok {
		t.Fatal("big must be reported refused")
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0] != "edge" {
		t.Fatalf("Skipped = %v; want [edge]", rep.Skipped)
	}
}

func TestLoadOnMissingDirectoryErrors(t *testing.T) {
	h := New(filepath.Join(t.TempDir(), "does-not-exist"), managerPolicy(), nil)
	if _, err := h.Load(context.Background()); err == nil {
		t.Fatal("a missing package directory must be an error, not a silent empty load")
	}
}
