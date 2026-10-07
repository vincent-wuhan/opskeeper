package federationchild

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// fakeClient is a tunnel client that never opens a socket. The properties
// under test are about what the child decides and what it says, none of which
// needs a network — and a test that needed one would be testing the network.
type fakeClient struct {
	mu       sync.Mutex
	handlers map[string]tunnel.Handler
	calls    map[string]int
	// onCall answers an outgoing RPC. Nil means "accept and leave resp zero".
	onCall func(method string, req any, resp any) error
	// fetches answers the store's fetches by URL, and fetchErr is returned
	// for a URL that is not in it. A test that wants a real transfer says
	// what is at the URL; a test that wants a broken one says so
	// explicitly rather than relying on a network being absent.
	fetches  map[string][]byte
	fetchErr error
	// receiver is the one this child was assembled around, so a test can
	// ask what it has decided without the assembly handing out a fifth
	// value at every call site.
	receiver *federation.Receiver
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		handlers: map[string]tunnel.Handler{},
		calls:    map[string]int{},
		fetches:  map[string][]byte{},
	}
}

// serve is the store's Fetcher, reading from this client.
//
// It is installed by newChild rather than by each test, because the property
// under test is that a sourced push works end to end — fetch, digest, unpack,
// verify, promote — and a test that wired the fetcher itself would only be
// testing the wiring.
func (c *fakeClient) serve(_ context.Context, rawURL string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if body, ok := c.fetches[rawURL]; ok {
		return body, nil
	}
	if c.fetchErr != nil {
		return nil, c.fetchErr
	}
	return nil, os.ErrNotExist
}

// fetchFor registers what is at a URL and returns the client, so a test reads
// as one statement.
func (c *fakeClient) fetchFor(rawURL string, body []byte) *fakeClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetches[rawURL] = body
	return c
}

// receiverOf is the child's receiver, for asserting what it decided.
func (c *fakeClient) receiverOf(t *testing.T) *federation.Receiver {
	t.Helper()
	if c.receiver == nil {
		t.Fatalf("this client was not assembled by newChild and has no receiver")
	}
	return c.receiver
}

func (c *fakeClient) Dial(context.Context) error { return nil }
func (c *fakeClient) RegisterHandler(method string, h tunnel.Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[method] = h
}
func (c *fakeClient) Call(_ context.Context, method string, req, resp any) error {
	c.mu.Lock()
	c.calls[method]++
	on := c.onCall
	c.mu.Unlock()
	if on == nil {
		return nil
	}
	return on(method, req, resp)
}
func (c *fakeClient) AcceptStream() (tunnel.StreamConn, error) { return nil, nil }
func (c *fakeClient) OnReconnect(func())                       {}
func (c *fakeClient) Close() error                             { return nil }

func (c *fakeClient) callCount(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method]
}

// invoke calls a registered handler the way the tunnel would, so the tests go
// through the same decode/encode boundary the root does.
func (c *fakeClient) invoke(t *testing.T, method string, req any) tunnel.ClusterPolicyResponse {
	t.Helper()
	c.mu.Lock()
	h, ok := c.handlers[method]
	c.mu.Unlock()
	if !ok {
		t.Fatalf("no handler registered for %s", method)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body, err := h(context.Background(), tunnel.Session{}, method, raw)
	if err != nil {
		t.Fatalf("handler %s returned a transport error: %v", method, err)
	}
	var resp tunnel.ClusterPolicyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func (c *fakeClient) invokeState(t *testing.T, req tunnel.ClusterStateRequest) tunnel.ClusterStateResponse {
	t.Helper()
	c.mu.Lock()
	h := c.handlers[tunnel.MethodClusterState]
	c.mu.Unlock()
	if h == nil {
		t.Fatalf("no handler registered for %s", tunnel.MethodClusterState)
	}
	raw, _ := json.Marshal(req)
	body, err := h(context.Background(), tunnel.Session{}, tunnel.MethodClusterState, raw)
	if err != nil {
		t.Fatalf("handler returned a transport error: %v", err)
	}
	var resp tunnel.ClusterStateResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// signedTree writes a valid signed policy tree into the store's staging area
// and returns its path and the envelope the root would have published.
func signedTree(t *testing.T, store *Store, signer *pluginmanifest.Signer, name, version string) (string, pluginmanifest.Envelope) {
	t.Helper()
	return signedTreeAt(t, filepath.Join(store.VersionsDir(), version), signer, name, version)
}

// signedTreeAt is signedTree with a caller-chosen directory.
//
// It exists because a test sometimes needs two trees claiming the same
// package version — a genuine one and a forged one — and naming the directory
// after the version would make the second write quietly replace the first.
func signedTreeAt(t *testing.T, root string, signer *pluginmanifest.Signer, name, version string) (string, pluginmanifest.Envelope) {
	t.Helper()
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(t, filepath.Join(root, pluginmanifest.ManifestFile), manifestYAML(name, version))
	write(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool\n")

	env, err := signer.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return root, env
}

func manifestYAML(name, version string) string {
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
    - {name: host_probe_tcp, class: read}
  required_scopes:
    - host.read
  audit: {emits: true, mutates: false}
  approval: {required: false}
  install: {strategy: rolling, min_edge_version: 0.1.0}
`
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "policy"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func newSigner(t *testing.T) *pluginmanifest.Signer {
	t.Helper()
	s, _, err := pluginmanifest.GenerateSigner("release-2026")
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

// policy is what this child cluster permits: read-only, up to L2, and
// nothing outside its own host. The two version fields are not decoration —
// pluginmanifest refuses a package whose min_edge_version it cannot show is
// met, and refuses rather than assuming in the permissive direction.
func policy() pluginmanifest.Policy {
	p := pluginmanifest.PolicyFor(domain.SafetyL2, domain.RadiusNamespace,
		domain.Scopes{domain.ScopeHostRead})
	p.NodeVersion = "0.4.0"
	p.PigVersion = "0.3.0"
	return p
}

// newChild assembles the whole child side over fakes.
func newChild(t *testing.T, cluster string) (*Agent, *fakeClient, *Store, *pluginmanifest.Signer) {
	t.Helper()
	id, err := federation.NewClusterID(cluster)
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	store := newStore(t)
	signer := newSigner(t)
	recv, err := federation.NewReceiver(id, trustFor(t, signer), policy(), store)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	client := newFakeClient()
	client.receiver = recv
	store.fetch = client.serve
	agent, err := NewAgent(client, recv, store,
		federation.Cluster{ID: id, Name: "child", Version: "0.4.0", EdgeCount: 12},
		"provisioning-token", nil)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	agent.Register()
	return agent, client, store, signer
}

func bundle(id federation.ClusterID, version uint64, env pluginmanifest.Envelope) tunnel.ClusterPolicyRequest {
	return tunnel.ClusterPolicyRequest{
		Bundle: federation.Bundle{
			ClusterID:      id,
			Version:        version,
			PackageName:    env.Name,
			PackageVersion: env.Version,
			Envelope:       env.Transport(),
			Reason:         "rollout",
		},
	}
}
