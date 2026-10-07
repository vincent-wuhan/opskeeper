package federationlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

var (
	// ErrUnbound means no authenticated caller is currently speaking for
	// that cluster.
	//
	// It is a distinct error from a failed call because the two mean
	// different things to an operator: a failed call says the child is
	// there and did not answer, and this says the root has not seen it
	// since it was enrolled. Collapsing them would make a cluster that
	// never connected look like one that is merely busy.
	ErrUnbound = errors.New("federation: no caller is bound to that cluster")

	// ErrProtocol means the peer answered in a shape this root cannot
	// reconcile with what it sent.
	//
	// It exists because the alternative is worse than an error. A push
	// for version 9 that comes back carrying version 7 is not a fact
	// about the cluster; it is a fact about the plumbing, and writing it
	// into the ledger would make the console report a version the root
	// never published as if the child had adopted it. Refusing to answer
	// at all is the only safe reading of an answer that does not fit.
	ErrProtocol = errors.New("federation: the child answered about a different decision than the one sent")
)

// Caller is the tunnel surface this package needs.
//
// It is the manager's reverse-call client narrowed to one method, and it is
// an interface rather than the concrete client so that this package — the part
// of the root that is allowed to push policy at a cluster — can be exercised
// without a broker. The concrete client satisfies it as it stands.
type Caller interface {
	Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error)
}

// Enrolled is what the root knows about a cluster, as the link needs it.
//
// One field, and the field is the whole argument. The registry behind this
// port also knows the newest version the root ever published, and the link
// must NOT answer a hello with that number: a child that is already ahead of
// the ledger would be told it is behind, and a child that trusts the answer
// would refuse the next push as a replay. That reasoning is three sentences
// long and it used to live in a comment next to a field on somebody else's
// struct — `m.Acknowledged`, next to `m.HighestIssued`, `m.Behind()` and
// `m.LastAck`, none of which this package had any use for.
//
// A named field carries the argument with it. A bare uint64 would be shorter
// and would be one careless edit away from the wrong number, with nothing on
// screen to say which of the two it should have been.
type Enrolled struct {
	// Acknowledged is the last version the child confirmed, which is what
	// the wire is answered with. Never the newest version this root
	// published.
	Acknowledged uint64
}

// Clusters is the root's own answer to "may this caller act for that
// cluster, and what do I believe about it".
type Clusters interface {
	Authenticate(id floorfed.ClusterID, token string, claimed floorfed.Cluster) (Enrolled, error)
	// Known is for the log line this handler writes on a refusal, and
	// nothing else. See Registry.Known for why the wire cannot answer
	// this question but the operator's terminal can.
	Known(id floorfed.ClusterID) bool
}

// Option configures a Link.
type Option func(*Links)

// WithHeartbeat sets the interval the root asks a child to check in at. It
// is a hint the child may ignore; the root does not enforce it.
func WithHeartbeat(d time.Duration) Option {
	return func(l *Links) {
		if d > 0 {
			l.heartbeat = d
		}
	}
}

// WithLogger sets where refusals and pushes are reported.
func WithLogger(log *slog.Logger) Option {
	return func(l *Links) {
		if log != nil {
			l.log = log
		}
	}
}

// Links is the root's table of who speaks for whom, and the outbound half of
// the cluster channel.
type Links struct {
	caller    Caller
	clusters  Clusters
	log       *slog.Logger
	heartbeat time.Duration

	mu sync.RWMutex
	// byCluster is the direction a publish needs: cluster to the caller
	// that may act for it.
	byCluster map[floorfed.ClusterID]uint64
	// byEdge is the reverse, and it is a set rather than a value because
	// one caller may legitimately speak for more than one child. It
	// exists so that a disconnection can forget everything that caller
	// was speaking for in one step.
	byEdge map[uint64]map[floorfed.ClusterID]struct{}
}

// NewLink builds the root side of the cluster channel.
func NewLink(caller Caller, clusters Clusters, opts ...Option) (*Links, error) {
	if caller == nil {
		return nil, errors.New("federation: a link needs a tunnel to push over")
	}
	if clusters == nil {
		return nil, errors.New("federation: a link needs a registry to check callers against")
	}
	l := &Links{
		caller:    caller,
		clusters:  clusters,
		log:       slog.Default(),
		heartbeat: 60 * time.Second,
		byCluster: map[floorfed.ClusterID]uint64{},
		byEdge:    map[uint64]map[floorfed.ClusterID]struct{}{},
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

// The Pusher port is satisfied by construction, and the assertion used to
// live here — "rather than in the package that consumes it so that a change
// to either side fails at compile time instead of at the first publish".
// It does not any more, and the reason is the only reason this file is not
// importing the federation domain at all: `Pusher` is declared over there,
// so an assertion against it is a dependency on it. The guarantee is
// unchanged and the assertion now lives with the wiring that hands a *Links
// to that port — cmd/opskeeper/federation_wiring.go — which is a place that
// already imports both sides and is not itself a bounded context.

// HandleHello answers cluster.hello.
//
// The signature is an unnamed func type on purpose: it is assignable to the
// manager tunnel client's handler type without either package importing the
// other, which is what keeps this dependency pointing one way.
//
// It returns an error only for a caller it could not read. A caller it read
// and does not accept gets ClusterHelloResponse{Accepted: false}, because
// those are different events: a child that cannot parse the answer retries,
// and a child that was refused does not, and collapsing them turns a
// provisioning mistake into a reconnect loop.
func (l *Links) HandleHello(_ context.Context, edgeID uint64, body []byte) ([]byte, error) {
	var req tunnel.ClusterHelloRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("federation: cluster.hello: %w", err)
	}
	return json.Marshal(l.hello(edgeID, req))
}

func (l *Links) hello(edgeID uint64, req tunnel.ClusterHelloRequest) tunnel.ClusterHelloResponse {
	// A hello with no authenticated session behind it is not a hello. The
	// tunnel authenticates before it dispatches, so this should be
	// unreachable — and if it ever is reached, binding on it would let
	// anyone who can open a connection claim a cluster.
	if edgeID == 0 {
		return refuseFederation("this connection carries no authenticated session")
	}
	if !req.Cluster.ID.Valid() {
		return refuseFederation("the identity in the hello is not a cluster identity")
	}
	if req.ProvisioningToken == "" {
		return refuseFederation("no provisioning token")
	}
	enrolled, err := l.clusters.Authenticate(req.Cluster.ID, req.ProvisioningToken, req.Cluster)
	if err != nil {
		// The wire gets one sentence for every refusal, whatever went
		// wrong. The registry does not tell this handler whether the
		// cluster is unknown or the token is wrong, and repeating its
		// discipline here is what keeps the wire from leaking what the
		// console already knows.
		//
		// The operator's log does not have to keep that discipline,
		// because the operator is not the party the oracle protects
		// against — an attacker is. And it matters: the commonest cause
		// of this refusal is a root that restarted and forgot every
		// member, which then refuses a child's own valid token and
		// looks, from both ends, exactly like a credential problem.
		// Without this the first hypothesis during an incident is
		// "someone rotated the token" or "someone is guessing at ours",
		// and both send the operator looking in the wrong place.
		reason := "the provisioning token does not match"
		if !l.clusters.Known(req.Cluster.ID) {
			reason = "this root has no member for that cluster; a root that " +
				"restarted without a durable Ledger forgets every one, " +
				"and re-enrolment mints a new token"
		}
		l.log.Warn("federation: cluster refused",
			slog.String("cluster", req.Cluster.ID.String()),
			slog.Uint64("edge_id", edgeID),
			slog.String("cause", reason),
			slog.Any("err", err),
		)
		return refuseFederation("this root does not serve that cluster")
	}

	l.bind(req.Cluster.ID, edgeID)
	l.log.Info("federation: cluster bound",
		slog.String("cluster", req.Cluster.ID.String()),
		slog.Uint64("edge_id", edgeID),
		slog.Uint64("root_believes", enrolled.Acknowledged),
	)
	return tunnel.ClusterHelloResponse{
		Accepted: true,
		// What the root believes the child is enforcing, which is the
		// last version the child confirmed — not the newest one the
		// root published. Sending the newer one would tell a child that
		// is already ahead of the ledger that it is behind, and a child
		// that trusted that would refuse the next push as a replay.
		PolicyVersion:    enrolled.Acknowledged,
		HeartbeatSeconds: int(l.heartbeat / time.Second),
	}
}

// PushPolicy sends one decision to a child and returns its verdict.
func (l *Links) PushPolicy(ctx context.Context, id floorfed.ClusterID, req tunnel.ClusterPolicyRequest) (tunnel.ClusterPolicyResponse, error) {
	edgeID, ok := l.edgeOf(id)
	if !ok {
		return tunnel.ClusterPolicyResponse{}, fmt.Errorf("%w: cluster %q", ErrUnbound, id)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return tunnel.ClusterPolicyResponse{}, fmt.Errorf("federation: encode push: %w", err)
	}
	raw, err := l.caller.Call(ctx, edgeID, tunnel.MethodClusterPolicy, body)
	if err != nil {
		// Transport, not decision. The caller must not read this as a
		// refusal: the child may never have seen the message at all, and
		// the version it was for is still worth pushing again.
		return tunnel.ClusterPolicyResponse{}, fmt.Errorf("federation: push version %d to cluster %q: %w", req.Bundle.Version, id, err)
	}
	var resp tunnel.ClusterPolicyResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return tunnel.ClusterPolicyResponse{}, fmt.Errorf("federation: cluster %q answered unreadably: %w", id, err)
	}
	// Every answer that is a decision names the version it is about. A
	// retryable answer is exempt on purpose: it means the child could not
	// decide, so its version field carries no claim to check.
	if !resp.Retryable && resp.Outcome.Version != req.Bundle.Version {
		return tunnel.ClusterPolicyResponse{}, fmt.Errorf("%w: pushed version %d, answered about version %d",
			ErrProtocol, req.Bundle.Version, resp.Outcome.Version)
	}
	return resp, nil
}

// AskState asks a child what it is enforcing.
//
// This is the only call in the whole channel whose answer is authoritative
// about the child rather than about the root, which is why it is a method of
// its own and not a field on the push response.
func (l *Links) AskState(ctx context.Context, id floorfed.ClusterID) (tunnel.ClusterStateResponse, error) {
	edgeID, ok := l.edgeOf(id)
	if !ok {
		return tunnel.ClusterStateResponse{}, fmt.Errorf("%w: cluster %q", ErrUnbound, id)
	}
	body, err := json.Marshal(tunnel.ClusterStateRequest{Cluster: id})
	if err != nil {
		return tunnel.ClusterStateResponse{}, fmt.Errorf("federation: encode state request: %w", err)
	}
	raw, err := l.caller.Call(ctx, edgeID, tunnel.MethodClusterState, body)
	if err != nil {
		return tunnel.ClusterStateResponse{}, fmt.Errorf("federation: ask cluster %q: %w", id, err)
	}
	var resp tunnel.ClusterStateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return tunnel.ClusterStateResponse{}, fmt.Errorf("federation: cluster %q answered unreadably: %w", id, err)
	}
	// A child that has never been told anything has no bundle to report,
	// so an empty one is the honest answer rather than a mismatch. A
	// non-empty one naming somebody else means this is not the answer to
	// the question that was asked, and returning it would tell an operator
	// that the wrong cluster is enforcing the wrong policy.
	if resp.Bundle.ClusterID != "" && resp.Bundle.ClusterID != id {
		return tunnel.ClusterStateResponse{}, fmt.Errorf("%w: asked cluster %q, answered with a bundle for %q",
			ErrProtocol, id, resp.Bundle.ClusterID)
	}
	return resp, nil
}

// Bound reports the caller currently speaking for a cluster.
func (l *Links) Bound(id floorfed.ClusterID) (uint64, bool) { return l.edgeOf(id) }

// Forget drops everything a disconnected caller was speaking for, and
// reports how many clusters actually lost their caller.
//
// The count is the count of released bindings, not the count of clusters the
// caller had ever claimed. A cluster that reconnected on another id is not
// affected by this caller going away, and a log line saying otherwise is a
// line an operator reads as "two clusters are offline" when one of them is
// answering.
//
// It is a map operation and that is the point: the caller id is opaque and
// the broker will hand the same number to somebody else, so leaving a stale
// binding behind would eventually push a policy to a process that never said
// hello. A child that comes back says hello again and is bound afresh.
func (l *Links) Forget(edgeID uint64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := l.byEdge[edgeID]
	delete(l.byEdge, edgeID)
	released := 0
	for id := range ids {
		if l.byCluster[id] == edgeID {
			delete(l.byCluster, id)
			released++
		}
	}
	return released
}

func (l *Links) bind(id floorfed.ClusterID, edgeID uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A reconnecting child gets a new caller id and says hello again, so
	// overwriting is the normal case rather than a conflict. The old id
	// stays in byEdge until the broker reports it gone, which costs one
	// map entry and is cleared by Forget.
	l.byCluster[id] = edgeID
	if l.byEdge[edgeID] == nil {
		l.byEdge[edgeID] = map[floorfed.ClusterID]struct{}{}
	}
	l.byEdge[edgeID][id] = struct{}{}
}

func (l *Links) edgeOf(id floorfed.ClusterID) (uint64, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	edgeID, ok := l.byCluster[id]
	return edgeID, ok
}

func refuseFederation(reason string) tunnel.ClusterHelloResponse {
	return tunnel.ClusterHelloResponse{Accepted: false, Reason: reason}
}
