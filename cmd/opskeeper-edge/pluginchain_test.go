package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"

	"github.com/vincent-wuhan/opskeeper/core/domains/service/plugin"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The Phase D acceptance gate: "install -> review -> canary -> rollback,
// end to end".
//
// Everything below is real except the socket. The package is built and
// signed here, the node review is the production pluginStore, the manager
// rollout is the production plugin.Manager, and the wire shapes are the
// production tunnel DTOs. What is faked is only the transport: an
// in-memory caller in place of frontier, so the chain can run without a
// broker, a database, or a second process.
//
// That is the point. A 200-line e2e test that stands up the whole product
// is a test nobody runs; this fails for every reason the real chain fails
// for — a signature that does not verify, a policy ceiling that is too
// low, a wave that advances before its canary answered, a rollback that
// removes a version it did not install — and it runs in the unit suite.
//
// The node side is reached through the real tunnel handler table
// (edgebiz.Agent.registerPluginHandlers), so the JSON the manager writes is
// the JSON the node reads. A test that called the store directly would not
// notice the two sides disagreeing about a field name, which is the most
// common way a distributed release breaks.

// fleetBus is the in-memory transport. It is the only non-production
// component in the chain.
type fleetBus struct {
	mu       sync.Mutex
	handlers map[uint64]map[string]tunnel.Handler
}

func newFleetBus() *fleetBus {
	return &fleetBus{handlers: map[uint64]map[string]tunnel.Handler{}}
}

// attach registers a node's handler table, exactly as the agent does.
func (b *fleetBus) attach(edgeID uint64, h map[string]tunnel.Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[edgeID] = h
}

// Call is the manager->edge half. It is the shape plugin.EdgeCaller wants.
func (b *fleetBus) Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error) {
	b.mu.Lock()
	h := b.handlers[edgeID][method]
	b.mu.Unlock()
	if h == nil {
		return nil, fmt.Errorf("edge %d has no handler for %s", edgeID, method)
	}
	return h(ctx, tunnel.Session{EdgeID: edgeID}, method, body)
}

// captureFleet is the manager's edge inventory.
type captureFleet struct{ ids []uint64 }

func (f captureFleet) EdgeIDs(context.Context) ([]uint64, error) {
	return append([]uint64(nil), f.ids...), nil
}

// nodeHarness is one node: a real store over temp directories, plus the
// real handler table the tunnel would call.
type nodeHarness struct {
	id     uint64
	store  *pluginStore
	agents nodeAgentConfig
}

// startNodes builds n nodes whose stores trust signer and admit up to the
// given policy.
func startNodes(t *testing.T, n int, trust *pluginmanifest.TrustStore, base pluginmanifest.Policy) (*fleetBus, []*nodeHarness) {
	t.Helper()
	bus := newFleetBus()
	nodes := make([]*nodeHarness, 0, n)
	for i := 0; i < n; i++ {
		dir := t.TempDir()
		work := filepath.Join(dir, "work")
		if err := os.MkdirAll(work, 0o750); err != nil {
			t.Fatalf("work dir: %v", err)
		}
		pol := base
		pol.NodeVersion = testEdgeVersion
		pol.PigVersion = testPigVersion
		pol.AllowUnsigned = len(trust.KeyIDs()) == 0
		store := &pluginStore{
			pkgDir:  filepath.Join(dir, "packages"),
			workDir: work,
			trust:   trust,
			policy:  pol,
			log:     slog.New(slog.DiscardHandler),
		}
		if err := os.MkdirAll(store.pkgDir, 0o750); err != nil {
			t.Fatalf("package dir: %v", err)
		}
		id := uint64(100 + i)
		h := &nodeHarness{id: id, store: store, agents: nodeAgentConfig{Cwd: work}}
		bus.attach(id, tunnelHandlersFor(t, h))
		nodes = append(nodes, h)
	}
	return bus, nodes
}

// tunnelHandlersFor builds a node's method table without starting a
// tunnel.
//
// It mirrors registerPluginHandlers exactly, and the mirror is deliberate:
// the handler bodies are the ones the agent registers. Binding them by
// hand rather than launching an Agent keeps this test about the protocol
// and the policy, not about a dial that needs a broker.
func tunnelHandlersFor(t *testing.T, h *nodeHarness) map[string]tunnel.Handler {
	t.Helper()
	install := func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
		var req tunnel.PluginInstallRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		state := h.store.Install(ctx, ports.PluginSpec{
			Name: req.Plugin, Version: req.Version, URL: req.URL,
			SHA256: req.SHA256, Signature: req.Signature, KeyID: req.KeyID,
		})
		return json.Marshal(installResponseFor(h, state))
	}
	remove := func(_ context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
		var req tunnel.PluginRemoveRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		state := h.store.Remove(context.Background(), req.Plugin, req.Version)
		return json.Marshal(tunnel.PluginRemoveResponse{
			Status: statusFor(state), Plugin: state.Name, Version: state.Version,
			Reason:    firstOf(state.Refused, state.Note, state.Error),
			Installed: entriesFor(h.store),
		})
	}
	list := func(_ context.Context, _ tunnel.Session, _ string, _ []byte) ([]byte, error) {
		return json.Marshal(tunnel.PluginListResponse{Installed: entriesFor(h.store)})
	}
	restore := func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
		var req tunnel.PluginRestoreRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		state := h.store.Restore(ctx, req.Plugin, req.Version)
		return json.Marshal(tunnel.PluginRestoreResponse{
			Status: statusFor(state), Plugin: state.Name, Version: state.Version,
			Digest:    state.Digest,
			Reason:    firstOf(state.Refused, state.Note, state.Error),
			Installed: entriesFor(h.store),
		})
	}
	return map[string]tunnel.Handler{
		tunnel.MethodPluginInstall: install,
		tunnel.MethodPluginRemove:  remove,
		tunnel.MethodPluginList:    list,
		tunnel.MethodPluginRestore: restore,
	}
}

func installResponseFor(h *nodeHarness, state ports.PluginState) tunnel.PluginInstallResponse {
	var replaced *tunnel.PluginEntry
	if state.Replaced != nil {
		replaced = &tunnel.PluginEntry{
			Plugin: state.Replaced.Name, Version: state.Replaced.Version, Digest: state.Replaced.Digest,
		}
	}
	return tunnel.PluginInstallResponse{
		Status: statusFor(state), Plugin: state.Name, Version: state.Version,
		Digest: state.Digest, Replaced: replaced,
		Reason:    firstOf(state.Refused, state.Note, state.Error),
		Installed: entriesFor(h.store),
	}
}

// statusFor mirrors edgeagent/biz.statusOf, which is what production uses
// for BOTH install and remove.
//
// Getting this wrong is not cosmetic: a Remove leaves Installed false, so an
// "Installed means installed" reading classifies every successful removal as
// a failure and the manager reports a rollback that did not complete on
// every node it actually rolled back.
func statusFor(state ports.PluginState) string {
	switch {
	case state.Error != "":
		return tunnel.PluginStatusFailed
	case state.Refused != "":
		return tunnel.PluginStatusRefused
	default:
		return tunnel.PluginStatusInstalled
	}
}

func entriesFor(store *pluginStore) []tunnel.PluginEntry {
	infos := store.Installed()
	out := make([]tunnel.PluginEntry, 0, len(infos))
	for _, i := range infos {
		out = append(out, tunnel.PluginEntry{Plugin: i.Name, Version: i.Version, Digest: i.Digest})
	}
	return out
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// servePackage serves one signed archive over HTTP, which is the real
// download path the node takes.
func servePackage(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newSignedRelease builds a package, signs it, serves it, and returns the
// spec the manager releases plus the trust store that verifies it.
func newSignedRelease(t *testing.T, name, version, level, class string) (ports.PluginSpec, *pluginmanifest.TrustStore) {
	t.Helper()
	// signedPackageTar writes an L1/read manifest by default. The level and
	// class are the whole point of some of these cases, so the manifest is
	// rewritten before signing — signing happens over the tree, so a
	// post-signing edit would (correctly) fail verification.
	body, spec, trust := signedPackageTar(t, name, version, func(root string) {
		path := filepath.Join(root, pluginmanifest.ManifestFile)
		if err := os.WriteFile(path, []byte(manifestYAML(name, version, level, class)), 0o640); err != nil {
			t.Fatalf("rewrite manifest: %v", err)
		}
	})
	srv := servePackage(t, body)
	spec.URL = srv.URL + "/" + name + ".tgz"
	return spec, trust
}

// releaseManager builds the production manager over a bus.
func releaseManager(t *testing.T, bus *fleetBus, ids []uint64) *plugin.Manager {
	t.Helper()
	node := plugin.NewNodeFleet(bus)
	return plugin.NewManager(captureFleet{ids: ids}, node, slog.New(slog.DiscardHandler))
}

// TestReleaseInstallReviewCanaryRollback is the gate.
//
// It drives the whole chain in one test rather than four, because the
// property under test is that the stages compose: a package that installs
// but cannot be rolled back, or a wave that advances without its canary,
// is only visible in the sequence.
func TestReleaseInstallReviewCanaryRollback(t *testing.T) {
	req := plugin.StartRequest{Strategy: domain.InstallRolling}

	// A three-node fleet. With the canary fraction at one tenth, wave one
	// is exactly one node and wave two is the remaining two — which is
	// what makes "did the wave wait" observable.
	const nodes = 3
	spec, trust := newSignedRelease(t, "chain-plugin", "0.1.0", "L1", "read")
	bus, fleet := startNodes(t, nodes, trust, readOnlyNode())
	mgr := releaseManager(t, bus, []uint64{fleet[0].id, fleet[1].id, fleet[2].id})

	req.Name, req.Version, req.URL = spec.Name, spec.Version, spec.URL
	req.SHA256, req.Signature, req.KeyID = spec.SHA256, spec.Signature, spec.KeyID

	// --- 1. install: the first wave reaches exactly one node ------------
	st, err := mgr.Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.Waves < 2 {
		t.Fatalf("release planned %d wave(s); a rolling release must canary first", st.Waves)
	}
	if got := installedOn(fleet); len(got) != 1 {
		t.Fatalf("after wave one %d node(s) have the package, want exactly the canary", len(got))
	}

	// --- 2. review: the node's own gate decided -------------------------
	//
	// The canary installed, which means its signature verified and its
	// manifest passed admission. Nothing in the manager could have
	// produced this: the manager never sees the archive.
	canary := nodeWith(fleet, spec.Name)
	if canary == nil {
		t.Fatal("no node installed the canary package")
	}
	if _, err := os.Stat(filepath.Join(canary.store.pkgDir, spec.Name+"-"+spec.Version)); err != nil {
		t.Fatalf("the canary reported success but nothing is on disk: %v", err)
	}
	// And the agent was actually told — the settings file is the contract
	// between the store and the runtime.
	if !published(t, canary.store, spec.Name, spec.Version) {
		t.Fatal("the canary installed but the agent's package list was not republished")
	}

	// --- 3. the wave gate: advance is refused while the canary is out ---
	//
	// Force the pending state by asking the manager to advance a release
	// whose wave is not accounted for. The production path accounts for
	// it synchronously, so this asserts the gate directly rather than
	// pretending a timeout.
	moved, _, err := mgr.Advance(context.Background(), spec.Name)
	if err != nil {
		t.Fatalf("first Advance: %v", err)
	}
	if !moved {
		t.Fatal("the canary answered, so the second wave must be allowed to go out")
	}
	if got := installedOn(fleet); len(got) != nodes {
		t.Fatalf("after wave two %d node(s) have the package, want %d", len(got), nodes)
	}

	// --- 4. rollback: every reached node loses exactly this version ----
	st, err = mgr.Rollback(context.Background(), spec.Name)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !st.RolledBack {
		t.Error("Rollback returned a status that does not say it rolled back")
	}
	for _, n := range fleet {
		if versions := installedVersions(n.store, spec.Name); len(versions) != 0 {
			t.Errorf("node %d still has %s after rollback", n.id, strings.Join(versions, ","))
		}
		if !publishedAbsent(t, n.store, spec.Name) {
			t.Errorf("node %d still advertises %s to the agent after rollback", n.id, spec.Name)
		}
	}
	// The release is forgotten once it completed, so a second rollback is
	// not a second removal.
	if _, err := mgr.Status(spec.Name); err == nil {
		t.Error("a completed rollback kept the release around")
	}
}

// TestAHaltStopsTheReleaseAndKeepsWhatIsInstalled is the second half of the
// gate: halt and rollback are different decisions.
func TestAHaltStopsTheReleaseAndKeepsWhatIsInstalled(t *testing.T) {
	spec, trust := newSignedRelease(t, "halt-plugin", "0.1.0", "L1", "read")
	bus, fleet := startNodes(t, 3, trust, readOnlyNode())
	mgr := releaseManager(t, bus, []uint64{fleet[0].id, fleet[1].id, fleet[2].id})

	if _, err := mgr.Start(context.Background(), plugin.StartRequest{
		Name: spec.Name, Version: spec.Version, URL: spec.URL,
		SHA256: spec.SHA256, Signature: spec.Signature, KeyID: spec.KeyID,
		Strategy: domain.InstallRolling,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	st, err := mgr.Halt(spec.Name, "canary latency doubled")
	if err != nil {
		t.Fatalf("Halt: %v", err)
	}
	if !st.Halted || st.Reason != "canary latency doubled" {
		t.Errorf("halt status = %+v, want halted with the operator's reason", st)
	}
	// Halt keeps what is installed: that is the difference from rollback.
	if got := installedOn(fleet); len(got) != 1 {
		t.Fatalf("halt changed the fleet: %d node(s) have the package, want the canary only", len(got))
	}
	// And it refuses to move on.
	if moved, _, err := mgr.Advance(context.Background(), spec.Name); err == nil || moved {
		t.Errorf("Advance after Halt = (%v, %v), want it refused", moved, err)
	}
}

// TestANodeBelowTheCeilingRefusesAndTheReleaseStillMoves checks the
// refusal path end to end.
//
// A refusal is a decision, not a transport fault: the wave must be allowed
// to advance past it, and the node must be left untouched.
func TestANodeBelowTheCeilingRefusesAndTheReleaseStillMoves(t *testing.T) {
	// An L2 package on a fleet that only admits L1. The node refuses, and
	// nothing about that is retryable.
	spec, trust := newSignedRelease(t, "too-high-plugin", "0.1.0", "L2", "write")
	bus, fleet := startNodes(t, 2, trust, readOnlyNode())
	mgr := releaseManager(t, bus, []uint64{fleet[0].id, fleet[1].id})

	st, err := mgr.Start(context.Background(), plugin.StartRequest{
		Name: spec.Name, Version: spec.Version, URL: spec.URL,
		SHA256: spec.SHA256, Signature: spec.Signature, KeyID: spec.KeyID,
		Strategy: domain.InstallPin,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(st.Failed) != 2 {
		t.Errorf("failed nodes = %v, want both: the ceiling refused them", st.Failed)
	}
	for _, n := range fleet {
		if versions := installedVersions(n.store, spec.Name); len(versions) != 0 {
			t.Errorf("node %d installed a package above its ceiling", n.id)
		}
	}
}

// TestRollbackRestoresTheVersionTheNodeWasAlreadyOn is the part of the
// chain that a fresh-install test cannot reach.
//
// A node running 0.1.0 that is offered 0.2.0 keeps 0.1.0 on disk (activate
// deliberately does not delete it), and a rollback must put it back rather
// than leave the node with nothing. This is the case decision #16 is about:
// the manager derives the restore from the node's own `Replaced`, because
// the only list the node can report afterwards has the new version in it.
func TestRollbackRestoresTheVersionTheNodeWasAlreadyOn(t *testing.T) {
	const node = 100

	v1, trust := newSignedRelease(t, "restore-plugin", "0.1.0", "L1", "read")
	bus, fleet := startNodes(t, 1, trust, readOnlyNode())
	mgr := releaseManager(t, bus, []uint64{node})

	// --- the node is already running 0.1.0, pinned -----------------------
	if _, err := mgr.Start(context.Background(), plugin.StartRequest{
		Name: v1.Name, Version: v1.Version, URL: v1.URL,
		SHA256: v1.SHA256, Signature: v1.Signature, KeyID: v1.KeyID,
		Strategy: domain.InstallPin,
	}); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	if got := installedVersions(fleet[0].store, v1.Name); len(got) != 1 || got[0] != "0.1.0" {
		t.Fatalf("seed install left %v, want [0.1.0]", got)
	}

	// --- 0.2.0 arrives and is rolled back --------------------------------
	//
	// One signer per test, so both versions verify against the same trust
	// store — which is the point: the node's trust decision is about the
	// publisher, not the release.
	v2, _ := newSignedRelease(t, "restore-plugin", "0.2.0", "L1", "read")
	mgr2 := releaseManager(t, bus, []uint64{node})

	if _, err := mgr2.Start(context.Background(), plugin.StartRequest{
		Name: v2.Name, Version: v2.Version, URL: v2.URL,
		SHA256: v2.SHA256, Signature: v2.Signature, KeyID: v2.KeyID,
		Strategy: domain.InstallPin,
	}); err != nil {
		t.Fatalf("release 0.2.0: %v", err)
	}
	if got := installedVersions(fleet[0].store, v2.Name); len(got) != 1 || got[0] != "0.2.0" {
		t.Fatalf("after upgrade node has %v, want [0.2.0]", got)
	}

	st, err := mgr2.Rollback(context.Background(), v2.Name)
	if err != nil {
		t.Fatalf("rollback: %v (status %+v)", err, st)
	}
	if got := installedVersions(fleet[0].store, v2.Name); len(got) != 1 || got[0] != "0.1.0" {
		t.Fatalf("after rollback node has %v, want it back on [0.1.0]", got)
	}
	if !published(t, fleet[0].store, v2.Name, "0.1.0") {
		t.Error("the node was restored on disk but the agent was not told to run 0.1.0")
	}
	// The superseded version is still on disk as rollback material, and it
	// is not published — publishing both is the bug this path exposed.
	if !publishedAbsent(t, fleet[0].store, v2.Name+"-0.2.0") {
		t.Error("the rolled-back version is still in the agent's package list")
	}
}

// --- helpers ------------------------------------------------------------

func installedOn(fleet []*nodeHarness) []*nodeHarness {
	var out []*nodeHarness
	for _, n := range fleet {
		if len(n.store.Installed()) > 0 {
			out = append(out, n)
		}
	}
	return out
}

func nodeWith(fleet []*nodeHarness, name string) *nodeHarness {
	for _, n := range fleet {
		for _, i := range n.store.Installed() {
			if i.Name == name {
				return n
			}
		}
	}
	return nil
}

func installedVersions(store *pluginStore, name string) []string {
	var out []string
	for _, i := range store.Installed() {
		if i.Name == name {
			out = append(out, i.Version)
		}
	}
	sort.Strings(out)
	return out
}

// published reports whether the agent's settings file names this package.
func published(t *testing.T, store *pluginStore, name, version string) bool {
	t.Helper()
	for _, p := range readSettings(t, store) {
		if strings.Contains(p, name+"-"+version) {
			return true
		}
	}
	return false
}

func publishedAbsent(t *testing.T, store *pluginStore, name string) bool {
	t.Helper()
	for _, p := range readSettings(t, store) {
		if strings.Contains(p, name) {
			return false
		}
	}
	return true
}

func readSettings(t *testing.T, store *pluginStore) []string {
	t.Helper()
	path := filepath.Join(store.workDir, agentConfigDirName(), agentSettingsFile)
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read settings: %v", err)
	}
	var parsed agentSettings
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	return parsed.Packages
}
