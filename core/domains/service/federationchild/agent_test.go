package federationchild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Agent tests.
//
// Every one of these runs with no socket open. That is not a shortcut — it is
// the claim: a child cluster's answers come from state it already holds, and
// a test that needed a root would be testing the root.

// TestAFreshClusterIsEnforcingNothing is the state a cluster is in between
// enrolment and its first push, and it is a state a console has to be able to
// render honestly.
func TestAFreshClusterIsEnforcingNothing(t *testing.T) {
	agent, client, _, _ := newChild(t, "prod-cn-south")

	st := client.invokeState(t, tunnel.ClusterStateRequest{Cluster: federation.ClusterID("prod-cn-south")})
	if st.Policy != 0 {
		t.Errorf("Policy = %d for a cluster that has never been told anything", st.Policy)
	}
	if st.Enforcing {
		t.Errorf("Enforcing = true at version 0 — holding no policy is not enforcing a policy")
	}
	if got := agent.LastRootContact(); !got.IsZero() {
		t.Errorf("LastRootContact = %v before any root was heard from", got)
	}
}

func TestAPolicyPushIsAppliedAndAnswered(t *testing.T) {
	agent, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")
	root, env := signedTree(t, store, signer, "opskeeper-sre-readonly", "1.0.0")

	req := bundle(id, 1, env)
	req.StagedPath = root
	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)

	if !resp.Outcome.Accepted {
		t.Fatalf("outcome = %+v, want accepted (error %q)", resp.Outcome, resp.Error)
	}
	if resp.Retryable {
		t.Errorf("Retryable = true on an accepted push")
	}
	if resp.Outcome.Live != 1 {
		t.Errorf("Live = %d, want 1", resp.Outcome.Live)
	}
	if want := mustResolve(t, root); store.Live() != want {
		t.Errorf("store live = %q, want %q", store.Live(), want)
	}

	st := client.invokeState(t, tunnel.ClusterStateRequest{Cluster: id})
	if st.Policy != 1 || !st.Enforcing {
		t.Errorf("state = %+v, want policy 1 enforcing", st)
	}
	if st.Bundle.PackageName != "opskeeper-sre-readonly" {
		t.Errorf("state names package %q, want the one that was applied", st.Bundle.PackageName)
	}
	if st.NodeCount != 12 {
		t.Errorf("NodeCount = %d, want the child's own 12", st.NodeCount)
	}
	if agent.LastRootContact().IsZero() {
		t.Errorf("a push counts as contact with the root, but LastRootContact is still zero")
	}
}

// TestAnUnstagedPushIsRetryableAndTheRetryWorks is the at-least-once case:
// the receipt can arrive before the tree it refers to.
func TestAnUnstagedPushIsRetryableAndTheRetryWorks(t *testing.T) {
	_, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")
	root, env := signedTree(t, store, signer, "opskeeper-sre-readonly", "1.0.0")

	req := bundle(id, 1, env)
	req.StagedPath = filepath.Join(store.VersionsDir(), "1.0.0-not-here-yet")
	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)

	if resp.Outcome.Accepted {
		t.Fatalf("a push with no tree on disk was accepted")
	}
	if !resp.Retryable {
		t.Errorf("Retryable = false for a push whose tree has not arrived: %+v", resp)
	}

	// The tree lands; the same version now goes through.
	req.StagedPath = root
	resp = client.invoke(t, tunnel.MethodClusterPolicy, req)
	if !resp.Outcome.Accepted {
		t.Fatalf("the retry was not accepted: %+v (%s)", resp.Outcome, resp.Error)
	}
}

// TestARefusalIsNotRetryable is the other half, and the half that keeps a
// rollout from spinning: a cluster that declined a policy must not be told to
// send it again.
func TestARefusalIsNotRetryable(t *testing.T) {
	_, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")
	root, env := signedTree(t, store, signer, "opskeeper-sre-readonly", "1.0.0")

	// Signed by a key this cluster does not trust: a decision, and final.
	rogue, _, err := pluginmanifest.GenerateSigner("rogue")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	rogueRoot, _ := signedTreeAt(t, filepath.Join(store.VersionsDir(), "forged-1.0.0"),
		rogue, "opskeeper-sre-readonly", "1.0.0")

	req := bundle(id, 1, env)
	req.StagedPath = rogueRoot
	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)

	if resp.Outcome.Accepted {
		t.Fatalf("a policy signed by an untrusted key was applied")
	}
	if resp.Retryable {
		t.Errorf("Retryable = true for a signature refusal: %+v", resp)
	}
	if resp.Outcome.Live != 0 {
		t.Errorf("Live = %d after a refusal, want 0 — a cluster must never report a policy it is not enforcing", resp.Outcome.Live)
	}

	// The legitimate version 1 still works: the refusal was recorded
	// against a bundle whose own fields were fine, and the cluster did
	// not become permanently hostile to version 1.
	req.StagedPath = root
	if resp = client.invoke(t, tunnel.MethodClusterPolicy, req); !resp.Outcome.Accepted {
		t.Errorf("the legitimate version 1 was refused after an unrelated refusal: %+v (%s)", resp.Outcome, resp.Error)
	}
}

// TestARootCannotNameAPathOutsideTheStagingArea is the reason Store.bound
// exists. StagedPath arrives from the root, and a path the root can name is a
// path the root can write.
func TestARootCannotNameAPathOutsideTheStagingArea(t *testing.T) {
	_, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")
	signedTree(t, store, signer, "opskeeper-sre-readonly", "1.0.0")

	// A tree that really exists, really is signed, and is not where the
	// store promotes from.
	outside := filepath.Join(t.TempDir(), "elsewhere")
	write(t, filepath.Join(outside, pluginmanifest.ManifestFile), manifestYAML("opskeeper-sre-readonly", "1.0.0"))
	write(t, filepath.Join(outside, "extensions", "tool", "tools.go"), "package tool\n")
	outsideEnv, err := signer.Sign(outside)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := outsideEnv.WriteTo(outside); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	req := bundle(id, 1, outsideEnv)
	req.StagedPath = outside
	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)

	if resp.Outcome.Accepted {
		t.Fatalf("a policy outside the staging area was promoted")
	}
	if resp.Retryable {
		t.Errorf("Retryable = true for a path the cluster will never promote from — the root would retry forever")
	}
	if !strings.Contains(resp.Error, "staging") {
		t.Errorf("error = %q, want it to say the path was outside the staging area", resp.Error)
	}
	if store.Live() != "" {
		t.Errorf("the store reports %q live after refusing a path; a refused push must leave the state alone", store.Live())
	}
}

// TestAPathThatLooksInsideTheStagingAreaButIsNot pins the second half of the
// check: a symlink *inside* the staging area pointing out of it passes a
// prefix test and is still an escape.
func TestAPathThatLooksInsideTheStagingAreaButIsNot(t *testing.T) {
	_, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")
	_, env := signedTree(t, store, signer, "opskeeper-sre-readonly", "1.0.0")

	elsewhere := t.TempDir()
	escape := filepath.Join(store.VersionsDir(), "escape")
	if err := os.Symlink(elsewhere, escape); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	req := bundle(id, 1, env)
	req.StagedPath = escape
	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)

	if resp.Outcome.Accepted {
		t.Fatalf("a symlink out of the staging area was followed")
	}
	if resp.Retryable {
		t.Errorf("Retryable = true for an escape attempt")
	}
}

// TestHelloRecordsBothOutcomes: a refusal is a condition an operator has to
// see, and it is the same condition whatever the root's reason was.
func TestHelloRecordsBothOutcomes(t *testing.T) {
	agent, client, _, _ := newChild(t, "prod-cn-south")

	client.onCall = func(_ string, _, resp any) error {
		r := resp.(*tunnel.ClusterHelloResponse)
		r.Accepted = true
		r.PolicyVersion = 4
		r.HeartbeatSeconds = 30
		return nil
	}
	if err := agent.Hello(t.Context()); err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if ok, _ := agent.Accepted(); !ok {
		t.Errorf("Accepted() = false after a successful hello")
	}
	if client.callCount(tunnel.MethodClusterHello) != 1 {
		t.Errorf("hello was sent %d times, want 1", client.callCount(tunnel.MethodClusterHello))
	}

	client.onCall = func(_ string, _, resp any) error {
		r := resp.(*tunnel.ClusterHelloResponse)
		r.Accepted = false
		r.Reason = "this root has not enrolled that cluster"
		return nil
	}
	err := agent.Hello(t.Context())
	if err == nil {
		t.Fatalf("a refused hello returned no error — the operator would never see it")
	}
	if !strings.Contains(err.Error(), "not enrolled") {
		t.Errorf("error = %v, want it to carry the root's reason", err)
	}
	if ok, reason := agent.Accepted(); ok || reason == "" {
		t.Errorf("Accepted() = %v, %q; want a recorded refusal with its reason", ok, reason)
	}
}

// TestTheChildKeepsEnforcingWithTheRootGone is the plan's "子集群可独立运行"
// as a test rather than a claim: nothing below opens a connection, and the
// answers are still right afterwards.
func TestTheChildKeepsEnforcingWithTheRootGone(t *testing.T) {
	_, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")

	v1Root, v1Env := signedTree(t, store, signer, "opskeeper-sre-readonly", "1.0.0")
	req := bundle(id, 1, v1Env)
	req.StagedPath = v1Root
	if resp := client.invoke(t, tunnel.MethodClusterPolicy, req); !resp.Outcome.Accepted {
		t.Fatalf("apply v1: %+v", resp.Outcome)
	}
	v2Root, v2Env := signedTree(t, store, signer, "opskeeper-sre-readonly", "1.1.0")
	req = bundle(id, 2, v2Env)
	req.StagedPath = v2Root
	if resp := client.invoke(t, tunnel.MethodClusterPolicy, req); !resp.Outcome.Accepted {
		t.Fatalf("apply v2: %+v", resp.Outcome)
	}

	// The root goes away. A state request still gets a true answer.
	st := client.invokeState(t, tunnel.ClusterStateRequest{Cluster: id})
	if st.Policy != 2 || !st.Enforcing {
		t.Errorf("state = %+v, want policy 2 still enforcing with no root present", st)
	}

	// And a captured, still perfectly valid, still trusted v1 cannot walk
	// it back.
	req = bundle(id, 1, v1Env)
	req.StagedPath = v1Root
	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)
	if resp.Outcome.Live != 2 {
		t.Errorf("replaying v1 left the cluster on %d, want 2", resp.Outcome.Live)
	}
	if !resp.Outcome.Superseded {
		t.Errorf("replaying v1 was not reported as superseded: %+v", resp.Outcome)
	}
	if want := mustResolve(t, v2Root); store.Live() != want {
		t.Errorf("store live = %q, want the v2 tree %q", store.Live(), want)
	}
}

// TestAClusterThatNeverHeardFromARootSaysSoDistinguishably: "last seen three
// days ago, still enforcing version 9" is a healthy cluster, and reporting it
// the same way as a dead one is how a console teaches its operators to ignore
// it.
func TestAClusterThatNeverHeardFromARootSaysSoDistinguishably(t *testing.T) {
	_, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")
	root, env := signedTree(t, store, signer, "opskeeper-sre-readonly", "1.0.0")
	req := bundle(id, 1, env)
	req.StagedPath = root
	client.invoke(t, tunnel.MethodClusterPolicy, req)

	st := client.invokeState(t, tunnel.ClusterStateRequest{Cluster: id})
	if st.LastRootContact.IsZero() {
		t.Errorf("LastRootContact is zero after a push that came from a root")
	}
	if !st.Enforcing {
		t.Errorf("Enforcing = false for a cluster that is enforcing version 1")
	}
}

func TestNewAgentRefusesAnIncompleteWiring(t *testing.T) {
	id, _ := federation.NewClusterID("prod-cn-south")
	store := newStore(t)
	signer := newSigner(t)
	recv, err := federation.NewReceiver(id, trustFor(t, signer), policy(), store)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	client := newFakeClient()
	good := federation.Cluster{ID: id}

	cases := map[string]struct {
		client  tunnel.Client
		recv    *federation.Receiver
		store   *Store
		cluster federation.Cluster
		token   string
	}{
		"no client":   {nil, recv, store, good, "t"},
		"no receiver": {client, nil, store, good, "t"},
		"no store":    {client, recv, nil, good, "t"},
		"bad cluster": {client, recv, store, federation.Cluster{}, "t"},
		"no token":    {client, recv, store, good, ""},
	}
	for name, tc := range cases {
		if _, err := NewAgent(tc.client, tc.recv, tc.store, tc.cluster, tc.token, nil); err == nil {
			t.Errorf("[%s] NewAgent accepted an incomplete wiring", name)
		}
	}
}

// mustResolve compares paths in the same form the store reports them in.
//
// Store.bound runs EvalSymlinks, so on a system where the temp directory sits
// behind a symlink (/var -> /private/var on macOS) the store's answer is the
// resolved path and the caller's is not. Comparing the raw strings would fail
// for a reason that has nothing to do with what is under test.
func mustResolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return resolved
}
