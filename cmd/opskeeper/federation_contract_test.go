package main

// The cluster channel, end to end, with both real ends.
//
// The unit tests on either side drive their own half against a fake of the
// other, which is the right way to test a piece and the wrong way to test a
// seam. What only this file can see is the class of break where both halves
// are individually correct and the pair does not work: a field renamed on one
// side and not the other, an archive shaped the way the packer writes it and
// not the way the unpacker expects, a digest computed over different bytes on
// each side. None of those fail to compile, and every one of them refuses
// every rollout in production with a log that says "not staged" and no more.
//
// So this file assembles the shipping root and the shipping child — the same
// Publisher, FileDistributor, Links, Store, Receiver and Agent — and pushes a
// real policy across a real source.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
	edgefed "github.com/vincent-wuhan/opskeeper/core/domains/service/federationchild"
	managersvcfedlink "github.com/vincent-wuhan/opskeeper/core/domains/service/federationlink"
	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// loopbackCaller is the one hop between the two real halves: what the root
// pushed goes straight to the child's registered handler, through the same
// JSON the tunnel would carry.
//
// The marshalling is not decoration. A contract test passing structs by
// reference would not notice a json tag renamed on one side, and that break
// is invisible to both halves' own tests.
// It is the shape federationlink.Caller declares, which is the byte-level
// one the tunnel actually has — so the root marshals, this hands the bytes
// over, the child unmarshals, and a field renamed on one side of the wire is
// caught here rather than in a rollout.
type loopbackCaller struct{ handlers map[string]tunnel.Handler }

func (c *loopbackCaller) RegisterHandler(method string, h tunnel.Handler) {
	c.handlers[method] = h
}

func (c *loopbackCaller) Call(ctx context.Context, _ uint64, method string, body []byte) ([]byte, error) {
	h, ok := c.handlers[method]
	if !ok {
		return nil, errors.New("contract harness: no handler for " + method)
	}
	return h(ctx, tunnel.Session{}, method, body)
}

// childTunnelClient is the minimum tunnel.Client the child Agent needs. Its
// own Call is refused: these tests bind the two ends directly, and the
// enrolment token exchange is covered on its own.
type childTunnelClient struct{ loop *loopbackCaller }

func (c *childTunnelClient) RegisterHandler(method string, h tunnel.Handler) {
	c.loop.RegisterHandler(method, h)
}
func (c *childTunnelClient) Dial(context.Context) error                   { return nil }
func (c *childTunnelClient) Call(context.Context, string, any, any) error { return context.Canceled }
func (c *childTunnelClient) AcceptStream() (tunnel.StreamConn, error)     { return nil, nil }
func (c *childTunnelClient) OnReconnect(func())                           {}
func (c *childTunnelClient) Close() error                                 { return nil }

// bothEnds is a root and a child that can actually talk to each other.
type bothEnds struct {
	svc       *fedbiz.Service
	link      *managersvcfedlink.Links
	loop      *loopbackCaller
	child     *edgefed.Agent
	store     *edgefed.Store
	signer    *pluginmanifest.Signer
	dist      *fedbiz.FileDistributor
	artifacts string
	id        floorfed.ClusterID
}

func newBothEnds(t *testing.T) *bothEnds {
	t.Helper()

	signer, _, err := pluginmanifest.GenerateSigner("release-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}

	// --- the child: its own trust store, its own policy, its own disk ---
	trust := pluginmanifest.NewTrustStore()
	if err := trust.Trust(signer.KeyID(), signer.PublicKey()); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	pol := pluginmanifest.PolicyFor(domain.SafetyL2, domain.RadiusNamespace,
		domain.Scopes{domain.ScopeHostRead})
	pol.NodeVersion = "0.4.0"
	pol.PigVersion = "0.3.0"

	store, err := edgefed.NewStore(filepath.Join(t.TempDir(), "child-policy"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	recv, err := floorfed.NewReceiver(id, trust, pol, store)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	loop := &loopbackCaller{handlers: map[string]tunnel.Handler{}}
	child, err := edgefed.NewAgent(&childTunnelClient{loop: loop}, recv, store,
		floorfed.Cluster{ID: id, Name: "north", Version: "0.4.0", EdgeCount: 24},
		"provisioning-token", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	child.Register()

	// --- the root ---
	reg := fedbiz.NewRegistry(nil)
	pub, err := fedbiz.NewPublisher(reg, signer)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	svc, err := fedbiz.NewService(reg, pub)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	artifacts := t.TempDir()
	dist, err := fedbiz.NewFileDistributor(artifacts, "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	link, err := managersvcfedlink.NewLink(loop, clusterRegistrar{reg: reg},
		managersvcfedlink.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	// Enrolment first: the token a child presents is minted here, and a
	// hello that arrives before the root knows the cluster is refused —
	// which is the correct order and worth this file exercising rather than
	// working around.
	token, err := svc.Enroll(id, "north")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	hello, err := json.Marshal(tunnel.ClusterHelloRequest{
		Cluster:           floorfed.Cluster{ID: id, Name: "north", Version: "0.4.0", EdgeCount: 24},
		ProvisioningToken: token,
	})
	if err != nil {
		t.Fatalf("marshal the hello: %v", err)
	}
	raw, err := link.HandleHello(t.Context(), 1, hello)
	if err != nil {
		t.Fatalf("HandleHello: %v", err)
	}
	var accepted tunnel.ClusterHelloResponse
	if err := json.Unmarshal(raw, &accepted); err != nil {
		t.Fatalf("decode the hello answer: %v", err)
	}
	if !accepted.Accepted {
		t.Fatalf("the root refused its own child: %s", accepted.Reason)
	}
	svc.SetDelivery(link, dist, dist)

	return &bothEnds{svc: svc, link: link, loop: loop, child: child, store: store,
		signer: signer, dist: dist, artifacts: artifacts, id: id}
}

// setDelivery re-points the root at a different artifact view without
// rebuilding anything else, which is what an operator fixing a mount path
// does. The link is the same one, so the binding survives.
//
// It deliberately does not remember what it was given as the good
// distributor. The first version of this helper did, and a test that broke
// delivery and then repaired it was repairing it with the broken object —
// which showed up as a redelivery that still pointed at the unmounted path,
// and read like the retry logic being wrong rather than the test.
func (e *bothEnds) setDelivery(t *testing.T, dist *fedbiz.FileDistributor) {
	t.Helper()
	e.svc.SetDelivery(e.link, dist, dist)
}

// TestAPolicyCrossesTheChannelAndIsEnforced is the one test that would have
// caught every break this seam can have.
//
// A real tree, signed by the root's release key, packed by the root's
// distributor, named by a real source, pushed as a real wire message, fetched
// by the child over that source, unpacked, verified against the child's own
// trust store, admitted by the child's own policy, and promoted atomically.
// Every step is shipping code.
func TestAPolicyCrossesTheChannelAndIsEnforced(t *testing.T) {
	ends := newBothEnds(t)

	res, err := ends.svc.Publish(t.Context(), ends.id, fedbiz.PublishRequest{
		StagedRoot: ends.signTree(t, "opskeeper-sre-readonly", "1.0.0"),
		Reason:     "the first rollout",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Bundle.Version != 1 {
		t.Errorf("version = %d, want 1", res.Bundle.Version)
	}
	if !res.Delivery.Attempted {
		t.Fatalf("no delivery was attempted: %+v", res.Delivery)
	}
	if !res.Delivery.Delivered {
		t.Fatalf("the child did not take the policy: %+v (error %q)", res.Delivery.Verdict, res.Delivery.Error)
	}

	// The child is enforcing it, from a tree it fetched itself, through the
	// symlink its own store keeps.
	st := ends.child.HandleState(t.Context(), tunnel.ClusterStateRequest{Cluster: ends.id})
	if st.Policy != 1 || !st.Enforcing {
		t.Errorf("child state = %+v, want policy 1 enforcing", st)
	}
	live := ends.store.Live()
	if live == "" {
		t.Fatal("the child's store points at nothing")
	}
	// Both sides are resolved before comparing. Store.bound runs
	// EvalSymlinks, so on a system where the temp directory sits behind a
	// symlink (/var -> /private/var on macOS) the store's answer is the
	// resolved path and VersionsDir() is not — and comparing the raw
	// strings would fail for a reason that has nothing to do with what is
	// under test.
	if !strings.HasPrefix(live, resolved(t, ends.store.VersionsDir())) {
		t.Errorf("the live tree is %q, which is not inside %q", live, resolved(t, ends.store.VersionsDir()))
	}
	if _, err := os.Stat(filepath.Join(live, pluginmanifest.ManifestFile)); err != nil {
		t.Errorf("the promoted tree has no manifest: %v", err)
	}
	// The root's ledger agrees — from the child's own answer, not from the
	// fact that the push was sent.
	m, _ := ends.svc.Member(ends.id)
	if m.Acknowledged != 1 || !m.LastAck.Accepted {
		t.Errorf("the root's ledger = %+v, want version 1 accepted", m)
	}
	if m.Behind() {
		t.Error("a cluster enforcing the newest version reports Behind")
	}
}

// TestASecondPolicySupersedesTheFirst is the ordinary case two minutes
// later, and the one that breaks if an archive is named after the package
// rather than the version: two clusters on the same tree would overwrite
// each other's bytes.
func TestASecondPolicySupersedesTheFirst(t *testing.T) {
	ends := newBothEnds(t)
	if _, err := ends.svc.Publish(t.Context(), ends.id, fedbiz.PublishRequest{
		StagedRoot: ends.signTree(t, "opskeeper-sre-readonly", "1.0.0"),
	}); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	v1 := ends.store.Live()

	res, err := ends.svc.Publish(t.Context(), ends.id, fedbiz.PublishRequest{
		StagedRoot: ends.signTree(t, "opskeeper-sre-readonly", "1.1.0"),
	})
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if !res.Delivery.Delivered {
		t.Fatalf("the second policy was not taken: %+v", res.Delivery)
	}
	if res.Bundle.Version != 2 {
		t.Errorf("version = %d, want 2", res.Bundle.Version)
	}
	// The v1 tree is still on disk, which is what makes a rollback a
	// rename rather than another download.
	if _, err := os.Stat(v1); err != nil {
		t.Errorf("the superseded tree was deleted: %v", err)
	}
	if ends.store.Live() == v1 {
		t.Error("the live link still points at version 1")
	}
	for _, want := range []string{"prod-cn-north-v1.tar.gz", "prod-cn-north-v2.tar.gz"} {
		if _, err := os.Stat(filepath.Join(ends.artifacts, want)); err != nil {
			t.Errorf("%s is missing: %v", want, err)
		}
	}
}

// TestAPolicyTheChildCannotFetchIsARetryAndNotARefusal is the whole reason
// the two failures are separated, over the real seam.
//
// The root writes where the child does not read — the single likeliest way
// to get delivery wrong. The child must answer "not yet", the version must
// not be spent, and the redelivery after the path is fixed must converge
// without a new version.
func TestAPolicyTheChildCannotFetchIsARetryAndNotARefusal(t *testing.T) {
	ends := newBothEnds(t)
	// The root now believes the child mounts the artifacts somewhere it
	// does not.
	broken, err := fedbiz.NewFileDistributor(ends.artifacts, "/mnt/not-mounted-here")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	ends.setDelivery(t, broken)

	res, err := ends.svc.Publish(t.Context(), ends.id, fedbiz.PublishRequest{
		StagedRoot: ends.signTree(t, "opskeeper-sre-readonly", "1.0.0"),
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Delivery.Delivered {
		t.Fatalf("a child that could not read the tree reported delivery: %+v", res.Delivery)
	}
	if !res.Delivery.Verdict.Retryable {
		t.Errorf("the child's answer was not retryable: %+v", res.Delivery.Verdict)
	}
	// Nothing was decided, so the ledger says the cluster has not weighed
	// in — not that it refused.
	m, _ := ends.svc.Member(ends.id)
	if m.Acknowledged != 0 || !m.LastAck.At.IsZero() {
		t.Errorf("the ledger recorded an answer the child never gave: %+v", m.LastAck)
	}
	if ends.store.Live() != "" {
		t.Errorf("the child is enforcing %q after a tree it never received", ends.store.Live())
	}

	// The mount is fixed, and the retry is the operator's next move.
	ends.setDelivery(t, ends.dist)
	again, err := ends.svc.Redeliver(t.Context(), ends.id)
	if err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if again.Bundle.Version != res.Bundle.Version {
		t.Errorf("the retry sent version %d, want %d — a retry must not mint a version",
			again.Bundle.Version, res.Bundle.Version)
	}
	if !again.Delivery.Delivered {
		t.Fatalf("the retry did not converge: %+v (error %q)", again.Delivery.Verdict, again.Delivery.Error)
	}
	if m, _ := ends.svc.Member(ends.id); m.Behind() {
		t.Errorf("a cluster that took the policy still reports Behind: %+v", m)
	}
}

// TestAChildThatDisagreesIsARefusalOverTheRealSeam is the other direction:
// a tree the root signed that this child does not permit. The child decides,
// and the root's ledger has to say it decided.
func TestAChildThatDisagreesIsARefusalOverTheRealSeam(t *testing.T) {
	ends := newBothEnds(t)
	// A tree the child will not admit: its own policy caps at L2, and
	// this one declares L3 with a scope the child does not hold.
	tree := ends.signTreeAt(t, "opskeeper-sre-readonly", "1.0.0", func(m string) string {
		return strings.Replace(m, "safety_level: L1", "safety_level: L3", 1)
	})

	res, err := ends.svc.Publish(t.Context(), ends.id, fedbiz.PublishRequest{StagedRoot: tree})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Delivery.Delivered {
		t.Fatalf("a tree the child does not permit was taken: %+v", res.Delivery.Verdict)
	}
	if res.Delivery.Verdict.Retryable {
		t.Error("a policy refusal came back retryable; a root would hammer it forever")
	}
	m, _ := ends.svc.Member(ends.id)
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

// resolved is filepath.EvalSymlinks as a test helper, for comparing a path
// the store reported against one the test built.
func resolved(t *testing.T, path string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return out
}

// signTree stages a tree and signs it with the root's own release key, read
// back off the wired publisher so the test cannot sign with a key the child
// does not trust.
func (e *bothEnds) signTree(t *testing.T, name, version string) string {
	t.Helper()
	return e.signTreeAt(t, name, version, func(m string) string { return m })
}

func (e *bothEnds) signTreeAt(t *testing.T, name, version string, edit func(string) string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "staged", version)
	if err := os.MkdirAll(filepath.Join(root, "extensions", "tool"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The manifest is spelled the way a shipped package spells it, not the
	// way this test first invented it. A made-up shape would be refused by
	// the child's own admission check and the test would be measuring the
	// manifest parser instead of the channel.
	manifest := edit(`apiVersion: opskeeper.io/v1
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
    - {name: host_probe_tcp, class: read}
  required_scopes:
    - host.read
  audit: {emits: true, mutates: false}
  approval: {required: false}
  install: {strategy: rolling, min_edge_version: 0.1.0}
`)
	if err := os.WriteFile(filepath.Join(root, pluginmanifest.ManifestFile), []byte(manifest), 0o640); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "extensions", "tool", "tools.go"), []byte("package tool\n"), 0o640); err != nil {
		t.Fatalf("write the tool: %v", err)
	}
	env, err := e.signer.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return root
}
