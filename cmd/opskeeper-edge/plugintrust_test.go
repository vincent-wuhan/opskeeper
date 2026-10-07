package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The node's end of the review pipeline.
//
// The library's tests prove the pipeline refuses what it should. These
// prove the *node* runs that pipeline — that configuring a trust store
// actually turns signature checking on, and that the safety ceiling
// actually keeps the L2 package off a host that was not given it. Both
// are wiring, and wiring is where a correct component gets left unused.

// shippedPackagesRoot locates the packages the repository ships, so these
// tests check that what we actually release is admissible.
func shippedPackagesRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "plugins", "pig-ops")
}

// signPackage signs a package directory and writes the sidecar into it.
func signPackage(t *testing.T, root string) *pluginmanifest.Signer {
	t.Helper()
	signer, _, err := pluginmanifest.GenerateSigner("acme-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	env, err := signer.Sign(root)
	if err != nil {
		t.Fatalf("Sign %s: %v", root, err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	t.Cleanup(func() {
		// The signature lives inside the package it signs, so a test that
		// leaves one behind changes what the next test in this package
		// sees. Removing it is not tidiness; it is the only way the
		// tests are independent.
		_ = os.Remove(filepath.Join(root, pluginmanifest.SignatureFile))
	})
	return signer
}

// trustStoreWith writes a trust store file for keyID and returns its path.
func trustStoreWith(t *testing.T, keyID string, pub ed25519.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trust.yaml")
	body := "keys:\n  - id: " + keyID + "\n    key: " + base64.StdEncoding.EncodeToString(pub) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write trust store: %v", err)
	}
	return path
}

func TestANodeWithATrustStoreRefusesAnUnsignedPackage(t *testing.T) {
	// The switch. A node that has been told who it trusts must not install
	// anything else, and the package here is one this repository ships —
	// so "it is our own package" is not an excuse the node may apply.
	base := t.TempDir()
	root := writePackage(t, base, "readonly", strings.Replace(readonlyManifest, "%s", "readonly", 1))

	_, signerPub, err := pluginmanifest.GenerateSigner("someone-else")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	t.Setenv("OPSKEEPER_EDGE_TRUST_STORE", trustStoreWith(t, "someone-else", signerPub))

	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("nodePluginPolicy: %v", err)
	}
	trust := loadTrustStore()
	if _, err := admitPackages([]string{root}, trust, pol); err == nil {
		t.Fatal("a node with a trust store installed an unsigned package")
	}
}

func TestANodeWithATrustStoreInstallsAProperlySignedPackage(t *testing.T) {
	// The other half: the switch is not a wall, it is a check. A signed
	// package from a trusted key installs exactly as an unsigned one did
	// before, or operators would turn the trust store off.
	base := t.TempDir()
	root := writePackage(t, base, "readonly", strings.Replace(readonlyManifest, "%s", "readonly", 1))
	signer := signPackage(t, root)

	t.Setenv("OPSKEEPER_EDGE_TRUST_STORE", trustStoreWith(t, signer.KeyID(), signer.PublicKey()))
	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("nodePluginPolicy: %v", err)
	}
	got, err := admitPackages([]string{root}, loadTrustStore(), pol)
	if err != nil {
		t.Fatalf("a properly signed package was refused: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("admitted %d packages, want 1", len(got))
	}
}

func TestANodeWithATrustStoreRefusesAPackageChangedAfterSigning(t *testing.T) {
	base := t.TempDir()
	root := writePackage(t, base, "readonly", strings.Replace(readonlyManifest, "%s", "readonly", 1))
	signer := signPackage(t, root)

	// The reviewed package grew a tool after it was signed. This is the
	// single most valuable thing the signature check does, and it is worth
	// proving on the node rather than only in the library.
	edited := strings.Replace(readonlyManifest, "%s", "readonly", 1) +
		"  tools:\n    - {name: host_restart_service, class: write}\n"
	if err := os.WriteFile(filepath.Join(root, "pig-ops.yaml"), []byte(edited), 0o640); err != nil {
		t.Fatalf("edit manifest: %v", err)
	}

	t.Setenv("OPSKEEPER_EDGE_TRUST_STORE", trustStoreWith(t, signer.KeyID(), signer.PublicKey()))
	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("nodePluginPolicy: %v", err)
	}
	if _, err := admitPackages([]string{root}, loadTrustStore(), pol); err == nil {
		t.Fatal("a package whose manifest was widened after signing was installed")
	}
}

func TestATrustStoreThatCannotBeReadIsAHardErrorRatherThanADowngrade(t *testing.T) {
	// The failure this guards against: an operator points at a trust store,
	// the path is wrong, and the node quietly carries on trusting nobody —
	// which on this node means allowing unsigned packages. A security
	// control that fails open on a typo is not a control.
	t.Setenv("OPSKEEPER_EDGE_TRUST_STORE", filepath.Join(t.TempDir(), "absent.yaml"))

	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("nodePluginPolicy: %v", err)
	}
	if _, err := admitPackages([]string{t.TempDir()}, loadTrustStore(), pol); err == nil {
		t.Error("a node with an unreadable trust store started anyway")
	}
}

func TestTheSafetyCeilingKeepsTheRepairPackageOffANodeThatWasNotGivenIt(t *testing.T) {
	// The B3 package is opt-in, and the opt-in is this setting. A node
	// that has never been told it may change things must not install the
	// package that can.
	base := t.TempDir()
	root := writePackage(t, base, "repair", strings.Replace(repairManifest, "%s", "repair", 1))

	t.Setenv("OPSKEEPER_EDGE_MAX_SAFETY_LEVEL", "")
	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("nodePluginPolicy: %v", err)
	}
	if pol.MaxSafetyLevel != domain.SafetyL1 {
		t.Fatalf("the default ceiling is %q, want L1", pol.MaxSafetyLevel)
	}
	if _, err := admitPackages([]string{root}, pluginmanifest.NewTrustStore(), pol); err == nil {
		t.Fatal("the L2 repair package was installed on a node whose ceiling is L1")
	}
}

func TestTheSafetyCeilingCanBeRaisedDeliberately(t *testing.T) {
	// …and the opt-in works, or the default above is a wall rather than a
	// default.
	base := t.TempDir()
	root := writePackage(t, base, "repair", strings.Replace(repairManifest, "%s", "repair", 1))

	t.Setenv("OPSKEEPER_EDGE_MAX_SAFETY_LEVEL", "L2")
	t.Setenv("OPSKEEPER_EDGE_MAX_BLAST_RADIUS", "pod")
	t.Setenv("OPSKEEPER_EDGE_PLUGIN_SCOPES", "host.read,host.write")
	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("nodePluginPolicy: %v", err)
	}
	if _, err := admitPackages([]string{root}, pluginmanifest.NewTrustStore(), pol); err != nil {
		t.Fatalf("the repair package was refused on a node configured to host it: %v", err)
	}
}

func TestAnUnrecognisedCeilingIsABootErrorRatherThanADefault(t *testing.T) {
	// A typo in a capability ceiling is a node whose policy is not the one
	// the operator believes it has. Guessing either way is wrong: guessing
	// low refuses everything and looks like a broken plugin, guessing high
	// hosts more than was asked for and looks like nothing.
	for _, key := range []string{
		"OPSKEEPER_EDGE_MAX_SAFETY_LEVEL", "OPSKEEPER_EDGE_MAX_BLAST_RADIUS",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "not-a-value")
			if _, err := nodePluginPolicy(); err == nil {
				t.Errorf("an unrecognised %s was accepted", key)
			}
		})
	}
}

func TestANodeGrantsOnlyTheScopesItsFirstPartyPackageNeeds(t *testing.T) {
	// A third-party package is refused until somebody widens the grant,
	// which is what makes installing one a decision.
	t.Setenv("OPSKEEPER_EDGE_PLUGIN_SCOPES", "")
	pol, err := nodePluginPolicy()
	if err != nil {
		t.Fatalf("nodePluginPolicy: %v", err)
	}
	if !pol.GrantedScopes.Has(domain.ScopeHostRead) {
		t.Error("the default grant does not include host.read, so the shipped read-only package would not install")
	}
	for _, s := range pol.GrantedScopes {
		switch s {
		case domain.ScopeHostWrite, domain.ScopeDBWrite, domain.ScopeMQWrite,
			domain.ScopeAlertWrite, domain.ScopeK8sExec:
			t.Errorf("the default grant includes the mutating scope %q", s)
		}
	}

	base := t.TempDir()
	root := writePackage(t, base, "greedy", fillTemplate(writeScopeManifest, "greedy", "db.write"))
	if _, err := admitPackages([]string{root}, pluginmanifest.NewTrustStore(), pol); err == nil {
		t.Error("a package asking for db.write was installed on a node that grants only reads")
	}
}

func TestTheShippedPackagesInstallOnANodeWithNoConfiguration(t *testing.T) {
	// The compatibility check that matters on every upgrade. A node that
	// booted before this change has no trust store and has not read any of
	// the new environment variables, and it must still come up.
	entries, err := os.ReadDir(shippedPackagesRoot(t))
	if err != nil {
		t.Fatalf("read the shipped packages: %v", err)
	}
	roots := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			roots = append(roots, filepath.Join(shippedPackagesRoot(t), e.Name()))
		}
	}
	if len(roots) == 0 {
		t.Fatal("no shipped packages found; this test is vacuous")
	}

	for _, key := range []string{
		"OPSKEEPER_EDGE_TRUST_STORE", "OPSKEEPER_EDGE_MAX_SAFETY_LEVEL",
		"OPSKEEPER_EDGE_MAX_BLAST_RADIUS", "OPSKEEPER_EDGE_PLUGIN_SCOPES",
	} {
		t.Setenv(key, "")
	}
	// The version is left as TestMain set it. A node always knows what it
	// runs — that is a property of the binary, not of configuration — and
	// the shipped packages all declare min_edge_version. Clearing it here
	// would make this a test about version reporting rather than about
	// policy defaults.
	// The production boot policy, so a wiring mistake is what this test
	// catches rather than a hand-built policy that cannot be wrong.
	pol, err := nodeBootPolicy("0.8.0")
	if err != nil {
		t.Fatalf("nodeBootPolicy: %v", err)
	}
	trust := loadTrustStore()

	// The read-only package is what every node runs, so it must install
	// unconfigured. The repair package must not — that is the opt-in, and
	// it is checked above.
	if _, err := admitPackages([]string{filepath.Join(shippedPackagesRoot(t), "opskeeper-sre-readonly")},
		trust, pol); err != nil {
		t.Fatalf("the shipped read-only package is not installable on a default node: %v", err)
	}
	if _, err := admitPackages([]string{filepath.Join(shippedPackagesRoot(t), "opskeeper-sre-repair")},
		trust, pol); err == nil {
		t.Error("the shipped repair package installed on a node that was never told it may change things")
	}
}

// repairManifest is an L2 package: it may change things, so it needs the
// scopes a read-only package does not.
const repairManifest = `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: %s
  version: 0.1.0
  vendor: opskeeper
spec:
  targets: [edge]
  safety_level: L2
  capabilities: [read, write]
  tools:
    - {name: host_probe_tcp, class: read}
    - {name: host_restart_service, class: write}
  required_scopes: [host.read, host.write]
  audit:
    emits: true
    mutates: false
  approval:
    required: true
    max_blast_radius: pod
  install:
    strategy: pin
`

// fillTemplate fills a two-placeholder manifest template. There are two
// of them so a test can name the package and choose the scope it asks for
// without string-splicing a manifest, which would make a typo in the
// template a test failure that looks like a policy failure.
func fillTemplate(tmpl, name, scope string) string {
	return strings.ReplaceAll(strings.Replace(tmpl, "%s", name, 1), "%s", scope)
}

// writeScopeManifest asks for a scope no default node grants.
const writeScopeManifest = `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: %s
  version: 0.1.0
  vendor: acme
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  required_scopes: [%s]
  audit:
    emits: true
    mutates: false
  approval:
    required: false
  install:
    strategy: rolling
`
