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

// TestEdgePolicyIsUnaffectedByPerKindCeilings 钉住 S-1 的第一条验收：
// 既有 Policy() 构造点的源码零改动、行为逐位不变。
//
// 节点侧的 PolicyFrom 从部署 profile 渲染策略，它不带 per-kind 条目，
// 于是每个 kind 的生效 ceiling 仍是 profile 给的全局 ceiling。
func TestEdgePolicyIsUnaffectedByPerKindCeilings(t *testing.T) {
	profile, err := pluginmanifest.ProfileForName(string(DefaultProfile))
	if err != nil {
		t.Fatal(err)
	}
	pol := PolicyFrom(profile)
	if len(pol.KindCeilings) != 0 {
		t.Fatalf("PolicyFrom() carries %d per-kind ceilings; want 0 (this construction point must stay unchanged)", len(pol.KindCeilings))
	}
	for _, k := range kernel.Kinds() {
		if got := pol.EffectiveSafetyCeiling(k); got != pol.MaxSafetyLevel {
			t.Fatalf("EffectiveSafetyCeiling(%q) = %s; want %s (no entry ⇒ the global ceiling)",
				k, got, pol.MaxSafetyLevel)
		}
	}
	if got := pol.WidestSafetyCeiling(); got != pol.MaxSafetyLevel {
		t.Fatalf("WidestSafetyCeiling() = %s; want %s", got, pol.MaxSafetyLevel)
	}

	// 准入判定也不变：与 profile ceiling 同级的包过，比它高的包被拒。
	host := kernel.NewHost(kernel.NewRegistry(), pol, nil)
	atCeiling := edgePluginManifest("at-ceiling", pol.MaxSafetyLevel, pol.GrantedScopes)
	if err := host.Admit(atCeiling); err != nil {
		t.Fatalf("a package at the profile ceiling must be admitted: %v", err)
	}
	if pol.MaxSafetyLevel.Rank() < domain.SafetyL3.Rank() {
		above := edgePluginManifest("above", domain.SafetyL3, pol.GrantedScopes)
		if err := host.Admit(above); err == nil {
			t.Fatal("a package above the profile ceiling must be refused")
		}
	}
}

// edgePluginManifest 造一份与节点侧 profile 准入对得上的最小 manifest。
func edgePluginManifest(name string, level domain.SafetyLevel, scopes domain.Scopes) pluginmanifest.Plugin {
	return pluginmanifest.Plugin{
		Manifest: domain.PluginManifest{
			APIVersion: domain.PluginAPIVersion,
			Kind:       domain.PluginKind,
			Metadata:   domain.PluginMeta{Name: name, Version: "1.0.0"},
			Spec: domain.PluginSpec{
				Targets:        domain.Targets{domain.TargetEdge},
				SafetyLevel:    level,
				Capabilities:   []domain.ToolClass{domain.ClassRead},
				RequiredScopes: scopes,
			},
		},
	}
}
