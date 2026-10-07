package tunnel

// Plugin distribution over the same channel as everything else a node
// does for the control plane.
//
// The edge *binary* upgrade channel (fetch_package / apply_package) moves
// hundreds of megabytes of collector tooling and restarts the process to
// install it. A plugin package is a few hundred kilobytes of Go sources
// and Markdown, and the thing it needs is not a restart but a review. So
// it gets its own method rather than being folded into the upgrade
// channel, and it never triggers a restart: the node rewrites the agent's
// package list and the running agent picks it up on its next package
// load.
//
// That last part is a deliberate choice with a cost, and the cost is
// worth naming. Restarting the edge would guarantee the new package is
// live, and it would also drop every in-flight turn on the node and make
// a rolling release a rolling outage. A package that is admitted but not
// yet loaded is a state an operator can see in agent.state; a node whose
// turns were dropped is an incident the release caused.

const (
	// MethodPluginInstall (manager → edge): fetch, verify, review and
	// activate one plugin package.
	//
	// The node answers with the review's own verdict rather than a bare
	// error, because a refusal and a failure are different events and an
	// operator needs to be able to tell them apart. A caller that got
	// only an error string would retry a signature refusal forever.
	MethodPluginInstall = "plugin.install"

	// MethodPluginRemove (manager → edge): take one package off. Used by
	// a rollback, and by an operator revoking a capability.
	MethodPluginRemove = "plugin.remove"

	// MethodPluginList (manager → edge): report the active package set.
	// The manager asks before a rollback so it can tell "removed" from
	// "was never installed", and an audit asking what this node can do
	// has one place to ask.
	MethodPluginList = "plugin.list"

	// MethodPluginRestore (manager → edge): re-activate a version this
	// node already has on disk.
	//
	// It is its own method rather than a mode of plugin.install because
	// what it needs from the wire is the opposite of install's: no URL,
	// no digest, no signature — just a name and a version the node can
	// find locally. Folding it into install would mean an install request
	// whose artifact fields are deliberately ignored, which is exactly
	// the kind of field a later reader assumes is checked.
	MethodPluginRestore = "plugin.restore"
)

// PluginInstallRequest asks a node to install one package.
//
// The bundle is named rather than inlined. A plugin is a few hundred
// kilobytes; inlining it would put a release into every node's tunnel
// buffer at once, and the tunnel is the same pipe carrying agent events.
type PluginInstallRequest struct {
	// Plugin, Version, URL, SHA256, Signature and KeyID mirror
	// ports.PluginSpec. They are spelled out rather than embedded so the
	// wire shape is stable if the port grows a field that is not part of
	// the protocol.
	Plugin    string `json:"plugin"`
	Version   string `json:"version"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature"`
	KeyID     string `json:"key_id,omitempty"`
}

// PluginInstallResponse is the node's verdict.
//
// Status is deliberately a closed set rather than a bool plus an error
// string. "installed", "refused" and "failed" are three different
// operational responses, and collapsing the first two into a success flag
// is how a rollout ends up treating a signature refusal as a node that
// needs retrying.
type PluginInstallResponse struct {
	Status  string `json:"status"`
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	Digest  string `json:"digest,omitempty"`
	// Replaced is what this package was before, when it was there at all.
	//
	// It travels with the answer rather than being derived by the caller,
	// for the same reason the node has to capture it: the only list the
	// node can report afterwards has the new version in it, so a manager
	// working the previous version out of Installed would be working it
	// out of the one piece of information that changed. A rollback
	// without this is a delete, and a delete on a node that was already
	// running the package removes the operator's own installation.
	Replaced *PluginEntry `json:"replaced,omitempty"`
	Reason   string       `json:"reason,omitempty"`
	// Installed is the package set after the request, so the manager
	// learns the node's whole state from one round trip and does not have
	// to ask again to find out what a refusal left behind.
	Installed []PluginEntry `json:"installed,omitempty"`
}

// Plugin install statuses.
const (
	// PluginStatusInstalled means the package passed the node's review and
	// is in the active set.
	PluginStatusInstalled = "installed"
	// PluginStatusRefused means the node declined it: bad signature, a
	// manifest that does not load, a package that does not target the
	// edge, or one above this node's policy ceiling. Retrying will not
	// change the answer.
	PluginStatusRefused = "refused"
	// PluginStatusFailed means the node could not carry out the request —
	// no disk, no network, an unreadable trust store. A later wave may
	// succeed where this one did not.
	PluginStatusFailed = "failed"
)

// PluginEntry is one package in a node's active set.
type PluginEntry struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	Digest  string `json:"digest,omitempty"`
}

// PluginRestoreRequest asks a node to put a version it already has back.
//
// There is deliberately no URL or digest here. The bytes are on the node,
// and a request that carried a source would be a request that could ask the
// node to fetch something new while calling itself a rollback.
type PluginRestoreRequest struct {
	Plugin string `json:"plugin"`
	// Version is the version to make active. It must be one this node has
	// installed; anything else is refused, not fetched.
	Version string `json:"version"`
}

// PluginRestoreResponse is the node's verdict on a restore.
//
// It carries the whole active set for the same reason the install answer
// does: a manager that had to ask again to find out what the restore left
// behind would be making one decision from two moments.
type PluginRestoreResponse struct {
	Status    string        `json:"status"`
	Plugin    string        `json:"plugin"`
	Version   string        `json:"version,omitempty"`
	Digest    string        `json:"digest,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	Installed []PluginEntry `json:"installed,omitempty"`
}

// PluginRemoveRequest asks a node to take one package off.
type PluginRemoveRequest struct {
	Plugin string `json:"plugin"`
	// Version, when set, requires the node to be removing exactly this
	// version. A rollback that raced a newer release would otherwise
	// remove the new one — the request means "undo what I put here", and
	// a version makes that checkable rather than hopeful.
	Version string `json:"version,omitempty"`
}

// PluginRemoveResponse is the node's answer.
type PluginRemoveResponse struct {
	Status string `json:"status"`
	Plugin string `json:"plugin"`
	// Version is what the node actually has for this package after the
	// request, when it still has it. A guard refusal needs it: "asked to
	// remove 0.1.0 but 0.2.0 is installed" is the sentence an operator
	// acts on, and it is unreadable without the version the node found.
	Version   string        `json:"version,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	Installed []PluginEntry `json:"installed,omitempty"`
}

// PluginListRequest is empty. It exists so the method has a body type.
type PluginListRequest struct{}

// PluginListResponse is the node's active package set.
type PluginListResponse struct {
	Installed []PluginEntry `json:"installed"`
}
