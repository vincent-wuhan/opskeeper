package federation

import (
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// Registry tests.
//
// The root's properties here are about identity and bookkeeping. The
// dangerous mistakes are enumerable: handing out a version twice, believing a
// child's claim without checking the token, and losing track of the fact that
// a child refused something.

func newEnvelope(t *testing.T, name, version string) pluginmanifest.Envelope {
	t.Helper()
	env := pluginmanifest.Envelope{
		KeyID:      "release-2026",
		Algorithm:  pluginmanifest.SignAlgorithm,
		Name:       name,
		Version:    version,
		TreeDigest: strings.Repeat("ab", 32),
		Signature:  "not-a-real-signature-but-present",
	}
	return env
}

func TestEnrollReturnsATokenThatAuthenticates(t *testing.T) {
	r := NewRegistry(nil)
	id, err := federation.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}

	token, err := r.Enroll(id, "华东生产一区")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if len(token) < 40 {
		t.Errorf("token is %d characters, which is short for %d bytes of entropy", len(token), provisioningTokenBytes)
	}

	m, err := r.Authenticate(id, token, federation.Cluster{ID: id, Name: "claimed name"})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if m.Cluster.ID != id {
		t.Errorf("member ID = %q, want %q", m.Cluster.ID, id)
	}
	if m.Cluster.LastSeen.IsZero() {
		t.Errorf("LastSeen was not set by a successful hello")
	}
}

// TestTheTokenIsNotRecoverableFromTheRegistry pins the property that makes a
// backup safe to keep: the root holds a hash, not the credential.
func TestTheTokenIsNotRecoverableFromTheRegistry(t *testing.T) {
	r := NewRegistry(nil)
	id, _ := federation.NewClusterID("prod-cn-north")
	token, err := r.Enroll(id, "north")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	m, ok := r.Member(id)
	if !ok {
		t.Fatalf("member missing after Enroll")
	}
	if strings.Contains(string(m.TokenHash[:]), token) {
		t.Errorf("the stored credential material contains the token itself")
	}
	// And the token must not authenticate with the stored bytes.
	if _, err := r.Authenticate(id, string(m.TokenHash[:]), m.Cluster); err == nil {
		t.Errorf("the stored hash authenticated as a token")
	}
}

// TestAuthenticateRefusesEverythingElseAndSaysNothingUseful: a caller that can
// tell "no such cluster" from "wrong token" can enumerate which clusters exist
// on a root it has no credential for.
func TestAuthenticateRefusesEverythingElseAndSaysNothingUseful(t *testing.T) {
	r := NewRegistry(nil)
	id, _ := federation.NewClusterID("prod-cn-north")
	token, err := r.Enroll(id, "north")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	unknown, _ := federation.NewClusterID("prod-cn-south")

	cases := map[string]struct {
		id    federation.ClusterID
		token string
	}{
		"unknown cluster, right token": {unknown, token},
		"right cluster, wrong token":   {id, "not-the-token"},
		"right cluster, empty token":   {id, ""},
		"unknown cluster, empty token": {unknown, ""},
	}
	var refusals []string
	for name, tc := range cases {
		_, err := r.Authenticate(tc.id, tc.token, federation.Cluster{ID: tc.id})
		if err == nil {
			t.Errorf("[%s] authenticated", name)
			continue
		}
		refusals = append(refusals, err.Error())
	}
	// Every refusal has the same text, so none of them distinguishes the
	// cases from the outside.
	for i := 1; i < len(refusals); i++ {
		if refusals[i] != refusals[0] {
			t.Errorf("refusal texts differ and so enumerate enrolled clusters:\n  %q\n  %q",
				refusals[0], refusals[i])
		}
	}
}

// TestReEnrollingRotatesTheToken: an operator who suspects a token leaked
// re-enrols, and the old credential must stop working — otherwise "rotate" is
// a word rather than an action.
func TestReEnrollingRotatesTheToken(t *testing.T) {
	r := NewRegistry(nil)
	id, _ := federation.NewClusterID("prod-cn-north")

	first, err := r.Enroll(id, "north")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	second, err := r.Enroll(id, "north")
	if err != nil {
		t.Fatalf("re-Enroll: %v", err)
	}
	if first == second {
		t.Fatalf("re-enrolment issued the same token")
	}
	if _, err := r.Authenticate(id, first, federation.Cluster{ID: id}); err == nil {
		t.Errorf("the previous token still authenticates after rotation")
	}
	if _, err := r.Authenticate(id, second, federation.Cluster{ID: id}); err != nil {
		t.Errorf("the rotated token does not authenticate: %v", err)
	}
}

// TestAuthenticateAdoptsTheChildSelfDescriptionWithoutTreatingItAsAuthority:
// the token proves the caller may act for this cluster. It does not make the
// caller's account of itself true, and the fields it may set are only the ones
// that are safe to be wrong about.
func TestAuthenticateAdoptsTheChildSelfDescriptionWithoutTreatingItAsAuthority(t *testing.T) {
	r := NewRegistry(nil)
	id, _ := federation.NewClusterID("prod-cn-north")
	token, _ := r.Enroll(id, "north")

	claimed := federation.Cluster{
		ID:         id,
		Name:       "华东生产一区",
		Version:    "0.4.0",
		EdgeCount:  128,
		TrustKeyID: "release-2026",
	}
	m, err := r.Authenticate(id, token, claimed)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if m.Cluster.Name != claimed.Name {
		t.Errorf("Name = %q, want the child's %q", m.Cluster.Name, claimed.Name)
	}
	if m.Cluster.Version != claimed.Version {
		t.Errorf("Version = %q, want %q", m.Cluster.Version, claimed.Version)
	}
	if m.Cluster.EdgeCount != claimed.EdgeCount {
		t.Errorf("EdgeCount = %d, want %d", m.Cluster.EdgeCount, claimed.EdgeCount)
	}
	if m.Cluster.TrustKeyID != claimed.TrustKeyID {
		t.Errorf("TrustKeyID = %q, want %q — this is the root's chance to catch a key mismatch before a rollout",
			m.Cluster.TrustKeyID, claimed.TrustKeyID)
	}
	// A negative count is nonsense rather than a small number, and it is
	// the one claim that is cheap to sanity-check.
	if _, err := r.Authenticate(id, token, federation.Cluster{ID: id, EdgeCount: -5}); err != nil {
		t.Errorf("Authenticate: %v", err)
	}
	m, _ = r.Member(id)
	if m.Cluster.EdgeCount == -5 {
		t.Errorf("a negative node count was recorded verbatim")
	}
}

func TestPublishAllocatesStrictlyIncreasingVersions(t *testing.T) {
	r := NewRegistry(nil)
	id, _ := federation.NewClusterID("prod-cn-north")
	if _, err := r.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	seen := map[uint64]bool{}
	for i := 1; i <= 25; i++ {
		b, err := r.Publish(id, newEnvelope(t, "opskeeper-sre-readonly", "1.0.0"), "rollout")
		if err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
		if seen[b.Version] {
			t.Fatalf("version %d was issued twice", b.Version)
		}
		seen[b.Version] = true
		if b.Version != uint64(i) {
			t.Errorf("Publish #%d returned version %d", i, b.Version)
		}
		if b.ClusterID != id {
			t.Errorf("bundle addressed to %q, want %q", b.ClusterID, id)
		}
		if _, err := b.Validate(); err != nil {
			t.Errorf("the root published a bundle its own child would refuse: %v", err)
		}
	}
}

// TestPublishRefusesAnUnpublishableEnvelope catches a publisher's own bug at
// the root, where the operator is looking, rather than as forty identical
// refusals at forty child clusters.
func TestPublishRefusesAnUnpublishableEnvelope(t *testing.T) {
	r := NewRegistry(nil)
	id, _ := federation.NewClusterID("prod-cn-north")
	if _, err := r.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	cases := map[string]func(e *pluginmanifest.Envelope){
		"no name":      func(e *pluginmanifest.Envelope) { e.Name = "" },
		"no version":   func(e *pluginmanifest.Envelope) { e.Version = "" },
		"no key":       func(e *pluginmanifest.Envelope) { e.KeyID = "" },
		"no signature": func(e *pluginmanifest.Envelope) { e.Signature = "" },
		"algorithm":    func(e *pluginmanifest.Envelope) { e.Algorithm = "rot13" },
		"digest short": func(e *pluginmanifest.Envelope) { e.TreeDigest = "abcd" },
		"digest upper": func(e *pluginmanifest.Envelope) { e.TreeDigest = strings.ToUpper(strings.Repeat("ab", 32)) },
	}
	for name, mutate := range cases {
		env := newEnvelope(t, "opskeeper-sre-readonly", "1.0.0")
		mutate(&env)
		if _, err := r.Publish(id, env, "rollout"); err == nil {
			t.Errorf("[%s] Publish accepted an envelope it cannot ship", name)
		}
	}
}

func TestPublishRefusesAnUnenrolledCluster(t *testing.T) {
	r := NewRegistry(nil)
	id, _ := federation.NewClusterID("prod-cn-north")
	if _, err := r.Publish(id, newEnvelope(t, "opskeeper-sre-readonly", "1.0.0"), ""); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("Publish to an unenrolled cluster = %v, want ErrNotEnrolled", err)
	}
}

// failingLedger refuses to record, which is the state a full disk or a
// read-only replica leaves the root in.
type failingLedger struct {
	err error
}

func (l failingLedger) SaveMember(Member) error                              { return nil }
func (l failingLedger) SaveHighestIssued(federation.ClusterID, uint64) error { return l.err }
func (l failingLedger) LoadMembers() ([]Member, error)                       { return nil, nil }

// TestAFailedLedgerSkipsAVersionRatherThanReissuingIt is the durability
// property. Once Publish has built a bundle the caller may already have it on
// the wire, so the number is spent whether or not the ledger took it. Reusing
// it would produce a version a child may already have refused, which reads as
// a transport fault and is chased as one.
func TestAFailedLedgerSkipsAVersionRatherThanReissuingIt(t *testing.T) {
	wantErr := errors.New("disk is full")
	r := NewRegistry(failingLedger{err: wantErr})
	id, _ := federation.NewClusterID("prod-cn-north")
	if _, err := r.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if _, err := r.Publish(id, newEnvelope(t, "opskeeper-sre-readonly", "1.0.0"), ""); !errors.Is(err, wantErr) {
		t.Fatalf("Publish = %v, want the ledger error", err)
	}

	// The ledger recovers; the next publish must not reuse version 1.
	r.ledger = nil
	b, err := r.Publish(id, newEnvelope(t, "opskeeper-sre-readonly", "1.0.0"), "")
	if err != nil {
		t.Fatalf("Publish after recovery: %v", err)
	}
	if b.Version != 2 {
		t.Errorf("version = %d after a failed publish, want 2 — version 1 was spent", b.Version)
	}
}

// TestARefusalIsAnAnswerAndNotAFault: a child declining a policy is the single
// most useful thing a rollout can hear, and it must not be shaped like a
// transport error or the root will retry it forever.
func TestARefusalIsAnAnswerAndNotAFault(t *testing.T) {
	r := NewRegistry(nil)
	id, _ := federation.NewClusterID("prod-cn-north")
	if _, err := r.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	b, err := r.Publish(id, newEnvelope(t, "opskeeper-sre-repair", "2.0.0"), "widen blast radius")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	refused := federation.Outcome{
		Version:  b.Version,
		Accepted: false,
		Live:     4,
		Reason:   "admission: exceeds this cluster's max safety level",
	}
	if err := r.Acknowledge(id, refused); err != nil {
		t.Fatalf("recording a refusal returned %v; a refusal is an answer, not a fault", err)
	}

	m, _ := r.Member(id)
	if m.Acknowledged != b.Version {
		t.Errorf("Acknowledged = %d, want %d", m.Acknowledged, b.Version)
	}
	if m.LastAck.Accepted {
		t.Errorf("LastAck reads as accepted: %+v", m.LastAck)
	}
	if !m.Behind() {
		t.Errorf("Behind() = false, want true — the root has published a version this child has not taken")
	}
}

// TestAcknowledgingAVersionTheRootNeverIssuedIsRefused: it means one of the
// two sides is wrong about the numbering, and overwriting the root's ledger
// would hide that rather than surface it.
func TestAcknowledgingAVersionTheRootNeverIssuedIsRefused(t *testing.T) {
	r := NewRegistry(nil)
	id, _ := federation.NewClusterID("prod-cn-north")
	if _, err := r.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if _, err := r.Publish(id, newEnvelope(t, "opskeeper-sre-readonly", "1.0.0"), ""); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	err := r.Acknowledge(id, federation.Outcome{Version: 99, Accepted: true})
	if err == nil {
		t.Fatalf("a version this root never issued was accepted as an acknowledgement")
	}
	if !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("refusal = %v, want it to wrap ErrUnknownVersion so the HTTP layer can answer 409 without matching on text", err)
	}
	m, _ := r.Member(id)
	if m.Acknowledged != 0 {
		t.Errorf("Acknowledged = %d after a refused acknowledgement, want 0", m.Acknowledged)
	}
}

func TestMembersAreListedInIdentityOrder(t *testing.T) {
	r := NewRegistry(nil)
	for _, raw := range []string{"prod-cn-south", "prod-cn-north", "eu-west"} {
		id, err := federation.NewClusterID(raw)
		if err != nil {
			t.Fatalf("NewClusterID(%q): %v", raw, err)
		}
		if _, err := r.Enroll(id, raw); err != nil {
			t.Fatalf("Enroll(%q): %v", raw, err)
		}
	}
	got := r.Members()
	want := []string{"eu-west", "prod-cn-north", "prod-cn-south"}
	if len(got) != len(want) {
		t.Fatalf("Members() = %d entries, want %d", len(got), len(want))
	}
	for i, m := range got {
		if m.Cluster.ID.String() != want[i] {
			t.Errorf("Members()[%d] = %q, want %q", i, m.Cluster.ID, want[i])
		}
	}
}
