// Package pluginhost is the edge-side plugin host: the node half of the
// microkernel.
//
// It is the mirror of the control plane's host. The kernel
// (core/floor/kernel) is the registry and the admission gate; the manager
// host (core/manager/middleware/pluginhost) walks a directory of packages
// and activates control-plane contributions; this one walks a directory of
// packages and activates node contributions — the collectors a node runs to
// produce telemetry.
//
// The symmetry is the point. Before this, a node's collector set was a
// literal slice in cmd/opskeeper-edge/main.go: promtail, otelcol, the
// in-process scraper, the database exporters. Adding a collector meant
// editing the node binary. Now a collector is a package a node admits
// through the same gate a tool package already passes, and the message a
// node reports on refusal is the message the control plane reports, because
// it is the same gate.
//
// Two halves make up "a node hosts a package", and this is only one of
// them:
//
//   - The tool packages (plugins/pig-ops/*) ship PiG extensions and are
//     loaded by the agent process. That path already exists.
//   - The collector packages ship a collector the Supervisor runs. That is
//     what this host activates — the signal-producing half, which is what
//     makes a node's telemetry set extensible without a node rebuild.
package pluginhost

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/plugins"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// CollectorSink is where an activated collector is registered. The edge
// Supervisor satisfies it; keeping it an interface is what lets a test
// stand in without starting a subprocess.
type CollectorSink interface {
	Register(p plugins.Plugin)
}

// Activator builds the contribution for one package.
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

// CollectorContribution contributes one collector under KindCollector,
// keyed by the collector's own signal name.
type CollectorContribution struct {
	// Plugin is the manifest name this contribution belongs to.
	Plugin string
	// Collector is the edge collector to run.
	Collector plugins.Plugin
}

func (c CollectorContribution) PluginName() string { return c.Plugin }

func (c CollectorContribution) Register(r kernel.Registrar) error {
	if c.Collector == nil {
		return fmt.Errorf("edge pluginhost: contribution for %q has no collector", c.Plugin)
	}
	return r.Register(kernel.KindCollector, c.Collector.Name(), c.Collector)
}

// Host admits and activates edge-targeted packages from a directory.
type Host struct {
	root string
	pol  kernel.Policy
	kern *kernel.Host
	sink CollectorSink
	log  *slog.Logger

	mu         sync.Mutex
	activators map[string]Activator
}

// New returns a host that reads packages from root and activates their
// collectors into sink.
func New(root string, pol kernel.Policy, sink CollectorSink, log *slog.Logger) *Host {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Host{
		root:       root,
		pol:        pol,
		kern:       kernel.NewHost(kernel.NewRegistry(), pol, log),
		sink:       sink,
		log:        log,
		activators: map[string]Activator{},
	}
}

// Registry returns the kernel registry the host activates into.
func (h *Host) Registry() *kernel.Registry { return h.kern.Registry() }

// Inventory returns this host's capability set: which packages it admitted
// and what each registered. It is the diagnostic surface — "what can this
// deployment reach" is otherwise knowable only by reading Go source.
func (h *Host) Inventory() kernel.Inventory { return h.kern.Inventory() }

// RegisterActivator registers how the package named name contributes.
func (h *Host) RegisterActivator(name string, a Activator) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activators[name] = a
}

// Report is what Load did.
type Report struct {
	Activated   []string
	Refused     map[string]string
	Skipped     []string
	NoActivator []string
}

// Load walks the package directory, admits every edge-targeted package,
// activates the contribution registered for each, and registers every
// resulting collector into the sink.
//
// Fail-closed in the same three ways as the control plane: a package aimed
// at the manager is skipped, one above the node's ceiling is refused, one
// whose activation fails partway is rolled back. One refusal never stops
// the others.
func (h *Host) Load(ctx context.Context) (Report, error) {
	rep := Report{Refused: map[string]string{}}

	packages, err := pluginmanifest.LoadAll(h.root)
	if err != nil {
		return rep, fmt.Errorf("edge pluginhost: reading packages from %s: %w", h.root, err)
	}

	h.mu.Lock()
	activators := make(map[string]Activator, len(h.activators))
	for k, v := range h.activators {
		activators[k] = v
	}
	h.mu.Unlock()

	for _, p := range packages {
		name := p.Name()
		if !p.RunsOn(h.pol.Target) {
			rep.Skipped = append(rep.Skipped, name)
			continue
		}
		if err := h.kern.Admit(p); err != nil {
			rep.Refused[name] = err.Error()
			h.log.Warn("edge plugin refused by admission", slog.String("plugin", name), slog.Any("err", err))
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

	// Register every activated collector with the sink. The order is the
	// registry's (by collector name) so the node's collector set is stable
	// across boots: a listing that reshuffled would be a diff an operator
	// has to read twice.
	if h.sink != nil {
		for _, e := range h.kern.Registry().All(kernel.KindCollector) {
			if c, ok := e.Impl.(plugins.Plugin); ok {
				h.sink.Register(c)
			}
		}
	}

	sort.Strings(rep.Activated)
	sort.Strings(rep.Skipped)
	sort.Strings(rep.NoActivator)
	return rep, nil
}

// DefaultProfile is the deployment profile a node assumes when the operator
// names none. finance-strong-consistency is the conservative end of the
// catalogue: it withholds k8s.exec, db.write and mq.write and refuses L3, so
// assuming it refuses rather than admits.
const DefaultProfile = pluginmanifest.ProfileFinance

// EdgePolicyFor returns the node's admission policy, rendered from the
// deployment profile the operator selected.
//
// The profile is the authoritative statement of what a fleet will host: its
// ceiling, blast radius and granted scopes are written down together with
// the package set they compose, and the manifest gate already checks each
// package against them. This function used to hardcode L1 with a single
// granted scope instead — and a real node run proved that wrong, refusing
// all five shipped packages with "not granted: topology.read, alert.read"
// and "plugin is L3, target policy allows at most L1". A policy that admits
// nothing the repository actually ships is not a conservative policy; it is
// a broken one.
//
// An unknown profile name is an error, and the caller must treat it as a
// refusal rather than falling back to something permissive.
func EdgePolicyFor(name pluginmanifest.ProfileName) (kernel.Policy, error) {
	profile, err := pluginmanifest.ProfileForName(string(name))
	if err != nil {
		return kernel.Policy{}, err
	}
	return PolicyFrom(profile), nil
}

// PolicyFrom renders a deployment profile as the node's kernel policy.
func PolicyFrom(profile pluginmanifest.Profile) kernel.Policy {
	pol := profile.Policy()
	return kernel.Policy{
		Target:         domain.TargetEdge,
		MaxSafetyLevel: pol.MaxSafetyLevel,
		MaxBlastRadius: pol.MaxBlastRadius,
		GrantedScopes:  pol.GrantedScopes,
	}
}
