package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Running a release from the control plane.
//
// Rollout knows what one node's answer means and when a wave may advance.
// This knows the things that are not about one release at all: which nodes
// are in the fleet, that two releases of one package must not run at once,
// and what a console can ask for. Keeping them apart is what lets the wave
// gate be tested against scripted nodes — the safety property is in Rollout,
// and it is testable without a database or a tunnel.
//
// The control plane's plugin transport is request/response: one call per
// node per wave, answered with the verdict. So a wave is dispatched and
// accounted for in one synchronous step, and there is no answer channel to
// keep. The async path exists in Rollout.Report for a transport that pushes
// results; nothing here depends on it.

// Errors a caller is expected to act on rather than log.
var (
	// ErrReleaseRunning guards against two releases of one package. Each
	// would form its own idea of what the node had before, and a rollback
	// of either would restore a version the other never replaced.
	ErrReleaseRunning = errors.New("a release for this package is already running")
	// ErrNoRelease names a package with no release to act on.
	ErrNoRelease = errors.New("no release for this package")
)

// Fleet is the control plane's edge inventory, as this package needs it.
//
// It is a port so a release can be tested without a database. It returns
// every node a release should reach, including ones that are offline: a
// node that is down is a node whose install fails, and a release that
// quietly skipped it would leave the fleet uneven with nothing recording
// why. The failure is named in Status.Failed instead.
type Fleet interface {
	EdgeIDs(ctx context.Context) ([]uint64, error)
}

// Manager owns the releases this control plane has started.
type Manager struct {
	fleet Fleet
	node  Node
	// versions is the compatibility matrix's only input. Optional, and
	// refused at use rather than at construction — see WithVersions.
	versions Versions
	log      *slog.Logger

	mu       sync.Mutex
	releases map[string]*Rollout
}

// NewManager builds a Manager. fleet and node are both required: without an
// inventory there is no node list, and without a dispatcher a release would
// be a plan that never leaves the process. Start refuses in either case.
func NewManager(fleet Fleet, node Node, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{fleet: fleet, node: node, log: log, releases: map[string]*Rollout{}}
}

// StartRequest is a console's request to put one package on the fleet.
type StartRequest struct {
	Name      string
	Version   string
	URL       string
	SHA256    string
	Signature string
	KeyID     string
	// Strategy is the package's install policy — rolling or pin. It comes
	// from the manifest the operator is releasing, not from the console,
	// because a strategy chosen per-invocation is a canary that some
	// releases silently skip.
	Strategy string
	// Nodes narrows the release to a named subset. Empty means the whole
	// fleet. It is how an operator gives a "rolling" package a smaller
	// first outing than its own metadata asks for.
	Nodes []uint64
}

// Spec renders the request as the package the nodes are asked for.
func (r StartRequest) Spec() ports.PluginSpec {
	return ports.PluginSpec{
		Name: r.Name, Version: r.Version, URL: r.URL,
		SHA256: r.SHA256, Signature: r.Signature, KeyID: r.KeyID,
	}
}

// Start plans a release, sends its first wave, and returns the plan.
//
// The first wave is sent before returning so the answer says whether the
// canary was reachable at all. A release that answered 200 to an operator
// and had not yet spoken to a single node would be a release whose first
// real news was a failure delivered into a log nobody is reading.
func (m *Manager) Start(ctx context.Context, req StartRequest) (Status, error) {
	if req.Name == "" || req.Version == "" || req.URL == "" {
		return Status{}, errors.New("a release needs a package name, a version and a URL")
	}
	if m.fleet == nil {
		return Status{}, errors.New("no fleet inventory is wired; cannot work out which nodes to release to")
	}
	if m.node == nil {
		return Status{}, errors.New("no node dispatcher is wired; the release could not reach any node")
	}

	m.mu.Lock()
	if _, running := m.releases[req.Name]; running {
		m.mu.Unlock()
		return Status{}, fmt.Errorf("%w: %s", ErrReleaseRunning, req.Name)
	}
	m.mu.Unlock()

	nodes := req.Nodes
	if len(nodes) == 0 {
		var err error
		nodes, err = m.fleet.EdgeIDs(ctx)
		if err != nil {
			return Status{}, fmt.Errorf("listing the fleet: %w", err)
		}
	}
	if len(nodes) == 0 {
		return Status{}, errors.New("the fleet has no nodes to release to")
	}

	spec := req.Spec()
	plan, err := Plan(spec, nodes, req.Strategy, m.node, m.log)
	if err != nil {
		return Status{}, err
	}

	// Register before dispatching. The plan is a complete job from the
	// moment it exists, so a release interrupted between "started" and
	// "first install" still shows up in List and can still be rolled
	// back. Registering after the first wave would make that window a
	// release nobody can see.
	m.mu.Lock()
	if _, running := m.releases[req.Name]; running {
		m.mu.Unlock()
		return Status{}, fmt.Errorf("%w: %s", ErrReleaseRunning, req.Name)
	}
	m.releases[req.Name] = plan
	m.mu.Unlock()

	if err := plan.Dispatch(ctx); err != nil {
		return plan.Status(), err
	}
	return plan.Status(), nil
}

// each calls fn for every release, in package-name order, with no lock
// held.
//
// The ordering is the same one List uses, so an automatic driver and a
// console polling at the same time walk the releases in the same order and
// cannot disagree about which one is "first". The lock is released before
// fn runs because fn drives a release, and driving one calls out to the
// fleet — holding the manager's mutex across a fleet call would let a
// console's status read block on a node that is down.
func (m *Manager) each(fn func(*Rollout)) {
	m.mu.Lock()
	names := make([]string, 0, len(m.releases))
	for name := range m.releases {
		names = append(names, name)
	}
	sort.Strings(names)
	plans := make([]*Rollout, 0, len(names))
	for _, name := range names {
		plans = append(plans, m.releases[name])
	}
	m.mu.Unlock()

	for _, plan := range plans {
		fn(plan)
	}
}

// Advance sends the next wave, if the current one is accounted for.
//
// It returns false with no error when the wave is still waiting on a node.
// That is not a failure: it is the gate doing its job, and a caller that
// treated it as one would be a caller that retried its way past the gate.
func (m *Manager) Advance(ctx context.Context, name string) (bool, Status, error) {
	plan, err := m.get(name)
	if err != nil {
		return false, Status{}, err
	}
	moved, err := plan.Advance(ctx)
	return moved, plan.Status(), err
}

// Halt stops a release without taking anything back.
//
// Halting and rolling back are separate because they are separate
// decisions. An operator who has just watched the canary go bad may want the
// release stopped *now* and the decision about the canary taken calmly;
// a method that rolled back as a side effect would remove that pause.
func (m *Manager) Halt(name, reason string) (Status, error) {
	plan, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	plan.Halt(reason)
	return plan.Status(), nil
}

// Rollback takes a release back off the nodes it reached, then forgets it.
//
// Nodes it never reached are left alone, and so are nodes that refused: a
// remove sent to a node that was never changed either does nothing or, on a
// node that already had the package, removes the operator's own
// installation. The release is only forgotten once the rollback completed,
// because a partially rolled back release is the record of which nodes
// still need attention.
func (m *Manager) Rollback(ctx context.Context, name string) (Status, error) {
	plan, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	if !plan.Halted() {
		plan.Halt("rolled back")
	}
	if err := plan.Rollback(ctx); err != nil {
		st := plan.Status()
		st.Reason = err.Error()
		return st, err
	}
	m.mu.Lock()
	delete(m.releases, name)
	m.mu.Unlock()
	return plan.Status(), nil
}

// Status returns one release.
func (m *Manager) Status(name string) (Status, error) {
	plan, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	return plan.Status(), nil
}

// List returns every running release, ordered by package name so a console's
// table does not reshuffle between polls.
func (m *Manager) List() []Status {
	m.mu.Lock()
	names := make([]string, 0, len(m.releases))
	plans := make(map[string]*Rollout, len(m.releases))
	for name, plan := range m.releases {
		names = append(names, name)
		plans[name] = plan
	}
	m.mu.Unlock()

	sort.Strings(names)
	out := make([]Status, 0, len(names))
	for _, name := range names {
		out = append(out, plans[name].Status())
	}
	return out
}

// Report records an answer that arrived outside a Dispatch call.
//
// Nothing in the current transport needs it — plugin.install answers the
// call it was asked by — but Rollout.Report exists for a transport that
// pushes results, and a Manager that hid it would leave that path with no
// way in. An answer for a package with no release is dropped rather than
// creating one: a node does not get to start a release.
func (m *Manager) Report(edgeID uint64, out Outcome) {
	name := out.Plugin
	if name == "" {
		return
	}
	plan, err := m.get(name)
	if err != nil {
		return
	}
	plan.Report(context.Background(), edgeID, out)
}

func (m *Manager) get(name string) (*Rollout, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	plan, ok := m.releases[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoRelease, name)
	}
	return plan, nil
}
