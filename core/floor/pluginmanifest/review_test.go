package pluginmanifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The review pipeline's tests are mostly about order.
//
// Each step is individually correct — the sdk validates a manifest, the
// signature verifies a tree, admission checks a policy — and the security
// of the whole thing rests on the sequence. A pipeline that admits first
// and verifies second would pass every one of these step-level tests and
// would be worthless, so the tests below are written to fail if the order
// changes.

// testNodeVersion is what the fixtures report as the node's own agent
// version. It is above every min_edge_version the test helpers write
// (0.1.0), so a test about ordering is not also a test about
// compatibility — and a node that reports no version at all is asserted on
// its own, in version_test.go.
const testNodeVersion = "0.8.0"

// testPigVersion is the agent build the fixtures model. It is spelled
// like a PiG release so a package that declares a min_pig_version can be
// written without the test also being about which number this is.
const testPigVersion = "0.3.0"

// signingPolicy is a policy that admits the L1 packages the test helpers
// build, so a test about ordering is not also a test about admission.
func signingPolicy() Policy {
	return testPolicy(domain.SafetyL3, domain.RadiusCluster,
		domain.Scopes{domain.ScopeHostRead, domain.ScopeHostWrite})
}

// testPolicy is PolicyFor plus the node's own agent version.
//
// Every real node has one, and every package the helpers build asks for
// min_edge_version 0.1.0 — so a fixture that left the version empty would
// be refused for a reason the test is not about. The fail-closed default
// for an unknown version is asserted on its own in version_test.go.
func testPolicy(max domain.SafetyLevel, radius domain.BlastRadius, scopes domain.Scopes) Policy {
	pol := PolicyFor(max, radius, scopes)
	pol.NodeVersion = testNodeVersion
	// The agent's version is a different number from the edge's, and a
	// real node states both. The fixture helpers write no
	// min_pig_version, so this is not load-bearing for them — it is here
	// so a test that adds one is testing the rule rather than a fixture
	// that forgot to model the node.
	pol.PigVersion = testPigVersion
	return pol
}

// signedPackage builds a valid, signed L1 package and returns it with a
// store that trusts its signer.
func signedPackage(t *testing.T, name string) (string, *TrustStore) {
	t.Helper()
	root := newPackage(t, name, "1.0.0")
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return root, trustFor(t, s)
}

func TestASignedPackageThatFitsThePolicyIsAllowed(t *testing.T) {
	root, store := signedPackage(t, "acme-probe")

	d := Review(root, store, signingPolicy())
	if !d.Allowed {
		t.Fatalf("a signed, in-policy package was refused: %s", d)
	}
	if d.Plugin != "acme-probe" || d.Version != "1.0.0" {
		t.Errorf("decision names %s v%s, want acme-probe v1.0.0", d.Plugin, d.Version)
	}
	if d.Manifest.Metadata.Name != "acme-probe" {
		t.Errorf("the decision carries manifest %q; an allowed decision must carry what it allowed",
			d.Manifest.Metadata.Name)
	}
	if d.Envelope.KeyID != "acme-2026" {
		t.Errorf("the decision carries key %q, want the one that actually signed", d.Envelope.KeyID)
	}
}

func TestTheSignatureIsCheckedBeforeTheManifestIsBelieved(t *testing.T) {
	// An unsigned package whose manifest would be perfectly acceptable is
	// still refused, and refused at the *signature* step. If the manifest
	// were parsed first and trusted, this package would be admitted.
	//
	// The directory is deliberately named differently from the package
	// inside it, so the assertion can tell the two apart. A decision that
	// reported the manifest's name at this point would be reporting
	// something it has no reason to believe yet.
	root := newPackage(t, "acme-probe", "1.0.0")
	renamed := filepath.Join(filepath.Dir(root), "download-0042")
	if err := os.Rename(root, renamed); err != nil {
		t.Fatalf("rename: %v", err)
	}
	root = renamed

	d := Review(root, NewTrustStore(), signingPolicy())
	if d.Allowed {
		t.Fatal("an unsigned package was admitted by a policy that would have accepted it")
	}
	if d.Step != StepSignature {
		t.Errorf("step = %q, want %q — the manifest must not be read before the tree is authenticated",
			d.Step, StepSignature)
	}
	if want := filepath.Base(root); d.Plugin != want {
		t.Errorf("the refused decision names %q, want the directory's name %q; the manifest "+
			"declares %q and nothing at this step has earned the right to repeat it",
			d.Plugin, want, "acme-probe")
	}
}

func TestAPackageExceedingTheNodePolicyIsRefusedAtAdmissionNotSignature(t *testing.T) {
	// The mirror image, and the reason the two steps are separate. This
	// package is genuine — right key, unmodified tree, valid manifest — and
	// it is still refused. Getting here is the point: the failure an
	// operator sees must name the policy ceiling, not accuse them of
	// running a forgery.
	root := t.TempDir()
	writeManifest(t, root, "acme-repair", "1.0.0", "L3", "write")
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	d := Review(root, trustFor(t, s), testPolicy(domain.SafetyL1, domain.RadiusNone,
		domain.Scopes{domain.ScopeHostRead}))
	if d.Allowed {
		t.Fatal("an L3 package was installed on a node whose ceiling is L1")
	}
	if d.Step != StepAdmission {
		t.Errorf("step = %q, want %q", d.Step, StepAdmission)
	}
	if d.Plugin != "acme-repair" {
		t.Errorf("the refusal names %q, want the package it refused; the signature was fine", d.Plugin)
	}
}

func TestTheSignatureIsCheckedBeforeAdmissionNotTheOtherWayRound(t *testing.T) {
	// The same package, unsigned. It would be refused either way, so the
	// *reason* is the assertion: an unsigned package must be caught by its
	// provenance, not by whatever the policy happened to say about it.
	// Otherwise a node that is misconfigured to be permissive would
	// silently start installing unsigned packages.
	root := t.TempDir()
	writeManifest(t, root, "acme-repair", "1.0.0", "L3", "write")

	d := Review(root, NewTrustStore(), testPolicy(domain.SafetyL3, domain.RadiusCluster,
		domain.Scopes{domain.ScopeHostRead, domain.ScopeHostWrite}))
	if d.Allowed {
		t.Fatal("an unsigned L3 package was installed on a permissive node")
	}
	if d.Step != StepSignature {
		t.Errorf("step = %q, want %q — a permissive policy must not become a signing bypass", d.Step, StepSignature)
	}
}

func TestAnUnsignedPackageIsAllowedOnlyWhereThatWasAskedFor(t *testing.T) {
	// The development node. It has to exist, or nobody can iterate on a
	// plugin — but it has to be a decision, and the decision has to be
	// visible afterwards.
	root := newPackage(t, "acme-probe", "1.0.0")

	pol := signingPolicy()
	d := Review(root, NewTrustStore(), pol)
	if d.Allowed {
		t.Fatal("an unsigned package was admitted by the default policy")
	}

	pol.AllowUnsigned = true
	d = Review(root, NewTrustStore(), pol)
	if !d.Allowed {
		t.Fatalf("a development node refused an unsigned package it was told to accept: %s", d)
	}
	if d.Envelope.KeyID != "" {
		t.Errorf("the decision carries key %q; an unsigned install must not look like a verified one",
			d.Envelope.KeyID)
	}
}

func TestATamperedPackageIsRefusedEvenWhereUnsignedIsAllowed(t *testing.T) {
	// AllowUnsigned is a licence to skip the *check*, not to skip
	// reality. A package that carries a signature and then has its code
	// changed is still a package that failed verification, and a node
	// configured for development must not become a node that installs
	// whatever is on the disk.
	root, store := signedPackage(t, "acme-probe")
	write(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool\n\n// replaced\n")

	pol := signingPolicy()
	pol.AllowUnsigned = true
	d := Review(root, store, pol)
	if d.Allowed {
		t.Fatal("a package with a broken signature was installed on a node that allows unsigned ones")
	}
	if d.Step != StepSignature {
		t.Errorf("step = %q, want %q", d.Step, StepSignature)
	}
}

func TestAVendorRestrictionNamesItself(t *testing.T) {
	// A package from a publisher this node does not work with is refused
	// with a reason the operator can act on. "Signature invalid" would
	// send them looking for a forgery that does not exist.
	root, store := signedPackage(t, "acme-probe")

	pol := signingPolicy()
	pol.AllowedVendors = []string{"opskeeper"}
	d := Review(root, store, pol)
	if d.Allowed {
		t.Fatal("a package from an unlisted vendor was admitted")
	}
	if d.Step != StepAdmission {
		t.Errorf("step = %q, want %q", d.Step, StepAdmission)
	}
	if !strings.Contains(d.Reason, "acme") {
		t.Errorf("reason = %q, want it to name the vendor that was refused", d.Reason)
	}

	pol.AllowedVendors = []string{"ACME"} // vendor matching is case-insensitive
	if d := Review(root, store, pol); !d.Allowed {
		t.Errorf("a listed vendor was refused on case alone: %s", d)
	}
}

func TestAManifestThatBreaksARuleIsRefusedEvenThoughItIsSigned(t *testing.T) {
	// A key is not a substitute for validation.
	//
	// This manifest is well-formed YAML with a real identity — so it can
	// be signed, and it is, with the node's own trusted key. What it is
	// not is valid: it declares a tool far above the capability ceiling it
	// claims. A pipeline that treated "signed" as "approved" would admit
	// it, and the node's allow-list would then contain a tool the
	// publisher never reviewed.
	root := filepath.Join(t.TempDir(), "acme-probe")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(t, filepath.Join(root, ManifestFile), `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: acme-probe
  version: 1.0.0
  vendor: acme
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  tools:
    - {name: host_probe_tcp, class: read}
    - {name: host_restart_service, class: destructive}
  required_scopes: [host.read]
  audit: {emits: true, mutates: false}
  approval: {required: false}
  install: {strategy: rolling}
`)
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	d := Review(root, trustFor(t, s), signingPolicy())
	if d.Allowed {
		t.Fatal("a package whose manifest declares a tool above its own ceiling was admitted")
	}
	if d.Step != StepManifest {
		t.Errorf("step = %q, want %q — the signature is sound, so the manifest is what refused it", d.Step, StepManifest)
	}
	if d.Plugin != "acme-probe" {
		t.Errorf("decision names %q, want the manifest's own name; by now it has been read", d.Plugin)
	}
}

func TestAMissingScopeIsNamedRatherThanSummarised(t *testing.T) {
	// The refusal an operator has to act on. "some scopes are missing" is
	// not actionable; the missing names are.
	root, store := signedPackage(t, "acme-probe")

	d := Review(root, store, testPolicy(domain.SafetyL3, domain.RadiusCluster,
		domain.Scopes{domain.ScopeTopologyRO}))
	if d.Allowed {
		t.Fatal("a package was admitted without the scopes it declares")
	}
	if d.Step != StepAdmission {
		t.Errorf("step = %q, want %q", d.Step, StepAdmission)
	}
	if !strings.Contains(d.Reason, string(domain.ScopeHostRead)) {
		t.Errorf("reason = %q, want it to name host.read as the scope that was not granted", d.Reason)
	}
}

func TestReviewAllReportsEveryPackageAndNotJustTheFirst(t *testing.T) {
	// A listing that stops at one bad package makes an operator believe
	// the rest are fine, which is the opposite of what they need.
	base := t.TempDir()
	good, store := signedPackage(t, "acme-good")
	copyTree(t, good, filepath.Join(base, "acme-good"))

	bad := newPackage(t, "acme-unsigned", "1.0.0")
	copyTree(t, bad, filepath.Join(base, "acme-unsigned"))

	decisions := ReviewAll(base, store, signingPolicy())
	if len(decisions) != 2 {
		t.Fatalf("got %d decisions, want one per package", len(decisions))
	}
	byName := map[string]Decision{}
	for _, d := range decisions {
		byName[d.Plugin] = d
	}
	if d := byName["acme-good"]; !d.Allowed {
		t.Errorf("the signed package was refused: %s", d)
	}
	if d := byName["acme-unsigned"]; d.Allowed {
		t.Error("the unsigned package was allowed")
	} else if d.Step != StepSignature {
		t.Errorf("the unsigned package was refused at %q, want %q", d.Step, StepSignature)
	}
}

func TestARefusedDecisionRendersSomethingAnOperatorCanRead(t *testing.T) {
	// Every refusal ends up in a log during an incident. The line has to
	// carry the package, the step and the reason, because the caller has
	// an id or two and no other context.
	root := newPackage(t, "acme-probe", "1.0.0")
	d := Review(root, NewTrustStore(), signingPolicy())

	// The package is named by its directory, because at the signature step
	// nothing about the package has been trusted yet — including whatever
	// its manifest calls itself. The operator still needs to know which
	// folder this was.
	line := d.String()
	for _, want := range []string{"acme-probe", StepSignature, "refused"} {
		if !strings.Contains(line, want) {
			t.Errorf("refusal line = %q, want it to mention %q", line, want)
		}
	}
}

// copyTree copies a package directory, which is how a test builds a
// catalog of several packages without a helper that knows about signing.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyTree(t, s, d)
			continue
		}
		body, err := os.ReadFile(s)
		if err != nil {
			t.Fatalf("read %s: %v", s, err)
		}
		if err := os.WriteFile(d, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", d, err)
		}
	}
}

func TestANodeThatCannotStateItsVersionRefusesAtTheCompatibilityStep(t *testing.T) {
	// The review is where this lands, so this is where it has to be
	// locked. A package asking for a minimum and a node that has not said
	// what it runs is the fail-closed case: the node would be guessing,
	// and the guess that is wrong in the permissive direction installs a
	// package the node cannot host.
	//
	// The step is asserted, not just the refusal, because "refused" alone
	// would be satisfied by catching it at admission — which would send an
	// operator to look at the node's policy ceiling for a problem that is
	// about the node's binary.
	root, store := signedPackage(t, "acme-probe")

	pol := PolicyFor(domain.SafetyL3, domain.RadiusCluster,
		domain.Scopes{domain.ScopeHostRead, domain.ScopeHostWrite})
	// No NodeVersion: this node cannot say what it is.
	d := Review(root, store, pol)
	if d.Allowed {
		t.Fatal("a node with no version admitted a package that declares a minimum")
	}
	if d.Step != StepVersion {
		t.Errorf("step = %q, want %q", d.Step, StepVersion)
	}
	if !strings.Contains(d.Reason, "min_edge_version") && !strings.Contains(d.Reason, "0.1.0") {
		t.Errorf("reason %q does not name the package's requirement, so an operator cannot act on it", d.Reason)
	}
}

func TestTheSamePackageIsAdmittedOnceTheNodeCanStateItsVersion(t *testing.T) {
	// The other half of the pair, so the test above cannot pass by
	// refusing everything. The only difference between the two is that
	// this node reports a version.
	root, store := signedPackage(t, "acme-probe")

	pol := PolicyFor(domain.SafetyL3, domain.RadiusCluster,
		domain.Scopes{domain.ScopeHostRead, domain.ScopeHostWrite})
	pol.NodeVersion = testNodeVersion
	if d := Review(root, store, pol); !d.Allowed {
		t.Fatalf("a node that states its version refused a package it can host: %s", d)
	}
}

// ---------------------------------------------------------------------
// the agent build is a second, independent axis
// ---------------------------------------------------------------------

func TestANodeThatCannotStateItsPiGVersionRefusesAtTheAgentStep(t *testing.T) {
	// The step matters. Both version refusals are answered by upgrading a
	// binary, and they are different binaries — an agent-axis refusal
	// reported as the edge step would have an operator roll the edge
	// package to fix a node whose problem is the `pig` on its PATH.
	root, store := signedPackageWithMinPig(t, "acme-pigprobe", "0.4.0")

	pol := testPolicy(domain.SafetyL3, domain.RadiusCluster,
		domain.Scopes{domain.ScopeHostRead, domain.ScopeHostWrite})
	pol.PigVersion = "" // this node cannot say what its agent is

	d := Review(root, store, pol)
	if d.Allowed {
		t.Fatal("a node with no PiG version admitted a package that declares a min_pig_version")
	}
	if d.Step != StepAgentVersion {
		t.Errorf("step = %q, want %q — an operator cannot tell which binary to upgrade from %q",
			d.Step, StepAgentVersion, d.Step)
	}
	if !strings.Contains(d.Reason, "0.4.0") {
		t.Errorf("reason %q does not name the package's requirement", d.Reason)
	}
}

func TestAPiGUpdateAloneAdmitsThePackage(t *testing.T) {
	// The other half of the pair, and the property that proves the axes
	// are independent: the edge version is unchanged and only the agent
	// moved, and that is enough. A check that read NodeVersion on both
	// axes would still refuse here.
	root, store := signedPackageWithMinPig(t, "acme-pigprobe2", "0.4.0")

	pol := testPolicy(domain.SafetyL3, domain.RadiusCluster,
		domain.Scopes{domain.ScopeHostRead, domain.ScopeHostWrite})
	pol.NodeVersion = testNodeVersion // unchanged and already new enough
	pol.PigVersion = ""               // still unknown: refused
	if d := Review(root, store, pol); d.Allowed {
		t.Fatal("precondition: the unknown agent version must be refused first")
	}
	pol.PigVersion = "0.5.0" // only the agent moved
	if d := Review(root, store, pol); !d.Allowed {
		t.Fatalf("an agent upgrade alone did not admit the package: %s", d)
	}
}

func TestAnEdgeUpgradeAloneDoesNotClearTheAgentStep(t *testing.T) {
	// The mirror image, and the one that catches the tempting
	// simplification of using the edge's version for both checks.
	root, store := signedPackageWithMinPig(t, "acme-pigprobe3", "0.4.0")

	pol := testPolicy(domain.SafetyL3, domain.RadiusCluster,
		domain.Scopes{domain.ScopeHostRead, domain.ScopeHostWrite})
	pol.NodeVersion = "9.9.9" // the edge is far ahead
	pol.PigVersion = "0.3.0"  // and the agent is not
	d := Review(root, store, pol)
	if d.Allowed {
		t.Fatal("a new edge admitted a package the node's agent is too old to host")
	}
	if d.Step != StepAgentVersion {
		t.Errorf("step = %q, want %q", d.Step, StepAgentVersion)
	}
}

// signedPackageWithMinPig builds a signed package whose install block
// declares a min_pig_version.
//
// It writes the field before signing rather than editing afterwards,
// because the signature covers the whole tree: a manifest edited after
// signing would be refused at the signature step, and the test would be
// asserting the wrong rule.
func signedPackageWithMinPig(t *testing.T, name, minPig string) (string, *TrustStore) {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: ` + name + `
  version: 1.0.0
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
  install: {strategy: rolling, min_edge_version: 0.1.0, min_pig_version: ` + minPig + `}
`
	write(t, filepath.Join(root, ManifestFile), manifest)
	write(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool\n\n// generated\n")

	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return root, trustFor(t, s)
}
