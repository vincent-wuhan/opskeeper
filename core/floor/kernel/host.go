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
// through it.
type scopedRegistrar struct {
	reg    *Registry
	plugin string
}

func (s scopedRegistrar) Register(kind Kind, name string, impl any) error {
	return s.reg.Register(Entry{Kind: kind, Name: name, Plugin: s.plugin, Impl: impl})
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
func NewHost(reg *Registry, pol Policy, log *slog.Logger) *Host {
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
func (h *Host) Admit(p pluginmanifest.Plugin) error {
	if !p.RunsOn(h.pol.Target) {
		return fmt.Errorf("kernel: plugin %q targets %v, host runs %q",
			p.Name(), p.Targets(), h.pol.Target)
	}
	return sdk.Admit(p.Manifest, sdk.Admission{
		GrantedScopes:  h.pol.GrantedScopes,
		MaxSafetyLevel: h.pol.MaxSafetyLevel,
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

	if err := c.Register(scopedRegistrar{reg: h.reg, plugin: p.Name()}); err != nil {
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
