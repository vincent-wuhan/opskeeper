// Package federationlink is the root's end of the cluster channel.
//
// It exists because the two ends of that channel do not have the same
// problem. The child side (service/federationchild) already answers from state
// it holds alone. The root side has no state to answer from at all: before
// this package there was no way for a root to reach a child it had enrolled,
// so the control plane could mint a provisioning token and sign a bundle and
// then had no path to either.
//
// What lives here is three things and nothing else:
//
//   - which authenticated caller may speak for which child cluster
//   - the two outbound calls on that channel (cluster.policy, cluster.state)
//   - the refusal to do either for a caller nobody has vouched for
//
// # Why a child dials as a node
//
// The wire contract in core/floor/tunnel already answers this: a child cluster
// dials the root the same way a node dials its manager, so the root's tunnel
// service authenticates it exactly as it authenticates an edge. Widening
// tunnel.Session with a cluster identity would have put a second credential on
// a path that already has one, and the second one would have needed its own
// enrolment story to say the same thing.
//
// So the provisioning token is not what gets this process through the door. It
// is what the root checks *after* the door, to decide whether the authenticated
// caller on the other end of it may act for the cluster it is claiming. An
// edge credential on its own is not enough to become a child, and a
// provisioning token on its own is not enough either: a token proves a
// relationship, not a connection.
//
// # The binding is a cache, not an authority
//
// Nothing here decides whether a cluster exists or whether a policy is
// allowed. Those answers belong to the registry and to the child's own
// receiver. This package remembers which edge id is currently speaking for
// which cluster so that a publish can be turned into a push, and it drops
// that memory the moment the answer stops coming.
package federationlink
