// Package pluginhost is the manager-side plugin host.
//
// It is the control-plane half of the microkernel. The kernel
// (core/floor/kernel) is the registry and the admission gate; this package
// is the thing that walks a directory of packages, admits each one through
// that gate, and activates the contribution registered for it.
//
// Before this package, "a plugin" was a node-side idea: packages were
// admitted and pushed to edges, and the control plane's own capabilities —
// its datasources, its adapter families, its providers — were compiled in
// and wired by hand. This is the seam that lets the control plane host a
// package the way a node does, with the same manifest, the same gate, and
// the same rollback.
//
// The model is deliberately the one the node already uses: a package is a
// declarative manifest plus a compiled-in contribution. OpsKeeper does not
// load third-party Go at runtime, so the manifest is admitted from disk and
// the contribution is looked up by package name in a registry the assembly
// layer fills. That split is what keeps the admission decision (a manifest
// on disk, reviewable, signable) separate from the implementation (code the
// operator chose to compile in).
package pluginhost

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// Activator builds the contribution for one package.
//
// It is a constructor rather than a value so a package that is activated
// twice — a test, a reload — gets a fresh contribution each time rather than
// one whose internal state has already been mutated by the first activation.
type Activator func() kernel.Contribution

// ToolPackageContribution registers the tools a package declares.
//
// A package whose manifest lists tools has something to contribute even when
// this build registered no activator for it: its tool declarations. That is
// what puts the node's tool surface — the shipped plugins/pig-ops packages —
// into the same registry its collectors appear in, so "what does this node
// offer" is one question with one answer.
//
// The implementation is not here: the kernel holds the class and limits, and
// a call reaches the host's own implementation by name, which is the
// invariant the gate enforces.
type ToolPackageContribution struct {
	// Plugin is the manifest name this contribution belongs to.
	Plugin string
	// Tools are the declarations from the manifest's spec.tools.
	Tools []domain.ToolDecl
}

func (c ToolPackageContribution) PluginName() string { return c.Plugin }

func (c ToolPackageContribution) Register(r kernel.Registrar) error {
	for _, t := range c.Tools {
		if err := r.Register(kernel.KindTool, t.Name, kernel.ToolDeclaration{Class: t.Class, Limits: t.Limits}); err != nil {
			return err
		}
	}
	return nil
}

// toolPackageActivator builds the contribution for a package that declares
// tools but has no registered activator.
func toolPackageActivator(p pluginmanifest.Plugin) Activator {
	return func() kernel.Contribution {
		return ToolPackageContribution{Plugin: p.Name(), Tools: p.Manifest.Spec.Tools}
	}
}

// Host admits and activates manager-targeted packages from a directory.
type Host struct {
	root string
	pol  kernel.Policy
	kern *kernel.Host
	log  *slog.Logger

	mu         sync.Mutex
	activators map[string]Activator
}

// New returns a host that reads packages from root and activates them into a
// kernel host built with pol.
//
// root is the directory whose subdirectories are packages (each with a
// pig-ops.yaml). It is read on Load, not at construction, so a host can be
// built before the directory exists — which is what a deployment with no
// manager packages does.
func New(root string, pol kernel.Policy, log *slog.Logger) *Host {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Host{
		root:       root,
		pol:        pol,
		kern:       kernel.NewHost(kernel.NewRegistry(), pol, log),
		log:        log,
		activators: map[string]Activator{},
	}
}

// Registry returns the kernel registry the host activates into. It is what a
// caller reads to resolve what is live — the datasource Set, the adapter
// family list, whatever the activated packages contributed.
func (h *Host) Registry() *kernel.Registry { return h.kern.Registry() }

// Kernel returns the underlying kernel host, for a caller that needs to
// deactivate a package or read the live set.
func (h *Host) Kernel() *kernel.Host { return h.kern }

// Inventory returns this host's capability set: which packages it admitted
// and what each registered. It is the diagnostic surface — "what can this
// deployment reach" is otherwise knowable only by reading Go source.
func (h *Host) Inventory() kernel.Inventory { return h.kern.Inventory() }

// RegisterActivator registers how the package named name contributes.
//
// A package with no registered activator is admitted and reported as such,
// not activated: it is a package this build was not compiled to run, which
// is a state an operator needs to see rather than a silent skip.
func (h *Host) RegisterActivator(name string, a Activator) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activators[name] = a
}

// Report is what Load did, in a form an operator or a test can assert on.
type Report struct {
	// Activated names the packages that were admitted and activated, sorted.
	Activated []string
	// Refused maps a package name to the reason it failed admission or
	// activation. Retrying will not change an admission refusal.
	Refused map[string]string
	// Skipped names the packages whose spec.targets did not include this
	// host's target, sorted.
	Skipped []string
	// NoActivator names the packages that were admitted but for which this
	// build registered no contribution, sorted.
	NoActivator []string
}

// Load walks the package directory, admits every manager-targeted package,
// and activates the contribution registered for each.
//
// It is fail-closed in the same three ways the node is: a package aimed at
// another target is skipped, a package above the policy ceiling or asking
// for an ungranted scope is refused, and a package whose activation fails
// partway is rolled back by the kernel. One package's refusal never stops
// the others — a control plane that lost every plugin because one manifest
// was bad would be the worst version of fail-closed.
func (h *Host) Load(ctx context.Context) (Report, error) {
	rep := Report{Refused: map[string]string{}}

	plugins, err := pluginmanifest.LoadAll(h.root)
	if err != nil {
		// A directory that cannot be read at all is the caller's problem —
		// a typo'd path, a permission — not a per-package refusal.
		return rep, fmt.Errorf("pluginhost: reading packages from %s: %w", h.root, err)
	}

	h.mu.Lock()
	activators := make(map[string]Activator, len(h.activators))
	for k, v := range h.activators {
		activators[k] = v
	}
	h.mu.Unlock()

	for _, p := range plugins {
		name := p.Name()
		if !p.RunsOn(h.pol.Target) {
			rep.Skipped = append(rep.Skipped, name)
			continue
		}
		if err := h.kern.Admit(p); err != nil {
			rep.Refused[name] = err.Error()
			h.log.Warn("manager plugin refused by admission",
				slog.String("plugin", name), slog.Any("err", err))
			continue
		}
		act, ok := activators[name]
		if !ok {
			// No registered activator. A package that declares tools still
			// has something to contribute — its tool declarations, so the
			// node's tool surface lands in the same registry its collectors
			// do. A package with neither an activator nor declared tools is
			// reported as such: this build was not compiled to run it.
			if len(p.Manifest.Spec.Tools) == 0 {
				rep.NoActivator = append(rep.NoActivator, name)
				continue
			}
			act = toolPackageActivator(p)
		}
		if err := h.kern.Activate(ctx, p, act()); err != nil {
			rep.Refused[name] = err.Error()
			continue
		}
		rep.Activated = append(rep.Activated, name)
	}

	sort.Strings(rep.Activated)
	sort.Strings(rep.Skipped)
	sort.Strings(rep.NoActivator)
	return rep, nil
}
