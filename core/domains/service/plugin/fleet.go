package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// EdgeCaller is the manager's cloud→edge dispatcher, as this package needs
// it. It is the same shape frontierbound.Client.Call has, declared here so
// the rollout logic does not import the transport and can be driven by a
// scripted fleet in tests.
type EdgeCaller interface {
	Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error)
}

// PolicyGate is asked, on this control plane, whether a package may be pushed
// to a node at all.
//
// It is a second opinion, not a replacement. The node still runs its own
// Review on the bytes that arrive, and a gate here cannot make a node accept
// something it would have refused. What the gate decides is narrower and
// earlier: whether this control plane should be offering the package in the
// first place. On a cluster federating to a root, that question has an owner
// above this process, and answering it locally would make every node in the
// cluster an independent policy decision — which is the thing federation
// exists to prevent.
//
// A nil gate is a control plane with no policy above it, which is the ordinary
// single-cluster case.
type PolicyGate interface {
	Check(name, version string) error
}

// NodeFleet is a Node backed by the manager's tunnel.
//
// It is deliberately thin: marshal, call, unmarshal, translate. Everything
// that decides *whether* a node should get a package lives on the node, and
// everything that decides *when* lives in Rollout. What is left here is the
// one thing neither of them can do, which is speak to a node that is not in
// this process.
//
// The one exception to the "everything lives on the node" rule is the gate
// above, and it is an exception because a node cannot know what its control
// plane's root has decided. See PolicyGate.
type NodeFleet struct {
	caller EdgeCaller
	gate   PolicyGate
}

// ErrNoTunnel is the one answer this adapter gives about itself rather than
// about a node.
//
// It is a sentinel rather than four copies of the same sentence because
// "this manager cannot reach any node" and "this node did not answer" are
// different facts with different remedies, and a caller that cannot tell
// them apart has to render both as the same failure. The first is fixed by
// configuration; the second is fixed by the node, and an operator who is
// told the wrong one will go looking in the wrong place — restarting a node
// whose agent is fine because the manager was never given a tunnel.
//
// The three mutating methods fold it into an Outcome's Reason because an
// Outcome is what a rollout reports, and a rollout that swallowed the
// distinction would print this sentence as a per-node failure on every node
// at once. Installed returns it as an error because a read has no Outcome
// to put it in.
var ErrNoTunnel = errors.New("the control plane has no tunnel to the fleet")

// NewNodeFleet builds the adapter. A nil caller is a programming error and
// is refused by the methods rather than answering "refused" for every node,
// because an unwired control plane looks exactly like a fleet-wide policy
// rejection from the outside.
func NewNodeFleet(caller EdgeCaller) *NodeFleet { return &NodeFleet{caller: caller} }

// SetPolicyGate installs the gate, or removes it with nil.
//
// Post-hoc for the same reason SetEdgeCaller is: on a child cluster the gate
// is built from a policy store that the federation wiring creates, and that
// wiring runs after the fleet exists. Calling it twice is not a way to change
// policy mid-rollout — a rollout in flight keeps the gate it started with,
// because a wave that silently changed its own rules halfway is worse than
// one that started under the wrong ones.
func (f *NodeFleet) SetPolicyGate(g PolicyGate) {
	if f == nil {
		return
	}
	f.gate = g
}

// admit is the gate's one question, asked before anything is marshalled.
//
// A refusal is a refusal and not a failure, because the two mean different
// things to a rollout: a failure is a node that did not answer and will be
// asked again, and a refusal is a decision that will not change by asking.
// Reporting it as StatusFailed would put a policy that says no into a retry
// loop, once per wave, forever.
func (f *NodeFleet) admit(spec ports.PluginSpec) *Outcome {
	if f.gate == nil {
		return nil
	}
	if err := f.gate.Check(spec.Name, spec.Version); err != nil {
		return &Outcome{
			Plugin: spec.Name,
			Status: StatusRefused,
			Reason: "refused by the policy in force on this control plane, before any node was asked: " + err.Error(),
		}
	}
	return nil
}

// Install asks one node to take a package.
func (f *NodeFleet) Install(ctx context.Context, edgeID uint64, spec ports.PluginSpec) Outcome {
	if f == nil || f.caller == nil {
		return Outcome{Status: StatusFailed, Reason: ErrNoTunnel.Error()}
	}
	if denied := f.admit(spec); denied != nil {
		return *denied
	}
	body, err := json.Marshal(tunnel.PluginInstallRequest{
		Plugin:    spec.Name,
		Version:   spec.Version,
		URL:       spec.URL,
		SHA256:    spec.SHA256,
		Signature: spec.Signature,
		KeyID:     spec.KeyID,
	})
	if err != nil {
		return Outcome{Status: StatusFailed, Reason: "malformed install request: " + err.Error()}
	}
	raw, err := f.caller.Call(ctx, edgeID, tunnel.MethodPluginInstall, body)
	if err != nil {
		// A transport error is an answer we did not get, not a refusal. It
		// is reported as failed so the operator sees the node — but see
		// Outcome and answered(): a wave that has one of these still moves,
		// which is why the node is named in the log and in Status.Failed.
		return Outcome{Status: StatusFailed, Reason: "no answer from the node: " + err.Error()}
	}
	var resp tunnel.PluginInstallResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Outcome{Status: StatusFailed, Reason: "unreadable answer from the node: " + err.Error()}
	}
	out := Outcome{
		Plugin:   resp.Plugin,
		Status:   resp.Status,
		Digest:   resp.Digest,
		Reason:   resp.Reason,
		Set:      infosOf(resp.Installed),
		Replaced: replacedOf(resp.Replaced),
	}
	if out.Reason == "" && out.Status == StatusRefused {
		out.Reason = "the node refused the package and gave no reason"
	}
	return out
}

// Remove asks one node to give a package back.
//
// Deliberately not gated. A gate on removal would be a gate that can only
// add, and a package the root has withdrawn from the policy is precisely the
// one a cluster most needs to be rid of: leaving it installed because
// removing it needs permission is a policy that cannot be enforced. The gate
// decides what may arrive, never what may leave.
func (f *NodeFleet) Remove(ctx context.Context, edgeID uint64, name, version string) Outcome {
	if f == nil || f.caller == nil {
		return Outcome{Status: StatusFailed, Reason: ErrNoTunnel.Error()}
	}
	body, err := json.Marshal(tunnel.PluginRemoveRequest{Plugin: name, Version: version})
	if err != nil {
		return Outcome{Status: StatusFailed, Reason: "malformed remove request: " + err.Error()}
	}
	raw, err := f.caller.Call(ctx, edgeID, tunnel.MethodPluginRemove, body)
	if err != nil {
		return Outcome{Status: StatusFailed, Reason: "no answer from the node: " + err.Error()}
	}
	var resp tunnel.PluginRemoveResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Outcome{Status: StatusFailed, Reason: "unreadable answer from the node: " + err.Error()}
	}
	return Outcome{
		Plugin: resp.Plugin,
		Status: resp.Status,
		Reason: resp.Reason,
		Set:    infosOf(resp.Installed),
	}
}

// Restore asks one node to put a version it already has back.
//
// There is no URL here on purpose. The manager asks for a version by name
// because the bytes are on the node; a restore that carried a source would
// be an install, and the manager's rollback path does not have one for the
// version it is going back to.
func (f *NodeFleet) Restore(ctx context.Context, edgeID uint64, name, version string) Outcome {
	if f == nil || f.caller == nil {
		return Outcome{Status: StatusFailed, Reason: ErrNoTunnel.Error()}
	}
	body, err := json.Marshal(tunnel.PluginRestoreRequest{Plugin: name, Version: version})
	if err != nil {
		return Outcome{Status: StatusFailed, Reason: "malformed restore request: " + err.Error()}
	}
	raw, err := f.caller.Call(ctx, edgeID, tunnel.MethodPluginRestore, body)
	if err != nil {
		return Outcome{Status: StatusFailed, Reason: "no answer from the node: " + err.Error()}
	}
	var resp tunnel.PluginRestoreResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Outcome{Status: StatusFailed, Reason: "unreadable answer from the node: " + err.Error()}
	}
	out := Outcome{
		Plugin: resp.Plugin,
		Status: resp.Status,
		Digest: resp.Digest,
		Reason: resp.Reason,
		Set:    infosOf(resp.Installed),
	}
	if out.Reason == "" && out.Status == StatusRefused {
		out.Reason = "the node refused the restore and gave no reason"
	}
	return out
}

// Installed asks one node what it is running.
func (f *NodeFleet) Installed(ctx context.Context, edgeID uint64) ([]ports.PluginInfo, error) {
	if f == nil || f.caller == nil {
		return nil, ErrNoTunnel
	}
	body, err := json.Marshal(tunnel.PluginListRequest{})
	if err != nil {
		return nil, fmt.Errorf("plugin.list: marshal: %w", err)
	}
	raw, err := f.caller.Call(ctx, edgeID, tunnel.MethodPluginList, body)
	if err != nil {
		// Wrapped, not flattened. The caller is entitled to know whether
		// the node stayed silent or answered with something unreadable,
		// because the first is a node to look at and the second is a
		// version skew between the manager and the node binary — the same
		// class of problem the compatibility matrix reports per node.
		return nil, fmt.Errorf("plugin.list: node %d did not answer: %w", edgeID, err)
	}
	var resp tunnel.PluginListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("plugin.list: node %d answered with something unreadable: %w", edgeID, err)
	}
	// A nil slice and an empty one are the same fact — the node runs
	// nothing — and are deliberately not distinguished here. The
	// distinction that matters is handled by the caller: a node that
	// could not be reached is an error, never an empty list.
	return infosOf(resp.Installed), nil
}

// infosOf renders the wire's package entries.
func infosOf(entries []tunnel.PluginEntry) []ports.PluginInfo {
	if len(entries) == 0 {
		return nil
	}
	out := make([]ports.PluginInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, ports.PluginInfo{Name: e.Plugin, Version: e.Version, Digest: e.Digest})
	}
	return out
}

// replacedOf renders what the node said it overwrote.
//
// A nil entry means "there was nothing there", and that is not the same as
// an entry with an empty version: the manager derives "restore nothing, just
// remove" from the former and "restore the version named here" from the
// latter, and collapsing the two would make the second case a delete.
func replacedOf(entry *tunnel.PluginEntry) *ports.PluginInfo {
	if entry == nil {
		return nil
	}
	return &ports.PluginInfo{Name: entry.Plugin, Version: entry.Version, Digest: entry.Digest}
}
