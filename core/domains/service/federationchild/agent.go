// Package federationchild is the child cluster's side of policy federation:
// the part that answers a root.
//
// It is the counterpart to service/federationlink, and the two are siblings
// on purpose: one is the root's outbound half and this is the child's
// inbound half, and neither is a part of anything else on this plane.
//
// It used to live at core/edge/federation, which was wrong in a way only a
// boundary check could find. Nothing in it is edge-agent code — it dials a
// root as a tunnel client, answers two RPCs, and keeps a directory of policy
// trees — and it was imported by the manager's own composition root, which
// means the manager was reaching into the edge module to assemble the
// manager's own control plane. The package's whole dependency closure is
// floor/federation and floor/tunnel, neither of which is edge anything.
//
// It stays out of biz for the reason it always did: it needs the rules, the
// wire and a filesystem store at once, and folding it into the registry would
// mean the thing that answers a root also decides who is allowed to ask.
package federationchild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Agent is one child cluster's connection to a root.
//
// Everything it answers can be answered with no root present, because every
// answer comes from state the child already holds: the version in force, the
// bundle that put it there, and the record of what it has decided. The only
// thing it needs the root for is being told about a newer one.
type Agent struct {
	client  tunnel.Client
	recv    *federation.Receiver
	store   *Store
	token   string
	now     func() time.Time
	log     *slog.Logger
	cluster federation.Cluster

	mu              sync.RWMutex
	accepted        bool
	refusal         string
	heartbeatSecs   int
	lastRootContact time.Time
}

// NewAgent builds a child-side federation agent.
func NewAgent(client tunnel.Client, recv *federation.Receiver, store *Store, cluster federation.Cluster, token string, log *slog.Logger) (*Agent, error) {
	if client == nil {
		return nil, errors.New("federation: an agent needs a tunnel client")
	}
	if recv == nil {
		return nil, errors.New("federation: an agent needs a receiver")
	}
	if store == nil {
		return nil, errors.New("federation: an agent needs a policy store")
	}
	if !cluster.ID.Valid() {
		return nil, fmt.Errorf("federation: %q is not a cluster identity", cluster.ID)
	}
	if token == "" {
		return nil, errors.New("federation: an agent needs its provisioning token")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Agent{
		client:  client,
		recv:    recv,
		store:   store,
		token:   token,
		now:     time.Now,
		log:     log,
		cluster: cluster,
	}, nil
}

// Register installs the handlers for the two calls a root makes.
//
// It does not send the hello. A hello is a statement about who this process
// is, and that statement is only true once the process is running, so the
// caller sends it after Dial — the same order the edge uses for
// register_edge.
func (a *Agent) Register() {
	a.client.RegisterHandler(tunnel.MethodClusterPolicy, func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
		var req tunnel.ClusterPolicyRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("federation: cluster.policy: %w", err)
		}
		return json.Marshal(a.HandlePolicy(ctx, req))
	})
	a.client.RegisterHandler(tunnel.MethodClusterState, func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
		var req tunnel.ClusterStateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("federation: cluster.state: %w", err)
		}
		return json.Marshal(a.HandleState(ctx, req))
	})
}

// Hello introduces this cluster to a root and records whether it was taken.
//
// A refusal is returned as an error, deliberately: the agent's job is to keep
// enforcing the last policy it accepted, and a root that will not talk to it
// is a condition an operator has to see, not a blip to retry into silence.
func (a *Agent) Hello(ctx context.Context) error {
	req := tunnel.ClusterHelloRequest{Cluster: a.selfDescription(), ProvisioningToken: a.token}
	var resp tunnel.ClusterHelloResponse
	if err := a.client.Call(ctx, tunnel.MethodClusterHello, req, &resp); err != nil {
		return fmt.Errorf("federation: cluster %q hello: %w", a.cluster.ID, err)
	}

	now := a.now()
	a.mu.Lock()
	a.accepted = resp.Accepted
	a.refusal = resp.Reason
	a.heartbeatSecs = resp.HeartbeatSeconds
	a.lastRootContact = now
	a.mu.Unlock()

	if !resp.Accepted {
		reason := resp.Reason
		if reason == "" {
			reason = "the root gave no reason"
		}
		return fmt.Errorf("federation: root refused cluster %q: %s", a.cluster.ID, reason)
	}
	a.log.Info("federation: registered with root",
		slog.String("cluster", a.cluster.ID.String()),
		slog.Uint64("root_policy_version", resp.PolicyVersion),
	)
	return nil
}

// Accepted reports whether a root has taken this cluster, and why not if it
// has not.
func (a *Agent) Accepted() (bool, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.accepted, a.refusal
}

// LastRootContact is when a root last answered. A cluster that has been
// enforcing version 9 on its own for three days is not a cluster in trouble —
// it is one doing exactly what the plan asked for, and the console needs to
// be able to say so.
func (a *Agent) LastRootContact() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lastRootContact
}

// HandlePolicy answers a push.
//
// The one thing worth reading twice is Retryable. The root has to tell
// "this cluster declined" from "this cluster could not have decided", because
// the first is final for that version and the second is an invitation to send
// it again. Flattening them into a transport error makes a rollout either
// abandon a policy that was merely waiting for its files, or hammer one the
// cluster will never accept.
func (a *Agent) HandlePolicy(ctx context.Context, req tunnel.ClusterPolicyRequest) tunnel.ClusterPolicyResponse {
	staged, err := a.stage(ctx, req)
	if err != nil {
		// The tree could not be put where Apply can look for it. No
		// contact with the policy itself has happened, so nothing is
		// recorded against the version and the root may push it again.
		//
		// This is the same answer a push naming a path that is not there
		// gets, and deliberately: both mean "not yet", and the root
		// treats them identically. What differs is only which side of the
		// channel had to do something first.
		a.touch()
		a.log.Warn("federation: policy tree not staged",
			slog.String("cluster", a.cluster.ID.String()),
			slog.Uint64("version", req.Bundle.Version),
			slog.String("err", err.Error()),
		)
		return tunnel.ClusterPolicyResponse{
			Outcome:   federation.Outcome{Version: req.Bundle.Version, Live: a.liveVersion(), At: a.now()},
			Retryable: true,
			Error:     err.Error(),
		}
	}
	out, err := a.recv.Apply(req.Bundle, staged)
	if err != nil {
		// Any contact counts, even a refusal: the root is reachable, which
		// is what LastRootContact is for.
		a.touch()

		retryable := errors.Is(err, federation.ErrNotStaged) || errors.Is(err, federation.ErrPromotionFailed)
		a.log.Warn("federation: policy push not applied",
			slog.String("cluster", a.cluster.ID.String()),
			slog.Uint64("version", req.Bundle.Version),
			slog.Uint64("live", out.Live),
			slog.Bool("retryable", retryable),
			slog.String("err", err.Error()),
		)
		return tunnel.ClusterPolicyResponse{
			Outcome:   out,
			Retryable: retryable,
			Error:     err.Error(),
		}
	}
	a.touch()
	a.log.Info("federation: policy applied",
		slog.String("cluster", a.cluster.ID.String()),
		slog.Uint64("version", req.Bundle.Version),
		slog.String("package", req.Bundle.PackageName+"@"+req.Bundle.PackageVersion),
	)
	return tunnel.ClusterPolicyResponse{Outcome: out}
}

// stage is where a push is turned into a path Apply can look at.
//
// The two halves of the wire are mutually exclusive, and this is where that
// is enforced rather than documented: a push naming both is refused instead
// of quietly resolving to whichever field was read first. A request whose
// meaning depends on the reader is not a request, and tolerating it would
// mean a root with a bug could point a child at one tree and sign it as
// another.
func (a *Agent) stage(ctx context.Context, req tunnel.ClusterPolicyRequest) (string, error) {
	hasPath := strings.TrimSpace(req.StagedPath) != ""
	hasSource := req.Source != nil && strings.TrimSpace(req.Source.URL) != ""
	switch {
	case hasPath && hasSource:
		return "", fmt.Errorf("federation: the push names both a staged path and a source, and a tree has one origin")
	case hasSource:
		return a.store.Receive(ctx, req.Source, req.Bundle.Version)
	default:
		// Neither named. The empty path is passed through rather than
		// refused here so that the receiver produces the answer, and it
		// produces the right one: "not staged yet", which is retryable and
		// records nothing. A root that has not finished staging the tree
		// is a root whose push arrived first, and that is a normal event
		// on a channel where the tree travels out of band.
		return req.StagedPath, nil
	}
}

// HandleState answers "what are you enforcing", with no root required.
//
// The request is taken and ignored on purpose. It carries the cluster it is
// about, but a state request is a question rather than an instruction, and
// there is exactly one answer this process can give: its own. Matching the
// field would add a code path whose only effect is to return less information
// to a root that is already asking the right child.
func (a *Agent) HandleState(_ context.Context, _ tunnel.ClusterStateRequest) tunnel.ClusterStateResponse {
	live, applied := a.recv.Live()
	return tunnel.ClusterStateResponse{
		Policy:          live,
		Bundle:          applied,
		LastRootContact: a.LastRootContact(),
		// Enforcing is false for a cluster that has never been told
		// anything. "Holding version 0" is not "enforcing version 0" —
		// the difference is whether a policy exists to enforce.
		Enforcing: live >= federation.MinBundleVersion,
		NodeCount: a.cluster.EdgeCount,
	}
}

// selfDescription is what this child says about itself. It is a claim; the
// root checks the token before believing any of it.
func (a *Agent) selfDescription() federation.Cluster {
	a.mu.RLock()
	defer a.mu.RUnlock()
	described := a.cluster
	described.LastSeen = a.now()
	return described
}

func (a *Agent) liveVersion() uint64 {
	live, _ := a.recv.Live()
	return live
}

func (a *Agent) touch() {
	now := a.now()
	a.mu.Lock()
	a.lastRootContact = now
	a.mu.Unlock()
}
