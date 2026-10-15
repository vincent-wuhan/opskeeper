package kernel

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// manifest builds a minimal, structurally valid plugin manifest aimed at one
// target with one safety level. It is the fixture the host's admission tests
// vary one field at a time.
func manifest(name string, target domain.DeploymentTarget, level domain.SafetyLevel, scopes domain.Scopes) pluginmanifest.Plugin {
	return pluginmanifest.Plugin{
		Manifest: domain.PluginManifest{
			APIVersion: domain.PluginAPIVersion,
			Kind:       domain.PluginKind,
			Metadata:   domain.PluginMeta{Name: name, Version: "1.0.0"},
			Spec: domain.PluginSpec{
				Targets:        domain.Targets{target},
				SafetyLevel:    level,
				Capabilities:   []domain.ToolClass{domain.ClassRead},
				RequiredScopes: scopes,
			},
		},
	}
}

// contrib is a test contribution: it registers n datasources, and can be told
// to fail after the first registration to exercise the rollback path.
type contrib struct {
	name     string
	entries  []Entry
	failAt   int // fail when registering the entry at this index; -1 = never
	regCount int
}

func (c *contrib) PluginName() string { return c.name }

func (c *contrib) Register(r Registrar) error {
	for i, e := range c.entries {
		if c.failAt >= 0 && i == c.failAt {
			return errors.New("contribution: deliberate failure")
		}
		if err := r.Register(e.Kind, e.Name, e.Impl); err != nil {
			return err
		}
		c.regCount++
	}
	return nil
}

func dsEntry(name string) Entry {
	return Entry{Kind: KindDatasource, Name: name, Impl: "impl-" + name}
}

// ── registry ────────────────────────────────────────────────────────────────

func TestRegisterAndLookup(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(Entry{Kind: KindDatasource, Name: "prom", Plugin: "p1", Impl: 1}); err != nil {
		t.Fatalf("register: %v", err)
	}
	e, ok := reg.Lookup(KindDatasource, "prom")
	if !ok {
		t.Fatal("prom should be registered")
	}
	if e.Plugin != "p1" || e.Impl != 1 {
		t.Fatalf("entry = %+v; want plugin p1 impl 1", e)
	}
	if _, ok := reg.Lookup(KindDatasource, "absent"); ok {
		t.Fatal("absent name must not resolve")
	}
	// A name is unique within a kind but not across kinds.
	if err := reg.Register(Entry{Kind: KindTool, Name: "prom", Plugin: "p1"}); err != nil {
		t.Fatalf("same name in a different kind must be allowed: %v", err)
	}
}

func TestRegisterRefusesDuplicate(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(Entry{Kind: KindDatasource, Name: "prom", Plugin: "p1"}); err != nil {
		t.Fatal(err)
	}
	err := reg.Register(Entry{Kind: KindDatasource, Name: "prom", Plugin: "p2"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v; want ErrDuplicate", err)
	}
	// The original must survive a refused takeover.
	e, _ := reg.Lookup(KindDatasource, "prom")
	if e.Plugin != "p1" {
		t.Fatalf("holder = %q; want p1 (a refused duplicate must not take over)", e.Plugin)
	}
}

func TestRegisterRefusesUnknownKind(t *testing.T) {
	reg := NewRegistry()
	err := reg.Register(Entry{Kind: Kind("teleporter"), Name: "x", Plugin: "p1"})
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("err = %v; want ErrUnknownKind", err)
	}
}

func TestRegisterRefusesEmptyName(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(Entry{Kind: KindTool, Name: "", Plugin: "p1"}); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("err = %v; want ErrEmptyName", err)
	}
}

func TestAllIsSorted(t *testing.T) {
	reg := NewRegistry()
	for _, n := range []string{"tempo", "loki", "prom"} {
		_ = reg.Register(Entry{Kind: KindDatasource, Name: n, Plugin: "p1"})
	}
	_ = reg.Register(Entry{Kind: KindTool, Name: "zzz", Plugin: "p1"})

	got := reg.All(KindDatasource)
	want := []string{"loki", "prom", "tempo"}
	if len(got) != len(want) {
		t.Fatalf("len = %d; want %d", len(got), len(want))
	}
	for i, e := range got {
		if e.Name != want[i] {
			t.Fatalf("All[%d] = %q; want %q (order must be stable)", i, e.Name, want[i])
		}
	}
}

func TestByPluginAndRemoveIsIdempotent(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(Entry{Kind: KindDatasource, Name: "prom", Plugin: "obs"})
	_ = reg.Register(Entry{Kind: KindDatasource, Name: "loki", Plugin: "obs"})
	_ = reg.Register(Entry{Kind: KindTool, Name: "query_promql", Plugin: "obs"})

	if got := len(reg.ByPlugin("obs")); got != 3 {
		t.Fatalf("ByPlugin = %d entries; want 3", got)
	}
	if n := reg.RemovePlugin("obs"); n != 3 {
		t.Fatalf("RemovePlugin = %d; want 3", n)
	}
	if reg.Len() != 0 {
		t.Fatalf("Len = %d after remove; want 0", reg.Len())
	}
	// Removing again is not an error and removes nothing.
	if n := reg.RemovePlugin("obs"); n != 0 {
		t.Fatalf("second RemovePlugin = %d; want 0 (idempotent)", n)
	}
}

// ── host admission ──────────────────────────────────────────────────────────

func edgePolicy() Policy {
	return Policy{
		Target:         domain.TargetEdge,
		MaxSafetyLevel: domain.SafetyL1,
		MaxBlastRadius: domain.RadiusNone,
	}
}

func TestAdmitMatchingTarget(t *testing.T) {
	h := NewHost(NewRegistry(), edgePolicy(), nil)
	if err := h.Admit(manifest("obs", domain.TargetEdge, domain.SafetyL1, nil)); err != nil {
		t.Fatalf("admit: %v", err)
	}
}

func TestRefusesWrongTarget(t *testing.T) {
	h := NewHost(NewRegistry(), edgePolicy(), nil)
	err := h.Admit(manifest("mgr", domain.TargetManager, domain.SafetyL1, nil))
	if err == nil {
		t.Fatal("a manager-targeted package must be refused by an edge host")
	}
}

func TestRefusesAboveCeiling(t *testing.T) {
	h := NewHost(NewRegistry(), edgePolicy(), nil)
	err := h.Admit(manifest("destructive", domain.TargetEdge, domain.SafetyL3, nil))
	if err == nil {
		t.Fatal("an L3 package must be refused by an L1 host")
	}
}

func TestRefusesUngrantedScope(t *testing.T) {
	pol := edgePolicy()
	pol.GrantedScopes = domain.Scopes{domain.ScopeHostRead}
	h := NewHost(NewRegistry(), pol, nil)
	// The package wants a scope the operator did not grant.
	err := h.Admit(manifest("wants-db", domain.TargetEdge, domain.SafetyL1,
		domain.Scopes{domain.ScopeDBRead}))
	if err == nil {
		t.Fatal("a package asking for an ungranted scope must be refused")
	}
	// The granted scope is admitted.
	if err := h.Admit(manifest("wants-host", domain.TargetEdge, domain.SafetyL1,
		domain.Scopes{domain.ScopeHostRead})); err != nil {
		t.Fatalf("granted scope must be admitted: %v", err)
	}
}

func TestZeroPolicyAdmitsNothing(t *testing.T) {
	// A host whose policy was never populated must refuse everything, not
	// admit everything. This is the fail-closed default.
	h := NewHost(NewRegistry(), Policy{}, nil)
	if err := h.Admit(manifest("anything", domain.TargetEdge, domain.SafetyL1, nil)); err == nil {
		t.Fatal("a zero-value policy must refuse every package")
	}
}

// ── host activation ─────────────────────────────────────────────────────────

func TestActivateRegistersContribution(t *testing.T) {
	reg := NewRegistry()
	h := NewHost(reg, edgePolicy(), nil)
	c := &contrib{name: "obs", failAt: -1, entries: []Entry{dsEntry("prom"), dsEntry("loki")}}

	if err := h.Activate(context.Background(), manifest("obs", domain.TargetEdge, domain.SafetyL1, nil), c); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if reg.Len() != 2 {
		t.Fatalf("registry has %d entries; want 2", reg.Len())
	}
	if !h.IsLive("obs") {
		t.Fatal("obs should be live after activation")
	}
	e, ok := reg.Lookup(KindDatasource, "prom")
	if !ok || e.Plugin != "obs" {
		t.Fatalf("prom entry = %+v ok=%v; want plugin obs", e, ok)
	}
}

func TestActivateRefusesUnadmitted(t *testing.T) {
	reg := NewRegistry()
	h := NewHost(reg, edgePolicy(), nil)
	c := &contrib{name: "big", failAt: -1, entries: []Entry{dsEntry("x")}}

	// An L3 package is above the edge ceiling; activation must write nothing.
	err := h.Activate(context.Background(), manifest("big", domain.TargetEdge, domain.SafetyL3, nil), c)
	if err == nil {
		t.Fatal("activating an unadmitted package must fail")
	}
	if reg.Len() != 0 {
		t.Fatalf("registry has %d entries after a refused activation; want 0", reg.Len())
	}
	if h.IsLive("big") {
		t.Fatal("a refused package must not be live")
	}
}

func TestActivateRollsBackOnFailure(t *testing.T) {
	reg := NewRegistry()
	h := NewHost(reg, edgePolicy(), nil)
	// Register the first entry, then fail on the second.
	c := &contrib{name: "half", failAt: 1, entries: []Entry{dsEntry("prom"), dsEntry("loki")}}

	err := h.Activate(context.Background(), manifest("half", domain.TargetEdge, domain.SafetyL1, nil), c)
	if err == nil {
		t.Fatal("a contribution that fails partway must return an error")
	}
	// The partial registration must be gone: a half-activated package is a
	// table holding entries nobody will clean up.
	if reg.Len() != 0 {
		t.Fatalf("registry has %d entries after a failed activation; want 0 (rolled back)", reg.Len())
	}
	if h.IsLive("half") {
		t.Fatal("a failed package must not be live")
	}
}

func TestActivateRefusesDouble(t *testing.T) {
	reg := NewRegistry()
	h := NewHost(reg, edgePolicy(), nil)
	m := manifest("obs", domain.TargetEdge, domain.SafetyL1, nil)
	if err := h.Activate(context.Background(), m, &contrib{name: "obs", failAt: -1, entries: []Entry{dsEntry("prom")}}); err != nil {
		t.Fatal(err)
	}
	err := h.Activate(context.Background(), m, &contrib{name: "obs", failAt: -1, entries: []Entry{dsEntry("loki")}})
	if err == nil {
		t.Fatal("re-activating a live package must be refused, not silently replace it")
	}
}

func TestActivateRefusesNameMismatch(t *testing.T) {
	reg := NewRegistry()
	h := NewHost(reg, edgePolicy(), nil)
	// The manifest says "obs" but the contribution calls itself "other".
	err := h.Activate(context.Background(),
		manifest("obs", domain.TargetEdge, domain.SafetyL1, nil),
		&contrib{name: "other", failAt: -1, entries: []Entry{dsEntry("x")}})
	if err == nil {
		t.Fatal("a contribution whose name disagrees with the manifest must be refused")
	}
	if reg.Len() != 0 {
		t.Fatal("a name-mismatched activation must write nothing")
	}
}

func TestRegistrarStampsPluginName(t *testing.T) {
	reg := NewRegistry()
	h := NewHost(reg, edgePolicy(), nil)
	// A contribution never sees Entry; it registers through the scoped
	// registrar, which stamps the package's own name. So an entry cannot be
	// filed under a name that would confuse a rollback.
	if err := h.Activate(context.Background(), manifest("obs", domain.TargetEdge, domain.SafetyL1, nil),
		&contrib{name: "obs", failAt: -1, entries: []Entry{dsEntry("prom")}}); err != nil {
		t.Fatal(err)
	}
	e, _ := reg.Lookup(KindDatasource, "prom")
	if e.Plugin != "obs" {
		t.Fatalf("entry plugin = %q; want obs (the registrar must stamp the package name)", e.Plugin)
	}
}

func TestDeactivateRemovesEntries(t *testing.T) {
	reg := NewRegistry()
	h := NewHost(reg, edgePolicy(), nil)
	if err := h.Activate(context.Background(), manifest("obs", domain.TargetEdge, domain.SafetyL1, nil),
		&contrib{name: "obs", failAt: -1, entries: []Entry{dsEntry("prom"), dsEntry("loki")}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Deactivate(context.Background(), "obs"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("registry has %d entries after deactivate; want 0", reg.Len())
	}
	if h.IsLive("obs") {
		t.Fatal("obs must not be live after deactivate")
	}
	// Deactivating again is not an error.
	if err := h.Deactivate(context.Background(), "obs"); err != nil {
		t.Fatalf("second deactivate must succeed: %v", err)
	}
}

func TestLiveIsSorted(t *testing.T) {
	reg := NewRegistry()
	h := NewHost(reg, edgePolicy(), nil)
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if err := h.Activate(context.Background(), manifest(n, domain.TargetEdge, domain.SafetyL1, nil),
			&contrib{name: n, failAt: -1, entries: []Entry{dsEntry(n)}}); err != nil {
			t.Fatal(err)
		}
	}
	got := h.Live()
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Live[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

func TestKindsIsClosedAndValid(t *testing.T) {
	ks := Kinds()
	if len(ks) != 6 {
		t.Fatalf("Kinds() = %d; want 6", len(ks))
	}
	for _, k := range ks {
		if !k.Valid() {
			t.Fatalf("declared kind %q must be Valid()", k)
		}
	}
	if Kind("nope").Valid() {
		t.Fatal("an undeclared kind must not be Valid()")
	}
}
