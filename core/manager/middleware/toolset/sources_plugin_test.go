package toolset

import (
	"context"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

func TestSourcesIsDerivedFromTheKernel(t *testing.T) {
	// The literal first-party list and the kernel-derived list must agree.
	// This is the check that keeps the microkernel path from silently
	// dropping a family: if Sources() returned fewer families than the
	// literal, the build's capability set would have shrunk without a diff.
	literal := firstPartySources()
	derived := Sources()
	if len(derived) != len(literal) {
		t.Fatalf("Sources() returned %d families; the first-party literal has %d — a family was dropped by the plugin path",
			len(derived), len(literal))
	}
	for i := range literal {
		if derived[i].Name != literal[i].Name {
			t.Fatalf("Sources()[%d] = %q; want %q (order and set must be preserved)",
				i, derived[i].Name, literal[i].Name)
		}
		if derived[i].Env != literal[i].Env {
			t.Fatalf("Sources()[%d].Env = %q; want %q", i, derived[i].Env, literal[i].Env)
		}
		if derived[i].Wire == nil {
			t.Fatalf("Sources()[%d] (%s) has no Wire; a family without a Wire registers nothing", i, derived[i].Name)
		}
	}
}

func TestThirdPartyFamilyJoinsTheSameRegistry(t *testing.T) {
	// A new adapter family is a contribution, not an edit to the literal.
	// This proves the third-party path: activate a family the first-party
	// set never named and read it back out of the same registry.
	host := kernel.NewHost(kernel.NewRegistry(), adapterPolicy(), nil)

	thirdParty := Source{
		Name: Family("mysql"),
		Env:  "OPSKEEPER_LOOP_MYSQL_DSN",
		Wire: func(context.Context, *middlewareregistry.Registry, string) (func(), error) {
			return func() {}, nil
		},
	}
	if err := host.Activate(context.Background(), adapterManifest(thirdParty.Name), familyPlugin{src: thirdParty}); err != nil {
		t.Fatalf("a third-party family must be admissible through the same gate: %v", err)
	}

	e, ok := host.Registry().Lookup(kernel.KindAdapter, string(thirdParty.Name))
	if !ok {
		t.Fatal("the third-party family must appear in the registry")
	}
	got, ok := e.Impl.(Source)
	if !ok {
		t.Fatal("the registered impl must be a Source")
	}
	if got.Env != thirdParty.Env {
		t.Fatalf("Env = %q; want %q", got.Env, thirdParty.Env)
	}
	if e.Plugin != adapterPluginPrefix+"mysql" {
		t.Fatalf("entry plugin = %q; want %q (the registrar must stamp the family's plugin name)",
			e.Plugin, adapterPluginPrefix+"mysql")
	}
}

func TestFamilyPluginRollsBackByName(t *testing.T) {
	host := kernel.NewHost(kernel.NewRegistry(), adapterPolicy(), nil)
	src := firstPartySources()[0]
	if err := host.Activate(context.Background(), adapterManifest(src.Name), familyPlugin{src: src}); err != nil {
		t.Fatal(err)
	}
	if host.Registry().Len() != 1 {
		t.Fatalf("registry has %d entries; want 1", host.Registry().Len())
	}
	if err := host.Deactivate(context.Background(), adapterPluginPrefix+string(src.Name)); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if host.Registry().Len() != 0 {
		t.Fatalf("registry has %d entries after rollback; want 0", host.Registry().Len())
	}
}

func TestAdapterPolicyAdmitsEveryFirstPartyFamily(t *testing.T) {
	// Every first-party family must clear the policy written for them. A
	// family the policy refuses is one Sources() would silently drop.
	host := kernel.NewHost(kernel.NewRegistry(), adapterPolicy(), nil)
	for _, src := range firstPartySources() {
		if err := host.Admit(adapterManifest(src.Name)); err != nil {
			t.Errorf("first-party family %q is refused by the adapter policy: %v", src.Name, err)
		}
	}
}

// TestAdapterPolicyIsUnaffectedByPerKindCeilings 钉住 S-1 的第一条验收：
// 既有 Policy() 构造点的源码零改动、行为逐位不变。
//
// adapterPolicy() 不带 per-kind 条目，于是每个 kind 的生效 ceiling 仍是
// 全局的 L2，准入判定与改动前逐位相同。
func TestAdapterPolicyIsUnaffectedByPerKindCeilings(t *testing.T) {
	pol := adapterPolicy()
	if len(pol.KindCeilings) != 0 {
		t.Fatalf("adapterPolicy() carries %d per-kind ceilings; want 0 (this construction point must stay unchanged)", len(pol.KindCeilings))
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

	// 准入判定也不变：真实出厂的 adapter family 仍然过。
	host := kernel.NewHost(kernel.NewRegistry(), adapterPolicy(), nil)
	for _, src := range firstPartySources() {
		if err := host.Admit(adapterManifest(src.Name)); err != nil {
			t.Fatalf("first-party family %q must still be admitted: %v", src.Name, err)
		}
	}
}
