package ports

import "context"

// PluginSpec is one package the control plane wants on a node.
//
// It carries a URL and a digest rather than the package bytes, for the
// same reason the edge upgrade channel does: a node pulls, so a release is
// served once and cached by every node that fetches it, and a 200-node
// rollout does not put 200 copies of the same tarball through the control
// plane's process at the same time.
type PluginSpec struct {
	// Name and Version identify the package. The node refuses a package
	// whose manifest disagrees with them, so a manager that mislabels a
	// release is refused rather than installed under the wrong name — a
	// version that does not say what it is cannot be rolled back to.
	Name    string
	Version string
	// URL is where the tarball is served. http and https only.
	URL string
	// SHA256 is the digest of the tarball as downloaded, lower hex. It is
	// checked before the archive is opened, so a corrupt or substituted
	// body never reaches the extractor.
	SHA256 string
	// Signature is the detached ed25519 envelope over the package tree
	// digest, base64. The node verifies it against its own trust store and
	// never against anything the control plane says about the signer.
	Signature string
	// KeyID names the key the node should have used. It is a hint that
	// makes a mismatch legible; it is not authority, and a node whose
	// trust store does not hold the key refuses regardless of what the
	// manager believed.
	KeyID string
}

// PluginInfo is one package in a node's active set.
//
// It is not a PluginSpec because a spec is a request and a request carries
// things a node deliberately does not keep: the URL it came from, the
// transport digest, the signature envelope. Keeping those would mean a
// node holding a manager's description of a package rather than the
// package, and a listing that could be edited into disagreeing with what
// is actually installed. The digest here is recomputed from the tree, not
// remembered from the request.
type PluginInfo struct {
	Name    string
	Version string
	// Digest is the tree digest over the installed directory. It is what
	// a manager compares against what it meant to ship.
	Digest string
}

// PluginState is what a node reports about a package after a request.
//
// The distinction between Refused and Failed is the one an operator needs
// at 3am: a refusal is a decision (bad signature, wrong target, above the
// node's ceiling) and retrying it will fail identically, while a failure is
// this node's problem (no disk, no network) and may well succeed on the
// next wave.
type PluginState struct {
	Name    string
	Version string
	// Digest is the tree digest the node computed for what it installed.
	// It is echoed so the manager can compare it against what it meant to
	// ship, which is the only way to notice that a mirror served two
	// different bodies for one version.
	Digest string
	// Replaced is what this package was before, when it was there at all.
	// It is nil when the package was not installed.
	//
	// The node cannot be asked for this after the fact and the manager
	// cannot derive it: the only list the node can report is the one
	// after the install, and the entry for this name in it is the new
	// version. So the node has to say what it overwrote, and it has to say
	// it at the moment it knows. Without it a rollback is a delete, and a
	// delete on a node that was already running the package removes the
	// operator's own installation.
	Replaced *PluginInfo
	// Installed reports whether the package is in the agent's package set
	// *now*. It is a post-state, not an event: a Remove answers false
	// because the package is gone, and a successful Install answers true
	// because the package is there. A caller that needs to know which of
	// those happened reads Refused and Error, which are mutually
	// exclusive and both empty on success.
	Installed bool
	// Note is for a caller that did something and needs to say what. It
	// carries no verdict: "was not installed" and "already installed" are
	// both notes on a state the node reports faithfully either way.
	Note string
	// Refused carries the gate's own reason when the package was declined.
	// Retrying will not change the answer.
	Refused string
	// Error carries a transport or host failure. Never set alongside
	// Refused.
	Error string
}

// PluginInstaller is a node's ability to take a package on and off.
//
// It is a port rather than an in-process call because the review it
// performs — signature, manifest, then this node's own policy — is the
// node's decision, not the control plane's, and the control plane must not
// be able to reach past it. The manager may ask for a package; only the
// node decides whether it stays.
//
// Implementations must be safe for concurrent use and must leave the
// previous package set completely intact on any failure. A half-installed
// package set is a node whose agent has lost tools it had an hour ago,
// and the model cannot tell the difference between that and a bug in the
// toolset.
type PluginInstaller interface {
	// Install stages, verifies, reviews and activates a package. It
	// returns the resulting state, which reports a refusal as a refusal
	// rather than as an error.
	Install(ctx context.Context, spec PluginSpec) PluginState
	// Remove takes a package off. Removing a package that is not
	// installed is not an error — a rollback that is already done should
	// succeed.
	//
	// version, when set, requires the node to be removing exactly that
	// version. It is a guard and not a lookup: a rollback that raced a
	// newer release would otherwise remove the new one, and the request
	// means "undo what I put here". A node that finds a different version
	// installed refuses and changes nothing — a refusal an operator can
	// read, where a deletion would be indistinguishable from success.
	Remove(ctx context.Context, name, version string) PluginState
	// Installed reports the active set, sorted by name. The manager asks
	// before a rollback so it can tell "removed" from "was never there",
	// and an audit asking what this node can do has one place to ask.
	Installed() []PluginInfo
	// Restore re-activates a version this node already has on disk, with
	// no fetch and no review of new bytes.
	//
	// It exists because Install cannot do this job. An upgrade keeps the
	// superseded version, so the bytes are already here — but Install
	// takes a PluginSpec, and the only spec the manager still holds when
	// it decides to roll back is the one for the version it is removing.
	// Asking the node to re-install the old version by URL would mean the
	// manager remembering every version's coordinates forever, and a
	// rollback that needs the release server to still be serving an
	// artifact it may have garbage-collected.
	//
	// Restore makes the rollback what it should always have been: an
	// operation on what this node has, decided by this node, requiring
	// nothing of anybody else. A version that is not on disk is refused —
	// the node does not fetch on demand, because a restore that downloads
	// is an install wearing a different name.
	Restore(ctx context.Context, name, version string) PluginState
}
