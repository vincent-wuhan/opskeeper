package federation

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

var (
	// ErrNotEnrolled means no child has been provisioned for that identity.
	ErrNotEnrolled = errors.New("federation: no child is enrolled for that cluster")

	// ErrUnknownVersion means a child acknowledged a version this root
	// never issued. It is a conflict rather than an internal error because
	// it means one of the two sides is wrong about the numbering, and
	// whose fault that is decides what an operator does next.
	ErrUnknownVersion = errors.New("federation: acknowledged a version this root never issued")

	// ErrRefused is what Authenticate returns for every failed hello,
	// whether the cluster is unknown or the token is wrong.
	//
	// The two are one error at the boundary because at the boundary the
	// difference is a free cluster-enumeration oracle for anyone who can
	// open a connection. A caller that can tell "no such cluster" from
	// "wrong token" learns which clusters exist on a root it has no
	// credential for; that is a smaller problem than getting in, and still
	// not one worth having.
	ErrRefused = errors.New("federation: cluster hello refused")
)

// provisioningTokenBytes is the entropy in a provisioning token.
//
// Thirty-two bytes because this credential's entire job is to be
// unguessable, and because a cluster's token is presented on every reconnect
// by a component that is, by design, running unattended. It is not a
// human-typed secret, so there is no argument for making it short.
const provisioningTokenBytes = 32

// Member is one enrolled child cluster as the root knows it.
type Member struct {
	// Cluster is the identity and the claim the child makes about
	// itself. The root records it; it does not believe it.
	Cluster federation.Cluster
	// TokenHash is the SHA-256 of the provisioning token. The token
	// itself is returned to the operator once at enrolment and never
	// stored, because a store that holds the secret is a store whose
	// backup holds the secret.
	TokenHash [sha256.Size]byte
	// HighestIssued is the largest version this root has ever handed out
	// for this cluster.
	//
	// It is separate from the child's live version on purpose. The child's
	// live version is what it enforces and can go stale in either
	// direction; this is what the root has published and must never
	// reissue, or a child that lost state would accept a version it has
	// already seen and the monotonic rule on the child would refuse it —
	// leaving a rollout that can never complete.
	HighestIssued uint64
	// IssuedBundle is the decision HighestIssued refers to.
	//
	// It is here so a delivery can be retried without spending a version.
	// The alternative is the obvious design — a push that failed is
	// re-pushed by publishing again — and it is wrong in a way that only
	// shows up in production: every retry would mint a new version, each
	// one would be newer than the last so nothing would refuse it, and a
	// cluster with a flaky link would accumulate a version per attempt
	// while the operator watched a number climb for no reason.
	//
	// Like the rest of the registry it is in memory, and a root that
	// restarts forgets which bundle its newest version referred to. That
	// is the same limitation HighestIssued already has and it is fixed by
	// the same durable Ledger, not by anything here.
	IssuedBundle federation.Bundle
	// Acknowledged is the last version the child confirmed, and whether
	// it took it. A root that has published 9 and last heard "7
	// refused" is a different situation from one that has heard nothing.
	Acknowledged uint64
	// LastAck describes the outcome of the last acknowledgement.
	LastAck federation.Outcome
	// LastContact is when the child last spoke to this root.
	LastContact time.Time
}

// Behind reports whether the child is enforcing what this root last published.
//
// A refusal counts as behind, which is the whole point of the distinction. A
// cluster that acknowledged the newest version and declined it is not caught
// up — it is running something the root did not publish for it, on purpose,
// and an operator looking at that needs to be told so rather than shown a
// green tick beside a version number.
//
// It is also deliberately not "is the child healthy". A cluster enforcing
// version 9 while the root has published 10 and heard nothing back is a
// different page from one whose last acknowledgement was a refusal: the first
// might be disconnected, the second is answering.
func (m Member) Behind() bool {
	return m.Acknowledged < m.HighestIssued ||
		(m.Acknowledged == m.HighestIssued && !m.LastAck.Accepted)
}

// Registry is the root's membership and version ledger.
//
// It is in memory, and that is a real limitation rather than a simplification:
// a root that restarts forgets which versions it issued, and the next publish
// would reuse one. The guard is HighestIssued being written back through
// Ledger before a publish is acknowledged, so a durable Ledger is what makes
// the monotonic guarantee survive a restart — see the port's documentation.
type Registry struct {
	ledger Ledger
	now    func() time.Time

	mu      sync.RWMutex
	members map[federation.ClusterID]*Member
}

// Ledger is where the registry's state outlives the process.
//
// It is a port rather than a repository type because the interesting
// implementations are not databases: one is Postgres for a single root, and
// another is the root's own audit chain, where a policy version is recorded
// because an operator can later ask "what was this cluster told on the 3rd".
type Ledger interface {
	// SaveMember writes a member. It must be durable before the caller
	// acts on the token it is about to return, or a crash between the two
	// leaves an operator holding a credential for a cluster this root
	// has forgotten.
	SaveMember(m Member) error
	// SaveHighestIssued records the newest version handed out for a
	// cluster. Called before the bundle is published, never after: a
	// bundle that was sent but not recorded can be reissued, and a
	// reissued version is one a child may already have refused.
	SaveHighestIssued(id federation.ClusterID, version uint64) error
	// LoadMembers returns everything this root has enrolled, for Restore.
	//
	// This is the half the port was missing, and its absence is why no
	// implementation shipped: a Ledger that could only save would survive
	// no restart in any useful sense — a root would come back believing
	// it had never enrolled a cluster, refuse every child's own valid
	// token, and need a human to re-enrol them one at a time. Writing
	// the Postgres implementation first would have produced exactly that
	// and looked finished.
	//
	// An empty result is a legitimate answer: a root that has enrolled
	// nothing. An error is not, and Restore treats it as fatal — see
	// there.
	LoadMembers() ([]Member, error)
}

// NewRegistry builds a registry over a ledger.
func NewRegistry(ledger Ledger) *Registry {
	return &Registry{
		ledger:  ledger,
		now:     time.Now,
		members: map[federation.ClusterID]*Member{},
	}
}

// Restore repopulates the registry from its Ledger.
//
// It is separate from NewRegistry on purpose. A constructor that reached
// for the disk would make "the registry is in memory" true only until the
// first error, and it would force every test that wants a registry to have
// a ledger. Here the choice is explicit: a caller that passes no ledger
// gets an empty registry, and a caller that has one has to say so.
//
// The failure mode is the reason this is not best-effort. A root that could
// not read its Ledger and started anyway would serve an empty membership,
// which is indistinguishable from the restart lockout it was meant to
// prevent — and it would do so silently, having logged a warning, at the
// exact moment an operator is relying on it. Refusing to start is the
// answer that cannot be wrong: an operator sees a root that is down rather
// than a root that has quietly forgotten every cluster it governs.
func (r *Registry) Restore() error {
	if r.ledger == nil {
		return nil
	}
	members, err := r.ledger.LoadMembers()
	if err != nil {
		return fmt.Errorf("federation: restore enrolled clusters: %w", err)
	}
	restored := make(map[federation.ClusterID]*Member, len(members))
	for _, m := range members {
		if !m.Cluster.ID.Valid() {
			// A row with no identity cannot be authenticated against,
			// and carrying it would make Members() list something
			// nothing can ever bind to.
			return fmt.Errorf("federation: the ledger holds a member with no cluster identity")
		}
		if _, dup := restored[m.Cluster.ID]; dup {
			// Two rows for one cluster means the ledger has no
			// primary key. Picking a winner would make the token
			// that works depend on read order.
			return fmt.Errorf("federation: the ledger holds two members for %q", m.Cluster.ID)
		}
		held := m
		restored[m.Cluster.ID] = &held
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.members = restored
	return nil
}

// Enroll provisions a child cluster and returns its provisioning token.
//
// The token is returned exactly once and is not recoverable: it is handed to
// whoever runs the child cluster, and the root keeps only its hash. An operator
// who loses it re-enrols, which is a deliberate inconvenience — a "resend me
// the token" button is a "show me the credential" button with extra steps.
func (r *Registry) Enroll(id federation.ClusterID, name string) (string, error) {
	if !id.Valid() {
		return "", fmt.Errorf("federation: %q is not a cluster identity", id)
	}

	secret := make([]byte, provisioningTokenBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("federation: generate a provisioning token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret)

	m := &Member{
		Cluster:   federation.Cluster{ID: id, Name: name, JoinedAt: r.now()},
		TokenHash: sha256.Sum256([]byte(token)),
	}
	if r.ledger != nil {
		if err := r.ledger.SaveMember(*m); err != nil {
			// The token is discarded rather than returned: a token the
			// root cannot remember is a token nobody can revoke.
			return "", fmt.Errorf("federation: enrol %q: %w", id, err)
		}
	}

	r.mu.Lock()
	r.members[id] = m
	r.mu.Unlock()
	return token, nil
}

// Known reports whether this root has a member for the cluster, without
// saying anything about its token.
//
// It exists for one caller: the server-side log at the hello boundary. The
// wire deliberately cannot tell "no such cluster" from "wrong token" — that
// distinction is a free cluster-enumeration oracle (see ErrRefused) — and
// the cost of that discipline is that an operator reading a refusal has no
// way to tell the two apart either. The most common cause by far is the
// root having restarted with no durable Ledger, which forgets every member
// and then refuses each child's own perfectly valid token.
//
// A log line is read by the operator, not by the caller that would have to
// open a connection to learn it, so distinguishing here leaks nothing the
// refusal itself was protecting. Nothing else may use this: a second caller
// that can tell the cases apart is the oracle, rebuilt one layer up.
func (r *Registry) Known(id federation.ClusterID) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.members[id]
	return ok
}

// Member returns one enrolled cluster.
func (r *Registry) Member(id federation.ClusterID) (Member, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.members[id]
	if !ok {
		return Member{}, false
	}
	return *m, true
}

// BundleFor returns the decision this root issued as its newest version for
// a cluster.
//
// It exists for the retry path, and the ordering matters: a redelivery must
// re-send exactly the decision that was issued. Re-deriving a bundle from
// whatever the newest envelope happens to be would be a second decision
// wearing the first version's number, and a child that had already refused
// the original would be asked about it again under a name that is no longer
// true.
func (r *Registry) BundleFor(id federation.ClusterID) (federation.Bundle, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.members[id]
	if !ok || m.HighestIssued < federation.MinBundleVersion {
		return federation.Bundle{}, false
	}
	return m.IssuedBundle, true
}

// Members lists every enrolled cluster, ordered by identity so a console and
// a test see the same order.
func (r *Registry) Members() []Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Member, 0, len(r.members))
	for _, m := range r.members {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster.ID < out[j].Cluster.ID })
	return out
}

// Authenticate answers a cluster.hello.
//
// It returns a member only when the cluster is enrolled and the token is the
// one this root issued for it. Every other answer is ErrRefused, for both
// reasons at once — see that error for why they are not told apart.
func (r *Registry) Authenticate(id federation.ClusterID, token string, claimed federation.Cluster) (Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	m, ok := r.members[id]
	if !ok {
		// Same error as a bad token, on purpose. See ErrRefused.
		return Member{}, ErrRefused
	}
	got := sha256.Sum256([]byte(token))
	// Constant time even though the hash is 32 bytes of SHA-256 output: the
	// comparison is cheap and the habit is the point.
	if subtle.ConstantTimeCompare(got[:], m.TokenHash[:]) != 1 {
		return Member{}, ErrRefused
	}

	// The token proved this caller may act for this cluster. It did not
	// make the caller's self-description true, so the parts that are
	// safe to learn are learned and the parts that are not are simply
	// not adopted.
	//
	// Name and Version are a display string and a compatibility input;
	// neither is a security decision here, so recording what the child
	// says about itself is useful and harmless. EdgeCount is recorded
	// for capacity questions and is explicitly never used for
	// accounting — see Member.
	m.Cluster.Name = claimed.Name
	if claimed.Version != "" {
		m.Cluster.Version = claimed.Version
	}
	if claimed.EdgeCount >= 0 {
		m.Cluster.EdgeCount = claimed.EdgeCount
	}
	if claimed.TrustKeyID != "" {
		// A child naming the key it will verify policy with is the
		// root's chance to catch a mismatch before a rollout, rather
		// than shipping a bundle this particular child cannot check.
		m.Cluster.TrustKeyID = claimed.TrustKeyID
	}
	m.Cluster.LastSeen = r.now()
	m.LastContact = r.now()

	return *m, nil
}

// Publish allocates the next version for a cluster and builds the bundle that
// carries it.
//
// The version is allocated before the envelope is attached and is written to
// the ledger before this returns, because a version that was sent and not
// recorded will be handed out again on the next publish — and a child that
// already refused it will refuse the reissue, leaving a rollout that looks
// like a transport problem and is not one.
func (r *Registry) Publish(id federation.ClusterID, env pluginmanifest.Envelope, reason string) (federation.Bundle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	m, ok := r.members[id]
	if !ok {
		return federation.Bundle{}, ErrNotEnrolled
	}

	version := m.HighestIssued + 1
	if version < federation.MinBundleVersion {
		version = federation.MinBundleVersion
	}
	if err := validateEnvelope(env); err != nil {
		return federation.Bundle{}, err
	}

	b := federation.Bundle{
		ClusterID:      id,
		Version:        version,
		PackageName:    env.Name,
		PackageVersion: env.Version,
		Envelope:       env.Transport(),
		Reason:         reason,
		IssuedAt:       r.now().UTC(),
	}
	// The bundle is checked here as well as on the child. The child's
	// check is the one that protects the cluster; this one exists so a
	// publisher's own bug is caught at the root, where the operator is
	// looking, instead of arriving as a refusal at forty child clusters.
	if _, err := b.Validate(); err != nil {
		return federation.Bundle{}, fmt.Errorf("federation: refusing to publish a bundle that does not validate: %w", err)
	}

	m.HighestIssued = version
	m.IssuedBundle = b
	if r.ledger != nil {
		if err := r.ledger.SaveHighestIssued(id, version); err != nil {
			// The version is deliberately NOT rolled back here. Once
			// the caller holds the bundle it may already be on the
			// wire, and handing out the same number twice is worse
			// than skipping one.
			return federation.Bundle{}, fmt.Errorf("federation: record version %d for %q: %w", version, id, err)
		}
	}
	return b, nil
}

// Acknowledge records a child's answer to a push.
//
// It is not an error for a child to acknowledge a refusal — that is a
// perfectly good answer and the one a rollout most needs to hear. What it is
// an error to do is lose it, because a root that stops hearing about a
// refused version will keep believing the child is merely behind.
func (r *Registry) Acknowledge(id federation.ClusterID, out federation.Outcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	m, ok := r.members[id]
	if !ok {
		return ErrNotEnrolled
	}
	// An answer about no version is not an answer. Recording it would
	// overwrite LastAck with a zero outcome, and LastAck is the only
	// record that a child *declined* something — the refusal a rollout
	// most needs to hear, and the one a root that stops hearing about
	// goes on to misread as a cluster that is merely behind.
	if out.Version < federation.MinBundleVersion {
		return fmt.Errorf("%w: cluster %q acknowledged version %d, which is not a version anything can be issued as",
			ErrUnknownVersion, id, out.Version)
	}
	// A child's answer about a version this root never issued is not
	// recorded. It means one of the two is wrong about the numbering, and
	// overwriting the root's own ledger with the child's version would
	// hide that instead of surfacing it.
	if out.Version > m.HighestIssued {
		return fmt.Errorf("%w: cluster %q acknowledged %d, highest issued is %d",
			ErrUnknownVersion, id, out.Version, m.HighestIssued)
	}
	m.Acknowledged = out.Version
	m.LastAck = out
	m.LastContact = r.now()

	// Written through, not just held in memory, because the hello reply
	// sends Acknowledged back to the child as the version this root
	// believes it is enforcing. A root that restarted and forgot would
	// answer "version 0" to a cluster running version 9, and the child
	// would read that as either a replay or a rollback depending on
	// which way it compares. SaveMember writes the whole row, so this is
	// the same call Enroll makes rather than a second write shape.
	if r.ledger != nil {
		if err := r.ledger.SaveMember(*m); err != nil {
			// The in-memory answer stands. A refusal to record it is
			// not a reason to forget it for this process lifetime, and
			// the next acknowledgement will try again.
			return fmt.Errorf("federation: record the answer from %q: %w", id, err)
		}
	}
	return nil
}

// validateEnvelope refuses an envelope that could not become a bundle.
func validateEnvelope(env pluginmanifest.Envelope) error {
	switch {
	case env.Name == "":
		return errors.New("federation: the envelope names no package")
	case env.Version == "":
		return errors.New("federation: the envelope names no package version")
	case env.KeyID == "":
		return errors.New("federation: the envelope names no signing key")
	case env.Signature == "":
		return errors.New("federation: the envelope carries no signature")
	case env.Algorithm != pluginmanifest.SignAlgorithm:
		return fmt.Errorf("federation: the envelope claims algorithm %q, this build only signs %q",
			env.Algorithm, pluginmanifest.SignAlgorithm)
	case len(env.TreeDigest) != sha256.Size*2:
		return fmt.Errorf("federation: the envelope's tree digest is %d characters, want %d",
			len(env.TreeDigest), sha256.Size*2)
	case !isLowerHex(env.TreeDigest):
		return fmt.Errorf("federation: the envelope's tree digest is not lower-hex: %s", env.TreeDigest)
	}
	return nil
}

func isLowerHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && s == lower(s)
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'F' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
