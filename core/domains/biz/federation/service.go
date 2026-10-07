package federation

import (
	"context"
	"errors"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// ErrNoReleaseKey means this root holds no key it can sign a policy with.
//
// It is a distinct error from ErrNothingToPublish, and the difference is the
// whole point of having it: ErrNothingToPublish says "I looked at the tree
// you named and could not put a signature on it", which is an operator who
// pointed at the wrong directory. This one says "this root cannot sign
// anything at all", which is a deployment that was never given a release
// key. Both are refusals, and neither is a bug in the request.
var ErrNoReleaseKey = errors.New("federation: this root holds no release key, so it can issue no policy")

// Service is the root's cluster federation, composed.
//
// It exists because the two halves have genuinely different availability. The
// registry needs nothing but memory: a root with no release key can still
// enrol a child, still remember what it last heard, and still answer "what is
// this cluster enforcing" as long as the child said hello. The publisher
// needs the release key, and a root without one can do exactly one thing less:
// issue a decision.
//
// Folding those into one type with a nil publisher means the degradation is a
// property of the design rather than something the assembly root has to
// remember. The alternative — refusing to construct a publisher at all — turns
// a missing key into a control plane that will not start, which is the
// difference between "one feature is off" and "the platform is down".
type Service struct {
	reg *Registry
	pub *Publisher
	// push_ and dist are both optional, and each being absent is a
	// different fact an operator needs to be able to read.
	//
	// push_ is the tunnel. Without it this root cannot tell any child
	// anything, and a publish still issues a version — because the ledger
	// records what this root decided, not what arrived.
	//
	// dist is where the bytes go. Without it there is nowhere for a child
	// to fetch a tree from, so a delivery attempt is not made at all
	// rather than being made with an empty URL.
	push_ Pusher
	dist  Distributor
	// redeliverer answers for a delivery that already happened. It is
	// separate from dist because the two answer different questions about
	// the same bytes: where do they go, and where did they go.
	redeliverer Redeliverer
}

// NewService composes the registry and, when there is a release key, the
// publisher.
//
// pub may be nil, and that is the keyless root. reg may not: a service with
// no registry cannot answer who it has enrolled, which is the one question it
// must always be able to answer.
func NewService(reg *Registry, pub *Publisher) (*Service, error) {
	if reg == nil {
		return nil, errors.New("federation: a service needs a registry")
	}
	return &Service{reg: reg, pub: pub}, nil
}

// Registry exposes the membership this service answers from, so a caller that
// has a Service does not also have to be handed a Registry.
func (s *Service) Registry() *Registry { return s.reg }

// SetDelivery wires the two halves a publish needs beyond signing.
//
// Both may be nil and both are back-filled rather than required at
// construction, for the reason the publisher is optional: a root that has
// been given no release key and no tunnel still enrols clusters and answers
// what they enforce, and refusing to assemble that root would turn a
// deployment detail into a control plane that will not start.
func (s *Service) SetDelivery(push Pusher, dist Distributor, redeliver Redeliverer) {
	s.push_ = push
	s.dist = dist
	s.redeliverer = redeliver
}

// CanPublish reports whether this root can issue a policy at all.
//
// It is a question the console asks rather than one it infers from a failed
// publish, because "this root cannot sign" and "the tree you named is not a
// package" send an operator to different pages.
func (s *Service) CanPublish() bool { return s.pub != nil }

// Enroll provisions a child cluster and returns its provisioning token, once.
func (s *Service) Enroll(id federation.ClusterID, name string) (string, error) {
	return s.reg.Enroll(id, name)
}

// Members lists every enrolled child, in identity order.
func (s *Service) Members() []Member { return s.reg.Members() }

// Member is one child's ledger state.
func (s *Service) Member(id federation.ClusterID) (Member, bool) { return s.reg.Member(id) }

// Publish issues the next policy version to one cluster.
//
// With no release key it refuses before touching the registry, so a keyless
// root cannot spend a version number on a decision it was never able to make.
// The ledger is the thing that must not lie here: HighestIssued is what the
// monotonic guarantee is built on, and burning one on a refusal would leave
// the cluster permanently one version ahead of any policy it can receive.
func (s *Service) Publish(ctx context.Context, id federation.ClusterID, req PublishRequest) (PublishResult, error) {
	if s.pub == nil {
		return PublishResult{}, ErrNoReleaseKey
	}
	res, err := s.pub.Publish(ctx, id, req)
	if err != nil {
		return PublishResult{}, err
	}
	// The version is issued. Delivery is attempted afterwards and its
	// failure is reported inside the result rather than as an error,
	// because an error here would tell the operator the publish did not
	// happen — and it did. What did not happen is that a cluster heard
	// about it, and the two facts need to stay separable.
	res.Delivery = s.deliver(ctx, id, res.Bundle, req.StagedRoot)
	return res, nil
}

// Redeliver puts an already-issued version on the wire again.
//
// It is the retry, and it is deliberately a different call from Publish
// rather than a flag on it. Re-publishing to retry would mint a new version
// per attempt, and every one of them would be newer than the last so nothing
// would object — a cluster with a flaky link would climb a version number
// for every retry while the operator watched it happen. This sends the exact
// bundle the ledger says was issued, so a child that already refused it
// replays that refusal instead of being asked about a decision nobody made.
func (s *Service) Redeliver(ctx context.Context, id federation.ClusterID) (PublishResult, error) {
	b, ok := s.reg.BundleFor(id)
	if !ok {
		return PublishResult{}, fmt.Errorf("%w: cluster %q has issued no policy to redeliver", ErrNotEnrolled, id)
	}
	if s.redeliverer == nil {
		return PublishResult{}, fmt.Errorf("%w: cluster %q has an issued policy but this root keeps no record of where its bytes went",
			ErrNoDeliveryPath, id)
	}
	src, err := s.redeliverer.SourceFor(b)
	if err != nil {
		return PublishResult{}, err
	}
	res := PublishResult{Bundle: b}
	res.Delivery = s.push(ctx, id, b, src)
	return res, nil
}

// deliver packages the tree and puts the bundle on the wire.
func (s *Service) deliver(ctx context.Context, id federation.ClusterID, b federation.Bundle, stagedRoot string) Delivery {
	if s.push_ == nil {
		return Delivery{Error: "this root has no tunnel to its children"}
	}
	if s.dist == nil {
		return Delivery{Error: ErrNoDeliveryPath.Error()}
	}
	src, err := s.dist.Distribute(ctx, b, stagedRoot)
	if err != nil {
		return Delivery{Attempted: true, Error: err.Error()}
	}
	return s.push(ctx, id, b, src)
}

// Redeliverer reports where an already-delivered tree can be fetched from
// again.
//
// It is a second port rather than a reuse of Distributor because a redelivery
// cannot repack, and that is not a limitation of the implementation. The
// bytes a child will compare against were written when the decision was first
// published; a tar of the same tree produced twice can differ in a header
// field, and the digest is checked before anything is unpacked. So a repack
// would produce a source whose digest does not match the archive already on
// the child's disk — a retryable integrity failure, forever, for a decision
// the child would otherwise have applied on the first try.
//
// Naming the archive by cluster and version is what makes this work: the
// file from the first delivery is still on disk under the name this decision
// will always have.
type Redeliverer interface {
	// SourceFor returns the source a previous delivery of b used.
	SourceFor(b federation.Bundle) (tunnel.PolicySource, error)
}

// Acknowledge records what a child answered about a version.
func (s *Service) Acknowledge(id federation.ClusterID, out federation.Outcome) error {
	return s.reg.Acknowledge(id, out)
}
