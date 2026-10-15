package pluginhost

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/plugins"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// ── fakes ───────────────────────────────────────────────────────────────────

type fakeCollector struct{ name string }

func (f fakeCollector) Name() string                         { return f.name }
func (f fakeCollector) Configure(plugins.PluginConfig) error { return nil }
func (f fakeCollector) Start(context.Context) error          { return nil }
func (f fakeCollector) Stop(context.Context) error           { return nil }
func (f fakeCollector) HealthSnapshot() plugins.PluginHealth {
	return plugins.PluginHealth{Name: f.name}
}

// nopRegistrar satisfies kernel.Registrar without a registry, so a
// contribution can be exercised in isolation.
type nopRegistrar struct{}

func (nopRegistrar) Register(kernel.Kind, string, any) error { return nil }

type fakeSink struct{ registered []string }

func (s *fakeSink) Register(p plugins.Plugin) { s.registered = append(s.registered, p.Name()) }

// ── helpers ─────────────────────────────────────────────────────────────────

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

func collectorActivator(name, signal string) Activator {
	return func() kernel.Contribution {
		return CollectorContribution{Plugin: name, Collector: fakeCollector{name: signal}}
	}
}

// ── tests ───────────────────────────────────────────────────────────────────

func TestLoadActivatesCollectorAndRegistersIt(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "vec", manifestYAML("vec", "edge", "L1"))

	sink := &fakeSink{}
	h := New(base, mustPolicy(t), sink, nil)
	h.RegisterActivator("vec", collectorActivator("vec", "logs"))

	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rep.Activated) != 1 || rep.Activated[0] != "vec" {
		t.Fatalf("Activated = %v; want [vec]", rep.Activated)
	}
	if len(sink.registered) != 1 || sink.registered[0] != "logs" {
		t.Fatalf("sink registered %v; want [logs]", sink.registered)
	}
}

func TestLoadSkipsManagerPackages(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "manager-only", manifestYAML("manager-only", "manager", "L1"))

	sink := &fakeSink{}
	h := New(base, mustPolicy(t), sink, nil)
	h.RegisterActivator("manager-only", collectorActivator("manager-only", "logs"))

	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0] != "manager-only" {
		t.Fatalf("Skipped = %v; want [manager-only]", rep.Skipped)
	}
	if len(sink.registered) != 0 {
		t.Fatal("a manager-targeted package must not register a collector on a node")
	}
}

func TestCollectorAboveCeilingIsRefused(t *testing.T) {
	base := t.TempDir()
	// A collector package that declares L3 is refused by the L1 node ceiling:
	// a collector produces telemetry and nothing else.
	writePackage(t, base, "greedy", manifestYAML("greedy", "edge", "L3"))

	sink := &fakeSink{}
	h := New(base, mustPolicy(t), sink, nil)
	h.RegisterActivator("greedy", collectorActivator("greedy", "logs"))

	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rep.Refused["greedy"]; !ok {
		t.Fatalf("an L3 collector package must be refused; report = %+v", rep)
	}
	if len(sink.registered) != 0 {
		t.Fatal("a refused package must register nothing")
	}
}

func TestContributionWithoutCollectorErrors(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "empty", manifestYAML("empty", "edge", "L1"))

	h := New(base, mustPolicy(t), &fakeSink{}, nil)
	// A contribution that registers no collector must fail activation, not
	// silently activate with nothing behind it.
	h.RegisterActivator("empty", func() kernel.Contribution {
		return CollectorContribution{Plugin: "empty"}
	})

	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Activated) != 0 {
		t.Fatalf("Activated = %v; want empty (a collector-less contribution must fail)", rep.Activated)
	}
	if _, ok := rep.Refused["empty"]; !ok {
		t.Fatalf("the failure must be reported as a refusal; report = %+v", rep)
	}
}

func TestCollectorSetIsStableOrder(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "c", manifestYAML("c", "edge", "L1"))
	writePackage(t, base, "a", manifestYAML("a", "edge", "L1"))
	writePackage(t, base, "b", manifestYAML("b", "edge", "L1"))

	sink := &fakeSink{}
	h := New(base, mustPolicy(t), sink, nil)
	h.RegisterActivator("a", collectorActivator("a", "zzz"))
	h.RegisterActivator("b", collectorActivator("b", "mmm"))
	h.RegisterActivator("c", collectorActivator("c", "aaa"))

	if _, err := h.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The sink order is the registry's (by collector name), not the package
	// order — so a listing is stable across boots.
	want := []string{"aaa", "mmm", "zzz"}
	for i := range want {
		if sink.registered[i] != want[i] {
			t.Fatalf("registered[%d] = %q; want %q (stable order)", i, sink.registered[i], want[i])
		}
	}
}

func TestCollectorContributionRejectsNilCollector(t *testing.T) {
	var c CollectorContribution
	if err := c.Register(nopRegistrar{}); err == nil {
		t.Fatal("a nil collector must be refused at registration")
	}
}

func TestDefaultPolicyComesFromTheProfile(t *testing.T) {
	pol, err := EdgePolicyFor(DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if pol.Target != domain.TargetEdge {
		t.Fatalf("Target = %q; want edge", pol.Target)
	}
	// The ceiling is the profile's, not this package's. That is the whole
	// fix: a policy written here made a real node refuse every shipped
	// package, because it was a statement about one person's guess at what
	// a node runs rather than about what the fleet said it will host.
	profile, err := pluginmanifest.ProfileForName(string(DefaultProfile))
	if err != nil {
		t.Fatal(err)
	}
	if pol.MaxSafetyLevel != profile.Policy().MaxSafetyLevel {
		t.Fatalf("ceiling = %q; want the profile's %q (the policy must be the profile's, not a local constant)",
			pol.MaxSafetyLevel, profile.Policy().MaxSafetyLevel)
	}
	if pol.MaxBlastRadius != profile.Policy().MaxBlastRadius {
		t.Fatalf("max radius = %q; want the profile's %q",
			pol.MaxBlastRadius, profile.Policy().MaxBlastRadius)
	}
}
