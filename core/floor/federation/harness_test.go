package federation

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The helpers here build real signed trees on a real filesystem and sign them
// with a real ed25519 key. That is deliberate: the properties under test are
// about what happens when bytes on disk do not match a signature, and a fake
// that computes a digest over a map would pass a version of this code that
// fails on a real package.

// newSignedTree writes a valid plugin package, signs it, and returns its root
// and the envelope the signature produced.
func newSignedTree(t *testing.T, signer *pluginmanifest.Signer, name, version string) (string, pluginmanifest.Envelope) {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
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
  safety_level: L1
  capabilities: [read]
  tools:
    - {name: host_probe_tcp, class: read}
  required_scopes:
    - host.read
  audit: {emits: true, mutates: false}
  approval: {required: false}
  install: {strategy: rolling, min_edge_version: 0.1.0}
`
	writeFile(t, filepath.Join(root, pluginmanifest.ManifestFile), manifest)
	writeFile(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool\n\n// generated\n")

	return root, signTree(t, signer, root)
}

// signTree signs a root and writes the sidecar into it.
//
// Both halves, because they are two halves: Sign produces the envelope and
// WriteTo puts it where VerifyDir looks for it. A tree that was signed but not
// written is an unsigned tree as far as every reader of this package is
// concerned, which is the same property that stops a bundle from shipping its
// own copy of the key.
func signTree(t *testing.T, signer *pluginmanifest.Signer, root string) pluginmanifest.Envelope {
	t.Helper()
	env, err := signer.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return env
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newSigner(t *testing.T, keyID string) *pluginmanifest.Signer {
	t.Helper()
	s, _, err := pluginmanifest.GenerateSigner(keyID)
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	return s
}

func trustFor(t *testing.T, signers ...*pluginmanifest.Signer) *pluginmanifest.TrustStore {
	t.Helper()
	store := pluginmanifest.NewTrustStore()
	for _, s := range signers {
		if err := store.Trust(s.KeyID(), s.PublicKey()); err != nil {
			t.Fatalf("Trust: %v", err)
		}
	}
	return store
}

// testPolicy is the policy a child cluster in these tests enforces.
//
// The two version fields are not decoration. pluginmanifest refuses a package
// whose min_edge_version cannot be shown to be met, and it refuses rather than
// assumes in the permissive direction — so a policy that leaves them blank
// refuses everything, which is the correct default and a confusing one to
// rediscover. A real child fills them from what it reports at registration.
func testPolicy() pluginmanifest.Policy {
	p := pluginmanifest.PolicyFor(domain.SafetyL2, domain.RadiusNamespace,
		domain.Scopes{domain.ScopeHostRead})
	p.NodeVersion = "0.4.0"
	p.PigVersion = "0.3.0"
	return p
}

// switcher records what it was asked to promote and can be made to fail.
type switcher struct {
	mu       sync.Mutex
	live     string
	calls    int
	failWith error
}

func (s *switcher) Switch(staged string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		// The contract: on error the previous tree is still live. A
		// real implementation gets this from an atomic rename; the fake
		// gets it from not touching live.
		return s.live, s.failWith
	}
	s.calls++
	s.live = staged
	return staged, nil
}

func (s *switcher) Live() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

func (s *switcher) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newHarness builds a receiver, a signer, a trust store and a switcher that all
// agree, which is the boring case every test starts from.
func newHarness(t *testing.T, cluster string) (*Receiver, *pluginmanifest.Signer, *switcher) {
	t.Helper()
	id, err := NewClusterID(cluster)
	if err != nil {
		t.Fatalf("NewClusterID(%q): %v", cluster, err)
	}
	signer := newSigner(t, "release-2026")
	store := trustFor(t, signer)
	sw := &switcher{}
	r, err := NewReceiver(id, store, testPolicy(), sw)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	return r, signer, sw
}

// bundleFor builds a bundle naming a tree that has already been staged.
func bundleFor(t *testing.T, id ClusterID, version uint64, env pluginmanifest.Envelope) Bundle {
	t.Helper()
	return Bundle{
		ClusterID:      id,
		Version:        version,
		PackageName:    env.Name,
		PackageVersion: env.Version,
		Envelope:       env.Transport(),
		Reason:         "policy rollout",
	}
}
