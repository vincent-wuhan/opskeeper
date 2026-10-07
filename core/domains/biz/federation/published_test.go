package federation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// A root that does not serve its own artifacts.
//
// FileDistributor is the path most on-prem deployments take and it needs
// nothing. This file is the other shape: the deployment publishes policy
// trees into a store — an object bucket, a CDN, whatever the organisation
// already runs — and the root addresses that store instead of writing a
// directory a child happens to share.
//
// The properties worth pinning are all about one thing: the root names a URL
// only after it has compared its own signed bytes against what the store says
// it is serving. Everything else in this file follows from that.

// fakeStore is a PublishedLedger backed by a map, which is the whole shape of
// the port: a name, a digest, and whether it is there.
type fakeStore struct {
	digests map[string]string
	asked   []string
}

// RecordPublished is the write half, which the manifest-backed ledger has and
// a read-only one does not. Tests that need the "store took it and does not
// serve it" state use a store without this method rather than a store with it
// and an empty map — those are different situations and only one of them is a
// ledger that forgot.
func (s *fakeStore) RecordPublished(name, digest string) error {
	s.digests[name] = digest
	return nil
}

func (s *fakeStore) PublishedDigest(name string) (string, bool) {
	s.asked = append(s.asked, name)
	d, ok := s.digests[name]
	return d, ok
}

// testSigner is a release key for a test that needs a signed tree.
func testSigner(t *testing.T) *pluginmanifest.Signer {
	t.Helper()
	signer, _, err := pluginmanifest.GenerateSigner("release-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	return signer
}

// digestOfArchive is what a deployment's publish step would see after
// uploading: the digest of the bytes the root actually wrote.
func digestOfArchive(t *testing.T, dir, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read the packed archive: %v", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func newPublished(t *testing.T, base string, store *fakeStore) (*PublishedDistributor, string) {
	t.Helper()
	local, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	d, err := NewPublishedDistributor(local, base, store)
	if err != nil {
		t.Fatalf("NewPublishedDistributor: %v", err)
	}
	return d, local.Dir()
}

func TestAPublishedTreeIsAddressedAtTheStoreAndNotAtThisRootsDisk(t *testing.T) {
	store := &fakeStore{digests: map[string]string{}}
	d, dir := newPublished(t, "https://artifacts.example.com/opskeeper/policies", store)

	// The store learns the digest the way a real publish step would: the root
	// packs, and the deployment uploads those bytes. Modelling it as a
	// callback rather than as a pre-seeded map is what makes the test about
	// the order of the two, not just about the URL shape.
	id, _ := federation.NewClusterID("prod-cn-north")
	b := federation.Bundle{ClusterID: id, Version: 7}
	signer := testSigner(t)
	seed := func(name, digest string) { store.digests[name] = digest }

	// First pass: not there yet.
	_, err := d.Distribute(t.Context(), b, stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0"))
	if !errors.Is(err, ErrNotPublished) {
		t.Fatalf("a store with nothing in it gave %v, want ErrNotPublished", err)
	}

	// The deployment's publish step runs, and from now on the digest the
	// store reports is the one the root just wrote to disk.
	seeded := digestOfArchive(t, dir, archiveName(b))
	seed(archiveName(b), seeded)

	src, err := d.Distribute(t.Context(), b, stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0"))
	if err != nil {
		t.Fatalf("Distribute after the store caught up: %v", err)
	}
	if want := "https://artifacts.example.com/opskeeper/policies/" + archiveName(b); src.URL != want {
		t.Errorf("URL = %q, want %q", src.URL, want)
	}
	if strings.HasPrefix(src.URL, "file://") {
		t.Errorf("URL = %q, want an address in the store rather than on this root's disk", src.URL)
	}
	if src.ArchiveSHA256 == "" {
		t.Error("ArchiveSHA256 is empty; the child checks the digest before it unpacks, so an empty one is a child-side integrity failure waiting for a transfer")
	}
}

// TestAStoreHoldingDifferentBytesIsAConflictAndNotARetry is the distinction the
// two sentinels exist for, and it is the reason ErrPublishedMismatch is not a
// retryable error.
//
// Every other failure in this file is a timing problem a later attempt fixes.
// This one is a disagreement between two authorities: the store says it is
// serving bytes, and those bytes are not what this root signed. Retrying
// produces the same conflict on every pass, and a delivery that never converges
// and never says why is the failure Distributor's own contract warns about.
func TestAStoreHoldingDifferentBytesIsAConflictAndNotARetry(t *testing.T) {
	store := &fakeStore{digests: map[string]string{}}
	d, _ := newPublished(t, "https://artifacts.example.com/policies", store)

	id, _ := federation.NewClusterID("prod-cn-north")
	b := federation.Bundle{ClusterID: id, Version: 3}
	store.digests[archiveName(b)] = strings.Repeat("ab", 32)

	_, err := d.Distribute(t.Context(), b, stagingRoot(t, testSigner(t), "opskeeper-sre-readonly", "1.0.0"))
	if !errors.Is(err, ErrPublishedMismatch) {
		t.Fatalf("a store holding different bytes gave %v, want ErrPublishedMismatch", err)
	}
	if errors.Is(err, ErrNotPublished) {
		t.Error("a conflict is being reported as a not-yet, which would make an operator wait for a publish that will never fix it")
	}
	// The message has to carry the digest the store is holding, because the
	// question a human asks is "which of these two is right" and neither side
	// knows. Naming only this root's digest would answer a question nobody
	// asked.
	if !strings.Contains(err.Error(), strings.Repeat("ab", 32)) {
		t.Errorf("error = %v, want it to carry the digest the store is holding", err)
	}
}

// TestTheArtifactBaseUrlMustNameASchemeAndAHost keeps the decision about where
// signed policy trees go inside the code that owns it.
//
// "example.com/policies" is the shape a person actually types, and a
// distributor that assumed https for it would publish signed policy trees to
// an address nobody chose — over whatever scheme the string happened to
// resolve to.
func TestTheArtifactBaseUrlMustNameASchemeAndAHost(t *testing.T) {
	local, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	store := &fakeStore{digests: map[string]string{}}

	for _, base := range []string{
		"",
		"   ",
		"example.com/policies",
		"artifacts.example.com",
		"ftp://artifacts.example.com/policies",
		"file:///var/lib/opskeeper/policies",
		"https://",
	} {
		if _, err := NewPublishedDistributor(local, base, store); err == nil {
			t.Errorf("NewPublishedDistributor(%q) succeeded; a base that does not name an http or https place "+
				"is a place nobody agreed to publish policy to", base)
		}
	}
	for _, base := range []string{
		"https://artifacts.example.com/policies",
		"http://artifacts.internal/policies",
	} {
		if _, err := NewPublishedDistributor(local, base, store); err != nil {
			t.Errorf("NewPublishedDistributor(%q) = %v, want it accepted", base, err)
		}
	}
}

// TestARedeliveryAddressesTheSameBytesTheFirstDeliveryNamed is the property
// that made this a wrapper rather than a second implementation.
//
// A redelivery cannot repack. A tar of the same tree produced twice can differ
// in one header field, and the child compares the digest before it unpacks
// anything — so a repacked archive is a digest that matches nothing, and the
// symptom is a rollout that fails its integrity check on every retry with no
// explanation.
func TestARedeliveryAddressesTheSameBytesTheFirstDeliveryNamed(t *testing.T) {
	store := &fakeStore{digests: map[string]string{}}
	d, dir := newPublished(t, "https://artifacts.example.com/policies/", store)

	id, _ := federation.NewClusterID("prod-cn-north")
	b := federation.Bundle{ClusterID: id, Version: 11}
	signer := testSigner(t)
	staged := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")

	// Not published yet, so the first delivery is the one that writes the
	// bytes this root will be held to.
	if _, err := d.Distribute(t.Context(), b, staged); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("first Distribute = %v, want ErrNotPublished", err)
	}
	store.digests[archiveName(b)] = digestOfArchive(t, dir, archiveName(b))

	first, err := d.Distribute(t.Context(), b, staged)
	if err != nil {
		t.Fatalf("Distribute: %v", err)
	}
	again, err := d.SourceFor(b)
	if err != nil {
		t.Fatalf("SourceFor: %v", err)
	}
	if first != again {
		t.Errorf("a redelivery named %+v where the first delivery named %+v; a repack would "+
			"produce a digest the child cannot match against anything", again, first)
	}
	if first.URL != "https://artifacts.example.com/policies/"+archiveName(b) {
		t.Errorf("URL = %q, want the trailing slash on the base not to double up", first.URL)
	}
}

// TestTheStoreIsAskedAboutEveryDeliveryAndOnlyAfterTheBytesExist pins the
// order. The root packs first so it knows the digest of what it signed, and
// asks second so the URL it hands out is one it has compared rather than one it
// hopes for.
func TestTheStoreIsAskedAboutEveryDeliveryAndOnlyAfterTheBytesExist(t *testing.T) {
	store := &fakeStore{digests: map[string]string{}}
	d, dir := newPublished(t, "https://artifacts.example.com/policies", store)

	id, _ := federation.NewClusterID("prod-cn-north")
	b := federation.Bundle{ClusterID: id, Version: 2}
	staged := stagingRoot(t, testSigner(t), "opskeeper-sre-readonly", "1.0.0")

	if _, err := d.Distribute(t.Context(), b, staged); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("Distribute = %v, want ErrNotPublished", err)
	}
	if len(store.asked) != 1 || store.asked[0] != archiveName(b) {
		t.Errorf("the store was asked about %v, want exactly one question about %s", store.asked, archiveName(b))
	}
	// The bytes exist locally even though the delivery did not happen, which
	// is what lets a redelivery name them exactly.
	if digestOfArchive(t, dir, archiveName(b)) == "" {
		t.Error("a refused delivery wrote no archive, so the redelivery path has nothing exact to name")
	}
}

// TestTheZeroConfigurationPathIsUnaffected is the regression this file could
// have caused and did not: a deployment that shares a mount still gets a
// file:// URL and still needs no store.
func TestTheZeroConfigurationPathIsUnaffected(t *testing.T) {
	dist, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	id, _ := federation.NewClusterID("prod-cn-north")
	src, err := dist.Distribute(t.Context(),
		federation.Bundle{ClusterID: id, Version: 1},
		stagingRoot(t, testSigner(t), "opskeeper-sre-readonly", "1.0.0"))
	if err != nil {
		t.Fatalf("Distribute: %v", err)
	}
	if !strings.HasPrefix(src.URL, "file://") {
		t.Errorf("URL = %q, want the shared-mount path to still be a file URL", src.URL)
	}
}

// TestTheManifestIsReReadRatherThanCached is the property that makes a file
// the right shape for this port.
//
// The publish step runs out of band — a CI job, a cron, a person — so a
// manifest loaded once at boot is a snapshot that is stale for the rest of the
// process's life. A root that had to be restarted to notice its own artifact
// was uploaded is a root somebody will restart by guessing why.
func TestTheManifestIsReReadRatherThanCached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "published.json")
	ledger, err := NewManifestLedger(path)
	if err != nil {
		t.Fatalf("NewManifestLedger: %v", err)
	}
	if _, ok := ledger.PublishedDigest("prod-v1.tar.gz"); ok {
		t.Error("a manifest that does not exist yet answered that it does")
	}

	// Written after the ledger was built, which is the whole ordering the
	// publish step produces.
	writeManifest(t, path, map[string]string{"prod-v1.tar.gz": strings.Repeat("cd", 32)}, "")
	digest, ok := ledger.PublishedDigest("prod-v1.tar.gz")
	if !ok {
		t.Fatal("a manifest written after the ledger was built is not visible to it")
	}
	if digest != strings.Repeat("cd", 32) {
		t.Errorf("digest = %q, want the one written", digest)
	}
	if _, ok := ledger.PublishedDigest("other-v1.tar.gz"); ok {
		t.Error("a name the manifest does not mention answered that it does")
	}
}

// TestAManifestThatCannotBeReadAnswersNotThere covers the three ways a file
// deployment wrote can be unusable, and the reasoning is the same for all of
// them: the only caller asks "may I name this URL yet", and every one of these
// means no. Distinguishing them would be reporting, and reporting is the
// publish step's job — it knows whether it ran.
func TestAManifestThatCannotBeReadAnswersNotThere(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"malformed.json":    "{not json",
		"empty.json":        "",
		"wrong-shape.json":  `["a", "b"]`,
		"blank-digest.json": `{"prod-v1.tar.gz": "  "}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			writeManifest(t, path, nil, body)
			ledger, err := NewManifestLedger(path)
			if err != nil {
				t.Fatalf("NewManifestLedger: %v", err)
			}
			if _, ok := ledger.PublishedDigest("prod-v1.tar.gz"); ok {
				t.Error("an unusable manifest answered that it holds the tree")
			}
		})
	}
}

func writeManifest(t *testing.T, path string, entries map[string]string, raw string) {
	t.Helper()
	body := raw
	if entries != nil {
		encoded, err := json.Marshal(entries)
		if err != nil {
			t.Fatalf("marshal the manifest: %v", err)
		}
		body = string(encoded)
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
}
