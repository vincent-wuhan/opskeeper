package kernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// Policy is a target's admission ceilings: what a host on this target will
// admit, regardless of what a package asks for.
//
// Every field has a fail-closed zero value. An unpopulated Policy admits
// nothing, because sdk.Admit refuses a zero-value Admission rather than
// treating it as the most permissive policy in the system.
type Policy struct {
	// Target is where this host runs. A package whose spec.targets does not
	// include it is refused: a plugin aimed at the manager has no business
	// registering into a node's table.
	Target domain.DeploymentTarget
	// MaxSafetyLevel is the highest safety level the target will host.
	MaxSafetyLevel domain.SafetyLevel
	// MaxBlastRadius is the widest reach an approval on this target may
	// carry.
	MaxBlastRadius domain.BlastRadius
	// GrantedScopes is the set of credential scopes the operator granted.
	// A package asking for a scope outside this set is refused.
	GrantedScopes domain.Scopes
	// KindCeilings overrides MaxSafetyLevel for one extension point. An
	// entry that names a kind is that kind's ceiling; a kind with no entry
	// falls back to MaxSafetyLevel. The fallback is the whole point: a
	// policy that says nothing about a kind behaves exactly as it did
	// before this field existed.
	//
	// The two directions exist because extension points genuinely differ.
	// A datasource is L1 because it only reads, while a notifier is L3
	// because every send creates an object in somebody else's system. One
	// host runs both, so one flat ceiling cannot be correct for both —
	// raising the global one to fit the notifier would hand L3 admission to
	// every manager-targeted tool package, which is the decision the
	// deployment profiles exist to keep out of package authors' hands. An
	// entry may move the ceiling either way, but only through the entry
	// point whose direction matches the move; see WithKindCeiling and
	// WithWiderKindCeiling.
	KindCeilings map[Kind]domain.SafetyLevel
}

// EffectiveSafetyCeiling reports the ceiling that applies to one extension
// point: the entry that names it, or MaxSafetyLevel when it has none.
//
// An entry carrying the zero value is treated as absent, because a zero
// SafetyLevel ranks as L3 and would otherwise read as the most permissive
// ceiling in the system. NewHost refuses such an entry outright, so this
// fallback is only reachable on a Policy that never went through a host.
func (p Policy) EffectiveSafetyCeiling(k Kind) domain.SafetyLevel {
	if lvl, ok := p.KindCeilings[k]; ok && lvl != "" {
		return lvl
	}
	return p.MaxSafetyLevel
}

// WidestSafetyCeiling reports the most permissive ceiling this policy allows
// for any kind: the global one and every entry.
//
// It exists for the package-level gate. Admit sees a manifest, and a manifest
// declares one safety level for the whole package without saying which kinds
// its contributions land in — so the widest is the only answer that does not
// refuse a package the host would later accept. The per-kind decision is made
// where the kind is known: Activate, through the contribution's registrar.
func (p Policy) WidestSafetyCeiling() domain.SafetyLevel {
	widest := p.MaxSafetyLevel
	for _, lvl := range p.KindCeilings {
		if lvl.Rank() > widest.Rank() {
			widest = lvl
		}
	}
	return widest
}

// WithKindCeiling records a per-kind ceiling that is no wider than the one
// the kind already has. It is the narrowing entry point.
//
// Asking it to widen panics rather than quietly doing nothing: a policy that
// silently ignored the write would leave every package at the old ceiling
// with no signal that the operator's change never took effect, which is the
// failure mode that makes a policy look like it works while it does not. Use
// WithWiderKindCeiling to widen.
//
// The returned policy owns a fresh map; the receiver's KindCeilings is never
// written through, so a Policy value shared with other holders keeps the
// ceilings it was built with.
func (p Policy) WithKindCeiling(k Kind, level domain.SafetyLevel) Policy {
	mustValidKindCeiling(k, level)
	if level.Rank() > p.EffectiveSafetyCeiling(k).Rank() {
		panic(fmt.Sprintf("kernel: WithKindCeiling(%q, %s) widens that kind past %s; use WithWiderKindCeiling",
			k, level, p.EffectiveSafetyCeiling(k)))
	}
	return p.withCeiling(k, level)
}

// WithWiderKindCeiling records a per-kind ceiling that is no narrower than
// the one the kind already has. It is the widening entry point, and it panics
// on a request to narrow — same reasoning as WithKindCeiling, read the other
// way round.
func (p Policy) WithWiderKindCeiling(k Kind, level domain.SafetyLevel) Policy {
	mustValidKindCeiling(k, level)
	if level.Rank() < p.EffectiveSafetyCeiling(k).Rank() {
		panic(fmt.Sprintf("kernel: WithWiderKindCeiling(%q, %s) narrows that kind below %s; use WithKindCeiling",
			k, level, p.EffectiveSafetyCeiling(k)))
	}
	return p.withCeiling(k, level)
}

// mustValidKindCeiling refuses a kind outside the closed set and a level
// outside L0..L3. An unrecognised level ranks as L3, so accepting a typo'd
// one here would record the most permissive ceiling the system has.
func mustValidKindCeiling(k Kind, level domain.SafetyLevel) {
	if !k.Valid() {
		panic(fmt.Sprintf("kernel: kind ceiling names unknown kind %q", k))
	}
	if !level.Valid() {
		panic(fmt.Sprintf("kernel: kind ceiling for %q is not a safety level: %q", k, level))
	}
}

func (p Policy) withCeiling(k Kind, level domain.SafetyLevel) Policy {
	ceilings := make(map[Kind]domain.SafetyLevel, len(p.KindCeilings)+1)
	for existing, lvl := range p.KindCeilings {
		ceilings[existing] = lvl
	}
	ceilings[k] = level
	p.KindCeilings = ceilings
	return p
}

// Registrar is the scoped view a contribution registers through.
//
// It is not the registry. The host hands each contribution a registrar that
// stamps the package's own name on every entry, so a contribution cannot
// file an entry under another package's name — which would be a way to make
// a rollback remove something it did not install, or leave behind something
// it did.
type Registrar interface {
	Register(kind Kind, name string, impl any) error
}

// Contribution is what a plugin package contributes when it is activated.
//
// A package implements this once. Register is handed a scoped registrar and
// adds whatever entries the package contributes; the kernel does not care
// which kinds, only that every entry is recorded against the package's name
// so the whole thing can be rolled back.
type Contribution interface {
	// PluginName is the manifest name this contribution belongs to. It is
	// the key the registry records every entry under, so a rollback removes
	// exactly this package's entries.
	PluginName() string
	// Register adds the package's entries. Returning an error rolls the
	// whole package back: the host removes every entry this Register wrote
	// before it returns.
	Register(r Registrar) error
}

// scopedRegistrar stamps one package's name onto every entry registered
// through it, and enforces the per-kind ceiling on each one.
type scopedRegistrar struct {
	reg    *Registry
	plugin string
	level  domain.SafetyLevel
	policy Policy
}

func (s scopedRegistrar) Register(kind Kind, name string, impl any) error {
	if err := s.checkCeiling(kind, name); err != nil {
		return err
	}
	return s.reg.Register(Entry{Kind: kind, Name: name, Plugin: s.plugin, Impl: impl})
}

// checkCeiling refuses an entry whose kind the policy caps below this
// package's own level.
//
// The ceiling for a kind is the policy's per-kind entry when it has one, and
// the policy's global ceiling when it has none — the same fallback
// EffectiveSafetyCeiling applies. That fallback is what keeps a per-kind
// entry from becoming a hole: a policy whose global ceiling is L2 and whose
// notifier entry is L3 admits an L3 package at the package gate, and this
// check is what refuses that package's L3 entries in every other kind.
//
// The check lives here rather than in Admit because this is the first point
// that knows both which kind the entry lands in and how much this package
// declared — a manifest carries one safety level for the whole package, so a
// package contributing to a capped kind and an uncapped one cannot be judged
// before it starts registering. Returning an error from here is what puts
// the refusal on the existing whole-package rollback path in Activate, rather
// than inventing a second one.
func (s scopedRegistrar) checkCeiling(kind Kind, name string) error {
	ceiling := s.policy.EffectiveSafetyCeiling(kind)
	if s.level.Rank() <= ceiling.Rank() {
		return nil
	}
	scope := fmt.Sprintf("policy ceiling for kind %q", kind)
	if _, perKind := s.policy.KindCeilings[kind]; perKind {
		scope = fmt.Sprintf("ceiling for kind %q", kind)
	}
	return fmt.Errorf("kernel: entry %s: plugin is %s, %s allows at most %s",
		entryKey{kind: kind, name: name}, s.level, scope, ceiling)
}

// Admitted is a package the host has accepted, paired with its contribution.
type Admitted struct {
	Plugin       pluginmanifest.Plugin
	Contribution Contribution
}

// Host admits plugin packages and activates their contributions against one
// registry.
//
// It is the only thing that should call Registry.Register. A plugin reaches
// the registry through Activate, which admits it first; a plugin that called
// Register directly would be one the admission gate never saw, and the point
// of the gate is that it is not skippable from inside a package.
type Host struct {
	reg  *Registry
	pol  Policy
	log  *slog.Logger
	mu   sync.Mutex
	live map[string]Admitted
}

// NewHost returns a host over reg with the given policy. A nil logger is
// replaced with a discard logger so the host is usable in a test.
//
// It validates every per-kind ceiling and panics on one it cannot enforce. A
// host that cannot honour its own policy is not a conservative host, it is a
// broken one: an unrecognised level ranks as L3, so a typo'd entry would
// record the most permissive ceiling in the system and quietly admit
// packages the operator meant to cap. Validating here rather than in the
// builders alone covers a Policy built as a struct literal, which can carry
// an entry the entry points would have refused.
func NewHost(reg *Registry, pol Policy, log *slog.Logger) *Host {
	for k, lvl := range pol.KindCeilings {
		mustValidKindCeiling(k, lvl)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Host{reg: reg, pol: pol, log: log, live: make(map[string]Admitted)}
}

// Registry returns the registry the host activates into.
func (h *Host) Registry() *Registry { return h.reg }

// Policy returns the host's admission policy.
func (h *Host) Policy() Policy { return h.pol }

// Admit decides whether a package may run on this host, without activating
// it. It is the same gate Activate uses, exposed so a caller can preflight a
// package — the manager preflighting a rollout, a test asserting a refusal —
// without mutating the table.
//
// Two clauses are the host's own, layered on top of the manifest admission:
// the package must name this target, and it must have been validated.
//
// The safety ceiling here is the widest one the policy allows for any kind,
// not the global one. A manifest declares one level for a whole package
// without saying which extension points its contributions land in, so a
// package-level gate cannot apply a per-kind cap without refusing packages
// the host would accept. Where the policy carries per-kind ceilings, passing
// this gate is therefore necessary but not sufficient: Activate is where the
// kind is known and the cap is applied, and a package refused there is rolled
// back whole.
func (h *Host) Admit(p pluginmanifest.Plugin) error {
	if !p.RunsOn(h.pol.Target) {
		return fmt.Errorf("kernel: plugin %q targets %v, host runs %q",
			p.Name(), p.Targets(), h.pol.Target)
	}
	return sdk.Admit(p.Manifest, sdk.Admission{
		GrantedScopes:  h.pol.GrantedScopes,
		MaxSafetyLevel: h.pol.WidestSafetyCeiling(),
		MaxBlastRadius: h.pol.MaxBlastRadius,
	})
}

// Activate admits a package and registers its contribution.
//
// The order is fixed and it is the whole safety argument: admit first,
// register second. A package that fails admission writes nothing. A package
// whose Register fails partway is rolled back to the state before it ran, so
// a half-activated package never leaves the table holding entries no one
// will clean up.
//
// Activating a package that is already live is an error rather than a
// replace, for the same reason Register refuses a duplicate: replacing a
// live package silently is how a rollback becomes impossible.
func (h *Host) Activate(ctx context.Context, p pluginmanifest.Plugin, c Contribution) error {
	if err := h.Admit(p); err != nil {
		h.log.Warn("plugin refused by admission",
			slog.String("plugin", p.Name()), slog.Any("err", err))
		return err
	}
	if c == nil {
		return fmt.Errorf("kernel: plugin %q has no contribution", p.Name())
	}
	if c.PluginName() != p.Name() {
		return fmt.Errorf("kernel: contribution names %q but manifest names %q",
			c.PluginName(), p.Name())
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.live[p.Name()]; ok {
		return fmt.Errorf("kernel: plugin %q is already active", p.Name())
	}

	if err := c.Register(scopedRegistrar{
		reg:    h.reg,
		plugin: p.Name(),
		level:  p.Manifest.Spec.SafetyLevel,
		policy: h.pol,
	}); err != nil {
		// Roll back whatever Register managed to write before it failed.
		removed := h.reg.RemovePlugin(p.Name())
		h.log.Warn("plugin activation failed; rolled back",
			slog.String("plugin", p.Name()),
			slog.Int("entries_removed", removed),
			slog.Any("err", err))
		return fmt.Errorf("kernel: activating %q: %w", p.Name(), err)
	}

	h.live[p.Name()] = Admitted{Plugin: p, Contribution: c}
	h.log.Info("plugin activated",
		slog.String("plugin", p.Name()),
		slog.String("target", string(h.pol.Target)),
		slog.Int("entries", len(h.reg.ByPlugin(p.Name()))))
	return nil
}

// Deactivate removes every entry the named package registered. It is the
// rollback path: an operator pulling a package, a canary that failed, a test
// tearing down.
//
// Deactivating a package that is not live is not an error. A rollback that
// is already done should succeed.
func (h *Host) Deactivate(_ context.Context, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	removed := h.reg.RemovePlugin(name)
	delete(h.live, name)
	if removed > 0 {
		h.log.Info("plugin deactivated",
			slog.String("plugin", name), slog.Int("entries_removed", removed))
	}
	return nil
}

// Live returns the names of the packages that are currently active, sorted.
func (h *Host) Live() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.live))
	for name := range h.live {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

// ErrNotLive is returned by a caller that expected a package to be active.
var ErrNotLive = errors.New("kernel: plugin is not active")

// IsLive reports whether a package is currently active.
func (h *Host) IsLive(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.live[name]
	return ok
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
