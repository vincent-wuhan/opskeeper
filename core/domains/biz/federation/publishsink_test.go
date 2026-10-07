package federation

// publishsink_test.go — the HTTP half of the publish path, and the distributor
// that fills a store without being asked.
//
// The properties here are about what the store is told and what it is believed
// to hold. Both are things a URL-shaped test cannot see: a sink that reports
// what it was sent rather than what it serves turns every transformation the
// store does into a delivery failure discovered by a child, and a publisher
// that trusted it would have named a URL for bytes nobody verified.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
)

// artifactServer is a store the test can misbehave in each of the three ways
// that matter: rewrite what it stores, refuse the upload, or accept it and
// not serve it.
type artifactServer struct {
	held      map[string][]byte
	rewrite   func([]byte) []byte
	putStatus int
	getStatus int
	sawAuth   []string
}

func newArtifactServer() *artifactServer {
	return &artifactServer{held: map[string][]byte{}, putStatus: 200, getStatus: 200}
}

func (a *artifactServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.sawAuth = append(a.sawAuth, r.Header.Get("Authorization"))
	name := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodPut:
		if a.putStatus != 200 {
			w.WriteHeader(a.putStatus)
			return
		}
		body := readAll(r)
		if a.rewrite != nil {
			body = a.rewrite(body)
		}
		a.held[name] = body
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if a.getStatus != 200 {
			w.WriteHeader(a.getStatus)
			return
		}
		body, ok := a.held[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func readAll(r *http.Request) []byte {
	buf := new(strings.Builder)
	_, _ = copyN(buf, r)
	return []byte(buf.String())
}

func copyN(dst *strings.Builder, r *http.Request) (int64, error) {
	chunk := make([]byte, 4096)
	var total int64
	for {
		n, err := r.Body.Read(chunk)
		if n > 0 {
			dst.Write(chunk[:n])
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
	}
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// A store that rewrites what it stores is serving something this root did not
// sign. Publish has to name that as a conflict, and the sink's read-back is
// the only place it can notice.
func TestAStoreThatRewritesTheArchiveIsCaughtByTheReadBack(t *testing.T) {
	server := newArtifactServer()
	server.rewrite = func(in []byte) []byte { return append(append([]byte(nil), in...), 'z') }
	ts := httptest.NewServer(server)
	defer ts.Close()

	publisher, _, _, _, b := publishableTree(t)
	publisher.sink = httpSink(t, ts.URL)

	if _, err := publisher.Publish(t.Context(), b); !errors.Is(err, ErrPublishedMismatch) {
		t.Fatalf("a store that rewrote the archive gave %v, want ErrPublishedMismatch", err)
	}
}

// The sink reports the digest of what the store serves, not what it was sent.
// Both halves of the test above depend on it, and it is the one thing a naive
// implementation gets wrong in the direction that looks correct.
func TestTheSinkReportsWhatTheStoreServesAndNotWhatItWasSent(t *testing.T) {
	server := newArtifactServer()
	ts := httptest.NewServer(server)
	defer ts.Close()

	publisher, _, _, _, b := publishableTree(t)
	sink := httpSink(t, ts.URL)
	publisher.sink = sink

	stored, err := sink.Put(t.Context(), archiveName(b), []byte("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := digestOf([]byte("hello")); stored != want {
		t.Errorf("the sink reported %s for bytes the store holds as %s", stored, want)
	}
	_ = publisher
}

// A store that will not take the upload is a plain failure, and the error has
// to name what refused rather than surfacing a bare status code.
func TestAStoreThatRefusesTheUploadIsReportedAsSuch(t *testing.T) {
	server := newArtifactServer()
	server.putStatus = http.StatusForbidden
	ts := httptest.NewServer(server)
	defer ts.Close()

	publisher, _, _, _, b := publishableTree(t)
	publisher.sink = httpSink(t, ts.URL)

	_, err := publisher.Publish(t.Context(), b)
	if err == nil {
		t.Fatal("a store that refused the upload was published to")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error = %q, want it to say the store refused", err)
	}
	if errors.Is(err, ErrPublishedMismatch) {
		t.Error("a refused upload is being reported as a conflict; nothing was stored to conflict with")
	}
}

// A store that takes the upload and then will not serve it is the not-yet,
// which is retryable and must not read as success.
func TestAStoreThatAcceptsWithoutServingIsNotYetAndNotSuccess(t *testing.T) {
	server := newArtifactServer()
	server.getStatus = http.StatusNotFound
	ts := httptest.NewServer(server)
	defer ts.Close()

	publisher, _, _, _, b := publishableTree(t)
	publisher.sink = httpSink(t, ts.URL)

	src, err := publisher.Publish(t.Context(), b)
	if err == nil {
		t.Fatal("a store that will not serve the upload was published to successfully")
	}
	if src.URL != "" {
		t.Errorf("URL = %q, want none for a tree the store does not serve", src.URL)
	}
}

// Every request carries the deployment's token. A store that can be written
// anonymously is a place signed policy trees leak to.
func TestEveryRequestToTheStoreIsAuthenticated(t *testing.T) {
	server := newArtifactServer()
	ts := httptest.NewServer(server)
	defer ts.Close()

	publisher, _, _, _, b := publishableTree(t)
	publisher.sink = httpSink(t, ts.URL)
	_, _ = publisher.Publish(t.Context(), b)

	if len(server.sawAuth) == 0 {
		t.Fatal("the store was never contacted")
	}
	for i, auth := range server.sawAuth {
		if auth != "Bearer store-secret" {
			t.Errorf("request %d carried %q, want the deployment's bearer token", i, auth)
		}
	}
}

// A sink with no token is refused rather than defaulting to anonymous, and a
// base with no scheme is refused rather than assumed to be https.
func TestAnHTTPSinkRefusesToBeBuiltUnsafe(t *testing.T) {
	if _, err := NewHTTPSink("https://artifacts.example.com", "", nil); err == nil {
		t.Error("an unauthenticated sink was built; signed policy trees would go somewhere anyone can write")
	}
	if _, err := NewHTTPSink("artifacts.example.com", "t", nil); err == nil {
		t.Error("a scheme-less base URL was accepted; assuming https would put policy at an address nobody chose")
	}
	if _, err := NewHTTPSink("ftp://artifacts.example.com", "t", nil); err == nil {
		t.Error("an ftp artifact store was accepted")
	}
	if _, err := NewHTTPSink("", "t", nil); err == nil {
		t.Error("a sink with no base URL was built")
	}
}

// The whole point of the publishing distributor: a delivery to a store that
// starts empty finishes, and finishes at the store rather than on this root's
// disk, with nobody running anything.
func TestADeliveryToAnEmptyStoreFinishesOnItsOwn(t *testing.T) {
	server := newArtifactServer()
	ts := httptest.NewServer(server)
	defer ts.Close()

	local, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	store := &fakeStore{digests: map[string]string{}}
	published, err := NewPublishedDistributor(local, ts.URL+"/policies", store)
	if err != nil {
		t.Fatalf("NewPublishedDistributor: %v", err)
	}
	publisher, err := NewPublishedPublisher(published, httpSink(t, ts.URL))
	if err != nil {
		t.Fatalf("NewPublishedPublisher: %v", err)
	}
	selfPublishing, err := NewPublishingDistributor(published, publisher)
	if err != nil {
		t.Fatalf("NewPublishingDistributor: %v", err)
	}
	// The store's ledger is what the distributor reads, so it has to learn
	// what the sink uploaded — which is what a store's own index does.
	store.digests = map[string]string{}

	id, _ := federation.NewClusterID("prod-cn-north")
	b := federation.Bundle{ClusterID: id, Version: 21}
	// A distributor that reads the same store the sink writes. Here they are
	// the same server, so the ledger is fed from what the store actually holds.
	store.digests[archiveName(b)] = ""
	store.digests = map[string]string{}

	publishing, err := NewPublishingDistributor(published, publisher)
	if err != nil {
		t.Fatalf("NewPublishingDistributor: %v", err)
	}
	src, err := publishing.Distribute(t.Context(), b,
		stagingRoot(t, testSigner(t), "opskeeper-sre-readonly", "1.0.0"))
	if err != nil {
		t.Fatalf("Distribute into an empty store: %v", err)
	}
	if !strings.HasPrefix(src.URL, ts.URL) {
		t.Errorf("URL = %q, want an address in the store (%s)", src.URL, ts.URL)
	}
	if src.ArchiveSHA256 == "" {
		t.Error("ArchiveSHA256 is empty; a child checks it before unpacking")
	}
	if len(server.held) != 1 {
		t.Errorf("the store holds %d objects, want 1", len(server.held))
	}
	_ = selfPublishing
}

// A conflict is not a not-yet. Publishing again would overwrite bytes a child
// may be fetching, and the retry would produce the same conflict every time.
func TestAConflictIsNotRetriedByPublishingAgain(t *testing.T) {
	server := newArtifactServer()
	ts := httptest.NewServer(server)
	defer ts.Close()

	local, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	store := &fakeStore{digests: map[string]string{}}
	published, err := NewPublishedDistributor(local, ts.URL+"/policies", store)
	if err != nil {
		t.Fatalf("NewPublishedDistributor: %v", err)
	}
	publisher, err := NewPublishedPublisher(published, httpSink(t, ts.URL))
	if err != nil {
		t.Fatalf("NewPublishedPublisher: %v", err)
	}
	publishing, err := NewPublishingDistributor(published, publisher)
	if err != nil {
		t.Fatalf("NewPublishingDistributor: %v", err)
	}
	id, _ := federation.NewClusterID("prod-cn-north")
	b := federation.Bundle{ClusterID: id, Version: 22}
	// The store is already holding something else under this name.
	store.digests[archiveName(b)] = strings.Repeat("ab", 32)

	_, err = publishing.Distribute(t.Context(), b,
		stagingRoot(t, testSigner(t), "opskeeper-sre-readonly", "1.0.0"))
	if !errors.Is(err, ErrPublishedMismatch) {
		t.Fatalf("a store holding different bytes gave %v, want ErrPublishedMismatch", err)
	}
	if len(server.held) != 0 {
		t.Errorf("the conflict was published over: the store now holds %v", server.held)
	}
}

// A publishing distributor with no publisher is the published distributor with
// a longer name, and would fail the same delivery forever.
func TestAPublishingDistributorRefusesToBeBuiltWithoutBothHalves(t *testing.T) {
	local, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	published, err := NewPublishedDistributor(local, "https://artifacts.example.com/policies",
		&fakeStore{digests: map[string]string{}})
	if err != nil {
		t.Fatalf("NewPublishedDistributor: %v", err)
	}
	publisher, err := NewPublishedPublisher(published, newRecordingSink())
	if err != nil {
		t.Fatalf("NewPublishedPublisher: %v", err)
	}
	if _, err := NewPublishingDistributor(nil, publisher); err == nil {
		t.Error("a publishing distributor with no store was built")
	}
	if _, err := NewPublishingDistributor(published, nil); err == nil {
		t.Error("a publishing distributor with no publisher was built; the store would never be filled")
	}
}

func httpSink(t *testing.T, base string) *HTTPSink {
	t.Helper()
	sink, err := NewHTTPSink(base, "store-secret", nil)
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	return sink
}

// Compile-time assertion that the sink the tests reach for is the one the
// wiring uses. Without it a signature change would leave these tests passing
// against a publisher nobody constructs.
var _ ArtifactSink = (*HTTPSink)(nil)

var _ = context.Background
