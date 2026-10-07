package federation

import (
	"context"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Pusher reaches one child cluster over the tunnel.
//
// It is a port, and it lives here rather than beside the HTTP handler that
// uses it, because the interesting implementation is on the other side of
// the tunnel — the manager's reverse-call client — and a port declared by its
// consumer in the layer above would have the implementation importing
// upward to satisfy it. Two callers want this shape: the control plane's
// routes, and the rollout that stages a tree and then tells a child about
// it. Neither of them should have to know how a child is reached.
//
// It is separate from Registry on purpose. Membership and the version ledger
// are things this root owns and can answer with or without a tunnel; asking a
// child what it is enforcing is a round trip, and a root whose tunnel is down
// must still be able to say who it has enrolled and what it has sent them.
type Pusher interface {
	// PushPolicy delivers one decision and returns the child's verdict.
	//
	// A returned error is always a transport or protocol failure, never a
	// refusal: a child that declined a version answers with a verdict and
	// no error, and the difference is the whole reason Retryable exists
	// on the wire.
	PushPolicy(ctx context.Context, id federation.ClusterID, req tunnel.ClusterPolicyRequest) (tunnel.ClusterPolicyResponse, error)
	// AskState asks the child what it is enforcing. It is the only call
	// whose answer is authoritative about the child rather than about
	// this root.
	AskState(ctx context.Context, id federation.ClusterID) (tunnel.ClusterStateResponse, error)
}
