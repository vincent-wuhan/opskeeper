package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The tests below are about the control plane's half of a release: which
// nodes it reaches, that two releases of one package cannot overlap, and
// that what a node said survives the wire. The wave gate itself is
// Rollout's, and its tests are in rollout_test.go.

// fixedFleet is an inventory that returns what it was given.
type fixedFleet struct {
	ids []uint64
	err error
}

func (f fixedFleet) EdgeIDs(context.Context) ([]uint64, error) { return f.ids, f.err }

func releaseRequest(name, version string) StartRequest {
	return StartRequest{
		Name: name, Version: version,
		URL:    "https://mirror.opskeeper.internal/plugins/" + name + "-" + version + ".tar.gz",
		SHA256: strings.Repeat("a", 64), Signature: "c2ln", KeyID: "ops-2026",
		Strategy: domain.InstallRolling,
	}
}

// fleet25 is twenty-five nodes. The size is deliberate: a tenth of it is
// two and a half, so a canary that is rounded down to zero, rounded up to
// the whole fleet, or computed in floating point shows up as a wrong count
// rather than as nothing.
func fleet25() []uint64 { return nodes(25) }

func newManager(t *testing.T, n *scriptedNode) *Manager {
	t.Helper()
	return NewManager(fixedFleet{ids: fleet25()}, n, nil)
}

// reached lists the edges the scripted fleet put the package on.
func reached(n *scriptedNode, name string) []uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []uint64
	for id := range n.state {
		for _, p := range n.state[id] {
			if p.Name == name {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------
// starting a release
// ---------------------------------------------------------------------

func TestStartSendsOnlyTheCanary(t *testing.T) {
	// The first call has to be the canary and nothing else. A Start that
	// fanned out to the whole fleet would be a release that ran its own
	// gate and then ignored it, and the operator's first news of a bad
	// package would be a fleet-wide failure report.
	n := newFleet(fleet25()...)
	st, err := newManager(t, n).Start(context.Background(), releaseRequest("acme", "1.0.0"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if st.Wave != 1 || st.Waves < 2 {
		t.Errorf("status = wave %d of %d, want the release to open on its own canary wave", st.Wave, st.Waves)
	}
	if got := len(reached(n, "acme")); got != 2 {
		t.Errorf("%d nodes received the package in the first wave, want a tenth of 25 (2)", got)
	}
	if st.Done {
		t.Error("the release reports Done after its first wave, so the remaining waves were never planned")
	}
}

func TestAPinnedReleaseGoesOutInOneWave(t *testing.T) {
	// A pin is the operator having said this version will not be
	// auto-upgraded. Canary-ing it would be theatre: the next release will
	// not be canaried either.
	n := newFleet(fleet25()...)
	req := releaseRequest("acme", "1.0.0")
	req.Strategy = domain.InstallPin
	st, err := newManager(t, n).Start(context.Background(), req)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if st.Waves != 1 {
		t.Errorf("waves = %d, want 1 for a pinned package", st.Waves)
	}
	if got := len(reached(n, "acme")); got != 25 {
		t.Errorf("%d nodes received the pinned package, want all 25", got)
	}
}

func TestTwoReleasesOfOnePackageCannotRunAtOnce(t *testing.T) {
	// Each release forms its own idea of what a node had before. If both
	// ran, rolling back either would restore a version the other never
	// replaced — putting a package back on the fleet for no reason anyone
	// can see.
	n := newFleet(fleet25()...)
	m := newManager(t, n)
	if _, err := m.Start(context.Background(), releaseRequest("acme", "1.0.0")); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := m.Start(context.Background(), releaseRequest("acme", "1.1.0")); !errors.Is(err, ErrReleaseRunning) {
		t.Errorf("second start error = %v, want ErrReleaseRunning", err)
	}
}

func TestAReleaseWithoutAURLIsRefused(t *testing.T) {
	// A spec with no URL would fail on the first node with "the node could
	// not carry out the request", sending an operator to inspect nodes for
	// a mistake made in the console.
	req := releaseRequest("acme", "1.0.0")
	req.URL = ""
	if _, err := newManager(t, newFleet(fleet25()...)).Start(context.Background(), req); err == nil {
		t.Fatal("a release with no URL was accepted")
	}
}

func TestAReleaseIsVisibleBeforeItsFirstNodeIsTouched(t *testing.T) {
	// The plan is registered before the first request goes out, so the
	// release is a job an operator can halt and roll back from the moment
	// it exists. A manager that registered after dispatching would have a
	// window in which the first node has the package and nothing in List
	// knows about it — and that node could never be rolled back, because a
	// rollback works off the plan.
	//
	// The observation is made from inside the first Install, which is the
	// only moment that distinguishes the two orderings.
	n := newFleet(fleet25()...)
	obs := &observingNode{inner: n}
	m := NewManager(fixedFleet{ids: fleet25()}, obs, nil)
	obs.manager = m

	if _, err := m.Start(context.Background(), releaseRequest("acme", "1.0.0")); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(obs.visibleDuringInstall) == 0 {
		t.Fatal("no install was observed, so the fixture proved nothing")
	}
	for i, visible := range obs.visibleDuringInstall {
		if visible == 0 {
			t.Fatalf("during install #%d the release was not in List; a node can hold the package "+
				"while the control plane has no record of the release that put it there", i+1)
		}
	}
}

// observingNode records how many releases the manager knows about at the
// moment each Install is dispatched.
type observingNode struct {
	inner   Node
	manager *Manager
	// visibleDuringInstall holds, per install, the number of releases List
	// reported at that instant.
	visibleDuringInstall []int
}

func (o *observingNode) Install(ctx context.Context, edgeID uint64, spec ports.PluginSpec) Outcome {
	o.visibleDuringInstall = append(o.visibleDuringInstall, len(o.manager.List()))
	return o.inner.Install(ctx, edgeID, spec)
}

func (o *observingNode) Remove(ctx context.Context, edgeID uint64, name, version string) Outcome {
	return o.inner.Remove(ctx, edgeID, name, version)
}

func (o *observingNode) Restore(ctx context.Context, edgeID uint64, name, version string) Outcome {
	return o.inner.Restore(ctx, edgeID, name, version)
}

func TestHaltDoesNotWaitForTheWaveItIsStopping(t *testing.T) {
	// Halt is the method an operator reaches for when a canary is going
	// wrong, and a wave is one request per node. If the mutex were held
	// across the wave, Halt would block until every node in it had
	// answered — an emergency stop that waits for the thing it is
	// stopping. The test halts from inside the first node's Install, which
	// is the only place that can tell the two designs apart, and bounds
	// the wait so a regression fails instead of hanging.
	n := newFleet(fleet25()...)
	obs := &haltingNode{inner: n}
	m := NewManager(fixedFleet{ids: fleet25()}, obs, nil)
	obs.manager = m

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := m.Start(context.Background(), releaseRequest("acme", "1.0.0")); err != nil {
			t.Errorf("start: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Start never returned; Halt could not take the lock while a wave was in flight")
	}
	if !obs.halted {
		t.Fatal("the halt never reached the manager")
	}
	// The halt is recorded, so the release cannot walk on to the next wave.
	st, err := m.Status("acme")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Halted {
		t.Errorf("status = %+v, want the release reported as halted", st)
	}
	if _, _, err := m.Advance(context.Background(), "acme"); err == nil {
		t.Error("advance stepped over a halt that arrived mid-wave")
	}
}

// haltingNode halts its own release from inside the first install.
type haltingNode struct {
	inner   Node
	manager *Manager
	halted  bool
}

func (h *haltingNode) Install(ctx context.Context, edgeID uint64, spec ports.PluginSpec) Outcome {
	out := h.inner.Install(ctx, edgeID, spec)
	if !h.halted {
		h.halted = true
		// Synchronous, so a deadlock shows up here rather than as a
		// goroutine leak nobody looks at.
		_, _ = h.manager.Halt(spec.Name, "the canary is alerting")
	}
	return out
}

func (h *haltingNode) Remove(ctx context.Context, edgeID uint64, name, version string) Outcome {
	return h.inner.Remove(ctx, edgeID, name, version)
}

func (h *haltingNode) Restore(ctx context.Context, edgeID uint64, name, version string) Outcome {
	return h.inner.Restore(ctx, edgeID, name, version)
}

func TestAnEmptyFleetIsRefused(t *testing.T) {
	// A fleet-wide install of nothing is a success nobody can see.
	m := NewManager(fixedFleet{}, newFleet(), nil)
	if _, err := m.Start(context.Background(), releaseRequest("acme", "1.0.0")); err == nil {
		t.Fatal("a release to an empty fleet was accepted")
	}
}

func TestAManagerWithNoFleetOrNoDispatcherRefusesToStart(t *testing.T) {
	// An unwired control plane must not report a successful release. This
	// is the failure that looks most like a fleet-wide policy rejection.
	if _, err := NewManager(nil, newFleet(1), nil).Start(context.Background(), releaseRequest("acme", "1.0.0")); err == nil {
		t.Error("a release started with no inventory")
	}
	if _, err := NewManager(fixedFleet{ids: []uint64{1}}, nil, nil).Start(context.Background(), releaseRequest("acme", "1.0.0")); err == nil {
		t.Error("a release started with no node dispatcher")
	}
}

// ---------------------------------------------------------------------
// halting and rolling back
// ---------------------------------------------------------------------

func TestHaltDoesNotTakeThePackageBack(t *testing.T) {
	// Halting and rolling back are separate decisions. An operator who has
	// just watched the canary go bad may want the release stopped now and
	// the rollback decided calmly — possibly leaving the canary up while
	// they look at it.
	n := newFleet(fleet25()...)
	m := newManager(t, n)
	if _, err := m.Start(context.Background(), releaseRequest("acme", "1.0.0")); err != nil {
		t.Fatalf("start: %v", err)
	}
	before := reached(n, "acme")
	if _, err := m.Halt("acme", "hold"); err != nil {
		t.Fatalf("halt: %v", err)
	}
	if after := reached(n, "acme"); len(after) != len(before) {
		t.Errorf("a halt changed the nodes holding the package: %v -> %v", before, after)
	}
}

func TestRollbackPutsThePreviousVersionBack(t *testing.T) {
	// The property that makes a rollback a rollback. The node says what it
	// replaced and the manager installs that version back, rather than
	// leaving the node empty — a rollback to nothing is an outage the
	// operator did not ask for.
	n := newFleet(fleet25()...)
	m := newManager(t, n)
	for _, id := range fleet25() {
		n.seed(id, ports.PluginInfo{Name: "acme", Version: "0.2.0", Digest: "d-0.2.0"})
	}
	if _, err := m.Start(context.Background(), releaseRequest("acme", "0.3.0")); err != nil {
		t.Fatalf("start: %v", err)
	}
	touched := reached(n, "acme")
	if len(touched) == 0 {
		t.Fatal("the canary wave installed on no node")
	}
	if _, err := m.Rollback(context.Background(), "acme"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	for _, id := range touched {
		if !n.installedOn(id, "acme") {
			t.Errorf("node %d lost the package entirely; a rollback should restore 0.2.0", id)
		}
		if got := n.versions(id); len(got) != 1 || got[0] != "acme@0.2.0" {
			t.Errorf("node %d is running %v, want acme@0.2.0", id, got)
		}
	}
}

func TestRollbackLeavesUnreachedNodesAlone(t *testing.T) {
	// A node that refused was never changed. A remove sent to it would
	// either do nothing or, on a node that already had the package, remove
	// the operator's own installation.
	n := newFleet(fleet25()...)
	m := newManager(t, n)
	for _, id := range fleet25() {
		n.seed(id, ports.PluginInfo{Name: "acme", Version: "0.2.0", Digest: "d-0.2.0"})
		n.install[id] = Outcome{Status: StatusRefused, Reason: "above this node's ceiling"}
	}
	if _, err := m.Start(context.Background(), releaseRequest("acme", "0.3.0")); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := m.Rollback(context.Background(), "acme"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	for _, id := range fleet25() {
		if got := n.versions(id); len(got) != 1 || got[0] != "acme@0.2.0" {
			t.Errorf("node %d is running %v, want its own acme@0.2.0 untouched", id, got)
		}
	}
	for _, c := range n.calls {
		if strings.HasPrefix(c, "remove:") {
			t.Errorf("the rollback issued %q on a fleet where every node refused", c)
		}
	}
}

func TestRollbackKeepsTheReleaseWhenItDidNotComplete(t *testing.T) {
	// A release that failed to roll back is the record of which nodes still
	// have the package. Forgetting it would turn the retry into a release
	// nobody can find.
	n := newFleet(fleet25()...)
	m := newManager(t, n)
	if _, err := m.Start(context.Background(), releaseRequest("acme", "1.0.0")); err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, id := range reached(n, "acme") {
		n.failRemove[id] = true
	}
	if _, err := m.Rollback(context.Background(), "acme"); err == nil {
		t.Fatal("rollback reported success while every removal failed")
	}
	if _, err := m.Status("acme"); err != nil {
		t.Errorf("the release vanished after a failed rollback: %v", err)
	}
}

func TestRollbackForgetsTheReleaseOnceItCompleted(t *testing.T) {
	n := newFleet(fleet25()...)
	m := newManager(t, n)
	if _, err := m.Start(context.Background(), releaseRequest("acme", "1.0.0")); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := m.Rollback(context.Background(), "acme"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := m.Status("acme"); !errors.Is(err, ErrNoRelease) {
		t.Errorf("status after a completed rollback = %v, want ErrNoRelease", err)
	}
	// And the slot is free again, so the operator can ship a fix. A lock
	// that outlived its release would make the first rollback the last
	// release of that package.
	if _, err := m.Start(context.Background(), releaseRequest("acme", "1.0.1")); err != nil {
		t.Errorf("starting a new release after a rollback: %v", err)
	}
}

func TestAdvanceOnAnUnknownReleaseSaysSo(t *testing.T) {
	m := newManager(t, newFleet(fleet25()...))
	if _, _, err := m.Advance(context.Background(), "nobody"); !errors.Is(err, ErrNoRelease) {
		t.Errorf("advance error = %v, want ErrNoRelease", err)
	}
}

func TestListReportsEveryRunningReleaseInNameOrder(t *testing.T) {
	// Sorted so a console's table does not reshuffle between polls.
	n := newFleet(fleet25()...)
	m := newManager(t, n)
	for _, name := range []string{"zeta", "alpha", "mid"} {
		if _, err := m.Start(context.Background(), releaseRequest(name, "1.0.0")); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
	}
	names := make([]string, 0, 3)
	for _, st := range m.List() {
		names = append(names, st.Plugin)
	}
	if got := strings.Join(names, ","); got != "alpha,mid,zeta" {
		t.Errorf("list = %v, want alpha,mid,zeta", names)
	}
}

// ---------------------------------------------------------------------
// the node adapter
// ---------------------------------------------------------------------

// wireNode is an EdgeCaller that answers with canned tunnel payloads, so
// the adapter can be tested without a tunnel.
type wireNode struct {
	mu      sync.Mutex
	methods []string
	respond func(method string, body []byte) ([]byte, error)
}

func (w *wireNode) Call(_ context.Context, _ uint64, method string, body []byte) ([]byte, error) {
	w.mu.Lock()
	w.methods = append(w.methods, method)
	w.mu.Unlock()
	return w.respond(method, body)
}

func TestInstallCarriesTheFieldsTheNodeReviewNeeds(t *testing.T) {
	// The node verifies the signature against its own trust store, so the
	// envelope and the transport digest have to survive the wire. A
	// request that dropped the signature would be refused by every node
	// with a trust store, and the release would look like a fleet-wide
	// policy problem.
	var got tunnel.PluginInstallRequest
	w := &wireNode{respond: func(_ string, body []byte) ([]byte, error) {
		if err := json.Unmarshal(body, &got); err != nil {
			return nil, err
		}
		return json.Marshal(tunnel.PluginInstallResponse{
			Status: tunnel.PluginStatusInstalled, Plugin: "acme", Version: "1.0.0", Digest: "tree",
		})
	}}
	spec := ports.PluginSpec{
		Name: "acme", Version: "1.0.0", URL: "https://m/x.tar.gz",
		SHA256: strings.Repeat("b", 64), Signature: "ZW52", KeyID: "ops-2026",
	}
	out := NewNodeFleet(w).Install(context.Background(), 7, spec)
	if out.Status != StatusInstalled {
		t.Fatalf("install = %+v", out)
	}
	for _, c := range []struct{ name, got, want string }{
		{"plugin", got.Plugin, spec.Name},
		{"version", got.Version, spec.Version},
		{"url", got.URL, spec.URL},
		{"sha256", got.SHA256, spec.SHA256},
		{"signature", got.Signature, spec.Signature},
		{"key_id", got.KeyID, spec.KeyID},
	} {
		if c.got != c.want {
			t.Errorf("%s on the wire = %q, want %q", c.name, c.got, c.want)
		}
	}
	if out.Digest != "tree" {
		t.Errorf("digest = %q, want the digest the node computed", out.Digest)
	}
	if out.Plugin != "acme" {
		t.Errorf("outcome names %q, want the package it answered about", out.Plugin)
	}
}

func TestARefusalIsNotReportedAsAFailure(t *testing.T) {
	// A refusal is a decision; a failure is this node's problem. A manager
	// that collapsed them would retry a signature rejection against every
	// node in the fleet, forever.
	w := &wireNode{respond: func(string, []byte) ([]byte, error) {
		return json.Marshal(tunnel.PluginInstallResponse{
			Status: tunnel.PluginStatusRefused, Plugin: "acme", Version: "1.0.0",
			Reason: "package is above this node's ceiling (L3 > L1)",
		})
	}}
	out := NewNodeFleet(w).Install(context.Background(), 3, ports.PluginSpec{Name: "acme"})
	if out.Status != StatusRefused {
		t.Fatalf("status = %q, want %q", out.Status, StatusRefused)
	}
	if out.Reason == "" {
		t.Error("the refusal carries no reason, so an operator cannot fix it")
	}
}

func TestARefusalWithNoReasonStillSaysSomething(t *testing.T) {
	// An empty reason on the wire is not "success". A refusal with no
	// sentence would reach the console as a blank row an operator cannot
	// act on.
	w := &wireNode{respond: func(string, []byte) ([]byte, error) {
		return json.Marshal(tunnel.PluginInstallResponse{
			Status: tunnel.PluginStatusRefused, Plugin: "acme", Version: "1.0.0",
		})
	}}
	out := NewNodeFleet(w).Install(context.Background(), 3, ports.PluginSpec{Name: "acme"})
	if out.Status != StatusRefused || out.Reason == "" {
		t.Errorf("outcome = %+v, want a refusal that explains itself", out)
	}
}

func TestATransportErrorIsAFailureQuotingTheError(t *testing.T) {
	// The tunnel not answering is not the node refusing. Reporting it as a
	// refusal would send an operator to read a policy on a node that never
	// heard the question.
	w := &wireNode{respond: func(string, []byte) ([]byte, error) {
		return nil, errors.New("edge 12 is offline")
	}}
	out := NewNodeFleet(w).Install(context.Background(), 12, ports.PluginSpec{Name: "acme"})
	if out.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", out.Status, StatusFailed)
	}
	if !strings.Contains(out.Reason, "edge 12 is offline") {
		t.Errorf("reason = %q, want the transport error in it", out.Reason)
	}
}

func TestAnUnwiredFleetFailsRatherThanRefusing(t *testing.T) {
	// "Failed" says come back later; "refused" says give up. A control
	// plane with no tunnel must not look like a fleet-wide policy
	// rejection.
	out := NewNodeFleet(nil).Install(context.Background(), 1, ports.PluginSpec{Name: "acme"})
	if out.Status != StatusFailed {
		t.Errorf("status = %q, want %q for an unwired control plane", out.Status, StatusFailed)
	}
}

func TestInstallReportsWhatTheNodeReplaced(t *testing.T) {
	// This is the field the whole rollback depends on. Without it the
	// manager has only the post-install list, whose entry for this name is
	// the *new* version — so a rollback would restore the release it is
	// meant to be undoing.
	w := &wireNode{respond: func(string, []byte) ([]byte, error) {
		return json.Marshal(tunnel.PluginInstallResponse{
			Status: tunnel.PluginStatusInstalled, Plugin: "acme", Version: "1.0.0",
			Replaced: &tunnel.PluginEntry{Plugin: "acme", Version: "0.9.0", Digest: "d-0.9.0"},
			Installed: []tunnel.PluginEntry{
				{Plugin: "acme", Version: "1.0.0", Digest: "d-1.0.0"},
			},
		})
	}}
	out := NewNodeFleet(w).Install(context.Background(), 4, ports.PluginSpec{Name: "acme"})
	if out.Replaced == nil {
		t.Fatal("Replaced is nil, so a rollback would be a delete")
	}
	if out.Replaced.Version != "0.9.0" {
		t.Errorf("replaced version = %q, want 0.9.0, the version that was there before", out.Replaced.Version)
	}
	if len(out.Set) != 1 || out.Set[0].Version != "1.0.0" {
		t.Errorf("post-install set = %+v, want the node's own report", out.Set)
	}
}

func TestAFreshInstallReportsNothingReplaced(t *testing.T) {
	// A nil Replaced and one with an empty version are not the same
	// answer: the first means there was nothing there. Collapsing them
	// would make a rollback try to restore a package that never existed.
	w := &wireNode{respond: func(string, []byte) ([]byte, error) {
		return json.Marshal(tunnel.PluginInstallResponse{
			Status: tunnel.PluginStatusInstalled, Plugin: "acme", Version: "1.0.0",
		})
	}}
	out := NewNodeFleet(w).Install(context.Background(), 4, ports.PluginSpec{Name: "acme"})
	if out.Replaced != nil {
		t.Errorf("Replaced = %+v, want nil for a package that was not there", out.Replaced)
	}
}

func TestRemoveCarriesTheVersionGuard(t *testing.T) {
	// The node compares this against what it actually has. Dropping it on
	// the way out would make a rollback that raced a newer release remove
	// the newer one and call it a success.
	var got tunnel.PluginRemoveRequest
	w := &wireNode{respond: func(_ string, body []byte) ([]byte, error) {
		if err := json.Unmarshal(body, &got); err != nil {
			return nil, err
		}
		return json.Marshal(tunnel.PluginRemoveResponse{Status: tunnel.PluginStatusInstalled, Plugin: "acme"})
	}}
	NewNodeFleet(w).Remove(context.Background(), 9, "acme", "1.0.0")
	if got.Plugin != "acme" || got.Version != "1.0.0" {
		t.Errorf("remove request = %+v, want the package and the version", got)
	}
}

func TestANodeThatRefusesARemoveIsNotCountedAsDone(t *testing.T) {
	w := &wireNode{respond: func(string, []byte) ([]byte, error) {
		return json.Marshal(tunnel.PluginRemoveResponse{
			Status: tunnel.PluginStatusRefused, Plugin: "acme", Version: "1.1.0",
			Reason: "asked to remove 1.0.0 but 1.1.0 is installed",
		})
	}}
	out := NewNodeFleet(w).Remove(context.Background(), 9, "acme", "1.0.0")
	if out.Status != StatusRefused {
		t.Fatalf("status = %q, want %q", out.Status, StatusRefused)
	}
	if !strings.Contains(out.Reason, "1.1.0") {
		t.Errorf("reason = %q, want the version the node actually found", out.Reason)
	}
}

func TestInstalledAsksTheListMethod(t *testing.T) {
	w := &wireNode{respond: func(method string, _ []byte) ([]byte, error) {
		if method != tunnel.MethodPluginList {
			return nil, fmt.Errorf("unexpected method %q", method)
		}
		return json.Marshal(tunnel.PluginListResponse{Installed: []tunnel.PluginEntry{
			{Plugin: "opskeeper-sre-readonly", Version: "0.1.0", Digest: "d"},
		}})
	}}
	got, err := NewNodeFleet(w).Installed(context.Background(), 5)
	if err != nil {
		t.Fatalf("installed: %v", err)
	}
	if len(got) != 1 || got[0].Name != "opskeeper-sre-readonly" {
		t.Errorf("installed = %+v", got)
	}
}

func TestAnUnreadableAnswerIsAFailureNotARefusal(t *testing.T) {
	// A node that answers with bytes the manager cannot parse has not
	// decided anything. Calling it a refusal would send an operator to
	// look at a policy that was never consulted.
	w := &wireNode{respond: func(string, []byte) ([]byte, error) { return []byte("{not json"), nil }}
	out := NewNodeFleet(w).Install(context.Background(), 1, ports.PluginSpec{Name: "acme"})
	if out.Status != StatusFailed {
		t.Errorf("status = %q, want %q", out.Status, StatusFailed)
	}
}
