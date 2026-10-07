package federation

// publish_test.go — the write half of the published artifact store.
//
// PublishedDistributor's tests seed the store's digest by hand and call it
// "the deployment's publish step". That step had no code behind it, so these
// tests are about the one thing that is easy to get wrong and impossible to
// notice here: the bytes that reach the store must be the bytes this root
// signed. A publisher that repacks passes every URL-shaped assertion and
// produces a tree no child can verify.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
)

// recordingSink is an artifact store that keeps what it was handed.
//
// transform models the failure the Put contract is written against: a store
// that changes the bytes it stores. When it is set, the sink reports the
// digest of what it will serve rather than of what it was given, which is the
// disagreement Publish has to catch.
type recordingSink struct {
	held      map[string][]byte
	asked     []string
	digests   map[string]string
	transform func([]byte) []byte
	err       error
}

func newRecordingSink() *recordingSink {
	return &recordingSink{held: map[string][]byte{}, digests: map[string]string{}}
}

func (s *recordingSink) Put(_ context.Context, name string, data []byte) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.asked = append(s.asked, name)
	s.held[name] = append([]byte(nil), data...)
	stored := data
	if s.transform != nil {
		stored = s.transform(data)
	}
	sum := sha256.Sum256(stored)
	digest := hex.EncodeToString(sum[:])
	s.digests[name] = digest
	return digest, nil
}

// seededSink serves only what it has been told it holds, which is how a real
// store behaves: an upload accepted now may not be readable now.
func (s *recordingSink) seed() {
	for name, data := range s.held {
		sum := sha256.Sum256(data)
		s.digests[name] = hex.EncodeToString(sum[:])
	}
}

// publishableTree delivers one bundle and returns the publisher, the sink, the
// store ledger and the bundle, with the store holding nothing.
//
// The delivery happens exactly once, through the distributor, and it is the
// last thing that touches the archive on disk. That is not tidiness: Distribute
// packs, so calling it a second time rewrites the archive with a freshly signed
// manifest and the digest the child would compare against is a different one.
// Every assertion below therefore reaches the distributor through SourceFor,
// which re-reads rather than repacks — the same discipline the package's own
// Redeliverer contract rests on.
func publishableTree(t *testing.T) (*PublishedPublisher, *PublishedDistributor, *recordingSink, *fakeStore, federation.Bundle) {
	t.Helper()
	local, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	store := &fakeStore{digests: map[string]string{}}
	published, err := NewPublishedDistributor(local, "https://artifacts.example.com/policies", store)
	if err != nil {
		t.Fatalf("NewPublishedDistributor: %v", err)
	}
	sink := newRecordingSink()
	publisher, err := NewPublishedPublisher(published, sink)
	if err != nil {
		t.Fatalf("NewPublishedPublisher: %v", err)
	}
	id, _ := federation.NewClusterID("prod-cn-north")
	b := federation.Bundle{ClusterID: id, Version: 11}

	// Deliver once. The store has nothing, so this is the not-yet a publish
	// runs from.
	if _, err := published.Distribute(t.Context(), b,
		stagingRoot(t, testSigner(t), "opskeeper-sre-readonly", "1.0.0")); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("the unpushed tree gave %v, want ErrNotPublished; the fixture is not the state "+
			"a publish runs from", err)
	}
	return publisher, published, sink, store, b
}

// catchUp makes the store ledger report what the sink is holding, which is
// what a store does once an accepted upload becomes readable.
func catchUp(sink *recordingSink, store *fakeStore) {
	sink.seed()
	for name, digest := range sink.digests {
		store.digests[name] = digest
	}
}

// The whole reason this file exists: publishing is what turns a store that
// says "not yet" into one a child can fetch from, and the URL it hands back
// is in the store rather than on this root's disk.
func TestAPublishedTreeGetsAnAddressOnlyOnceTheStoreHoldsIt(t *testing.T) {
	publisher, _, _, _, b := publishableTree(t)

	src, err := publisher.Publish(t.Context(), b)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	want := "https://artifacts.example.com/policies/" + archiveName(b)
	if src.URL != want {
		t.Errorf("URL = %q, want %q", src.URL, want)
	}
	if strings.HasPrefix(src.URL, "file://") {
		t.Errorf("URL = %q, want an address in the store rather than on this root's disk", src.URL)
	}
	if src.ArchiveSHA256 == "" {
		t.Error("ArchiveSHA256 is empty; the child checks the digest before it unpacks")
	}
}

// The property the child depends on: the bytes handed to the store are the
// bytes the delivery already hashed, not a repack of the same tree.
func TestTheBytesThatReachTheStoreAreTheBytesThatWereSigned(t *testing.T) {
	publisher, published, sink, _, b := publishableTree(t)

	if _, err := publisher.Publish(t.Context(), b); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	uploaded, ok := sink.held[archiveName(b)]
	if !ok {
		t.Fatalf("the store was never asked to hold %s; it was asked for %v", archiveName(b), sink.asked)
	}
	sum := sha256.Sum256(uploaded)
	if got := hex.EncodeToString(sum[:]); got != digestOfArchive(t, published.local.Dir(), archiveName(b)) {
		t.Errorf("the store was handed bytes hashing to %s, and the signed archive hashes to %s; "+
			"a child compares the digest before it unpacks, so these can never be reconciled",
			got, digestOfArchive(t, published.local.Dir(), archiveName(b)))
	}
}

// Publishing twice is what a re-run does, and it has to produce the same bytes
// both times. A publisher that repacked would satisfy every other assertion
// in this file and hand a child a digest that matches nothing.
func TestPublishingTwiceProducesTheSameBytesBothTimes(t *testing.T) {
	publisher, _, sink, store, b := publishableTree(t)

	_, _ = publisher.Publish(t.Context(), b)
	first := append([]byte(nil), sink.held[archiveName(b)]...)
	catchUp(sink, store)
	if _, err := publisher.Publish(t.Context(), b); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	second := sink.held[archiveName(b)]
	if len(first) != len(second) {
		t.Fatalf("two publishes of one tree produced %d and %d bytes", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("byte %d differs between two publishes of the same tree; the archive is "+
				"being rebuilt rather than read back", i)
		}
	}
}

// A store that transforms what it stores is serving something other than what
// this root signed. That is a conflict, not a retry, and no URL may be named.
func TestAStoreThatChangesTheBytesIsAConflictAndNotAURL(t *testing.T) {
	publisher, _, sink, _, b := publishableTree(t)
	sink.transform = func(in []byte) []byte { return append(append([]byte(nil), in...), 'x') }

	src, err := publisher.Publish(t.Context(), b)
	if !errors.Is(err, ErrPublishedMismatch) {
		t.Fatalf("a store that rewrote the archive gave %v, want ErrPublishedMismatch", err)
	}
	if src.URL != "" {
		t.Errorf("a URL was named (%q) for a tree this root cannot vouch for", src.URL)
	}
}

// Nothing to publish is a distinct answer from "publish failed". A publish is
// the second half of a delivery, and a caller that treated it as the first
// would be shipping a tree nobody signed.
func TestPublishingATreeThatWasNeverPackedIsRefused(t *testing.T) {
	publisher, _, sink, _, _ := publishableTree(t)
	id, _ := federation.NewClusterID("prod-cn-north")
	missing := federation.Bundle{ClusterID: id, Version: 12}

	_, err := publisher.Publish(t.Context(), missing)
	if err == nil {
		t.Fatal("a tree that was never packed was published")
	}
	// The error alone is not enough: an unreadable directory and an unpacked
	// tree both fail to read, and the two call for different next steps. The
	// refusal has to name the one that actually happened.
	if !strings.Contains(err.Error(), "has not been packed yet") {
		t.Errorf("refused with %q, which does not say that the tree was never packed", err)
	}
	if len(sink.asked) != 0 {
		t.Errorf("the store was asked to hold %v for a tree that does not exist", sink.asked)
	}
}

// A publisher that accepts no distributor or no sink would name a URL it
// cannot write and report success from an upload that never happened.
func TestAPublisherRefusesToBeBuiltWithoutBothHalves(t *testing.T) {
	local, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	published, err := NewPublishedDistributor(local, "https://artifacts.example.com/policies",
		&fakeStore{digests: map[string]string{}})
	if err != nil {
		t.Fatalf("NewPublishedDistributor: %v", err)
	}
	if _, err := NewPublishedPublisher(nil, newRecordingSink()); err == nil {
		t.Error("a publisher with no store was built; it has nowhere to publish to")
	}
	if _, err := NewPublishedPublisher(published, nil); err == nil {
		t.Error("a publisher with no sink was built; it would report an upload that never happened")
	}
}
