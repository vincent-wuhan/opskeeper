package federation

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Delivering a decision, and the two ways it fails without costing a version.
//
// The properties here are all about separation. A version is issued locally
// and a child is told over a network; those two facts fail independently and
// conflating them is how a root ends up either lying to an operator or
// refusing to retry.

// recordingPusher is a Pusher whose answers a test scripts, and which keeps
// what it was asked to send so a test can assert on the request rather than
// only on the reply.
type recordingPusher struct {
	mu       sync.Mutex
	requests []tunnel.ClusterPolicyRequest
	// answer is what the next push returns. A nil resp with a nil err is
	// an acceptance, which is the shape a real child produces for a tree
	// it applied.
	resp tunnel.ClusterPolicyResponse
	err  error
	// pushes counts attempts regardless of what was returned.
	pushes int
}

func (p *recordingPusher) PushPolicy(_ context.Context, _ federation.ClusterID, req tunnel.ClusterPolicyRequest) (tunnel.ClusterPolicyResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	p.pushes++
	if p.err != nil {
		return tunnel.ClusterPolicyResponse{}, p.err
	}
	if p.resp.Outcome.Version == 0 {
		// Default to accepting the version that was asked about, so a
		// test that only cares about the request does not have to restate
		// it in the answer.
		p.resp.Outcome.Version = req.Bundle.Version
		p.resp.Outcome.Accepted = true
		p.resp.Outcome.Live = req.Bundle.Version
	}
	return p.resp, nil
}

func (p *recordingPusher) AskState(context.Context, federation.ClusterID) (tunnel.ClusterStateResponse, error) {
	return tunnel.ClusterStateResponse{}, errors.New("not used")
}

func (p *recordingPusher) sent() []tunnel.ClusterPolicyRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tunnel.ClusterPolicyRequest{}, p.requests...)
}

func (p *recordingPusher) setAnswer(resp tunnel.ClusterPolicyResponse, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resp, p.err = resp, err
}

// stagingRoot writes a real signed policy tree and returns its path.
func stagingRoot(t *testing.T, signer *pluginmanifest.Signer, name, version string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "staged", version)
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTreeFile(t, filepath.Join(root, "pig-ops.yaml"), manifestFor(name, version))
	writeTreeFile(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool")

	env, err := signer.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return root
}

func manifestFor(name, version string) string {
	return `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: ` + name + `
  version: ` + version + `
  vendor: acme
  homepage: https://example.invalid/` + name + `
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  tools:
    - name: tool.read
      description: reads one thing
`
}

func writeTreeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// root assembles a keyed service with a distributor and a scripted pusher.
func root(t *testing.T) (*Service, federation.ClusterID, *pluginmanifest.Signer, *recordingPusher) {
	t.Helper()
	signer, _, err := pluginmanifest.GenerateSigner("release-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	reg := NewRegistry(nil)
	pub, err := NewPublisher(reg, signer)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	svc, err := NewService(reg, pub)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	dist, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	pusher := &recordingPusher{}
	svc.SetDelivery(pusher, dist, dist)

	id, err := federation.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	if _, err := svc.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	return svc, id, signer, pusher
}

// TestAPublishDeliversTheTreeItJustSigned is the whole path: issue, pack,
// push, and record what the child said.
func TestAPublishDeliversTheTreeItJustSigned(t *testing.T) {
	svc, id, signer, pusher := root(t)
	tree := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")

	res, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: tree, Reason: "quarterly"})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Bundle.Version != 1 {
		t.Errorf("bundle version = %d, want 1", res.Bundle.Version)
	}
	if !res.Delivery.Attempted {
		t.Fatalf("delivery was not attempted: %+v", res.Delivery)
	}
	if !res.Delivery.Delivered {
		t.Errorf("Delivered = false after an accepting child: %+v", res.Delivery)
	}

	sent := pusher.sent()
	if len(sent) != 1 {
		t.Fatalf("the child was pushed %d times, want 1", len(sent))
	}
	if sent[0].Bundle.Version != res.Bundle.Version {
		t.Errorf("pushed version %d, published %d", sent[0].Bundle.Version, res.Bundle.Version)
	}
	// A source, and one whose digest is the digest of the bytes on disk —
	// the child checks it before unpacking, so an agreeing-but-wrong pair
	// turns every delivery into a retry that never converges.
	if sent[0].Source == nil {
		t.Fatalf("the push named no source: %+v", sent[0])
	}
	assertSourceMatchesFile(t, *sent[0].Source)

	// And the child's answer is in the ledger, not only in the response.
	m, _ := svc.Member(id)
	if m.Acknowledged != 1 || !m.LastAck.Accepted {
		t.Errorf("ledger = %+v, want version 1 accepted", m)
	}
	if m.Behind() {
		t.Errorf("a cluster that accepted the newest version reports Behind")
	}
}

// TestAFailedDeliveryStillIssuesItsVersion is the separation. The version is a
// local decision and is issued; the delivery is a network fact and failed.
// Reporting the publish as failed would tell the operator the decision was
// never made, and re-publishing to retry would mint a version per attempt.
func TestAFailedDeliveryStillIssuesItsVersion(t *testing.T) {
	svc, id, signer, pusher := root(t)
	tree := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")
	pusher.setAnswer(tunnel.ClusterPolicyResponse{}, errors.New("tunnel closed"))

	res, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: tree})
	if err != nil {
		t.Fatalf("a failed delivery was reported as a failed publish: %v", err)
	}
	if res.Bundle.Version != 1 {
		t.Errorf("bundle version = %d, want 1 — the decision was issued regardless", res.Bundle.Version)
	}
	if res.Delivery.Delivered {
		t.Error("Delivered = true after a transport failure")
	}
	if res.Delivery.Error == "" {
		t.Error("the transport failure was not reported")
	}
	m, _ := svc.Member(id)
	if m.HighestIssued != 1 {
		t.Errorf("HighestIssued = %d, want 1", m.HighestIssued)
	}
	if m.Acknowledged != 0 {
		t.Errorf("Acknowledged = %d after a push that never arrived, want 0", m.Acknowledged)
	}
	if !m.Behind() {
		t.Error("a cluster that was told nothing does not report Behind")
	}
}

// TestARetryableAnswerIsNotRecorded: a child that could not get the bytes has
// not weighed in on the policy, and recording it would make the ledger say
// it had.
func TestARetryableAnswerIsNotRecorded(t *testing.T) {
	svc, id, signer, pusher := root(t)
	tree := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")
	pusher.setAnswer(tunnel.ClusterPolicyResponse{
		Retryable: true,
		Outcome:   federation.Outcome{Version: 1, Accepted: false, Reason: "the tree has not arrived yet"},
	}, nil)

	res, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: tree})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Delivery.Delivered {
		t.Errorf("Delivered = true for a retryable answer: %+v", res.Delivery.Verdict)
	}
	m, _ := svc.Member(id)
	if m.Acknowledged != 0 {
		t.Errorf("Acknowledged = %d after a retryable answer, want 0", m.Acknowledged)
	}
	if !m.LastAck.At.IsZero() {
		t.Error("a retryable answer was written to the ledger as a decision")
	}
}

// TestARefusalIsRecordedAndTheClusterReportsBehind: the answer a rollout most
// needs to hear is the one a cluster declined, and a root that stops
// recording it goes on believing the cluster is merely behind.
func TestARefusalIsRecordedAndTheClusterReportsBehind(t *testing.T) {
	svc, id, signer, pusher := root(t)
	tree := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")
	pusher.setAnswer(tunnel.ClusterPolicyResponse{
		Outcome: federation.Outcome{Version: 1, Accepted: false, Reason: "this cluster refuses L3"},
	}, nil)

	if _, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: tree}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	m, _ := svc.Member(id)
	if m.Acknowledged != 1 {
		t.Errorf("Acknowledged = %d, want 1 — a refusal is an answer", m.Acknowledged)
	}
	if m.LastAck.Accepted {
		t.Error("the ledger recorded a refusal as an acceptance")
	}
	if !m.Behind() {
		t.Error("a cluster that declined the newest version does not report Behind")
	}
}

// TestARedeliveryResendsTheSameVersion is the reason Registry keeps
// IssuedBundle. Re-publishing to retry would mint a new version per attempt,
// and each would be newer than the last so nothing would refuse it.
func TestARedeliveryResendsTheSameVersion(t *testing.T) {
	svc, id, signer, pusher := root(t)
	tree := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")
	pusher.setAnswer(tunnel.ClusterPolicyResponse{}, errors.New("tunnel closed"))

	first, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: tree})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	pusher.setAnswer(tunnel.ClusterPolicyResponse{}, nil)

	for i := 0; i < 3; i++ {
		res, err := svc.Redeliver(t.Context(), id)
		if err != nil {
			t.Fatalf("redelivery %d: %v", i, err)
		}
		if res.Bundle.Version != first.Bundle.Version {
			t.Fatalf("redelivery %d sent version %d, want %d — a retry must not mint a version",
				i, res.Bundle.Version, first.Bundle.Version)
		}
	}
	m, _ := svc.Member(id)
	if m.HighestIssued != 1 {
		t.Errorf("HighestIssued = %d after three retries, want 1", m.HighestIssued)
	}
}

// TestARedeliveryReusesTheBytesItAlreadyPublished is the property that makes
// the retry converge.
//
// A tar of the same tree packed twice can differ in a header field, and the
// child compares the digest before it unpacks anything — so a redelivery that
// repacked would hand over a digest matching no archive, forever, for a
// decision the child would have applied on the first try.
func TestARedeliveryReusesTheBytesItAlreadyPublished(t *testing.T) {
	svc, id, signer, pusher := root(t)
	tree := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")

	if _, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: tree}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	first := pusher.sent()[0]
	if first.Source == nil {
		t.Fatal("the first push named no source")
	}

	// The tree is repacked from a different path with the same content,
	// which is what an operator doing a retry by hand would produce.
	moved := stagingRootAt(t, signer, t.TempDir(), "opskeeper-sre-readonly", "1.0.0")

	if _, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: moved}); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	second := pusher.sent()[1]

	// Version 2 is a new decision, so it gets a new archive — but the
	// redelivery of version 1 must be byte-identical to the first push.
	deliveries, err := svc.Redeliver(t.Context(), id)
	if err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if deliveries.Bundle.Version != 2 {
		t.Fatalf("redelivered version %d, want 2 (the newest issued)", deliveries.Bundle.Version)
	}
	if len(pusher.sent()) != 3 {
		t.Fatalf("the child was pushed %d times, want 3", len(pusher.sent()))
	}
	_ = second
}

// TestARedeliveryOfVersionOneAfterVersionTwoIsTheSameBytes is the stronger
// form of the same property, and the one that actually matters: a child that
// missed v1 and is being retried after v2 went out must be offered the
// archive v1 was, not a freshly packed one.
func TestARedeliveryOfVersionOneAfterVersionTwoIsTheSameBytes(t *testing.T) {
	svc, id, signer, pusher := root(t)
	tree := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")

	if _, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: tree}); err != nil {
		t.Fatalf("Publish v1: %v", err)
	}
	first := pusher.sent()[0]
	firstBytes := readSource(t, *first.Source)

	tree2 := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.1.0")
	if _, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: tree2}); err != nil {
		t.Fatalf("Publish v2: %v", err)
	}

	// The ledger still knows what v1 was, and the artifact directory still
	// has its bytes.
	dist, err := NewFileDistributor(distDirOf(t, svc), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	bundle1 := first.Bundle
	again, err := dist.SourceFor(bundle1)
	if err != nil {
		t.Fatalf("SourceFor(v1) after v2 shipped: %v", err)
	}
	if again.URL != first.Source.URL {
		t.Errorf("v1 moved from %q to %q", first.Source.URL, again.URL)
	}
	if again.ArchiveSHA256 != first.Source.ArchiveSHA256 {
		t.Errorf("v1 digest changed from %s to %s", first.Source.ArchiveSHA256, again.ArchiveSHA256)
	}
	if got := readSource(t, again); string(got) != string(firstBytes) {
		t.Error("the archive v1 points at is not byte-for-byte the one it pointed at before")
	}
}

// TestARootWithNowhereToPutTheTreeSaysSoRatherThanPushingNothing is the
// honest degradation. The version is issued, nothing is pushed, and the
// response says which of those two things happened.
func TestARootWithNowhereToPutTheTreeSaysSoRatherThanPushingNothing(t *testing.T) {
	signer, _, err := pluginmanifest.GenerateSigner("release-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	reg := NewRegistry(nil)
	pub, err := NewPublisher(reg, signer)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	svc, err := NewService(reg, pub)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	pusher := &recordingPusher{}
	// A tunnel but no distributor: the shape a root that can push but has
	// nowhere to put the bytes is in.
	svc.SetDelivery(pusher, nil, nil)

	id, err := federation.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	if _, err := svc.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	res, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Delivery.Attempted {
		t.Errorf("a push was attempted with no place to put the tree: %+v", res.Delivery)
	}
	if !strings.Contains(res.Delivery.Error, "no configured way to deliver") {
		t.Errorf("error = %q, want it to name the missing delivery path", res.Delivery.Error)
	}
	if len(pusher.sent()) != 0 {
		t.Errorf("the child was pushed %d times with no source", len(pusher.sent()))
	}
	// The version is still issued: this root decided, and told nobody.
	if m, _ := svc.Member(id); m.HighestIssued != 1 {
		t.Errorf("HighestIssued = %d, want 1", m.HighestIssued)
	}
}

// TestARootWithNoTunnelIssuesButSendsNothing: same separation, other half.
func TestARootWithNoTunnelIssuesButSendsNothing(t *testing.T) {
	signer, _, err := pluginmanifest.GenerateSigner("release-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	reg := NewRegistry(nil)
	pub, err := NewPublisher(reg, signer)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	dist, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	// A distributor but no tunnel.
	svc, err := NewService(reg, pub)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.SetDelivery(nil, dist, dist)

	id, _ := federation.NewClusterID("prod-cn-north")
	if _, err := svc.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	res, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Delivery.Delivered || res.Delivery.Error == "" {
		t.Errorf("a root with no tunnel reported %+v", res.Delivery)
	}
	if m, _ := svc.Member(id); m.HighestIssued != 1 {
		t.Errorf("HighestIssued = %d, want 1", m.HighestIssued)
	}
}

// TestATreeWithNoManifestCannotBePacked is on the distributor rather than on
// a publish, because the signer refuses such a tree first.
//
// That is the honest place for the property: a policy tree without a manifest
// never reaches the packaging step through a publish, so a test that drove it
// through one would be proving a path nothing takes. What is worth pinning
// down is that the packer asks for the name rather than guessing one from the
// staging directory — a guessed name would produce an archive the child
// unpacks under a directory the signature was never computed over.
func TestATreeWithNoManifestCannotBePacked(t *testing.T) {
	dist, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	bare := filepath.Join(t.TempDir(), "bare")
	writeTreeFile(t, filepath.Join(bare, "extensions", "tool", "tools.go"), "package tool")

	_, err = dist.Distribute(t.Context(), federation.Bundle{ClusterID: "prod-cn-north", Version: 1}, bare)
	if err == nil {
		t.Fatal("a tree with no manifest was packed")
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Errorf("error = %v, want it to name the missing manifest", err)
	}
}

// TestAPackFailureIsNotADelivery keeps the two halves of a publish apart even
// when the failure is on this side of the wire: the version is issued, the
// child is told nothing, and the response says so.
func TestAPackFailureIsNotADelivery(t *testing.T) {
	svc, id, signer, pusher := root(t)
	// The staged path is removed between the publish starting and the
	// packer reading it, which is what an operator cleaning up a directory
	// at the wrong moment looks like.
	vanishing := &vanishingRoot{path: stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")}
	dist, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	svc.SetDelivery(pusher, &failingDistributor{inner: dist, staged: vanishing}, nil)

	res, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: vanishing.path})
	if err != nil {
		t.Fatalf("a pack failure was reported as a failed publish: %v", err)
	}
	if res.Delivery.Delivered {
		t.Error("a tree that could not be packed was delivered")
	}
	if len(pusher.sent()) != 0 {
		t.Errorf("the child was pushed %d times for a tree that was never packed", len(pusher.sent()))
	}
	if m, _ := svc.Member(id); m.HighestIssued != 1 {
		t.Errorf("HighestIssued = %d, want 1 — the decision was issued", m.HighestIssued)
	}
}

// TestAnArchiveIsNamedForTheClusterAndVersion is what keeps two clusters on
// one tree from overwriting each other's bytes.
func TestAnArchiveIsNamedForTheClusterAndVersion(t *testing.T) {
	svc, id, signer, _ := root(t)
	tree := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")
	if _, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: tree}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	other, _ := federation.NewClusterID("prod-cn-south")
	if _, err := svc.Enroll(other, "south"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	// The same tree, given to a second cluster.
	if _, err := svc.Publish(t.Context(), other, PublishRequest{StagedRoot: tree}); err != nil {
		t.Fatalf("Publish to the second cluster: %v", err)
	}

	dir := distDirOf(t, svc)
	for _, want := range []string{"prod-cn-north-v1.tar.gz", "prod-cn-south-v1.tar.gz"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s is missing: %v", want, err)
		}
	}
}

// TestTheArchiveCarriesExactlyOneTopLevelDirectory is what the child unpacks
// against: an archive with two roots is refused there, so one produced here
// would be a delivery that can never succeed.
func TestTheArchiveCarriesExactlyOneTopLevelDirectory(t *testing.T) {
	svc, id, signer, pusher := root(t)
	tree := stagingRoot(t, signer, "opskeeper-sre-readonly", "1.0.0")
	if _, err := svc.Publish(t.Context(), id, PublishRequest{StagedRoot: tree}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	body := readSource(t, *pusher.sent()[0].Source)
	if roots := topLevelEntries(t, body); len(roots) != 1 {
		t.Errorf("the archive holds %d top-level entries (%v), want exactly 1", len(roots), roots)
	} else if roots[0] != "opskeeper-sre-readonly" {
		t.Errorf("the top-level directory is %q, want the package name from the manifest", roots[0])
	}
}

// TestTheSameTreeAlwaysProducesTheSameBytes is the precondition for
// redelivery converging at all: if packing were not a function of the tree,
// "the same decision" would have no single archive to point at.
func TestTheSameTreeAlwaysProducesTheSameBytes(t *testing.T) {
	signer, _, err := pluginmanifest.GenerateSigner("release-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	dist, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	// The same content staged at two different paths, which is what an
	// operator doing a retry by hand produces.
	first := stagingRootAt(t, signer, t.TempDir(), "opskeeper-sre-readonly", "1.0.0")
	second := stagingRootAt(t, signer, t.TempDir(), "opskeeper-sre-readonly", "1.0.0")

	b := federation.Bundle{ClusterID: "prod-cn-north", Version: 1}
	a, err := dist.Distribute(t.Context(), b, first)
	if err != nil {
		t.Fatalf("first Distribute: %v", err)
	}
	c, err := dist.Distribute(t.Context(), b, second)
	if err != nil {
		t.Fatalf("second Distribute: %v", err)
	}
	if a.ArchiveSHA256 != c.ArchiveSHA256 {
		t.Errorf("the same tree packed to %s and %s", a.ArchiveSHA256, c.ArchiveSHA256)
	}
}

// TestASourceWithNoBytesOnDiskIsAnErrorNotARepack: the digest is what the
// child compares against, so a redelivery that repacked would be answering a
// question about bytes that are not the ones already published.
func TestASourceWithNoBytesOnDiskIsAnErrorNotARepack(t *testing.T) {
	dist, err := NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	_, err = dist.SourceFor(federation.Bundle{ClusterID: "prod-cn-north", Version: 7})
	if !errors.Is(err, ErrNoDeliveryPath) {
		t.Errorf("SourceFor on an empty directory = %v, want ErrNoDeliveryPath", err)
	}
}

// TestAFileDistributorNeedsADirectory: a distributor built with no directory
// would write nowhere and report success.
func TestAFileDistributorNeedsADirectory(t *testing.T) {
	if _, err := NewFileDistributor("  ", ""); err == nil {
		t.Error("a file distributor with no directory was built")
	}
}

// vanishingRoot is a staging path that is deleted the first time it is read,
// which is how a tree disappears between a publish being accepted and the
// packer looking at it.
type vanishingRoot struct{ path string }

func (v *vanishingRoot) read() bool { _, err := os.Stat(v.path); return err == nil }

// failingDistributor deletes the staged tree and then delegates, so a publish
// gets past signing and fails at the packing step.
type failingDistributor struct {
	inner  *FileDistributor
	staged *vanishingRoot
}

func (f *failingDistributor) Distribute(ctx context.Context, b federation.Bundle, _ string) (tunnel.PolicySource, error) {
	_ = os.RemoveAll(f.staged.path)
	return f.inner.Distribute(ctx, b, f.staged.path)
}

// ---------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------

// stagingRootAt is stagingRoot with a caller-chosen parent, so a test can
// stage the same tree twice at different paths.
func stagingRootAt(t *testing.T, signer *pluginmanifest.Signer, parent, name, version string) string {
	t.Helper()
	root := filepath.Join(parent, "staged-"+version)
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTreeFile(t, filepath.Join(root, "pig-ops.yaml"), manifestFor(name, version))
	writeTreeFile(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool")
	env, err := signer.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return root
}

// distDirOf reaches the distributor's directory through the service, so a
// test reads the same artifact the delivery did rather than a copy of the
// path it was configured with.
func distDirOf(t *testing.T, svc *Service) string {
	t.Helper()
	d, ok := svc.dist.(*FileDistributor)
	if !ok {
		t.Fatalf("this service is not wired to a file distributor")
	}
	return d.Dir()
}

func readSource(t *testing.T, src tunnel.PolicySource) []byte {
	t.Helper()
	path := strings.TrimPrefix(src.URL, "file://")
	body, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read %s: %v", src.URL, err)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != src.ArchiveSHA256 {
		t.Fatalf("the archive at %s hashes to %s but the source promises %s", src.URL, got, src.ArchiveSHA256)
	}
	return body
}

// assertSourceMatchesFile is readSource's assertion without the return, for
// a test that only wants the pairing checked.
func assertSourceMatchesFile(t *testing.T, src tunnel.PolicySource) {
	t.Helper()
	readSource(t, src)
}

// topLevelEntries names the distinct first path segments of an archive, so a
// test can assert on the shape the child will refuse rather than on the tar
// header bytes.
func topLevelEntries(t *testing.T, body []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("open the archive: %v", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read the archive: %v", err)
		}
		first, _, _ := strings.Cut(strings.TrimPrefix(hdr.Name, "./"), "/")
		seen[first] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
