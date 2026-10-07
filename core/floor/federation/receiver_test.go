package federation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// Receiver tests.
//
// The properties here are the ones federation is supposed to have, and each
// one is a way the design could be wrong in a way a happy-path test would not
// notice: a replay walks a cluster back to a weakened policy, a retry re-runs
// a decision, a refusal leaves the cluster on nothing, and a child with a dead
// root forgets what it was enforcing.

func TestASignedBundleIsApplied(t *testing.T) {
	r, signer, sw := newHarness(t, "prod-cn-north")
	root, env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")

	out, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 1, env), root)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !out.Accepted {
		t.Fatalf("outcome = %+v, want accepted", out)
	}
	if out.Live != 1 {
		t.Errorf("Live = %d, want 1", out.Live)
	}
	if sw.Live() != root {
		t.Errorf("switcher live = %q, want the staged root %q", sw.Live(), root)
	}
	if live, _ := r.Live(); live != 1 {
		t.Errorf("receiver Live() = %d, want 1", live)
	}
}

// TestAReplayedBundleCannotRollAClusterBack is the reason versions only go
// up.
//
// The bundle being replayed here is not forged and not tampered: it is a
// perfectly valid, perfectly signed policy that this cluster was enforcing ten
// minutes ago. Nothing about its cryptography is wrong. What makes replaying
// it an attack is that policy 6 was superseded, and a signature says nothing
// about recency — the key that signed it is still trusted and will still sign
// something else tomorrow.
func TestAReplayedBundleCannotRollAClusterBack(t *testing.T) {
	r, signer, sw := newHarness(t, "prod-cn-north")

	v6Root, v6Env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")
	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 6, v6Env), v6Root); err != nil {
		t.Fatalf("apply v6: %v", err)
	}
	v7Root, v7Env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.1.0")
	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 7, v7Env), v7Root); err != nil {
		t.Fatalf("apply v7: %v", err)
	}
	if sw.callCount() != 2 {
		t.Fatalf("switcher called %d times, want 2", sw.callCount())
	}

	// Now the old bundle comes back. It is answered from the record, and
	// that is the answer: version 6 was a real decision, delivered a
	// second time. The load-bearing part is that the answer says the
	// cluster is on 7 and marks 6 superseded — a replay that answered
	// "accepted, live 6" would be the bug, not the fix.
	out, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 6, v6Env), v6Root)
	if err != nil {
		t.Fatalf("replaying a previously-accepted version returned an error: %v", err)
	}
	if out.Live != 7 {
		t.Errorf("outcome Live = %d, want the cluster still on 7", out.Live)
	}
	if !out.Superseded {
		t.Errorf("outcome = %+v, want Superseded set — version 6 is no longer in force", out)
	}
	if sw.callCount() != 2 {
		t.Errorf("switcher called %d times after the replay, want 2 — a replay must not switch", sw.callCount())
	}
	if sw.Live() != v7Root {
		t.Errorf("live tree = %q, want the v7 tree", sw.Live())
	}
}

// TestRedeliveringTheSameVersionReplaysTheRecordedOutcome covers the delivery
// guarantee, which is at-least-once. A push that is retried after the ack was
// lost must not switch the tree a second time.
func TestRedeliveringTheSameVersionReplaysTheRecordedOutcome(t *testing.T) {
	r, signer, sw := newHarness(t, "prod-cn-north")
	root, env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")
	b := bundleFor(t, ClusterID("prod-cn-north"), 3, env)

	first, err := r.Apply(b, root)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	second, err := r.Apply(b, root)
	if err != nil {
		t.Fatalf("redelivery returned an error: %v", err)
	}
	if !second.Accepted || second.Live != first.Live || second.At != first.At {
		t.Errorf("redelivery = %+v, want the recorded outcome %+v", second, first)
	}
	if sw.callCount() != 1 {
		t.Errorf("switcher called %d times, want 1", sw.callCount())
	}
}

// TestARefusedVersionIsRememberedAndReplaysTheRefusal is the mirror of the
// case above, and the more important one: a refusal is a decision, and a retry
// must not re-open it.
//
// The observable is the tree disappearing. After the refusal the staged tree is
// deleted, so a re-run of the whole check would fail differently — and a child
// that re-reviews is a child whose verdict depends on what is on disk at the
// moment of the retry rather than on what it decided.
func TestARefusedVersionIsRememberedAndReplaysTheRefusal(t *testing.T) {
	r, signer, _ := newHarness(t, "prod-cn-north")
	root, env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")
	b := bundleFor(t, ClusterID("prod-cn-north"), 4, env)

	if _, err := r.Apply(b, root); err != nil {
		t.Fatalf("baseline apply: %v", err)
	}

	// A bundle whose tree is fine but whose claims disagree with its own
	// envelope is refused on shape.
	bad := b
	bad.Version = 5
	bad.PackageVersion = "9.9.9"
	first, err := r.Apply(bad, root)
	if err == nil {
		t.Fatalf("expected a refusal for a bundle that disagrees with its envelope")
	}
	if first.Accepted {
		t.Errorf("outcome = %+v, want refused", first)
	}

	// Take the tree away. A re-review would now fail with a different
	// error; a replay must return the recorded one.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove staged tree: %v", err)
	}
	second, err := r.Apply(bad, root)
	if err == nil {
		t.Fatalf("redelivery of a refused version reported success — the caller would conclude the cluster adopted a policy it refused")
	}
	if second.Accepted {
		t.Errorf("redelivery outcome = %+v, want the recorded refusal", second)
	}
	if second.Reason != first.Reason {
		t.Errorf("redelivery reason = %q, want the recorded %q", second.Reason, first.Reason)
	}
	if second.Live != first.Live {
		t.Errorf("redelivery Live = %d, want the recorded %d", second.Live, first.Live)
	}
}

func TestATamperedTreeIsRefused(t *testing.T) {
	r, signer, sw := newHarness(t, "prod-cn-north")
	root, env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")

	// Someone with write access to the staging directory changes the tree
	// after it was signed. Hashing cannot catch this — the attacker
	// recomputes it — and that is the entire reason the key is not in
	// that directory.
	writeFile(t, filepath.Join(root, "extensions", "tool", "tools.go"),
		"package tool\n\n// now it runs something else\n")

	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 1, env), root); err == nil {
		t.Fatalf("a tampered tree was applied")
	}
	if sw.callCount() != 0 {
		t.Errorf("switcher called %d times for a tampered tree, want 0", sw.callCount())
	}
	if live, _ := r.Live(); live != 0 {
		t.Errorf("Live = %d, want 0 — a refused policy must leave nothing half-applied", live)
	}
}

func TestABundleSignedByAnUntrustedKeyIsRefused(t *testing.T) {
	r, _, sw := newHarness(t, "prod-cn-north")
	// A different key, not in this cluster's trust store.
	rogue := newSigner(t, "rogue-2026")
	root, env := newSignedTree(t, rogue, "opskeeper-sre-readonly", "1.0.0")

	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 1, env), root); err == nil {
		t.Fatalf("a bundle signed by an untrusted key was applied")
	}
	if sw.callCount() != 0 {
		t.Errorf("switcher called %d times, want 0", sw.callCount())
	}
}

// TestAClusterRefusesAPolicyExceedingItsOwnLimits pins the difference between
// signed and permitted. The release key vouched for this package; that is a
// statement about the package, not about whether this cluster may hold it.
func TestAClusterRefusesAPolicyExceedingItsOwnLimits(t *testing.T) {
	r, signer, sw := newHarness(t, "prod-cn-north")

	// Same signer, so the signature is perfectly valid — but this cluster
	// only permits up to L2 and grants only host.read.
	root := filepath.Join(t.TempDir(), "opskeeper-sre-repair")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(root, pluginmanifest.ManifestFile), `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: opskeeper-sre-repair
  version: 2.0.0
  vendor: acme
  homepage: https://example.invalid/repair
spec:
  targets: [edge]
  safety_level: L3
  capabilities: [write]
  tools:
    - {name: restart_service, class: write}
  required_scopes:
    - host.write
  audit: {emits: true, mutates: false}
  approval: {required: true}
  install: {strategy: rolling, min_edge_version: 0.1.0}
`)
	writeFile(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool\n")
	env := signTree(t, signer, root)

	out, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 1, env), root)
	if err == nil {
		t.Fatalf("an L3 write package was applied to an L2 read-only cluster")
	}
	if out.Accepted {
		t.Errorf("outcome = %+v, want refused", out)
	}
	if sw.callCount() != 0 {
		t.Errorf("switcher called %d times, want 0", sw.callCount())
	}
}

// TestAMisroutedBundleIsRefusedAndDoesNotBurnTheVersion guards an operational
// mistake that would otherwise be unrecoverable.
//
// The root sent version 9 to the wrong cluster. If this receiver recorded the
// refusal, then the root's real version 9 for this cluster — which it has not
// sent yet — would be rejected as already-decided, and the cluster would be
// stuck one version behind forever.
func TestAMisroutedBundleIsRefusedAndDoesNotBurnTheVersion(t *testing.T) {
	r, signer, _ := newHarness(t, "prod-cn-north")
	root, env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")

	misrouted := bundleFor(t, ClusterID("prod-cn-south"), 9, env)
	if _, err := r.Apply(misrouted, root); !errors.Is(err, ErrMalformedBundle) {
		t.Fatalf("misrouted bundle: err = %v, want ErrMalformedBundle", err)
	}
	if _, recorded := r.Seen(9); recorded {
		t.Errorf("a misrouted bundle burned version 9 on this cluster")
	}

	// The root's legitimate 9 still lands.
	real := bundleFor(t, ClusterID("prod-cn-north"), 9, env)
	if _, err := r.Apply(real, root); err != nil {
		t.Fatalf("the legitimate version 9 was refused: %v", err)
	}
}

// TestAnUnstagedBundleIsRetryable separates the one failure that is not a
// decision. A tree that has not arrived yet is a transport problem, and the
// root must be able to send it again.
func TestAnUnstagedBundleIsRetryable(t *testing.T) {
	r, signer, _ := newHarness(t, "prod-cn-north")
	root, env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")
	b := bundleFor(t, ClusterID("prod-cn-north"), 2, env)

	// The bundle arrives before the tree does. That is an ordinary race
	// between two channels, not a decision about the policy.
	if _, err := r.Apply(b, ""); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("empty staging path: err = %v, want ErrNotStaged", err)
	}
	if _, recorded := r.Seen(2); recorded {
		t.Errorf("an unstaged bundle left a record, so the retry would be refused as already-decided")
	}

	// The tree arrives; the same version now applies.
	if _, err := r.Apply(b, root); err != nil {
		t.Fatalf("retry after staging: %v", err)
	}
	if live, _ := r.Live(); live != 2 {
		t.Errorf("Live = %d, want 2", live)
	}
}

// TestAFailedSwitchLeavesTheOldPolicyInForce is the property a cluster's
// availability rests on: a policy that cannot be installed must not take the
// previous one with it.
func TestAFailedSwitchLeavesTheOldPolicyInForce(t *testing.T) {
	r, signer, sw := newHarness(t, "prod-cn-north")
	goodRoot, goodEnv := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")
	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 1, goodEnv), goodRoot); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	nextRoot, nextEnv := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.1.0")
	sw.failWith = errors.New("no space left on device")

	out, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 2, nextEnv), nextRoot)
	if err == nil {
		t.Fatalf("a failed switch reported success")
	}
	if out.Accepted {
		t.Errorf("outcome = %+v, want refused", out)
	}
	if live, _ := r.Live(); live != 1 {
		t.Errorf("Live = %d, want the cluster still enforcing 1", live)
	}
	if sw.Live() != goodRoot {
		t.Errorf("live tree = %q, want the previous tree %q", sw.Live(), goodRoot)
	}
	if !errors.Is(err, ErrPromotionFailed) {
		t.Errorf("err = %v, want ErrPromotionFailed so the transport can tell this from a refusal", err)
	}
	// The load-bearing part. A full disk clears on its own, and a version
	// that recorded the failure would be refused forever — so the root
	// would conclude the cluster objects to the policy and publish a new
	// version that fails in exactly the same way.
	if _, recorded := r.Seen(2); recorded {
		t.Errorf("a failed switch left a record, so the version can never be retried")
	}
	sw.failWith = nil
	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 2, nextEnv), nextRoot); err != nil {
		t.Fatalf("retry after the condition cleared: %v", err)
	}
	if live, _ := r.Live(); live != 2 {
		t.Errorf("Live = %d after the retry, want 2", live)
	}
}

// TestAChildKeepsEnforcingWhileDisconnected is the plan's "子集群可独立运行"
// stated as a test: nothing here touches a network, because that is the
// property — the answer lives on the child, and asking it requires no root.
func TestAChildKeepsEnforcingWhileDisconnected(t *testing.T) {
	r, signer, _ := newHarness(t, "prod-cn-north")
	root, env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")
	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 7, env), root); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// The root goes away. Nothing in this receiver needs it: the version
	// in force, the bundle that put it there, and the record of what was
	// decided are all local.
	live, applied := r.Live()
	if live != 7 {
		t.Errorf("Live = %d, want 7", live)
	}
	if applied.PackageName != "opskeeper-sre-readonly" || applied.PackageVersion != "1.0.0" {
		t.Errorf("the child cannot name the policy it is enforcing: %+v", applied)
	}
	if applied.Reason != "policy rollout" {
		t.Errorf("Reason = %q, want the root's reason to survive on the child", applied.Reason)
	}
}

// TestAdoptFloorGoesForwardAndRefusesToGoBack covers the one way the floor may
// move down: it may not, remotely or locally. An operator resynchronising a
// child whose root was rebuilt is going forwards.
func TestAdoptFloorGoesForwardAndRefusesToGoBack(t *testing.T) {
	r, signer, _ := newHarness(t, "prod-cn-north")
	root, env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")
	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 7, env), root); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if err := r.AdoptFloor(3, "an operator tried to rewind"); err == nil {
		t.Errorf("AdoptFloor moved the floor backwards")
	}
	if live, _ := r.Live(); live != 7 {
		t.Errorf("Live = %d after a refused rewind, want 7", live)
	}

	if err := r.AdoptFloor(9, "root rebuilt, numbering restarted at 10"); err != nil {
		t.Fatalf("AdoptFloor(9): %v", err)
	}
	if live, _ := r.Live(); live != 9 {
		t.Errorf("Live = %d, want 9", live)
	}
	// Everything at or below the new floor is forgotten, so a root that
	// is genuinely renumbering from 9 is not locked out by stale history.
	if _, recorded := r.Seen(7); recorded {
		t.Errorf("version 7 survived an AdoptFloor(9)")
	}
}

// TestAVersionThisClusterHasNeverSeenIsStillRefused covers the operational case
// the monotonic rule is not written for.
//
// A root that was rebuilt restarts its numbering. From here that is
// indistinguishable from a replay, and the two are treated the same way on
// purpose: the child cannot tell them apart, and in both cases adopting the
// lower number is the wrong answer. What the child *can* do is keep saying so,
// which is why this path leaves no record — a confused root can keep pushing
// and be refused each time, rather than being locked out permanently by a
// decision it never actually made.
func TestAVersionThisClusterHasNeverSeenIsStillRefused(t *testing.T) {
	r, signer, sw := newHarness(t, "prod-cn-north")

	v7Root, v7Env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.1.0")
	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 7, v7Env), v7Root); err != nil {
		t.Fatalf("apply v7: %v", err)
	}

	// A root that restarted at 1. Perfectly valid signature, perfect tree.
	v1Root, v1Env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")
	out, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 1, v1Env), v1Root)
	if !errors.Is(err, ErrVersionRegressed) {
		t.Fatalf("err = %v, want ErrVersionRegressed", err)
	}
	if out.Live != 7 {
		t.Errorf("Live = %d, want 7", out.Live)
	}
	if sw.callCount() != 1 {
		t.Errorf("switcher called %d times, want 1", sw.callCount())
	}
	if _, recorded := r.Seen(1); recorded {
		t.Errorf("a regressed version left a record, so the root could never make progress")
	}

	// The documented way out is a person on the child, not a message from
	// the root.
	if err := r.AdoptFloor(7, "root rebuilt"); err != nil {
		t.Fatalf("AdoptFloor: %v", err)
	}
	newRoot, newEnv := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.2.0")
	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 8, newEnv), newRoot); err != nil {
		t.Fatalf("apply after resync: %v", err)
	}
}

// TestAForgedBundleCannotBurnAVersion is the denial-of-service this design has
// to refuse.
//
// Anyone who can open a connection can claim any version number they like, and
// the cheapest bundle to send is one with no signature at all. If such a bundle
// were recorded as "refused", the attacker has bought themselves a permanent
// lock: the root's genuine version N can never be applied, because the
// receiver has already decided version N and replays that decision on
// redelivery. The rollout then fails in a way that looks like a transport
// problem and is chased as one.
func TestAForgedBundleCannotBurnAVersion(t *testing.T) {
	r, signer, sw := newHarness(t, "prod-cn-north")

	root, env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")

	// A bundle that claims version 9 and carries an envelope signed by a
	// key this cluster has never heard of.
	rogue := newSigner(t, "rogue-2026")
	rogueRoot, rogueEnv := newSignedTree(t, rogue, "opskeeper-sre-readonly", "1.0.0")
	forged := Bundle{
		ClusterID:      ClusterID("prod-cn-north"),
		Version:        9,
		PackageName:    rogueEnv.Name,
		PackageVersion: rogueEnv.Version,
		Envelope:       rogueEnv.Transport(),
	}
	if _, err := r.Apply(forged, rogueRoot); err == nil {
		t.Fatalf("a bundle signed by an untrusted key was applied")
	}
	if _, recorded := r.Seen(9); recorded {
		t.Fatalf("a forgery was remembered against version 9, so the root's real version 9 is now unappliable")
	}

	// A malformed bundle that claims the same version must not do better.
	broken := Bundle{ClusterID: ClusterID("prod-cn-north"), Version: 9}
	if _, err := r.Apply(broken, root); err == nil {
		t.Fatalf("a bundle with no envelope was applied")
	}
	if _, recorded := r.Seen(9); recorded {
		t.Fatalf("a malformed bundle was remembered against version 9")
	}

	// A tampered tree, likewise.
	tampered, tamperedEnv := newSignedTree(t, signer, "opskeeper-sre-readonly", "2.0.0")
	writeFile(t, filepath.Join(tampered, "extensions", "tool", "tools.go"),
		"package tool\n\n// swapped after signing\n")
	tamperBundle := Bundle{
		ClusterID:      ClusterID("prod-cn-north"),
		Version:        9,
		PackageName:    tamperedEnv.Name,
		PackageVersion: tamperedEnv.Version,
		Envelope:       tamperedEnv.Transport(),
	}
	if _, err := r.Apply(tamperBundle, tampered); err == nil {
		t.Fatalf("a tampered tree was applied")
	}
	if _, recorded := r.Seen(9); recorded {
		t.Fatalf("a tampered tree was remembered against version 9")
	}

	// The root's real version 9 still lands.
	real := Bundle{
		ClusterID:      ClusterID("prod-cn-north"),
		Version:        9,
		PackageName:    env.Name,
		PackageVersion: env.Version,
		Envelope:       env.Transport(),
	}
	if _, err := r.Apply(real, root); err != nil {
		t.Fatalf("the root's genuine version 9 was refused: %v", err)
	}
	if sw.callCount() != 1 {
		t.Errorf("the switcher ran %d times, want 1", sw.callCount())
	}
}

// TestAPolicyRefusalByARefusalByTheRootsOwnSigningKey IS remembered, and this
// is the other half of the rule above.
//
// The difference is provenance. A bundle the release key vouched for, which
// this cluster read correctly and will not permit, is a real decision about
// real policy, and recording it is what lets a redelivery replay the refusal
// instead of re-running a check — and what lets the root's accounting tell an
// operator that the rollout stopped rather than went quiet.
func TestAPolicyRefusalByTheRootsOwnKeyIsRemembered(t *testing.T) {
	r, signer, sw := newHarness(t, "prod-cn-north")

	root := filepath.Join(t.TempDir(), "opskeeper-sre-repair")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(root, pluginmanifest.ManifestFile), `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: opskeeper-sre-repair
  version: 2.0.0
  vendor: acme
  homepage: https://example.invalid/repair
spec:
  targets: [edge]
  safety_level: L3
  capabilities: [write]
  tools:
    - {name: restart_service, class: write}
  required_scopes:
    - host.write
  audit: {emits: true, mutates: false}
  approval: {required: true}
  install: {strategy: rolling, min_edge_version: 0.1.0}
`)
	writeFile(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool\n")
	env := signTree(t, signer, root)
	if _, err := r.Apply(bundleFor(t, ClusterID("prod-cn-north"), 4, env), root); err == nil {
		t.Fatalf("an L3 write package was applied to an L2 read-only cluster")
	}

	out, recorded := r.Seen(4)
	if !recorded {
		t.Fatalf("a genuine signed policy refusal was not remembered, so a redelivery would re-run the check")
	}
	if out.Accepted || out.Reason == "" {
		t.Errorf("recorded outcome = %+v, want a refusal with a reason", out)
	}
	if sw.callCount() != 0 {
		t.Errorf("the switcher ran %d times for a refused policy, want 0", sw.callCount())
	}
}
