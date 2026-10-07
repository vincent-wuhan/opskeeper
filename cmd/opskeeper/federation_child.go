package main

// The child cluster's side of the cluster channel, composed in one file.
//
// It is the mirror of federation_wiring.go and it exists for the same reason
// that file does: what it assembles has parts that must agree, and getting
// one of them wrong does not crash. It produces a cluster that enrolled
// itself, answered state questions, and installed nothing.
//
// The three things it is worth reading before changing anything here:
//
//  1. Boot never waits for the root. A child whose root is unreachable still
//     starts, still serves its own nodes, and still enforces the last policy
//     it accepted. A child that refused to boot without its root would make
//     the root a single point of failure for the cluster's availability, which
//     is the one property "子集群可独立运行" is about. So Dial runs in the
//     background and its failure is a log line, not an exit.
//
//  2. Hello is re-sent on every reconnect, not only after the first Dial.
//     The root's binding table is in memory (see federation_wiring.go), so a
//     root that restarts forgets which caller speaks for which cluster. A
//     child that said hello once at boot would be unreachable forever after
//     the first root restart, with no error anywhere: the tunnel is up, the
//     cluster is running, and the root has never heard of it. Re-helloing is
//     what makes the child self-healing across a root restart, and it is why
//     the callback carries a timeout.
//
//  3. The policy ceiling and the trust store are read from the same
//     variables the edge agent reads. One cluster has one policy; a manager
//     and its nodes that each read their own copy would be two policies, and
//     the disagreement would show up as a package the manager offers and the
//     node refuses. The manager asks first so the operator learns at the
//     release rather than at the node.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	edgefed "github.com/vincent-wuhan/opskeeper/core/domains/service/federationchild"
	managersvcplugin "github.com/vincent-wuhan/opskeeper/core/domains/service/plugin"
	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

const (
	// federationRoleEnv names what this control plane is in a federation.
	// It is explicit rather than inferred, because every way of inferring
	// it is wrong somewhere: a root that federates membership but signs
	// nothing looks exactly like a child that has not been told its key
	// yet, and a deployment with a release key on disk is not a child.
	federationRoleEnv = "OPSKEEPER_FEDERATION_ROLE"
	// roleChild is the only value that turns the child side on. Anything
	// else, including an empty value, leaves the process a root or a
	// single cluster with no opinion about either.
	//
	t // Matched exactly, not case-insensitively. A forgiving match here
	// would be a hazard rather than a convenience: an operator who writes
	// CHILD in one unit file and child in another gets two different
	// behaviours on two machines, and the one that stayed a root would
	// report no error at all. The same strictness as the policy ceilings,
	// for the same reason.
	roleChild = "child"
	// federationClusterIDEnv is this cluster's own identity as its root
	// knows it. It is claimed, not proven — the root checks it against the
	// provisioning token, and a child that claims an identity it was not
	// issued is refused.
	federationClusterIDEnv = "OPSKEEPER_FEDERATION_CLUSTER_ID"
	// federationTokenEnv is the provisioning token the root issued. It is
	// the only thing standing between a process on the network and the
	// right to publish policy at a cluster, which is why it is a token and
	// not a shared secret the two ends derive independently.
	federationTokenEnv = "OPSKEEPER_FEDERATION_TOKEN"
	// federationPolicyDirEnv is where this cluster keeps the trees its root
	// sends. It is a directory the child owns: the store writes versions
	// into it and swaps one symlink inside it, and nothing else in the
	// deployment should be writing there.
	federationPolicyDirEnv = "OPSKEEPER_FEDERATION_POLICY_DIR"
	// federationTrustStoreEnv is deliberately the edge's variable. See
	// note 3 above: one cluster, one trust store, one file.
	federationTrustStoreEnv = "OPSKEEPER_EDGE_TRUST_STORE"
	// federationHelloTimeout bounds the hello, both after the first Dial
	// and on every reconnect.
	//
	// It is load-bearing rather than tidy. Reconnect callbacks run
	// sequentially from the tunnel's reconnect goroutine, so a hello that
	// never returns does not only fail to re-enrol this cluster — it stops
	// every callback registered after it from ever running again.
	federationHelloTimeout = 10 * time.Second
)

// federationChildWiring is the assembled child side.
type federationChildWiring struct {
	agent  *edgefed.Agent
	client tunnel.Client
	gate   *floorfed.LiveGate
	store  *edgefed.Store
	log    *slog.Logger
}

// newFederationChildWiring assembles the child side, or returns nil when this
// process is not a child.
//
// A nil, nil return is the ordinary answer for every single-cluster
// deployment and for a root, and it is why the caller does not need to know
// in advance which one it is. The same shape is used on the root side, where
// federationDistributor answers nil for a root that federates membership but
// not policy: capability follows configuration, not a default.
func newFederationChildWiring(cfg *config.Config, fleet *managersvcplugin.NodeFleet, log *slog.Logger) (*federationChildWiring, error) {
	if strings.TrimSpace(os.Getenv(federationRoleEnv)) != roleChild {
		return nil, nil
	}
	if cfg == nil {
		return nil, fmt.Errorf("federation: %s=child but there is no configuration to dial the root with", federationRoleEnv)
	}
	if log == nil {
		// Same reason NewAgent does it: a wiring whose only failure mode
		// is a nil logger is a wiring that crashes on the happy path of
		// every test that does not care about logging.
		log = slog.Default()
	}

	id, err := floorfed.NewClusterID(os.Getenv(federationClusterIDEnv))
	if err != nil {
		return nil, fmt.Errorf("federation: %s: %w", federationClusterIDEnv, err)
	}
	token := strings.TrimSpace(os.Getenv(federationTokenEnv))
	if token == "" {
		return nil, fmt.Errorf("federation: %s is required to enrol with a root", federationTokenEnv)
	}
	policyDir := strings.TrimSpace(os.Getenv(federationPolicyDirEnv))
	if policyDir == "" {
		return nil, fmt.Errorf("federation: %s is required; without a directory there is nowhere to put a policy", federationPolicyDirEnv)
	}

	// The trust store is the cluster's, and the policy ceiling is the
	// cluster's. Both are refused when absent rather than defaulted: an
	// empty trust store is safe but useless, and a zero Policy refuses
	// every package, so a child that came up with a default would look
	// exactly like one whose root had withdrawn everything.
	trustPath := strings.TrimSpace(os.Getenv(federationTrustStoreEnv))
	if trustPath == "" {
		return nil, fmt.Errorf("federation: %s is required; a child with no trusted key can enforce no policy", federationTrustStoreEnv)
	}
	trust, err := pluginmanifest.TrustStoreFromConfig(trustPath)
	if err != nil {
		return nil, fmt.Errorf("federation: %w", err)
	}
	policy, err := federationChildPolicy()
	if err != nil {
		return nil, err
	}

	// A process that both enrols with a root and holds a release key is
	// two contradictory identities in one binary. It is refused rather
	// than resolved: a child that can sign policy is a child whose root
	// cannot tell an instruction from an assertion, and no console would
	// show that.
	if signer, serr := federationReleaseSigner(); serr != nil {
		return nil, fmt.Errorf("federation: %s: %w", federationRoleEnv, serr)
	} else if signer != nil {
		return nil, fmt.Errorf(
			"federation: %s=child but %s is set; a child receives policy and must not be able to sign it",
			federationRoleEnv, federationReleaseKeyEnv)
	}

	store, err := edgefed.NewStore(policyDir)
	if err != nil {
		return nil, fmt.Errorf("federation: %w", err)
	}
	recv, err := floorfed.NewReceiver(id, trust, policy, store)
	if err != nil {
		return nil, fmt.Errorf("federation: %w", err)
	}

	client := tunnel.NewClient(tunnel.ClientConfig{
		ServerAddr: cfg.Edge.CloudAddr,
		AccessKey:  cfg.Edge.AccessKey,
		SecretKey:  cfg.Edge.SecretKey,
		Log:        log,
	})
	agent, err := edgefed.NewAgent(client, recv, store,
		floorfed.Cluster{ID: id, EdgeCount: 0},
		token, log)
	if err != nil {
		return nil, fmt.Errorf("federation: %w", err)
	}

	// The gate is over the store's own live link, so it follows every swap
	// the store makes without being told about them. See LiveGate: the
	// link is the state.
	gate, err := floorfed.NewLiveGate(filepath.Join(store.Base(), edgefed.LiveLinkName))
	if err != nil {
		return nil, fmt.Errorf("federation: %w", err)
	}
	fleet.SetPolicyGate(gate)

	log.Info("federation: this cluster is a child",
		slog.String("cluster", id.String()),
		slog.String("root", cfg.Edge.CloudAddr),
		slog.String("policy_dir", policyDir),
	)
	return &federationChildWiring{agent: agent, client: client, gate: gate, store: store, log: log}, nil
}

// Start brings the channel up without waiting for it.
//
// Handlers go on before Dial so they are primed for the first connection as
// well as every later one, and the hello goes in two places: once after the
// first Dial, and once per reconnect. See note 2 in the file header for why
// the second one is not redundant.
func (w *federationChildWiring) Start(ctx context.Context) {
	if w == nil {
		return
	}
	w.agent.Register()
	w.client.OnReconnect(func() {
		rctx, cancel := context.WithTimeout(context.Background(), federationHelloTimeout)
		defer cancel()
		if err := w.agent.Hello(rctx); err != nil {
			w.log.Warn("federation: re-enrolment after tunnel reconnect failed; the root does not know this cluster right now",
				slog.Any("err", err))
		}
	})

	go func() {
		// Dial retries on a jittered backoff until the first connect or
		// ctx cancel, so this goroutine is where "the root is down" is
		// spent. Nothing above it waits.
		if err := w.client.Dial(ctx); err != nil {
			if ctx.Err() == nil {
				// Not necessarily the root's fault. Close() also lands here,
				// and a deliberate shutdown is not an incident, so the
				// message says what is true — the channel did not come up —
				// rather than guessing why.
				w.log.Warn("federation: the channel to the root did not come up", slog.Any("err", err))
			}
			return
		}
		hctx, cancel := context.WithTimeout(ctx, federationHelloTimeout)
		defer cancel()
		if err := w.agent.Hello(hctx); err != nil {
			w.log.Error("federation: the root refused this cluster", slog.Any("err", err))
			return
		}
		w.reportEnforcement()
	}()
}

// reportEnforcement says what this cluster is enforcing, on the first
// successful enrolment.
//
// A child that is enforcing nothing is a normal state on the way up and a
// alarming one after the fact, and the only difference is whether an operator
// was told. It is logged here and then never again: the root can ask at any
// time through cluster.state, and a line in the journal per push would train
// people to stop reading the journal.
func (w *federationChildWiring) reportEnforcement() {
	state := w.agent.HandleState(context.Background(), tunnel.ClusterStateRequest{})
	live := state.Policy
	if live < floorfed.MinBundleVersion {
		w.log.Warn("federation: enrolled, and enforcing nothing yet — the root has not sent a policy",
			slog.Uint64("live", live),
		)
		return
	}
	w.log.Info("federation: enrolled and enforcing",
		slog.Uint64("live", live),
		slog.String("package", state.Bundle.PackageName+"@"+state.Bundle.PackageVersion),
		slog.String("live_policy", w.gate.Live()),
	)
}

// Close stops the channel.
func (w *federationChildWiring) Close() error {
	if w == nil {
		return nil
	}
	return w.client.Close()
}

// federationChildPolicy reads the cluster's own ceilings.
//
// The three variables are the edge agent's, on purpose — see note 3 in the
// file header. Unlike the edge, every one of them is required: the edge
// falls back to a profile, because a node is expected to come up on its own
// and refuse packages until configured, whereas a manager configured as a
// child and unable to state a ceiling has nothing to enforce with.
func federationChildPolicy() (pluginmanifest.Policy, error) {
	level := strings.TrimSpace(os.Getenv("OPSKEEPER_EDGE_MAX_SAFETY_LEVEL"))
	radius := strings.TrimSpace(os.Getenv("OPSKEEPER_EDGE_MAX_BLAST_RADIUS"))
	if level == "" || radius == "" {
		return pluginmanifest.Policy{}, fmt.Errorf(
			"federation: OPSKEEPER_EDGE_MAX_SAFETY_LEVEL and OPSKEEPER_EDGE_MAX_BLAST_RADIUS are both required on a child; " +
				"an unset ceiling refuses every package, and a child that cannot state one has no policy to enforce")
	}
	ceiling := domain.SafetyLevel(level)
	if !ceiling.Valid() {
		return pluginmanifest.Policy{}, fmt.Errorf(
			"federation: OPSKEEPER_EDGE_MAX_SAFETY_LEVEL=%q is not one of L0, L1, L2, L3", level)
	}
	width := domain.BlastRadius(radius)
	if !width.Valid() {
		return pluginmanifest.Policy{}, fmt.Errorf(
			"federation: OPSKEEPER_EDGE_MAX_BLAST_RADIUS=%q is not one of pod, single-ns, namespace, cluster", radius)
	}
	var scopes domain.Scopes
	for _, raw := range strings.Split(os.Getenv("OPSKEEPER_EDGE_PLUGIN_SCOPES"), ",") {
		if s := strings.TrimSpace(raw); s != "" {
			scopes = append(scopes, domain.Scope(s))
		}
	}
	return pluginmanifest.PolicyFor(ceiling, width, scopes), nil
}
