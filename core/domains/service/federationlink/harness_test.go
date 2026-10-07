package federationlink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The harness here is the one thing the unit tests cannot be: both ends of
// the cluster channel, talking over the real wire types, with a real
// signature on a real tree and a real receiver on the other end.
//
// The child is core/floor/federation.Receiver — the same code
// service/federationchild.Agent calls — driven directly instead of through a
// tunnel. What is under test is therefore the contract between the two ends:
// the shapes on the wire, the version an answer carries, and the difference
// between a decision and a transport failure. A field renamed on one side
// compiles perfectly and then refuses every rollout, which is the class of
// break neither end can see alone.

// childSwitcher is the child's atomic policy swap, and it counts because the
// number of swaps is how these tests tell "the child re-decided" from "the
// child remembered".
type childSwitcher struct {
	mu    sync.Mutex
	live  string
	calls int
}

func (s *childSwitcher) Switch(staged string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.live = staged
	return staged, nil
}

func (s *childSwitcher) Live() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

func (s *childSwitcher) swaps() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// federation is both ends at once.
type federation struct {
	root   *Links
	reg    *fedbiz.Registry
	pub    *fedbiz.Publisher
	child  *floorfed.Receiver
	sw     *childSwitcher
	stage  string
	caller *fakeCaller
	signer *pluginmanifest.Signer
	id     floorfed.ClusterID
	token  string

	mu sync.Mutex
	// pending is the tree the next push should stage, or empty to push
	// without one — which is how a test produces a child that has not
	// received its files yet.
	pending string
	// skipStaging stages the tree somewhere the child is not told to
	// look, which is a different failure with a different answer.
	skipStaging bool
}

func newFederation(t *testing.T) *federation {
	t.Helper()
	signer, _, err := pluginmanifest.GenerateSigner("release-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	trust := pluginmanifest.NewTrustStore()
	if err := trust.Trust(signer.KeyID(), signer.PublicKey()); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	// The child's own policy, and deliberately narrower than the root's
	// idea of what is publishable: a root that could pick the policy a
	// bundle is judged against would make the child's verification
	// decorative.
	pol := pluginmanifest.PolicyFor(domain.SafetyL2, domain.RadiusNamespace,
		domain.Scopes{domain.ScopeHostRead})
	pol.NodeVersion = "0.4.0"
	pol.PigVersion = "0.3.0"

	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	sw := &childSwitcher{}
	recv, err := floorfed.NewReceiver(id, trust, pol, sw)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	reg := fedbiz.NewRegistry(nil)
	pub, err := fedbiz.NewPublisher(reg, signer)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	token, err := reg.Enroll(id, "north production")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	fed := &federation{
		reg: reg, pub: pub, child: recv, sw: sw, signer: signer,
		stage: t.TempDir(), id: id, token: token,
	}
	// The tests sign with this same signer rather than making their own.
	// Two signers sharing a key id is a real thing to be robust against and
	// the verification below refuses it correctly, but a harness that hit
	// that on every test would be testing key distribution rather than the
	// channel.
	fed.caller = &fakeCaller{answer: fed.answer}
	link, err := NewLink(fed.caller, asPort(reg), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	fed.root = link
	return fed
}

// connect says hello, which is the only thing that binds the two ends.
func (f *federation) connect(t *testing.T) tunnel.ClusterHelloResponse {
	t.Helper()
	resp := sayHello(t, f.root, childEdgeID, f.id, f.token)
	if !resp.Accepted {
		t.Fatalf("hello refused: %s", resp.Reason)
	}
	return resp
}

// answer plays the child's half of every message the root sends.
func (f *federation) answer(_ uint64, method string, body []byte) ([]byte, error) {
	switch method {
	case tunnel.MethodClusterPolicy:
		var req tunnel.ClusterPolicyRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		f.mu.Lock()
		pending, skip := f.pending, f.skipStaging
		f.mu.Unlock()

		if skip {
			// The tree is on the child's disk, just not where the
			// receipt says. The child has to answer "not here yet",
			// and it has to be able to answer that again for the
			// same version.
			req.StagedPath = filepath.Join(f.stage, "not-where-the-receipt-said")
		} else if pending != "" {
			dst := filepath.Join(f.stage, "staging", "v"+strconv.FormatUint(req.Bundle.Version, 10))
			if err := copyTree(pending, dst); err != nil {
				return nil, err
			}
			req.StagedPath = dst
		}
		out, applyErr := f.child.Apply(req.Bundle, req.StagedPath)
		return json.Marshal(tunnel.ClusterPolicyResponse{
			Outcome: out,
			Retryable: errors.Is(applyErr, floorfed.ErrNotStaged) ||
				errors.Is(applyErr, floorfed.ErrPromotionFailed),
			Error: errText(applyErr),
		})
	case tunnel.MethodClusterState:
		live, applied := f.child.Live()
		return json.Marshal(tunnel.ClusterStateResponse{
			Policy:          live,
			Bundle:          applied,
			LastRootContact: time.Now(),
			Enforcing:       live >= floorfed.MinBundleVersion,
			NodeCount:       12,
		})
	}
	return nil, errors.New("federation harness: unknown method " + method)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// publish signs a tree, issues the next version, and pushes it.
func (f *federation) publish(t *testing.T, tree, reason string) (fedbiz.PublishResult, tunnel.ClusterPolicyResponse) {
	t.Helper()
	f.mu.Lock()
	f.pending, f.skipStaging = tree, false
	f.mu.Unlock()

	res, err := f.pub.Publish(context.Background(), f.id, fedbiz.PublishRequest{
		StagedRoot: tree,
		Reason:     reason,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	resp, err := f.root.PushPolicy(context.Background(), f.id, tunnel.ClusterPolicyRequest{
		Bundle: res.Bundle,
		// Deliberately a lie on the first attempt: the harness is the
		// thing that knows where it put the tree, and the receipt the
		// root sends is just a string.
		StagedPath: "/var/lib/opskeeper/staging/whatever-the-root-guessed",
	})
	if err != nil {
		t.Fatalf("PushPolicy: %v", err)
	}
	return res, resp
}

func TestAWholePolicyCrossesTheChannel(t *testing.T) {
	fed := newFederation(t)
	fed.connect(t)

	tree := writeTree(t, fed.signer, "opskeeper-sre-readonly", "1.0.0", "L1", "host.read", false)
	res, resp := fed.publish(t, tree, "first rollout")

	if res.Bundle.Version != 1 {
		t.Fatalf("first publish issued version %d, want 1", res.Bundle.Version)
	}
	if !resp.Outcome.Accepted {
		t.Fatalf("the child refused a policy it should have accepted: %s (retryable=%v)", resp.Error, resp.Retryable)
	}
	if resp.Retryable {
		t.Error("a decision came back marked retryable")
	}
	if resp.Outcome.Live != 1 {
		t.Errorf("live = %d after accepting version 1", resp.Outcome.Live)
	}
	if got := fed.sw.swaps(); got != 1 {
		t.Errorf("the child swapped its policy %d times, want 1", got)
	}

	// And the state call has to report the same fact the push did, from
	// the child's own memory rather than from anything the root remembers.
	st, err := fed.root.AskState(context.Background(), fed.id)
	if err != nil {
		t.Fatalf("AskState: %v", err)
	}
	if st.Policy != 1 || !st.Enforcing {
		t.Errorf("state = %+v, want version 1 enforcing", st)
	}
	if st.Bundle.PackageName != "opskeeper-sre-readonly" {
		t.Errorf("state names package %q, want the one that was pushed", st.Bundle.PackageName)
	}
}

// TestARefusedPolicyIsFinalAndInert is the property the whole monotonic rule
// exists to protect: a version the child read and declined must never be
// re-decided, and the redelivery must come back as the same refusal rather
// than as a success.
func TestARefusedPolicyIsFinalAndInert(t *testing.T) {
	fed := newFederation(t)
	fed.connect(t)

	// L3 with host.write, against a child holding L2 and host.read.
	tree := writeTree(t, fed.signer, "opskeeper-sre-repair", "1.0.0", "L3", "host.write", true)
	res, resp := fed.publish(t, tree, "widen what this cluster may do")

	if resp.Outcome.Accepted {
		t.Fatalf("the child accepted a package its own policy forbids: %+v", resp.Outcome)
	}
	if resp.Retryable {
		t.Error("an admission refusal was marked retryable; that would have the root re-decide a final answer")
	}
	if resp.Error == "" {
		t.Error("a refusal came back with no reason for the operator's log")
	}
	if got := fed.sw.swaps(); got != 0 {
		t.Errorf("the child swapped its policy %d times while refusing; want 0", got)
	}

	// The redelivery. Same bundle, same answer, still no swap — and the
	// error must come back with it, because an outcome replayed as a
	// success is the single worst bug this channel can have.
	again, err := fed.root.PushPolicy(context.Background(), fed.id, tunnel.ClusterPolicyRequest{
		Bundle: res.Bundle,
	})
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if again.Outcome.Accepted {
		t.Fatalf("a redelivered refusal came back accepted: %+v", again.Outcome)
	}
	if again.Error == "" {
		t.Error("the redelivered refusal lost its reason")
	}
	if got := fed.sw.swaps(); got != 0 {
		t.Errorf("a redelivery moved the child %d times", got)
	}

	// And the child is still on what it had, which is the answer an
	// operator needs at 3am: refusing did not cost it its last good policy.
	if live, _ := fed.child.Live(); live != 0 {
		t.Errorf("the child is enforcing version %d after only refusals, want 0", live)
	}
}

// TestAPushThatArrivedBeforeItsFilesIsRetryableAndBurnsNothing: "not here
// yet" and "not allowed" are different answers, and a version burned by the
// first one could never be applied afterwards.
func TestAPushThatArrivedBeforeItsFilesIsRetryableAndBurnsNothing(t *testing.T) {
	fed := newFederation(t)
	fed.connect(t)

	tree := writeTree(t, fed.signer, "opskeeper-sre-readonly", "1.0.0", "L1", "host.read", false)
	fed.mu.Lock()
	fed.pending, fed.skipStaging = tree, true
	fed.mu.Unlock()

	issued, err := fed.pub.Publish(context.Background(), fed.id, fedbiz.PublishRequest{StagedRoot: tree})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	lost, err := fed.root.PushPolicy(context.Background(), fed.id, tunnel.ClusterPolicyRequest{
		Bundle:     issued.Bundle,
		StagedPath: "/var/lib/opskeeper/staging/early",
	})
	if err != nil {
		t.Fatalf("PushPolicy: %v", err)
	}
	if !lost.Retryable {
		t.Errorf("a push whose files had not arrived was not retryable: %+v", lost)
	}
	if lost.Outcome.Accepted {
		t.Error("a push with no tree reported itself accepted")
	}
	if got := fed.sw.swaps(); got != 0 {
		t.Errorf("the child swapped %d times with nothing to swap", got)
	}

	// Now the files land and the same version goes out again. This is the
	// whole reason "not here yet" is not recorded as a decision: if the
	// version had been burned, this could only ever end in a refusal.
	fed.mu.Lock()
	fed.skipStaging = false
	fed.mu.Unlock()
	ok, err := fed.root.PushPolicy(context.Background(), fed.id, tunnel.ClusterPolicyRequest{
		Bundle: issued.Bundle,
	})
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if !ok.Outcome.Accepted {
		t.Fatalf("the real push of version %d was refused after an early one: %s", issued.Bundle.Version, ok.Error)
	}
	if got := fed.sw.swaps(); got != 1 {
		t.Errorf("the child swapped %d times, want exactly the one real switch", got)
	}
}

func TestARedeliveredAcceptedVersionIsAnsweredFromTheRecord(t *testing.T) {
	fed := newFederation(t)
	fed.connect(t)

	tree := writeTree(t, fed.signer, "opskeeper-sre-readonly", "1.0.0", "L1", "host.read", false)
	issued, first := fed.publish(t, tree, "first")
	if !first.Outcome.Accepted {
		t.Fatalf("first push refused: %s", first.Error)
	}

	// The child has since moved on, so the redelivered version is no
	// longer what is in force. The answer has to say so: without the
	// superseded flag this reads as a cheerful "yes, you are on v1" to a
	// root that is in fact two versions behind.
	tree2 := writeTree(t, fed.signer, "opskeeper-sre-readonly", "1.1.0", "L1", "host.read", false)
	if _, second := fed.publish(t, tree2, "second"); !second.Outcome.Accepted {
		t.Fatalf("second push refused: %s", second.Error)
	}

	again, err := fed.root.PushPolicy(context.Background(), fed.id, tunnel.ClusterPolicyRequest{
		Bundle: issued.Bundle,
		// The tree is long gone from staging by now, which is the
		// situation a retry after a delay actually lands in.
		StagedPath: filepath.Join(fed.stage, "gone"),
	})
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if !again.Outcome.Superseded {
		t.Errorf("redelivering version %d did not report it as superseded (live=%d)",
			issued.Bundle.Version, again.Outcome.Live)
	}
	if again.Outcome.Live != 2 {
		t.Errorf("live = %d on a redelivery of version 1, want the current 2", again.Outcome.Live)
	}
	if got := fed.sw.swaps(); got != 2 {
		t.Errorf("the child swapped %d times across two rollouts and a redelivery, want 2", got)
	}
}

// writeTree writes a real package directory and signs it.
//
// level, scope and writes are what the child's admission policy judges the
// result against, so a test produces a tree the child accepts or refuses by
// describing the package honestly rather than by forcing an answer.
func writeTree(t *testing.T, signer *pluginmanifest.Signer, name, version, level, scope string, writes bool) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	class, approval := "read", "false"
	if writes {
		class, approval = "write", "true"
	}
	manifest := `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: ` + name + `
  version: ` + version + `
  vendor: acme
  homepage: https://example.invalid/` + name + `
spec:
  targets: [edge]
  safety_level: ` + level + `
  capabilities: [` + class + `]
  tools:
    - {name: host_probe_tcp, class: ` + class + `}
  required_scopes:
    - ` + scope + `
  audit: {emits: true, mutates: false}
  approval: {required: ` + approval + `}
  install: {strategy: rolling, min_edge_version: 0.1.0}
`
	mustWrite(t, filepath.Join(root, pluginmanifest.ManifestFile), manifest)
	mustWrite(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool\n\n// generated\n")

	// The sidecar goes into the tree as well as into the bundle: the child
	// verifies the tree on disk and cross-checks it against the bundle's
	// own copy, and a harness that signed only one of the two would be
	// testing a path the real flow never takes.
	env, err := signer.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return root
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
}
