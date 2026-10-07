// Package plugin drives a plugin release across the fleet.
//
// The policy this package encodes is a rule about time: a new wave starts
// after the current one has proved itself, not after enough time has
// passed. A caller that loops "install, sleep, next" has written the
// second rule while believing it wrote the first, and the difference shows
// up as a package reaching every node while the node that would have
// caught it is still restarting.
//
// The plan itself — which nodes, in which order, in how many waves — is
// pluginmanifest.PlanRollout's, and it is not duplicated here. What lives
// here is everything the plan cannot express on its own: what a node's
// answer means, when a release is allowed to stop, and what happens to
// the nodes it already reached when it does.
package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Node is one edge, as this package needs it.
//
// It is a port rather than a tunnel client so the rollout logic can be
// tested against scripted nodes, and so nothing here has to know that
// the transport is a JSONL pipe. The three outcomes mirror the node's own
// three: it installed, it refused, or it could not carry out the request.
type Node interface {
	// Install asks a node to take a package.
	Install(ctx context.Context, edgeID uint64, spec ports.PluginSpec) Outcome
	// Remove asks a node to give one back.
	Remove(ctx context.Context, edgeID uint64, name, version string) Outcome
	// Restore asks a node to put a version it already has back in front.
	//
	// It is separate from Install because a restore has no artifact to
	// fetch: the superseded version is still on the node's disk, and the
	// manager has no URL for it — the only spec it holds is the one it is
	// rolling back. See ports.PluginInstaller.Restore.
	Restore(ctx context.Context, edgeID uint64, name, version string) Outcome
}

// Outcome is what a node said.
type Outcome struct {
	// Plugin is the package the node answered about. It travels with the
	// answer so a Report that arrives outside a Dispatch call — a transport
	// that pushes results rather than answering them — can be routed to the
	// release it belongs to without the caller holding a mapping.
	Plugin string
	// Status is one of the tunnel's three: installed, refused, failed.
	Status string
	// Digest is the tree digest the node computed, when it installed.
	Digest string
	// Reason is the node's own sentence. It is passed through rather than
	// summarised, because it is the only thing that says *why*.
	Reason string
	// Replaced is what the package was before, when it was there at all.
	//
	// It comes from the node rather than being derived from Set, and that
	// is the whole reason Set cannot be used for it: Set is the list
	// *after* the install, so the entry for this name in it is the new
	// version. A manager that read the previous version out of it would
	// restore the release it is rolling back.
	Replaced *ports.PluginInfo
	// Set is the node's package list after the request, for a caller that
	// wants to see the whole node rather than one entry.
	Set []ports.PluginInfo
}

// Status values, matching the wire's closed set.
const (
	StatusInstalled = "installed"
	StatusRefused   = "refused"
	StatusFailed    = "failed"
)

// answered reports whether a status is one of the three outcomes a node
// actually produces.
//
// The fourth case — a blank status — is the one the wave gate exists for,
// and it is not hypothetical. A tunnel call that times out, a node that
// went away between the release job's dial and its send, a transport
// error the adapter swallowed: all of them reach here as an outcome with
// nothing in it.
//
// The tempting reading is "that is a failure, record it and move on",
// and it is wrong in the direction that ships a bad package: the node is
// not known to have refused and is not known to have installed, and
// recording either would let the release walk past a node whose state is
// unknown. So a blank answer leaves the node pending, which is the only
// state that says "we do not know yet", and the wave cannot advance past
// it.
func answered(status string) bool {
	switch status {
	case StatusInstalled, StatusRefused, StatusFailed:
		return true
	}
	return false
}

// Rollout is one release moving across a set of nodes.
//
// A caller drives it with Start, then Report as answers arrive, then
// Advance when the wave is accounted for. It is not safe to drive from two
// goroutines; the mutex is here so a Report from a health poller and an
// Advance from a release job do not interleave into a state neither of
// them decided.
type Rollout struct {
	plan *pluginmanifest.Rollout
	spec ports.PluginSpec
	node Node
	log  *slog.Logger

	mu sync.Mutex
	// reached records the nodes this release put a package on, with the
	// version they had before. A rollback needs both, and it needs them
	// from before the install, which is why they are captured on the way
	// in rather than looked up afterwards.
	reached map[uint64]previous
	// halted stops the rollout. It is not a failure state: a release can
	// be halted and then rolled back, and the caller may look at why.
	halted bool
	// haltReason is one sentence for the operator.
	haltReason string
	// rolledBack records that a rollback has run, so a second call is a
	// no-op rather than a second removal of a package that is gone.
	rolledBack bool
}

// previousOf renders what the node said it replaced.
func previousOf(replaced *ports.PluginInfo) previous {
	if replaced == nil {
		return previous{}
	}
	return previous{version: replaced.Version, digest: replaced.Digest}
}

// previous is what a node had for this package before this release.
type previous struct {
	// version is empty when the package was not installed at all.
	version string
	digest  string
}

// Start plans a release and dispatches its first wave.
//
// The first wave is a canary, not "the first batch". For a pinned
// package the plan is a single wave, and that is the operator having said
// this one does not get canaried.
//
// Everything the caller needs to interpret what comes back is returned
// here rather than read off the plan, because the plan is an internal
// detail and a caller that reached into it would be coupled to the
// canary arithmetic.
func Start(ctx context.Context, node Node, spec ports.PluginSpec, nodeIDs []uint64, strategy string, log *slog.Logger) (*Rollout, error) {
	r, err := Plan(spec, nodeIDs, strategy, node, log)
	if err != nil {
		return nil, err
	}
	if err := r.Dispatch(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Plan builds a rollout without sending anything.
//
// It exists so a caller can publish the job — the waves, the node set, the
// canary — before the first request goes out, which is what makes a
// release that is interrupted between "started" and "first install" a job
// an operator can still see and roll back. A release that is only visible
// once it has already touched a node is a release whose first node cannot
// be undone.
func Plan(spec ports.PluginSpec, nodeIDs []uint64, strategy string, node Node, log *slog.Logger) (*Rollout, error) {
	plan, err := pluginmanifest.PlanRollout(spec.Name, spec.Version, nodeIDs, strategy)
	if err != nil {
		return nil, err
	}
	return &Rollout{
		plan:    plan,
		spec:    spec,
		node:    node,
		log:     log,
		reached: map[uint64]previous{},
	}, nil
}

// Dispatch sends the wave currently in flight.
//
// Start calls it for the first wave; a caller that built the plan with
// Plan calls it once it has recorded the job. Calling it twice for the
// same wave would re-install to every node in it, so a Release drives it
// through Advance instead, which only moves after the wave is accounted
// for.
//
// The lock is held only to read the wave, not across the requests. That
// matters: a wave is one HTTP-shaped call per node, and holding the mutex
// for the whole wave would make Halt — the one method an operator reaches
// for when a canary is going wrong — block until every node in the wave had
// answered. An emergency stop that waits for the thing it is stopping is
// not a stop.
func (r *Rollout) Dispatch(ctx context.Context) error {
	r.mu.Lock()
	wave := append([]uint64(nil), r.plan.Wave()...)
	r.mu.Unlock()
	return r.dispatch(ctx, wave)
}

// Spec returns the package being rolled out.
func (r *Rollout) Spec() ports.PluginSpec { return r.spec }

// Plan exposes the underlying plan for a console's progress display.
func (r *Rollout) Plan() *pluginmanifest.Rollout { return r.plan }

// dispatch sends one wave.
//
// It takes the wave's nodes as an argument rather than reading them off the
// plan so the caller can release the lock before any request goes out. Each
// result is folded back in under the lock, so a Halt arriving mid-wave is
// recorded the moment the node in flight has answered.
func (r *Rollout) dispatch(ctx context.Context, wave []uint64) error {
	for _, edgeID := range wave {
		out := r.node.Install(ctx, edgeID, r.spec)

		r.mu.Lock()
		if out.Status == StatusInstalled {
			r.reached[edgeID] = previousOf(out.Replaced)
			r.plan.Confirm(edgeID)
		} else if answered(out.Status) {
			reason := out.Reason
			if reason == "" {
				reason = "the node gave no reason"
			}
			r.plan.Fail(edgeID, fmt.Errorf("%s: %s", out.Status, reason))
		}
		r.mu.Unlock()
		r.logOutcome(ctx, edgeID, out)
	}
	return nil
}

// logOutcome records one node's answer.
//
// A refusal is logged at warn and a failure at error on purpose. A refusal
// means this node's configuration says no and will keep saying no; a
// failure means the request did not get through, and that is the one an
// operator should be woken for. A blank answer is the third case — a node
// nobody has heard from — and leaving it pending is deliberate; see
// answered.
func (r *Rollout) logOutcome(ctx context.Context, edgeID uint64, out Outcome) {
	if r.log == nil {
		return
	}
	switch {
	case out.Status == StatusInstalled:
		r.log.Info("plugin installed on node",
			slog.Uint64("edge", edgeID),
			slog.String("plugin", r.spec.Name),
			slog.String("version", r.spec.Version),
			slog.String("digest", out.Digest))
	case !answered(out.Status):
		r.log.Warn("plugin install produced no answer from node; leaving it pending",
			slog.Uint64("edge", edgeID),
			slog.String("plugin", r.spec.Name),
			slog.String("version", r.spec.Version))
	default:
		reason := out.Reason
		if reason == "" {
			reason = "the node gave no reason"
		}
		level := slog.LevelWarn
		if out.Status == StatusFailed {
			level = slog.LevelError
		}
		r.log.Log(ctx, level, "plugin not installed on node",
			slog.Uint64("edge", edgeID),
			slog.String("plugin", r.spec.Name),
			slog.String("status", out.Status),
			slog.String("reason", reason))
	}
}

// Report records a node's answer to a current-wave install.
//
// It is exported because the tunnel handler that receives the answer is
// not the goroutine that dispatched it: Start is called by a release job
// and the answers arrive on the tunnel's read loop.
func (r *Rollout) Report(ctx context.Context, edgeID uint64, out Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()

	inWave := false
	for _, id := range r.plan.Wave() {
		if id == edgeID {
			inWave = true
			break
		}
	}
	if !inWave {
		// An answer from a node that is not in the wave in flight is a
		// late answer to a wave that has moved on. Recording it would
		// confirm a node nobody is waiting for, and the release would
		// read as further along than it is.
		if r.log != nil {
			r.log.Warn("plugin install answer for a node outside the current wave",
				slog.Uint64("edge", edgeID), slog.String("wave", r.plan.Progress()))
		}
		return
	}
	if !answered(out.Status) {
		return
	}
	if out.Status == StatusInstalled {
		r.reached[edgeID] = previousOf(out.Replaced)
		r.plan.Confirm(edgeID)
		return
	}
	r.plan.Fail(edgeID, fmt.Errorf("%s: %s", out.Status, out.Reason))
}

// Advance moves to the next wave, if that is allowed.
//
// Three things refuse it, and each is a different reason to stop:
//
//   - somebody in the current wave has not answered yet;
//   - the rollout is halted;
//   - the current wave is the last one.
//
// The second is the interesting one. It is what makes a canary a canary:
// a halt reached during wave one must not be stepped over by a caller
// that only checks the first condition, because the caller that checks
// only the first condition is the caller that ships the package to the
// fleet.
func (r *Rollout) Advance(ctx context.Context) (bool, error) {
	r.mu.Lock()

	if r.halted {
		defer r.mu.Unlock()
		return false, fmt.Errorf("plugin rollout is halted: %s", r.haltReason)
	}
	if r.plan.Done() {
		r.mu.Unlock()
		return false, nil
	}
	if !r.plan.Advance() {
		// Not an error. The wave is simply not accounted for, and the
		// caller is expected to come back when it is.
		r.mu.Unlock()
		return false, nil
	}
	wave := append([]uint64(nil), r.plan.Wave()...)
	r.mu.Unlock()

	if err := r.dispatch(ctx, wave); err != nil {
		return true, err
	}
	return true, nil
}

// Halt stops the rollout and says why.
//
// It does not roll anything back by itself. A caller may want to look at
// the reason, or to stop the release and leave the canary running while a
// human decides — both are legitimate, and a method that did the rollback
// as a side effect would remove that choice.
func (r *Rollout) Halt(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.halted {
		return
	}
	r.halted = true
	r.haltReason = reason
	if r.log != nil {
		r.log.Error("plugin rollout halted",
			slog.String("plugin", r.spec.Name),
			slog.String("version", r.spec.Version),
			slog.String("reason", reason),
			slog.String("progress", r.plan.Progress()))
	}
}

// Halted reports whether the rollout has been stopped.
func (r *Rollout) Halted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.halted
}

// Rollback takes the release back off every node it reached.
//
// "Reached" is the set this release put a package on, and it is the right
// set: a node that refused or failed was never changed, so asking it to
// remove would be asking a node that never installed anything to take
// something away, and on a node that had the package *before* this
// release that would remove the operator's own installation.
//
// A node that had a previous version gets it back, and one that had
// nothing is left with nothing. Rolling forward is not an option: the
// point of a rollback is that somebody has decided this version is not
// the one to be running.
func (r *Rollout) Rollback(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.rolledBack {
		return nil
	}
	r.rolledBack = true

	ids := make([]uint64, 0, len(r.reached))
	for id := range r.reached {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var problems []string
	for _, id := range ids {
		before := r.reached[id]
		out := r.node.Remove(ctx, id, r.spec.Name, r.spec.Version)
		if out.Status == StatusFailed {
			problems = append(problems, fmt.Sprintf("node %d: %s", id, out.Reason))
			continue
		}
		if before.version == "" {
			if r.log != nil {
				r.log.Info("plugin rolled back to absent",
					slog.Uint64("edge", id), slog.String("plugin", r.spec.Name))
			}
			continue
		}
		// Put the node back on what it was running.
		//
		// This is a restore and not an install, and the difference is the
		// whole reason it works. The node still has the superseded
		// version's directory; what it does not have is that version in
		// its package list, because publishing one version per name is
		// what stops an upgraded node from running both. Asking the node
		// to *install* the old version would need a URL for it, and the
		// manager does not have one: r.spec describes the version being
		// removed, which is the one version a rollback must not reinstall.
		//
		// The node still reviews on the way back in, so this is not a
		// trust exemption — it is the same review, over bytes that were
		// already accepted once and may have changed since.
		back := r.node.Restore(ctx, id, r.spec.Name, before.version)
		if back.Status != StatusInstalled {
			problems = append(problems, fmt.Sprintf("node %d: could not restore %s: %s",
				id, before.version, back.Reason))
			continue
		}
		if r.log != nil {
			r.log.Info("plugin rolled back to previous version",
				slog.Uint64("edge", id),
				slog.String("plugin", r.spec.Name),
				slog.String("version", before.version))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("rollback did not complete on %d node(s): %v", len(problems), problems)
	}
	if r.log != nil {
		r.log.Info("plugin rollback complete",
			slog.String("plugin", r.spec.Name),
			slog.String("version", r.spec.Version),
			slog.Int("nodes", len(ids)))
	}
	return nil
}

// Status is what a console shows.
type Status struct {
	Plugin     string   `json:"plugin"`
	Version    string   `json:"version"`
	Wave       int      `json:"wave"`
	Waves      int      `json:"waves"`
	Summary    string   `json:"summary"`
	Pending    []uint64 `json:"pending,omitempty"`
	Failed     []uint64 `json:"failed,omitempty"`
	Halted     bool     `json:"halted,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Done       bool     `json:"done"`
	RolledBack bool     `json:"rolled_back,omitempty"`
}

// Status snapshots the rollout.
func (r *Rollout) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{
		Plugin:     r.plan.Plugin(),
		Version:    r.plan.Version(),
		Wave:       r.plan.WaveNumber(),
		Waves:      r.plan.WaveCount(),
		Summary:    r.plan.Progress(),
		Pending:    r.plan.Pending(),
		Failed:     r.plan.Failed(),
		Halted:     r.halted,
		Reason:     r.haltReason,
		Done:       r.plan.Done(),
		RolledBack: r.rolledBack,
	}
}
