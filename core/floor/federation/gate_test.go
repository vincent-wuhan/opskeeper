package federation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The gate's tests are about who decides.
//
// The signature answers "did the root's key vouch for these bytes". It does
// not answer "did the root mean for this cluster to run them", and those are
// different questions — a signed package is a package *somebody* published,
// and federation exists precisely so that a cluster does not run whatever its
// nodes happened to be offered. So the tests below build trees that are valid
// in every way except the one being varied, and assert on which of the three
// refusals comes back.

// buildPackage writes a valid, signed package into dir/name.
//
// Signed, because a tree only reaches the live link after Receiver.Apply
// reviewed it, and a fixture that skipped the signature would be testing a
// state the store cannot hold. The trust store is deliberately not returned:
// the gate has none, and a fixture that handed one back would invite a test
// asserting the gate uses it — which is the mistake this file's header is
// about.
func buildPackage(t *testing.T, dir, name, version, level string) string {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(root, pluginmanifest.ManifestFile), `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: `+name+`
  version: `+version+`
  vendor: acme
  homepage: https://example.invalid/`+name+`
spec:
  targets: [edge]
  safety_level: `+level+`
  capabilities: [read]
  tools:
    - {name: host_probe_tcp, class: read}
  required_scopes:
    - host.read
  audit: {emits: true, mutates: false}
  approval: {required: false}
  install: {strategy: rolling, min_edge_version: 0.1.0}
`)
	writeFile(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool\n\n// generated\n")

	signTree(t, newSigner(t, "acme-2026"), root)
	return root
}

// liveTree lays out a store the way the policy store does — versions/vN plus a
// live symlink — and returns the path of the link and the tree it names.
//
// The second return is the tree in force, not the versions directory, because
// that is the distinction the design turns on: v1 is history the moment v2
// lands, and a fixture that writes into versions/ has put nothing into the
// policy at all.
// gateLiveName mirrors edge/federation.LiveLinkName. It is spelled out here
// rather than imported because that constant belongs to the store, and this
// package is not allowed to depend on the store — the gate is handed a path
// precisely so that the question "what is the link called" stays the caller's.
const gateLiveName = "live"

func liveTree(t *testing.T) (link, tree string) {
	t.Helper()
	base := t.TempDir()
	tree = filepath.Join(base, "versions", "v1")
	if err := os.MkdirAll(tree, 0o750); err != nil {
		t.Fatalf("mkdir versions/v1: %v", err)
	}
	link = filepath.Join(base, gateLiveName)
	if err := os.Symlink(tree, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	return link, tree
}

func TestAGateNeedsThePathOfTheLiveLink(t *testing.T) {
	if _, err := NewLiveGate("  "); err == nil {
		t.Error("NewLiveGate accepted an empty path")
	}
}

// A cluster that has just been enrolled can install nothing, and saying so
// is the point. The alternative — treat a missing policy as permission — is
// how a child ends up running, for the minutes between hello and the first
// push, exactly the ungated set a root was supposed to be curating.
func TestAClusterWithNoPolicyYetCanInstallNothing(t *testing.T) {
	base := t.TempDir()
	gate, err := NewLiveGate(filepath.Join(base, gateLiveName))
	if err != nil {
		t.Fatalf("NewLiveGate: %v", err)
	}
	err = gate.Check("acme-probe", "1.0.0")
	if !errors.Is(err, ErrNoLivePolicy) {
		t.Fatalf("Check on an unenrolled cluster = %v, want ErrNoLivePolicy", err)
	}
	if _, err := gate.Carried(); !errors.Is(err, ErrNoLivePolicy) {
		t.Errorf("Carried on an unenrolled cluster = %v, want ErrNoLivePolicy", err)
	}
}

func TestTheLivePolicyIsTheGateOnAPackageTheRootPublished(t *testing.T) {
	link, tree := liveTree(t)
	buildPackage(t, tree, "acme-probe", "1.0.0", "L1")
	gate, err := NewLiveGate(link)
	if err != nil {
		t.Fatalf("NewLiveGate: %v", err)
	}
	if err := gate.Check("acme-probe", "1.0.0"); err != nil {
		t.Fatalf("a package the policy carries and the cluster trusts was refused: %v", err)
	}
}

// The refusal that carries the weight. This package is signed, its manifest
// is well-formed, and it would install on any single-cluster node without
// asking. It is refused here for one reason only: the root did not put it in
// the policy this cluster is enforcing.
func TestAPackageThePolicyDoesNotCarryIsRefusedEvenThoughItIsPerfectlySigned(t *testing.T) {
	link, tree := liveTree(t)
	buildPackage(t, tree, "acme-probe", "1.0.0", "L1")
	// A second, equally valid package that the root simply did not bless.
	buildPackage(t, t.TempDir(), "rogue-extra", "1.0.0", "L1")

	gate, err := NewLiveGate(link)
	if err != nil {
		t.Fatalf("NewLiveGate: %v", err)
	}
	err = gate.Check("rogue-extra", "1.0.0")
	if !errors.Is(err, ErrNotInPolicy) {
		t.Fatalf("an unblessed package = %v, want ErrNotInPolicy", err)
	}
}

// A version the policy does not carry is not the same package, and the
// message has to say which one is on offer — an operator who is told only
// that 2.0.0 was refused cannot tell "ask for 1.0.0" from "this cluster will
// never take it".
func TestAVersionThePolicyDoesNotCarrySaysWhichOneItDoes(t *testing.T) {
	link, tree := liveTree(t)
	buildPackage(t, tree, "acme-probe", "1.0.0", "L1")
	gate, err := NewLiveGate(link)
	if err != nil {
		t.Fatalf("NewLiveGate: %v", err)
	}
	err = gate.Check("acme-probe", "2.0.0")
	if !errors.Is(err, ErrNotInPolicy) {
		t.Fatalf("Check = %v, want ErrNotInPolicy", err)
	}
	if !strings.Contains(err.Error(), "1.0.0") {
		t.Errorf("refusal does not name the version the policy carries: %v", err)
	}
}

// ApplyPackage is handed no version, so an empty one must match whatever the
// policy carries rather than matching nothing. Getting this backwards would
// make the second half of every upgrade impossible on a federated cluster.
func TestAnEmptyVersionMatchesWhateverThePolicyCarries(t *testing.T) {
	link, tree := liveTree(t)
	buildPackage(t, tree, "acme-probe", "1.0.0", "L1")
	gate, err := NewLiveGate(link)
	if err != nil {
		t.Fatalf("NewLiveGate: %v", err)
	}
	if err := gate.Check("acme-probe", ""); err != nil {
		t.Fatalf("a versionless check against a policy that carries the package: %v", err)
	}
	if err := gate.Check("acme-absent", ""); !errors.Is(err, ErrNotInPolicy) {
		t.Fatalf("a versionless check for an absent package = %v, want ErrNotInPolicy", err)
	}
}

// The link is the state. A gate that cached its answer would keep permitting
// v1's package after v2 withdrew it, which is the one failure this whole
// mechanism cannot have.
func TestASwapChangesTheAnswerImmediately(t *testing.T) {
	link, tree := liveTree(t)
	buildPackage(t, tree, "acme-probe", "1.0.0", "L1")

	v2 := filepath.Join(filepath.Dir(tree), "v2")
	if err := os.MkdirAll(v2, 0o750); err != nil {
		t.Fatalf("mkdir v2: %v", err)
	}
	gate, err := NewLiveGate(link)
	if err != nil {
		t.Fatalf("NewLiveGate: %v", err)
	}
	if err := gate.Check("acme-probe", "1.0.0"); err != nil {
		t.Fatalf("before the swap: %v", err)
	}

	// v2 is an empty policy: the root withdrew the package.
	next := link + ".next"
	if err := os.Symlink(v2, next); err != nil {
		t.Fatalf("symlink next: %v", err)
	}
	if err := os.Rename(next, link); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := gate.Check("acme-probe", "1.0.0"); !errors.Is(err, ErrNotInPolicy) {
		t.Fatalf("after the swap = %v; a cached gate would still be allowing it", err)
	}
}

// The listing names packages; it does not rule on them. A console that
// displayed a verdict computed here would be showing an operator a decision
// this process is not entitled to make, so Carried returns no Allowed field
// at all and a test that tried to read one would not compile.
func TestTheListingNamesPackagesWithoutRulingOnThem(t *testing.T) {
	link, tree := liveTree(t)
	buildPackage(t, tree, "acme-probe", "1.0.0", "L1")
	buildPackage(t, tree, "acme-other", "2.1.0", "L1")
	gate, err := NewLiveGate(link)
	if err != nil {
		t.Fatalf("NewLiveGate: %v", err)
	}
	carried, err := gate.Carried()
	if err != nil {
		t.Fatalf("Carried: %v", err)
	}
	if len(carried) != 2 {
		t.Fatalf("Carried listed %d, want 2", len(carried))
	}
	want := map[string]string{"acme-probe": "1.0.0", "acme-other": "2.1.0"}
	for _, c := range carried {
		if want[c.Name] != c.Version {
			t.Errorf("carried %s@%s, want %s@%s", c.Name, c.Version, c.Name, want[c.Name])
		}
	}
	// The listing and the gate must not disagree about membership either:
	// a package the listing shows and the gate refuses is a console that
	// cannot be believed.
	for _, c := range carried {
		if err := gate.Check(c.Name, c.Version); err != nil {
			t.Errorf("the gate refuses %s@%s, which the policy carries: %v", c.Name, c.Version, err)
		}
	}
}
