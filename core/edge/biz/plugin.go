package biz

import (
	"context"
	"encoding/json"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Plugin distribution, on the edge side.
//
// The review is not here. It is on the other side of the PluginInstaller
// port, in the composition root, because the inputs to a review — the
// node's trust store, its policy ceiling, its granted scopes — are the
// operator's configuration and not this package's business. What is here
// is the transport: decode a request, hand it to the port, and translate
// the port's verdict into the wire shape.
//
// The translation is the part worth reading. A node's answer has three
// outcomes and the wire has to keep them apart: installed, refused,
// failed. Collapsing "refused" into "failed" would make a manager retry a
// signature rejection against every node in the fleet, and collapsing
// "refused" into "installed" would make it count a capability the node
// does not have.

// SetPluginInstaller wires the node's plugin store.
//
// It is post-construction and optional, for the same reason SetAgentBridge
// is: the store is built in main, where the trust store and the policy
// live, and after the Agent. A node with no installer registers none of
// the methods below, which the manager can tell apart from a node that has
// one and is refusing — a node that answers "unknown method" has not been
// configured, and a node that answers "refused" has.
func (a *Agent) SetPluginInstaller(installer ports.PluginInstaller) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pluginInstaller = installer
}

// pluginStore returns the installer, or nil.
func (a *Agent) pluginStore() ports.PluginInstaller {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.pluginInstaller
}

// registerPluginHandlers wires plugin.install / plugin.remove /
// plugin.list. Called from registerUpgradeHandlers' sibling, and a no-op
// on a node with no store.
func (a *Agent) registerPluginHandlers() {
	if a.pluginStore() == nil {
		return
	}

	a.client.RegisterHandler(tunnel.MethodPluginInstall,
		func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
			var req tunnel.PluginInstallRequest
			if err := jsonDecode(body, &req); err != nil {
				return nil, err
			}
			// The store is read per call rather than captured. A node
			// that wires its store after the tunnel comes up would
			// otherwise capture nil and answer every install with "not
			// provisioned" for the life of the process.
			return jsonEncode(a.handlePluginInstall(ctx, req, a.pluginStore()), nil)
		})

	a.client.RegisterHandler(tunnel.MethodPluginRemove,
		func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
			var req tunnel.PluginRemoveRequest
			if err := jsonDecode(body, &req); err != nil {
				return nil, err
			}
			return jsonEncode(a.handlePluginRemove(ctx, req), nil)
		})

	a.client.RegisterHandler(tunnel.MethodPluginList,
		func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
			var req tunnel.PluginListRequest
			if err := jsonDecode(body, &req); err != nil {
				return nil, err
			}
			return jsonEncode(a.handlePluginList(ctx, req), nil)
		})

	a.client.RegisterHandler(tunnel.MethodPluginRestore,
		func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
			var req tunnel.PluginRestoreRequest
			if err := jsonDecode(body, &req); err != nil {
				return nil, err
			}
			return jsonEncode(a.handlePluginRestore(ctx, req), nil)
		})
}

// handlePluginRestore re-activates a version this node already has.
//
// It is separate from handlePluginInstall because it answers the same wire
// shape from a different decision: no bytes arrive, so nothing can be
// refused for a bad signature or a bad digest, and the only two outcomes
// are "a version on this node is now active" and "it is not on this node".
func (a *Agent) handlePluginRestore(ctx context.Context, req tunnel.PluginRestoreRequest) tunnel.PluginRestoreResponse {
	store := a.pluginStore()
	if store == nil {
		return tunnel.PluginRestoreResponse{
			Status: tunnel.PluginStatusFailed,
			Plugin: req.Plugin,
			Reason: noPluginStore,
		}
	}
	state := store.Restore(ctx, req.Plugin, req.Version)
	return tunnel.PluginRestoreResponse{
		Status:    statusOf(state),
		Plugin:    state.Name,
		Version:   state.Version,
		Digest:    state.Digest,
		Reason:    firstNonEmpty(state.Refused, state.Note, state.Error),
		Installed: entriesOf(a, store),
	}
}

// handlePluginInstall implements MethodPluginInstall.
func (a *Agent) handlePluginInstall(ctx context.Context, req tunnel.PluginInstallRequest, store ports.PluginInstaller) tunnel.PluginInstallResponse {
	if store == nil {
		// Unreachable through the registered handler, which is only
		// registered when there is a store. Kept because a node that
		// skipped the check would answer a missing store with a nil
		// dereference, and that crash would be in the tunnel's read loop
		// rather than in the request that caused it.
		return tunnel.PluginInstallResponse{
			Status: tunnel.PluginStatusFailed,
			Plugin: req.Plugin, Version: req.Version,
			Reason: noPluginStore,
		}
	}

	state := store.Install(ctx, ports.PluginSpec{
		Name:      req.Plugin,
		Version:   req.Version,
		URL:       req.URL,
		SHA256:    req.SHA256,
		Signature: req.Signature,
		KeyID:     req.KeyID,
	})
	// What the node did to its own tool set, recorded here rather than in
	// the store: the store's job is the filesystem, and a signature
	// verified in a directory three layers down is a fact nobody reads.
	// This is also the row that makes the manager's plugin_release history
	// checkable — the console knows what it *meant* to ship, and this is
	// what the host actually ended up running.
	recordPluginRow(a, ports.ActionPluginInstall, state)
	return installResponse(state, entriesOf(a, store))
}

// recordPluginRow writes one plugin row to the node's own ledger.
//
// Both the install and the removal go through here, and the shape is
// identical, because an operator asking "what is on this host" needs the
// absence as much as the presence: a ledger that records every install and
// no removal cannot answer "is the vulnerable version gone", and a version
// that was rolled back and then quietly reinstalled by a retry looks like
// one continuous install.
//
// A nil sink records nothing and is not an error. That is a weaker
// guarantee than the gate's — the gate's sink is mandatory and the node
// refuses to boot without it — and it is deliberate: the gate is on the
// path of every privileged action, while this is bookkeeping about
// packaging. A node that cannot keep the first still refuses; a node that
// somehow cannot keep the second says nothing rather than failing a
// release.
func recordPluginRow(a *Agent, action ports.AuditAction, state ports.PluginState) {
	sink := a.auditSink()
	if sink == nil {
		return
	}
	detail := map[string]any{
		"version": state.Version,
		"status":  statusOf(state),
	}
	if state.Digest != "" {
		detail["digest"] = state.Digest
	}
	if state.Replaced != nil {
		detail["replaced_version"] = state.Replaced.Version
		detail["replaced_digest"] = state.Replaced.Digest
	}
	if state.Refused != "" {
		detail["refused"] = state.Refused
	}
	if state.Error != "" {
		detail["error"] = state.Error
	}
	body, err := json.Marshal(detail)
	if err != nil {
		return
	}
	_ = sink.Record(context.Background(), ports.AuditEntry{
		At:      time.Now().UTC(),
		Actor:   "node:plugin",
		Action:  action,
		Target:  state.Name,
		Outcome: statusOf(state),
		Class:   "write",
		Detail:  body,
	})
}

// handlePluginRemove implements MethodPluginRemove.
//
// The version is checked by the store, not here. A rollback that raced a
// newer release has to be able to say "remove exactly what I put here",
// and a handler that ignored the version would remove the new one instead —
// which is the one outcome a rollback must never produce.
func (a *Agent) handlePluginRemove(ctx context.Context, req tunnel.PluginRemoveRequest) tunnel.PluginRemoveResponse {
	store := a.pluginStore()
	if store == nil {
		return tunnel.PluginRemoveResponse{
			Status: tunnel.PluginStatusFailed,
			Plugin: req.Plugin,
			Reason: noPluginStore,
		}
	}
	state := store.Remove(ctx, req.Plugin, req.Version)
	recordPluginRow(a, ports.ActionPluginRemove, state)
	return tunnel.PluginRemoveResponse{
		Status:    statusOf(state),
		Plugin:    state.Name,
		Version:   state.Version,
		Reason:    firstNonEmpty(state.Refused, state.Note, state.Error),
		Installed: entriesOf(a, store),
	}
}

// handlePluginList implements MethodPluginList.
func (a *Agent) handlePluginList(_ context.Context, _ tunnel.PluginListRequest) tunnel.PluginListResponse {
	store := a.pluginStore()
	if store == nil {
		return tunnel.PluginListResponse{}
	}
	return tunnel.PluginListResponse{Installed: entriesOf(a, store)}
}

// installResponse renders a state on the wire.
//
// The active set rides along with every answer rather than needing a
// second round trip. A manager that has to ask again to find out what a
// refusal left behind is a manager making two decisions from two moments,
// and a package that arrived in between them belongs to neither.
func installResponse(state ports.PluginState, installed []tunnel.PluginEntry) tunnel.PluginInstallResponse {
	var replaced *tunnel.PluginEntry
	if state.Replaced != nil {
		replaced = &tunnel.PluginEntry{
			Plugin:  state.Replaced.Name,
			Version: state.Replaced.Version,
			Digest:  state.Replaced.Digest,
		}
	}
	return tunnel.PluginInstallResponse{
		Status:    statusOf(state),
		Plugin:    state.Name,
		Version:   state.Version,
		Digest:    state.Digest,
		Replaced:  replaced,
		Reason:    firstNonEmpty(state.Refused, state.Note, state.Error),
		Installed: installed,
	}
}

// statusOf maps a port state onto the wire's closed set.
//
// The mapping is in one place because the three outcomes are a safety
// distinction, not a formatting choice: a manager reads "refused" as
// "stop", "failed" as "try this node again later", and "installed" as
// "count it towards the wave". Getting one of them wrong turns a
// canary into a fleet-wide push or a stuck release.
func statusOf(state ports.PluginState) string {
	switch {
	case state.Error != "":
		return tunnel.PluginStatusFailed
	case state.Refused != "":
		return tunnel.PluginStatusRefused
	default:
		return tunnel.PluginStatusInstalled
	}
}

// entriesOf renders the node's active set for the wire.
func entriesOf(a *Agent, store ports.PluginInstaller) []tunnel.PluginEntry {
	infos := store.Installed()
	out := make([]tunnel.PluginEntry, 0, len(infos))
	for _, i := range infos {
		out = append(out, tunnel.PluginEntry{Plugin: i.Name, Version: i.Version, Digest: i.Digest})
	}
	if a != nil && a.log != nil {
		a.log.Debug("plugin set reported", "count", len(out))
	}
	return out
}

// firstNonEmpty returns the first non-blank value.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// noPluginStore is what a node without a store says, in the one sentence
// an operator needs. It is a refusal rather than a failure: nothing about
// the request is wrong, and retrying it will not conjure a store.
const noPluginStore = "this node has no plugin store configured; it is not provisioned for plugins"
