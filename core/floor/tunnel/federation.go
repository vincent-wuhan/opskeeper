package tunnel

import (
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
)

// The cluster channel: a root control plane and its child clusters.
//
// A child cluster is an opskeeper control plane of its own that runs its own
// nodes and its own approvals. The root does not reach into it and it does not
// ask the root anything at decision time — the plan's "中心下发策略而非实时
// 决策" means the root's job ends at publishing a version. What travels here
// is therefore three things and no more: who a cluster is, which policy
// version it should be enforcing, and what it is actually enforcing.
//
// The semantics of a policy version — the monotonic rule, the refusal replay,
// the last-known-good behaviour — are not in this file. They are in
// core/floor/federation, because they are the same rules whether a bundle
// arrives over this channel or over a file someone copied by hand, and a
// property that holds for one transport and not the other is a property
// nobody can rely on.
const (
	// MethodClusterHello is child → root, once per connection: this is
	// who I am and what I can accept.
	//
	// It is a push rather than a request the root makes, because the
	// child is the side that dials. The root answers it as a normal RPC
	// response, which is how the child learns whether the root will
	// speak to it at all — a cluster the root has not been provisioned
	// for is refused here rather than at the first policy push, so the
	// operator finds out at connect time instead of at rollout time.
	MethodClusterHello = "cluster.hello"

	// MethodClusterPolicy is root → child: enforce this version.
	//
	// The response is the child's own verdict, in the shape
	// federation.Outcome, because "refused" and "the tree has not
	// arrived yet" are different answers and the root has to tell them
	// apart: one is final for that version and the other is a retry.
	MethodClusterPolicy = "cluster.policy"

	// MethodClusterState is root → child: what are you enforcing?
	//
	// It is a separate method rather than a field on the policy push
	// response because the interesting time to ask is when there has
	// been no push for a while — a root that has lost track of what a
	// cluster is running, or an operator reconciling after a rebuild,
	// needs the child's answer and must not have to provoke a policy
	// rollout to get it.
	MethodClusterState = "cluster.state"
)

// ---------------------------------------------------------------------
// cluster.hello
// ---------------------------------------------------------------------

// ClusterHelloRequest is a child cluster introducing itself to a root.
//
// # Who is allowed to say this
//
// The root does not believe the Cluster field. It believes the authenticated
// tunnel session, which today carries exactly one thing — an EdgeID — and a
// child cluster dials the root the same way a node dials its manager.
//
// That is a deliberate choice and it is worth being explicit about what it
// costs and what it buys. The alternative is to widen tunnel.Session with a
// cluster identity, which touches the authentication path of every existing
// edge, and which would then need its own provisioning story for a credential
// that exists to say "this peer may act for cluster X". Putting the binding in
// the root's own registry keeps the wire and the auth path exactly as they are
// and moves the question to where the answer lives: the root decides which
// authenticated caller may speak for which cluster, and a child that names a
// cluster it was not provisioned for is refused.
//
// So the field below is a request to be believed, and the root's job is to
// check it against the session before it acts on anything else in this
// message.
type ClusterHelloRequest struct {
	// Cluster is the identity the child is asking to act as.
	Cluster federation.Cluster `json:"cluster"`
	// ProvisioningToken is the secret the root issued when it enrolled
	// this child. It is what turns the claim above into something the root
	// can check without having to have enrolled the caller by edge id.
	//
	// It is here rather than in the tunnel's geminio meta because the
	// tunnel meta is a connection-level fact and this is a
	// per-cluster one: the same edge identity may legitimately speak for
	// more than one child, and rotating one cluster's token must not
	// require re-dialling every node.
	ProvisioningToken string `json:"provisioning_token"`
}

// ClusterHelloResponse is the root's answer.
//
// Accepted false is the interesting case and it is deliberately not an error:
// the root understood the child, and is declining to speak to it. A child that
// retries forever because it got a connection error would hide a provisioning
// mistake behind a reconnect loop.
type ClusterHelloResponse struct {
	// Accepted is whether the root will serve this cluster.
	Accepted bool `json:"accepted"`
	// Reason explains a refusal, for the child's log. It names the
	// problem, not the policy.
	Reason string `json:"reason,omitempty"`
	// PolicyVersion is the version the root believes this cluster is on.
	// A child that disagrees has the more recent information — it is the
	// one holding the state — so the two can be reconciled without either
	// being assumed right.
	PolicyVersion uint64 `json:"policy_version,omitempty"`
	// HeartbeatSeconds is how often the child should call cluster.state
	// so that the root can tell a quiet child from a dead one. It is a
	// hint, not a contract: a child that cannot meet it is still
	// enforcing what it last accepted.
	HeartbeatSeconds int `json:"heartbeat_seconds,omitempty"`
}

// ---------------------------------------------------------------------
// cluster.policy
// ---------------------------------------------------------------------

// ClusterPolicyRequest is the root publishing one policy decision.
//
// It is federation.Bundle, not a second struct shaped like it. The bundle is
// validated against its own envelope on the child, and a wire struct that
// merely resembled it would be a place for the two to drift.
type ClusterPolicyRequest struct {
	Bundle federation.Bundle `json:"bundle"`
	// StagedPath is where the child should look for the tree, when the
	// tree is already on this machine.
	//
	// It is a path on the *child's* filesystem, and the child treats it
	// as untrusted input bounded to its own staging area; a root that can
	// name a path anywhere on the child's disk would be a remote
	// file-write primitive wearing a policy's clothes.
	//
	// It is mutually exclusive with Source. Both naming a place is not a
	// request with two hints, it is a request whose meaning depends on
	// which field a reader happens to check first.
	StagedPath string `json:"staged_path,omitempty"`
	// Source is where the child should *fetch* the tree from.
	//
	// It is the shape tunnel.PluginInstallRequest already uses for a
	// package, deliberately: a policy tree and a plugin package are both
	// signed trees that travel out of band and arrive as a receipt, and a
	// second spelling of that receipt would be a second thing to get
	// wrong.
	Source *PolicySource `json:"source,omitempty"`
}

// PolicySource is where a policy tree can be fetched from.
//
// The two fields are not redundant and the pairing is the point: URL says
// where to get it and ArchiveSHA256 says whether what arrived is what was
// offered. A source carrying a digest is a source whose corruption is
// detected before anything is unpacked; a source without one is a request to
// trust the transport, which for a file the root names and this child
// promotes to live policy is not a trade worth making.
//
// Neither field is authorisation. Anyone who can reach a child can put a URL
// in this message, and the answer to that is the one thing the whole channel
// is built on: the tree is only ever promoted after pluginmanifest.VerifyDir
// proves it carries a signature from a key this cluster trusts. The URL is
// transport, the digest is integrity, and the signature is authority.
type PolicySource struct {
	// URL is an http, https or file URL. The scheme is checked by the
	// child rather than trusted, because a source is attacker-supplied
	// input in exactly the way StagedPath is.
	URL string `json:"url"`
	// ArchiveSHA256 is the hex digest of the archive as it is served.
	// Lower hex, 64 characters.
	ArchiveSHA256 string `json:"archive_sha256"`
}

// ClusterPolicyResponse is the child's verdict on one push.
//
// Every field is present whether the push succeeded or not, and
// federation.Outcome carries the decision. A caller that only checked for an
// error would treat a refusal as a transport failure and retry a version that
// will never be accepted — the failure mode the plugin channel already calls
// out in MethodPluginInstall.
type ClusterPolicyResponse struct {
	Outcome federation.Outcome `json:"outcome"`
	// Retryable is true only when the child could not have decided: the
	// tree has not arrived, the connection dropped mid-push, the
	// filesystem is temporarily unusable. It is false for every decision
	// the child actually made, including a refusal.
	//
	// It is separate from the error because the two answers are produced
	// by different systems — a decision by the policy engine and a
	// condition by the transport — and a child that cannot tell them apart
	// will either hammer a refused version or abandon a pending one.
	Retryable bool `json:"retryable,omitempty"`
	// Error is the child's message, for the root's log. It is not
	// trusted by the root and is not used to decide anything.
	Error string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------
// cluster.state
// ---------------------------------------------------------------------

// ClusterStateRequest asks a child what it is enforcing.
type ClusterStateRequest struct {
	// Cluster is echoed for symmetry with the other two methods. It is
	// not a filter: a child answers for itself or not at all.
	Cluster federation.ClusterID `json:"cluster"`
}

// ClusterStateResponse is the answer, and it is answerable with no root in
// sight — which is the entire reason this method exists separately from the
// push.
//
// LastSeen is when the child last had any contact with a root, not when this
// response was written. A cluster that has been enforcing version 9 on its own
// for three days is not a cluster in trouble; it is a cluster doing exactly
// what the plan asked for, and the console should say so rather than showing it
// as unreachable.
type ClusterStateResponse struct {
	// Policy is the version in force.
	Policy uint64 `json:"policy"`
	// Bundle is the decision that put it there, so an operator reading
	// this does not have to correlate against the root's publish log.
	Bundle federation.Bundle `json:"bundle"`
	// LastRootContact is when this child last heard from any root.
	LastRootContact time.Time `json:"last_root_contact"`
	// Enforcing reports whether the child is actively applying this
	// policy. A false with a Policy set means the child is holding the
	// last version it accepted and knows it is degraded.
	Enforcing bool `json:"enforcing"`
	// NodeCount is the child’s own count of nodes, reported so the root
	// does not have to keep asking.
	NodeCount int `json:"node_count,omitempty"`
}
