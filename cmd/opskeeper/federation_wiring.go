package main

// The root's cluster federation, composed in one file.
//
// It is one file rather than a block in main() because what it assembles has
// four parts that must agree with each other — the registry, the publisher
// that may not exist, the tunnel-side binding table, and the HTTP routes —
// and the failure mode of getting one of them wrong is not a crash. It is a
// control plane that enrols child clusters and then quietly cannot reach any
// of them.

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strings"

	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"

	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
	managerserverfed "github.com/vincent-wuhan/opskeeper/core/domains/server/federation"
	managersvcfedlink "github.com/vincent-wuhan/opskeeper/core/domains/service/federationlink"
	managersvcfb "github.com/vincent-wuhan/opskeeper/core/manager/service/frontierbound"
)

const (
	// federationReleaseKeyEnv carries the root's ed25519 seed, base64, and
	// is what lets this root sign a policy bundle at all.
	//
	// It is an env var rather than a settings row on purpose, and the
	// reason is the plan's own constraint about not creating a second
	// source of truth for credentials: a signing *identity* is not a
	// setting an operator edits in a console, and putting it in the same
	// table as display preferences would make "who am I" a runtime
	// preference. It stays unset on every root that does not federate,
	// which is the shape a single-cluster deployment has.
	federationReleaseKeyEnv = "OPSKEEPER_FEDERATION_RELEASE_KEY"
	// federationKeyIDEnv names the key. It is a label, not authority: a
	// child that does not hold this key refuses the bundle whatever the
	// root calls it.
	federationKeyIDEnv = "OPSKEEPER_FEDERATION_KEY_ID"
	// defaultFederationKeyID matches the id the shipped test keys use, so
	// an operator who sets only the seed does not also have to remember
	// to set a name.
	defaultFederationKeyID = "release-2026"
	// federationArtifactDirEnv is where this root writes a policy tree so
	// a child can fetch it.
	//
	// A file:// source is the zero-configuration path, and it is not a
	// placeholder: a root and its children sharing a mount is the shape
	// most on-prem multi-cluster deployments already have, and it needs no
	// web server. A deployment that fronts this directory over https
	// instead would have a Distributor of its own; this is the one that
	// works with nothing configured but a path.
	federationArtifactDirEnv = "OPSKEEPER_FEDERATION_ARTIFACT_DIR"
	// federationArtifactPrefixEnv is the path the *child* sees this
	// directory at, when it is not the same one.
	//
	// It is separate because those two are genuinely different in every
	// deployment where they differ — a container mount, a chroot, an NFS
	// path the operator chose to spell differently — and one variable
	// cannot be both the write path and the read path.
	federationArtifactPrefixEnv = "OPSKEEPER_FEDERATION_ARTIFACT_PREFIX"

	// federationArtifactBaseURLEnv names a store this root publishes into
	// rather than a directory a child shares with it. It is the switch
	// between the two delivery shapes, and it is a separate variable rather
	// than a value of the prefix one because the two are not the same kind
	// of thing: the prefix is a path this root writes to, and the base is
	// a place a child fetches from.
	//
	// Set it and the root stops handing out file:// URLs and starts
	// addressing an artifact store — but only after it has compared the
	// bytes it signed against what the store says it is holding, so a
	// rollout can never point a child at bytes this root has not checked.
	federationArtifactBaseURLEnv = "OPSKEEPER_FEDERATION_ARTIFACT_BASE_URL"
	// federationArtifactManifestEnv is the JSON file the deployment's
	// publish step writes after uploading an archive, mapping archive name
	// to sha256. It is a file rather than a cloud client so that whatever
	// already puts the archive in the store also writes one line, and so
	// that adding federation does not add a vendor SDK and a second
	// credential path to the root.
	//
	// It is read on every delivery, not cached, because the publish step
	// runs out of band: a manifest read at boot is a snapshot that is
	// wrong for the rest of the process's life.
	federationArtifactManifestEnv = "OPSKEEPER_FEDERATION_ARTIFACT_MANIFEST"

	// The two that make this root able to fill the store it addresses.
	//
	// 它们是「有没有发布端」的开关，而此前这个包里根本没有发布端：一个
	// 命名了 BASE_URL 的根只能把 URL 指给子集群，然后永远停在
	// ErrNotPublished，直到有人用命令行把文件传上去。写下这两个变量等于
	// 说「这个根自己负责把树放进 store」，而没写就是既有的只读形状。
	federationArtifactStoreURLEnv   = "OPSKEEPER_FEDERATION_ARTIFACT_STORE_URL"
	federationArtifactStoreTokenEnv = "OPSKEEPER_FEDERATION_ARTIFACT_STORE_TOKEN"
)

// federationWiring is the assembled root side of the cluster channel.
type federationWiring struct {
	// handler serves the console routes. It is never nil: a root with no
	// release key still enrols children and answers what they enforce,
	// so mounting the routes is always right.
	handler *managerserverfed.Handler
	// link is the tunnel-side binding table. It is also the
	// ClusterHelloHandler the tunnel dispatches to, and the thing
	// subscribed to caller disconnections.
	link *managersvcfedlink.Links
	// service is the registry plus, when a release key was configured,
	// the publisher.
	service *fedbiz.Service
	// canPublish is the one line an operator's log needs on boot, because
	// "federation is mounted but cannot sign" is otherwise invisible.
	canPublish bool
	// canDeliver is the same for the other half. A root that can sign and
	// cannot deliver produces a version every time and tells no cluster
	// anything, and that reads as a working console until an operator
	// checks a cluster.
	canDeliver bool
	// artifactDir is where trees are written, empty when delivery is off.
	artifactDir string
}

// newFederationWiring assembles the root side.
//
// fbClient may be a disabled tunnel: the binding table is still built, the
// routes are still mounted, and every push fails with the tunnel's own
// disabled error rather than with a 404. A root whose tunnel is down is a
// root that has lost its way to its children, not a root that never had one.
//
// The registry is in memory and now has a Ledger underneath it, which is what
// makes a restart survivable. Without one the root came back believing it had
// never enrolled a cluster and refused every child's own still-valid
// provisioning token, and recovery meant re-enrolling them one at a time and
// handing each operator a new token. (Decision 145 measured that; the wire
// cannot say which of the two causes a refusal had, by design, so the log
// line at the hello boundary is where that shows up.)
//
// The file is the shipping Ledger because the supported topology is a single
// root process whose whole membership is a handful of rows. The port names
// the more interesting stores — Postgres, the audit chain — and neither is
// wired; switching is a change to the two lines below and nothing else,
// which is the point of the port existing.
//
// Restore is called before anything is served, and a failure to read the
// ledger is fatal. A root that started anyway with an empty membership would
// be doing exactly the thing the ledger prevents, silently, at the moment an
// operator is relying on it.
func newFederationWiring(fbClient *managersvcfb.Client, log *slog.Logger) (*federationWiring, error) {
	if fbClient == nil {
		return nil, fmt.Errorf("federation: no tunnel client to push over")
	}

	signer, err := federationReleaseSigner()
	if err != nil {
		return nil, err
	}

	// Declared as the interface and assigned conditionally on purpose. A
	// nil *FileLedger handed to NewRegistry would not be a nil Ledger —
	// an interface holding a nil pointer is not nil — so every
	// `r.ledger != nil` in the registry would be true and the first
	// enrolment would dereference nothing.
	var ledger fedbiz.Ledger
	if usable := federationLedger(log); usable != nil {
		ledger = usable
	}
	reg := fedbiz.NewRegistry(ledger)
	if ledger != nil {
		// Before the publisher, the link, and the routes. A ledger
		// that exists but cannot be read proves there was a membership
		// to lose, and starting with an empty one would re-enrol every
		// cluster and rotate every token. That is the one case where
		// refusing to start is the answer that cannot be wrong.
		fileLedger, _ := ledger.(*fedbiz.FileLedger)
		if err := reg.Restore(); err != nil {
			return nil, fmt.Errorf("federation: restore: %w (ledger: %s)", err, fileLedger.Path())
		}
		log.Info("federation: ledger loaded",
			slog.String("path", fileLedger.Path()),
			slog.Int("clusters", len(reg.Members())),
		)
	}
	var pub *fedbiz.Publisher
	if signer != nil {
		if pub, err = fedbiz.NewPublisher(reg, signer); err != nil {
			return nil, err
		}
	}
	svc, err := fedbiz.NewService(reg, pub)
	if err != nil {
		return nil, err
	}

	link, err := managersvcfedlink.NewLink(fbClient, clusterRegistrar{reg: reg}, managersvcfedlink.WithLogger(log))
	if err != nil {
		return nil, err
	}

	// Delivery is wired after the link because it is the link that carries
	// the push, and a distributor built first would be a tree with nowhere
	// to go.
	dist, err := federationDistributor()
	if err != nil {
		return nil, err
	}
	// The nil check is here and not only inside SetDelivery, and the reason
	// is worth stating because it is a Go trap rather than a design point:
	// federationDistributor returns a *FileDistributor, and handing a nil
	// one of those to an interface parameter produces a non-nil interface
	// holding a nil pointer. Every `svc.dist == nil` downstream would be
	// false and the first publish would dereference it.
	if dist == nil {
		log.Warn("federation: no artifact directory configured — versions can be issued and read, but no child can be told about one",
			slog.String("env", federationArtifactDirEnv),
		)
		svc.SetDelivery(link, nil, nil)
	} else {
		svc.SetDelivery(link, dist, dist)
	}
	// A caller that goes away must stop being reachable, because the
	// broker will hand the same number to somebody else. This is the one
	// subscription in the whole channel and it has no fallback: forget
	// it and a push eventually lands on a process that never said hello.
	fbClient.OnEdgeOffline(func(edgeID uint64) {
		if n := link.Forget(edgeID); n > 0 {
			log.Info("federation: released cluster bindings",
				slog.Uint64("caller", edgeID),
				slog.Int("clusters", n),
			)
		}
	})

	handler := managerserverfed.NewHandler(svc)
	handler.SetPusher(link)

	w := &federationWiring{
		handler:     handler,
		link:        link,
		service:     svc,
		canPublish:  svc.CanPublish(),
		canDeliver:  dist != nil,
		artifactDir: "",
	}
	if dist != nil {
		w.artifactDir = dist.Dir()
	}
	if !w.canPublish {
		log.Warn("federation: mounted without a release key — clusters can be enrolled and read, but no policy can be issued",
			slog.String("env", federationReleaseKeyEnv),
		)
	} else {
		log.Info("federation: release key loaded",
			slog.String("key_id", signer.KeyID()),
		)
	}
	if w.canDeliver {
		// The mode is in the log because the two shapes fail in opposite
		// directions and an operator reading a delivery failure needs to
		// know which one they are in. A file:// root fails when the mount
		// is not shared; a store root fails when the publish step has not
		// written the manifest yet, which looks like nothing at all from
		// the outside.
		mode := "shared-mount file://"
		if strings.TrimSpace(os.Getenv(federationArtifactBaseURLEnv)) != "" {
			mode = "artifact store over " + os.Getenv(federationArtifactBaseURLEnv)
		}
		log.Info("federation: policy delivery configured",
			slog.String("mode", mode),
			slog.String("artifact_dir", w.artifactDir),
		)
	}
	return w, nil
}

// federationDistributor builds the root's artifact distributor, or nil when
// no directory is configured.
//
// Nil is the answer for a root that federates membership but not policy, and
// it is a legitimate state rather than a misconfiguration: a single-cluster
// deployment, or one that wants the console to show what its children are
// enforcing without any way to change it. A publish on such a root still
// issues a version and reports that nobody was told, which is the honest
// pair of facts.
//
// A directory that is configured but unusable is an error, for the same
// reason a release key that will not load is: a deployment that was handed
// a path and cannot write to it has a real misconfiguration, and running on
// without saying so would leave an operator believing policy is being
// delivered when none is.
func federationDistributor() (policyDelivery, error) {
	dir := strings.TrimSpace(os.Getenv(federationArtifactDirEnv))
	if dir == "" {
		return nil, nil
	}
	local, err := fedbiz.NewFileDistributor(dir, os.Getenv(federationArtifactPrefixEnv))
	if err != nil {
		return nil, err
	}

	// No base URL means the zero-configuration shape: a root and its
	// children share a mount, and file:// is the whole of it.
	base := strings.TrimSpace(os.Getenv(federationArtifactBaseURLEnv))
	if base == "" {
		return local, nil
	}

	// A base with no manifest would hand out URLs whose bytes nobody has
	// compared against anything, and that is the one thing this path
	// exists to prevent. So the half that makes it safe is required rather
	// than defaulted: a deployment that named a store has to say how this
	// root learns what is in it.
	manifestPath := strings.TrimSpace(os.Getenv(federationArtifactManifestEnv))
	if manifestPath == "" {
		return nil, fmt.Errorf("federation: %s names a store, so %s must say how this root "+
			"learns what that store is holding; without it every delivery would name a URL "+
			"whose bytes were never compared against the tree this root signed",
			federationArtifactBaseURLEnv, federationArtifactManifestEnv)
	}
	ledger, err := fedbiz.NewManifestLedger(manifestPath)
	if err != nil {
		return nil, err
	}
	published, err := fedbiz.NewPublishedDistributor(local, base, ledger)
	if err != nil {
		return nil, err
	}

	// No store credentials means the read-only shape: this root knows what
	// the store holds and refuses to name a URL for anything else, which is
	// exactly right for a deployment where something else publishes.
	storeURL := strings.TrimSpace(os.Getenv(federationArtifactStoreURLEnv))
	if storeURL == "" {
		return published, nil
	}
	sink, err := fedbiz.NewHTTPSink(storeURL, os.Getenv(federationArtifactStoreTokenEnv), nil)
	if err != nil {
		return nil, fmt.Errorf("federation: %s names a store this root publishes to: %w",
			federationArtifactStoreURLEnv, err)
	}
	publisher, err := fedbiz.NewPublishedPublisher(published, sink)
	if err != nil {
		return nil, err
	}
	selfPublishing, err := fedbiz.NewPublishingDistributor(published, publisher)
	if err != nil {
		return nil, err
	}
	return selfPublishing, nil
}

// policyDelivery is the root's answer to "where does a child's tree come
// from", as the two ports the service needs together with the one question
// the operator's log line asks.
//
// It is an interface because the two shapes are different implementations
// rather than one implementation with a flag, and naming it here is what
// keeps the wrapping honest: a distributor that implemented only one of the
// two would fail to satisfy this, at the wiring, rather than at the first
// redelivery.
type policyDelivery interface {
	fedbiz.Distributor
	fedbiz.Redeliverer
	// Dir reports where the exact bytes are kept locally, which is
	// meaningful for both shapes: it is the directory a redelivery reads
	// rather than repacks.
	Dir() string
}

// federationReleaseSigner loads the root's signing key, or nil when none is
// configured.
//
// A configured key that does not load is an error rather than a nil: a
// deployment that was handed a key and cannot use it has a real
// misconfiguration, and answering that by quietly running a keyless root
// would leave an operator believing policy is being signed when none is.
func federationReleaseSigner() (*pluginmanifest.Signer, error) {
	raw := strings.TrimSpace(os.Getenv(federationReleaseKeyEnv))
	if raw == "" {
		return nil, nil
	}
	keyID := strings.TrimSpace(os.Getenv(federationKeyIDEnv))
	if keyID == "" {
		keyID = defaultFederationKeyID
	}
	seed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("federation: %s is not base64: %w", federationReleaseKeyEnv, err)
	}
	signer, err := pluginmanifest.NewSigner(keyID, ed25519.PrivateKey(seed))
	if err != nil {
		return nil, fmt.Errorf("federation: %s is not a usable release key: %w", federationReleaseKeyEnv, err)
	}
	return signer, nil
}

// federationLedgerPath is where the root's membership survives a restart.
//
// The default sits under /var/lib/opskeeper with the rest of the manager's
// durable state. It is overridable because a root running from a container
// image with a read-only root filesystem needs somewhere else to put it, and
// an operator who cannot move it cannot run a root at all.
func federationLedgerPath() string {
	return firstNonEmpty(os.Getenv("OPSKEEPER_FEDERATION_LEDGER"),
		"/var/lib/opskeeper/federation/ledger.json")
}

// federationLedger returns the ledger the root should keep its clusters in,
// or nil — with one loud line — when the configured path cannot be used.
//
// Degrading beats refusing here, and the reason is asymmetry. A root with no
// ledger is what this code did before the ledger existed: federation works,
// and a restart forgets. A root that refuses to wire federation because it
// cannot write a file has taken away a working feature over a durability
// detail, and it would do so on every read-only image and every deployment
// that has not mounted a volume. The operator is told exactly what is wrong
// and what to do about it, at boot, rather than finding out from a refusal
// during a rollout.
func federationLedger(log *slog.Logger) *fedbiz.FileLedger {
	ledger := fedbiz.NewFileLedger(federationLedgerPath())
	if err := ledger.Probe(); err != nil {
		log.Error("federation: the ledger path is not usable, so federation will not survive a restart",
			slog.String("path", ledger.Path()),
			slog.String("remedy", "make the path writable, or point "+
				"OPSKEEPER_FEDERATION_LEDGER at one that is"),
			slog.Any("err", err),
		)
		return nil
	}
	return ledger
}

// The two assertions below are the compile-time half of the federationlink
// cut (decision 275), and they are here rather than in the packages they
// check for one reason: each names a type from a bounded context this file is
// allowed to know about and those packages are not.
//
// federationlink used to import the federation domain for exactly two things —
// the Member that Clusters.Authenticate returned, and `var _ fedbiz.Pusher =
// (*Links)(nil)`. The first became a one-field projection the link declares
// for itself; the second is a promise about a port declared over here, and a
// promise can be checked by whoever holds both ends of it. Moving an
// assertion does not weaken it: the same mismatch fails to compile at wiring
// time instead, which is where every other mismatch in this file fails.
//
// The adapter is fifteen lines and it is in the composition root because that
// is the one place in the tree permitted to know both bounded contexts. Its
// test-only twin lives in federationlink's own test files, since core/domains
// cannot import cmd/opskeeper.
var (
	_ managersvcfedlink.Clusters = clusterRegistrar{}
	_ fedbiz.Pusher              = (*managersvcfedlink.Links)(nil)
)

// clusterRegistrar is the federation registry seen through the link's port.
type clusterRegistrar struct {
	reg *fedbiz.Registry
}

// Authenticate answers one cluster hello.
//
// The error is returned unwrapped on purpose: ErrRefused is one error for an
// unknown cluster and a wrong token on purpose (see fedbiz.ErrRefused — a
// caller that can tell them apart learns which clusters exist), and wrapping
// it here would be the first place that distinction starts to leak.
func (c clusterRegistrar) Authenticate(id floorfed.ClusterID, token string, claimed floorfed.Cluster) (managersvcfedlink.Enrolled, error) {
	m, err := c.reg.Authenticate(id, token, claimed)
	if err != nil {
		return managersvcfedlink.Enrolled{}, err
	}
	return enrolledFrom(m), nil
}

// enrolledFrom is the whole conversion, as a function, because the one thing
// that can go wrong in the adapter above is a single field and a function is
// the only shape a test can pin it on.
//
// Acknowledged and never HighestIssued: a child already ahead of the ledger
// must not be told it is behind, or it refuses the next push as a replay. The
// argument belongs to the link — it is a rule about what the LINK may say — and
// this is where the rule becomes a copy.
//
// It is extracted from the method rather than inlined because of what an
// earlier version of this comment claimed and could not back up: that the
// channel tests would catch a wrong field. They do not. Enrolling a cluster
// and saying hello exercises both adapters with Acknowledged and HighestIssued
// equal, so a swap is invisible to every test that exists; swapping the field
// in both copies passed 23 federationlink tests and the cmd contract test
// without a word. A rule that nobody can violate without a red test is a
// comment, and this one is now a function.
func enrolledFrom(m fedbiz.Member) managersvcfedlink.Enrolled {
	return managersvcfedlink.Enrolled{Acknowledged: m.Acknowledged}
}

func (c clusterRegistrar) Known(id floorfed.ClusterID) bool { return c.reg.Known(id) }
