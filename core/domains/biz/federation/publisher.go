package federation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// ErrNothingToPublish means the tree the operator named is not a package this
// root can sign.
var ErrNothingToPublish = errors.New("federation: no signed policy to publish")

// PublishRequest is an operator's instruction to give one cluster a new
// policy version.
//
// It names a tree, not an envelope. That is the whole design of this type and
// it is worth being blunt about the alternative: an endpoint that accepted a
// signature from the wire would let anyone who can reach the console ship a
// bundle signed by a key of their choosing, and the only thing standing
// between that and a cluster running an attacker-chosen policy would be
// whether the child's trust store happened to contain the attacker's key. The
// root holds the release key, so the root signs. A caller supplies bytes; this
// side of the wire decides what those bytes are worth.
type PublishRequest struct {
	// StagedRoot is a package tree on this root. It is signed as it
	// stands, and its manifest supplies the name and version — neither is
	// taken from the request, because an envelope that named a different
	// package would be the exact confusion the child's cross-check exists
	// to catch, and there is no reason for the root to create one.
	StagedRoot string `json:"staged_root"`
	// Reason is what the child logs and what an operator reads at 3am.
	// Free text, not signed over, and deliberately so: a signature that
	// covers a field whose only reader is a person buys nothing.
	Reason string `json:"reason,omitempty"`
}

// PublishResult is what an operator gets back from a publish.
type PublishResult struct {
	// Bundle is the decision that was issued. It is returned so the
	// caller can log exactly what went out, including the version, rather
	// than reconstructing it afterwards.
	Bundle federation.Bundle `json:"bundle"`
	// Member is the cluster's state after the publish. Its HighestIssued
	// has already moved, so a response that reported the pre-publish
	// numbers would be wrong in the direction that matters.
	Member Member `json:"-"`
	// Delivery is what happened when the decision went out. It is not an
	// error and it never will be: the version is issued either way, and
	// this is the separate fact of whether a cluster has heard about it.
	Delivery Delivery `json:"delivery"`
}

// Publisher issues policy bundles.
//
// It sits above Registry because the two have different jobs: Registry knows
// who the clusters are and which version numbers are spent, and Publisher is
// the only thing in the system that holds the release key.
type Publisher struct {
	reg    *Registry
	signer *pluginmanifest.Signer
	now    func() time.Time
}

// NewPublisher builds a Publisher over a registry and a signing key.
//
// signer is required rather than optional. A publisher that could be built
// without one is a publisher whose operator will discover the gap when a
// rollout silently has nowhere to go.
func NewPublisher(reg *Registry, signer *pluginmanifest.Signer) (*Publisher, error) {
	if reg == nil {
		return nil, errors.New("federation: a publisher needs a registry")
	}
	if signer == nil {
		return nil, errors.New("federation: a publisher needs the release signing key")
	}
	return &Publisher{reg: reg, signer: signer, now: time.Now}, nil
}

// Registry exposes the membership the publisher issues into, so a caller that
// has a Publisher does not also have to be handed the Registry separately.
func (p *Publisher) Registry() *Registry { return p.reg }

// Publish signs the tree and issues the next version to one cluster.
//
// Signing happens first, and a tree that cannot be signed never consumes a
// version number. That ordering is the difference between "the operator
// pointed at the wrong directory" and "this cluster is now permanently one
// version ahead of any policy", and the second is not a mistake anyone can
// undo from the console.
func (p *Publisher) Publish(_ context.Context, id federation.ClusterID, req PublishRequest) (PublishResult, error) {
	if _, ok := p.reg.Member(id); !ok {
		return PublishResult{}, ErrNotEnrolled
	}
	env, err := p.signer.Sign(req.StagedRoot)
	if err != nil {
		// No version was allocated and none was spent.
		return PublishResult{}, fmt.Errorf("%w: %v", ErrNothingToPublish, err)
	}
	bundle, err := p.reg.Publish(id, env, req.Reason)
	if err != nil {
		return PublishResult{}, err
	}
	member, _ := p.reg.Member(id)
	return PublishResult{Bundle: bundle, Member: member}, nil
}
