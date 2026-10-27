package toolset

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

func TestSourcesIsDerivedFromTheKernel(t *testing.T) {
	// Three properties, checked separately because they fail for different
	// reasons. A family the gate silently drops is invisible in a count that
	// is compared against a literal, and a family that is present but out of
	// order is a different bug with the same symptom at boot (the wrong
	// adapter wired first).
	literal := firstPartySources()
	derived := Sources()

	// (1) Every first-party family appears, exactly once. A family missing
	// here is one the host refused — the failure that shrinks the build's
	// capability set without a diff anywhere else.
	seen := map[Family]int{}
	for _, s := range derived {
		seen[s.Name]++
	}
	for _, src := range literal {
		switch n := seen[src.Name]; {
		case n == 0:
			t.Errorf("family %q is missing from Sources() — the gate dropped it", src.Name)
		case n > 1:
			t.Errorf("family %q appears %d times in Sources()", src.Name, n)
		}
	}

	// (2) Relative order: the first-party families come first, in
	// firstPartySources() order. The list may be longer than the literal — a
	// contributed family is allowed to follow — but nothing may be reordered
	// ahead of it.
	if len(derived) < len(literal) {
		t.Fatalf("Sources() returned %d families; the first-party literal has %d", len(derived), len(literal))
	}
	for i, src := range literal {
		if derived[i].Name != src.Name {
			t.Errorf("Sources()[%d] = %q; want %q (first-party order is the boot-wiring order)",
				i, derived[i].Name, src.Name)
		}
	}

	// (3) Each first-party entry is the literal's own contribution, not a
	// placeholder: the DSN variable the closed loop reads must be the one
	// firstPartySources() names, and a nil Wire would register nothing.
	for i, src := range literal {
		if derived[i].Env != src.Env {
			t.Errorf("Sources()[%d].Env = %q; want %q", i, derived[i].Env, src.Env)
		}
		if derived[i].Wire == nil {
			t.Errorf("Sources()[%d] (%s) has no Wire; a family without a Wire registers nothing", i, derived[i].Name)
		}
	}
}

func TestAThirdPartyFamilyAppearsInSources(t *testing.T) {
	// The bug this slice fixes: a contributed family activated into the
	// registry and then dropped on readback. It must now be a member of the
	// list, and it must be ordered after the first-party block — the
	// first-party prefix is a boot contract, not a suggestion.
	contributed := Family("exampleops")
	assertNoFamilyNameCollision(t, contributed)

	got := sourcesFrom([]Source{fakeSource(contributed, "OPSKEEPER_LOOP_EXAMPLEOPS_DSN")})

	idx := indexOfSource(got, contributed)
	if idx < 0 {
		t.Fatalf("family %q was activated but is absent from the result: %v", contributed, familyNames(got))
	}
	if want := len(firstPartySources()); idx < want {
		t.Errorf("family %q sits at %d; it must follow the first-party block of %d", contributed, idx, want)
	}

	// Same input, same list. The ordering is a contract, so it cannot depend
	// on registry internals or on map iteration.
	again := sourcesFrom([]Source{fakeSource(contributed, "OPSKEEPER_LOOP_EXAMPLEOPS_DSN")})
	if !sameSources(got, again) {
		t.Errorf("two calls with the same input returned different lists: %v then %v",
			familyNames(got), familyNames(again))
	}
}

func TestContributedFamiliesAreOrderedByFamilyName(t *testing.T) {
	// Deliberately passed in the opposite order to the expected result, so
	// this fails if the implementation returns its input order rather than
	// sorting.
	first, second := Family("exampleops"), Family("deltacloud")
	assertNoFamilyNameCollision(t, first)
	assertNoFamilyNameCollision(t, second)

	got := sourcesFrom([]Source{
		fakeSource(first, "OPSKEEPER_LOOP_EXAMPLEOPS_DSN"),
		fakeSource(second, "OPSKEEPER_LOOP_DELTACLOUD_DSN"),
	})

	i, j := indexOfSource(got, first), indexOfSource(got, second)
	if i < 0 || j < 0 {
		t.Fatalf("both contributed families must be in the result: %v", familyNames(got))
	}
	if j >= i {
		t.Errorf("%q is at %d and %q at %d; want %q first (lexicographic by family name)",
			first, i, second, j, second)
	}
}

func TestAFirstPartyFamilyIsNotShadowedByAContributedOneOfTheSameName(t *testing.T) {
	// Contributed families go through the same Activate as the first-party
	// ones, so a name collision is refused by the registry rather than
	// resolved by whoever was activated last. Without this the readback could
	// return two entries for one family, and the boot path would wire it twice.
	clash := FamilyPostgres
	got := sourcesFrom([]Source{
		fakeSource(clash, "OPSKEEPER_LOOP_PG_DSN_SQUATTER"),
	})

	if n := countSource(got, clash); n != 1 {
		t.Errorf("family %q appears %d times; want exactly 1 (the first-party contribution wins)", clash, n)
	}
	if got[0].Env != firstPartySources()[0].Env {
		t.Errorf("family %q resolves to Env %q; the first-party contribution must win",
			clash, got[0].Env)
	}
}

func TestTheDefaultGrantSetCoversTheReadScopesContributedFamiliesNeed(t *testing.T) {
	// The grant set grew by exactly two read scopes. The rest is a host-side
	// value the first-party families already relied on; a reordering or a
	// rename inside that prefix would be a silent change to what every
	// installed build grants.
	baseline := domain.Scopes{
		domain.ScopeDBRead, domain.ScopeDBWrite,
		domain.ScopeK8sRead, domain.ScopeK8sExec,
		domain.ScopeMQRead, domain.ScopeMQWrite,
		domain.ScopeHostRead, domain.ScopeHostWrite,
	}
	got := defaultAdapterScopes()

	want := len(baseline) + 2
	if len(got) != want {
		t.Errorf("default grant set has %d scopes; want %d", len(got), want)
	}
	for i, s := range baseline {
		if got[i] != s {
			t.Errorf("defaultAdapterScopes()[%d] = %q; want %q (the first %d are unchanged)",
				i, got[i], s, len(baseline))
		}
	}
	for _, s := range []domain.Scope{domain.ScopeTopologyRO, domain.ScopeCloudRead} {
		if !got.Has(s) {
			t.Errorf("default grant set is missing %q", s)
		}
	}
}

func TestAReaderOfTheNewScopesIsAdmittedByTheDefaultPolicy(t *testing.T) {
	host := kernel.NewHost(kernel.NewRegistry(), adapterPolicy(), nil)
	for _, s := range []domain.Scope{domain.ScopeCloudRead, domain.ScopeTopologyRO} {
		p := adapterManifest(Family("exampleops"))
		p.Manifest.Spec.RequiredScopes = domain.Scopes{s}
		if err := host.Admit(p); err != nil {
			t.Errorf("a manifest requiring only %q is refused by the default policy: %v", s, err)
		}
	}
}

func TestTheOldGrantSetRefusesAReaderOfANewScope(t *testing.T) {
	// A red light is the correct result here. This is the regression guard for
	// the whole reason GrantedScopes is a host-side value: if a future edit
	// "fixes" this by relaxing the gate, the check has been removed instead of
	// moved, and the failure would be silent everywhere.
	baseline := domain.Scopes{
		domain.ScopeDBRead, domain.ScopeDBWrite,
		domain.ScopeK8sRead, domain.ScopeK8sExec,
		domain.ScopeMQRead, domain.ScopeMQWrite,
		domain.ScopeHostRead, domain.ScopeHostWrite,
	}
	host := kernel.NewHost(kernel.NewRegistry(), AdapterPolicy(baseline), nil)

	p := adapterManifest(Family("exampleops"))
	p.Manifest.Spec.RequiredScopes = domain.Scopes{domain.ScopeCloudRead}

	err := host.Admit(p)
	if err == nil {
		t.Fatal("the old grant set admitted a cloud reader; the gate is no longer fail-closed")
	}
	if !strings.Contains(err.Error(), string(domain.ScopeCloudRead)) {
		t.Errorf("error %q does not name the missing scope %q", err, domain.ScopeCloudRead)
	}
}

func TestThePolicyCeilingIsIndependentOfTheGrantSet(t *testing.T) {
	// Widening scopes is the only thing a host may tune; the safety level and
	// the blast radius are the host's backstop and must not become a knob.
	base := AdapterPolicy(defaultAdapterScopes())
	empty := AdapterPolicy(nil)
	if base.MaxSafetyLevel != domain.SafetyL2 || base.MaxBlastRadius != domain.RadiusSingleNS {
		t.Errorf("AdapterPolicy changed the ceiling: safety=%s radius=%s",
			base.MaxSafetyLevel, base.MaxBlastRadius)
	}
	if empty.MaxSafetyLevel != base.MaxSafetyLevel || empty.MaxBlastRadius != base.MaxBlastRadius {
		t.Errorf("the ceiling varies with the grant set: %+v vs %+v", empty, base)
	}
	if empty.Target != domain.TargetManager {
		t.Errorf("Target = %q; want %q", empty.Target, domain.TargetManager)
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
// fakeSource is a contributed family that registers nothing real. The Wire
// signature is the real one so the boot path can call it, but no backend is
// constructed.
func fakeSource(name Family, env string) Source {
	return Source{
		Name: name,
		Env:  env,
		Wire: func(context.Context, *middlewareregistry.Registry, string) (func(), error) {
			return func() {}, nil
		},
	}
}

// assertNoFamilyNameCollision checks the assumption the ordering tests rest
// on: family names are prefixes of tool names ("pg.x" vs "pgx.y"), so a
// contributed name that is a prefix of a first-party one — or the reverse —
// would change what ParseFamily resolves for existing tools.
func assertNoFamilyNameCollision(t *testing.T, name Family) {
	t.Helper()
	for _, fp := range firstPartySources() {
		if strings.HasPrefix(string(fp.Name), string(name)) || strings.HasPrefix(string(name), string(fp.Name)) {
			t.Fatalf("test family %q and first-party family %q are prefix-related", name, fp.Name)
		}
	}
}

func indexOfSource(srcs []Source, name Family) int {
	for i, s := range srcs {
		if s.Name == name {
			return i
		}
	}
	return -1
}

func countSource(srcs []Source, name Family) int {
	n := 0
	for _, s := range srcs {
		if s.Name == name {
			n++
		}
	}
	return n
}

func familyNames(srcs []Source) []string {
	out := make([]string, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, string(s.Name))
	}
	return out
}

func sameSources(a, b []Source) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Env != b[i].Env {
			return false
		}
	}
	return true
}
