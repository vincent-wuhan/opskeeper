package plugin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The tests below are about one thing: a release that goes wrong stops
// reaching nodes, and the nodes it already reached get put back.
//
// The plan in pluginmanifest decides which nodes and in what order. What
// can go wrong afterwards — and what these tests are about — is a release
// that has already installed on the canary and is still walking outward,
// a rollback that removes somebody else's installation, and a caller that
// steps over a halt because it only checked whether the wave was
// accounted for.

// scriptedNode is a fleet of nodes with per-node scripted answers.
type scriptedNode struct {
	mu sync.Mutex
	// install answers per edge id. A node with no entry installs.
	install map[uint64]Outcome
	// state is each node's *active* package set, mutated by the driver.
	state map[uint64][]ports.PluginInfo
	// retained is what is on disk. It is separate from state because the
	// real node keeps a superseded version's directory for a rollback and
	// stops publishing it; a fixture with one set could not express the
	// difference, and the difference is the whole reason Restore exists.
	retained map[uint64][]ports.PluginInfo
	// calls records every request, in order, as "op:edge".
	calls []string
	// removes that fail.
	failRemove map[uint64]bool
	// silent nodes produce no answer at all, the way a tunnel call that
	// timed out does. They stay pending, which is the state the wave gate
	// exists to hold the release on.
	silent map[uint64]bool
}

func newFleet(ids ...uint64) *scriptedNode {
	n := &scriptedNode{
		install:    map[uint64]Outcome{},
		state:      map[uint64][]ports.PluginInfo{},
		retained:   map[uint64][]ports.PluginInfo{},
		failRemove: map[uint64]bool{},
		silent:     map[uint64]bool{},
	}
	for _, id := range ids {
		n.state[id] = nil
		n.retained[id] = nil
	}
	return n
}

// Install behaves the way a node does, and the fixture copies the two
// details that matter rather than approximating them:
//
//   - it reports Replaced, the entry it overwrote, because that is the
//     only moment the information exists. A fixture that let the manager
//     read the previous version out of the post-state would be testing a
//     protocol the node does not speak, and a manager written against it
//     would restore the release it is rolling back.
//   - a refusal does not move the node, which is the property the rollback
//     logic depends on when it decides who was reached.
func (n *scriptedNode) Install(_ context.Context, edgeID uint64, spec ports.PluginSpec) Outcome {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, fmt.Sprintf("install:%d:%s@%s", edgeID, spec.Name, spec.Version))
	if n.silent[edgeID] {
		return Outcome{}
	}

	if scripted, ok := n.install[edgeID]; ok {
		scoped := scripted
		if scripted.Status == StatusInstalled {
			before := find(n.state[edgeID], spec.Name)
			n.state[edgeID] = upsert(n.state[edgeID], ports.PluginInfo{
				Name: spec.Name, Version: spec.Version, Digest: "d-" + spec.Version,
			})
			n.retained[edgeID] = retain(n.retained[edgeID], ports.PluginInfo{
				Name: spec.Name, Version: spec.Version, Digest: "d-" + spec.Version,
			})
			if before != nil && before.Version != spec.Version {
				cp := *before
				scoped.Replaced = &cp
			}
		}
		scoped.Set = append([]ports.PluginInfo(nil), n.state[edgeID]...)
		return scoped
	}

	var before *ports.PluginInfo
	if b := find(n.state[edgeID], spec.Name); b != nil && b.Version != spec.Version {
		cp := *b
		before = &cp
	}
	n.state[edgeID] = upsert(n.state[edgeID], ports.PluginInfo{
		Name: spec.Name, Version: spec.Version, Digest: "d-" + spec.Version,
	})
	n.retained[edgeID] = retain(n.retained[edgeID], ports.PluginInfo{
		Name: spec.Name, Version: spec.Version, Digest: "d-" + spec.Version,
	})
	return Outcome{
		Status:   StatusInstalled,
		Digest:   "d-" + spec.Version,
		Replaced: before,
		Set:      append([]ports.PluginInfo(nil), n.state[edgeID]...),
	}
}

// find returns the entry for a name, or nil.
func find(list []ports.PluginInfo, name string) *ports.PluginInfo {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

func (n *scriptedNode) Remove(_ context.Context, edgeID uint64, name, version string) Outcome {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, fmt.Sprintf("remove:%d:%s@%s", edgeID, name, version))
	if n.failRemove[edgeID] {
		return Outcome{Status: StatusFailed, Reason: "the store directory is not writable"}
	}
	var out []ports.PluginInfo
	for _, p := range n.state[edgeID] {
		if p.Name == name && p.Version == version {
			continue
		}
		out = append(out, p)
	}
	n.state[edgeID] = out
	// The bytes go too. The real store removes the directory, which is
	// why a restore of a version that was removed is a refusal.
	var kept []ports.PluginInfo
	for _, p := range n.retained[edgeID] {
		if p.Name == name && p.Version == version {
			continue
		}
		kept = append(kept, p)
	}
	n.retained[edgeID] = kept
	return Outcome{Status: StatusInstalled, Set: append([]ports.PluginInfo(nil), out...)}
}

// Restore behaves the way the real node does: it reactivates bytes that are
// already on disk and refuses a version that is not there.
//
// The refusal is the load-bearing half. A restore that fell back to
// fetching would make every rollback an install, and the manager — which
// has no URL for the version it is restoring — would ship a release while
// believing it had undone one.
func (n *scriptedNode) Restore(_ context.Context, edgeID uint64, name, version string) Outcome {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, fmt.Sprintf("restore:%d:%s@%s", edgeID, name, version))
	if n.silent[edgeID] {
		return Outcome{}
	}
	// Anything currently active is on disk by definition — the real store
	// lists its package directory, and a package can only be in the agent's
	// set if its directory is there. So a fixture that seeded `state`
	// directly, as the tests that pre-load a fleet do, still has the bytes
	// a restore needs.
	if p := find(n.retained[edgeID], name); p == nil || p.Version != version {
		if active := find(n.state[edgeID], name); active == nil || active.Version != version {
			return Outcome{
				Status: StatusRefused,
				Reason: fmt.Sprintf("%s@%s is not installed on this node; a restore does not fetch", name, version),
			}
		}
	}
	n.state[edgeID] = upsert(n.state[edgeID], ports.PluginInfo{
		Name: name, Version: version, Digest: "d-" + version,
	})
	return Outcome{Status: StatusInstalled, Digest: "d-" + version, Set: append([]ports.PluginInfo(nil), n.state[edgeID]...)}
}

// seed puts a package on a node the way a previous release would have:
// active *and* on disk.
//
// Tests that pre-load a fleet must set both, because the real node keeps a
// superseded version's directory and a fixture that only set the active set
// could not be restored to — which is the difference the rollback path
// depends on.
func (n *scriptedNode) seed(edgeID uint64, infos ...ports.PluginInfo) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, info := range infos {
		n.state[edgeID] = upsert(n.state[edgeID], info)
		n.retained[edgeID] = retain(n.retained[edgeID], info)
	}
}

func (n *scriptedNode) versions(edgeID uint64) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, p := range n.state[edgeID] {
		out = append(out, p.Name+"@"+p.Version)
	}
	sort.Strings(out)
	return out
}

func (n *scriptedNode) installedOn(edgeID uint64, name string) bool {
	for _, v := range n.versions(edgeID) {
		if strings.HasPrefix(v, name+"@") {
			return true
		}
	}
	return false
}

// retain records that a package version's bytes are on disk.
//
// It is not upsert. upsert is keyed by name, which is right for the active
// set — a node publishes one version per name — and wrong for the disk,
// where an upgrade deliberately keeps the version it superseded so a
// rollback has something to go back to. A fixture that keyed the disk by
// name would lose the superseded version at the exact moment the rollback
// needed it, and would report a correct restore as a refusal.
func retain(list []ports.PluginInfo, p ports.PluginInfo) []ports.PluginInfo {
	for _, e := range list {
		if e.Name == p.Name && e.Version == p.Version {
			return list
		}
	}
	return append(append([]ports.PluginInfo(nil), list...), p)
}

func upsert(list []ports.PluginInfo, p ports.PluginInfo) []ports.PluginInfo {
	out := make([]ports.PluginInfo, 0, len(list)+1)
	replaced := false
	for _, e := range list {
		if e.Name == p.Name {
			out = append(out, p)
			replaced = true
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, p)
	}
	return out
}

func specFor(name, version string) ports.PluginSpec {
	return ports.PluginSpec{
		Name: name, Version: version,
		URL:       "https://releases.invalid/" + name + ".tar.gz",
		SHA256:    strings.Repeat("a", 64),
		Signature: "sig",
	}
}

func nodes(count int) []uint64 {
	out := make([]uint64, 0, count)
	for i := 1; i <= count; i++ {
		out = append(out, uint64(i))
	}
	return out
}

// The canary comes first and nothing else goes out until it is answered.
func TestTheFirstWaveIsTheCanaryAndNothingElseGoesOutBeforeItIsAnswered(t *testing.T) {
	fleet := newFleet(nodes(50)...)
	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(50), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.Plan().WaveNumber() != 1 {
		t.Fatalf("wave = %d, want the canary to be wave 1", r.Plan().WaveNumber())
	}
	wave := r.Plan().Wave()
	// A tenth, and never zero: a canary sized by a rule somebody chose,
	// not one that a small fleet rounds away to nothing.
	if len(wave) != 5 {
		t.Fatalf("the first wave holds %d nodes, want a tenth of fifty", len(wave))
	}
	touched := 0
	for _, id := range nodes(50) {
		if fleet.installedOn(id, "acme-probe") {
			touched++
		}
	}
	if touched != len(wave) {
		t.Errorf("%d nodes have the package, want exactly the %d in the canary", touched, len(wave))
	}
}

func TestAFleetTooSmallToTakeATenthStillGetsACanary(t *testing.T) {
	// Rounding is the failure mode here. 50 * 1 / 10 is 5; 5 * 1 / 10 is
	// 0, and a rollout whose first wave is empty is a rollout that starts
	// by installing to everything except a canary it never ran.
	fleet := newFleet(nodes(5)...)
	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(5), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := len(r.Plan().Wave()); got != 1 {
		t.Errorf("the first wave holds %d nodes, want 1", got)
	}
}

func TestTheNextWaveIsNotDispatchedWhileTheCanaryIsUnanswered(t *testing.T) {
	// Every node here produces no answer, the way a tunnel call that timed
	// out does. The canary's answers arrive out of band through Report,
	// which is the shape a real release has: the job dispatches, the
	// tunnel's read loop answers later.
	fleet := newFleet(nodes(50)...)
	for _, id := range nodes(50) {
		fleet.silent[id] = true
	}
	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(50), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := len(r.Plan().Pending()); got != len(r.Plan().Wave()) {
		t.Fatalf("%d of %d canary nodes are pending, want all of them", got, len(r.Plan().Wave()))
	}
	if advanced, _ := r.Advance(context.Background()); advanced {
		t.Error("the next wave went out while the canary had not answered")
	}

	// The canary answers. Only now may the release move.
	for _, id := range r.Plan().Pending() {
		r.Report(context.Background(), id, Outcome{Status: StatusInstalled})
	}
	if got := r.Status().Pending; len(got) != 0 {
		t.Errorf("%d canary nodes are still pending after they answered", len(got))
	}
	advanced, err := r.Advance(context.Background())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !advanced {
		t.Fatal("the release did not move once the canary had answered")
	}
	if r.Plan().WaveNumber() != 2 {
		t.Fatalf("the release is on wave %d, want 2", r.Plan().WaveNumber())
	}
	if got := len(r.Plan().Pending()); got != len(r.Plan().Wave()) {
		t.Fatalf("%d of %d wave-two nodes are pending, want all of them", got, len(r.Plan().Wave()))
	}

	// Wave two is out and silent. Wave three must not be.
	if advanced, _ := r.Advance(context.Background()); advanced {
		t.Error("wave three went out while wave two had not answered")
	}
	if r.Plan().WaveNumber() != 2 {
		t.Errorf("the release is on wave %d, want it held at 2", r.Plan().WaveNumber())
	}
}

// The canary's whole job. A halt reached in wave one must stop the
// release walking outward, and a caller that only checked "is the wave
// accounted for" would ship the package to the fleet.
func TestAHaltStopsTheReleaseEvenThoughTheCanaryIsAccountedFor(t *testing.T) {
	fleet := newFleet(nodes(50)...)
	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(50), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The canary is installed and something downstream noticed it broke
	// the node: the agent is Degraded, the node is missing its tools, the
	// process will not come up. The release is stopped.
	r.Halt("the canary's agent came up Degraded")

	if !r.Halted() {
		t.Fatal("Halt did not take")
	}
	if _, err := r.Advance(context.Background()); err == nil {
		t.Error("Advance succeeded on a halted rollout; the release would keep walking outward")
	}

	installed := 0
	for _, id := range nodes(50) {
		if fleet.installedOn(id, "acme-probe") {
			installed++
		}
	}
	if want := len(r.Plan().Wave()); installed != want {
		t.Errorf("%d nodes have the package, want only the canary's %d", installed, want)
	}
}

func TestARollbackTakesThePackageBackOffEveryNodeTheReleaseReached(t *testing.T) {
	fleet := newFleet(nodes(30)...)
	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(30), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Push two more waves out so the release is genuinely in flight.
	for i := 0; i < 2; i++ {
		ok, err := r.Advance(context.Background())
		if err != nil || !ok {
			t.Fatalf("Advance %d: ok=%v err=%v", i, ok, err)
		}
	}
	reached := 0
	for _, id := range nodes(30) {
		if fleet.installedOn(id, "acme-probe") {
			reached++
		}
	}
	if reached < 2 {
		t.Fatalf("only %d nodes have the package; the rollback would have nothing to undo", reached)
	}

	r.Halt("the canary broke its node")
	if err := r.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	for _, id := range nodes(30) {
		if fleet.installedOn(id, "acme-probe") {
			t.Errorf("node %d still has the package after the rollback", id)
		}
	}
}

func TestARollbackRestoresTheVersionANodeWasAlreadyOn(t *testing.T) {
	// The case that makes rollback more than a delete. Half the fleet is
	// on 0.9.0; the release puts 1.0.0 on them; a rollback that just
	// removes leaves them on nothing, which is a worse outage than the
	// one that triggered it.
	fleet := newFleet(nodes(30)...)
	for _, id := range nodes(30) {
		if id%2 == 0 {
			fleet.seed(id, ports.PluginInfo{Name: "acme-probe", Version: "0.9.0", Digest: "d-0.9.0"})
		}
	}
	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(30), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := r.Advance(context.Background()); err != nil {
			t.Fatalf("Advance: %v", err)
		}
	}
	if err := r.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	for _, id := range nodes(30) {
		want := []string{"acme-probe@0.9.0"}
		if id%2 == 1 {
			want = nil
		}
		if got := fleet.versions(id); !equal(got, want) {
			t.Errorf("node %d is on %v, want %v", id, got, want)
		}
	}
}

func TestARollbackRefusesRatherThanFetchesWhenTheOldVersionIsGone(t *testing.T) {
	// The property that makes a restore a restore: the bytes must already
	// be on the node. A node that has lost the previous version — the
	// store directory was cleaned, the disk was replaced — must refuse,
	// because the alternative is that a rollback silently becomes an
	// install, and the manager has no URL for the version it is rolling
	// back to. Shipping the release it meant to undo is the one outcome a
	// rollback must never produce.
	fleet := newFleet(nodes(20)...)
	// Every node has 0.9.0 active but not on disk: the operator cleaned the
	// store, or the directory was lost.
	for _, id := range nodes(20) {
		fleet.state[id] = []ports.PluginInfo{{Name: "acme-probe", Version: "0.9.0", Digest: "d-0.9.0"}}
	}

	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(20), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Only the calls the rollback itself makes are examined. The release's
	// own installs are earlier in the log, and counting them would make
	// this test fail on the release rather than on the rollback.
	before := len(fleet.calls)
	if err := r.Rollback(context.Background()); err == nil {
		t.Fatal("a rollback whose previous version is not on disk reported success")
	}
	duringRollback := fleet.calls[before:]
	// And no node was left running the release under cover of the failure.
	for _, id := range nodes(20) {
		for _, v := range fleet.versions(id) {
			if v == "acme-probe@1.0.0" {
				t.Errorf("node %d is still on the rolled-back release %s", id, v)
			}
		}
	}
	for _, c := range duringRollback {
		if strings.HasPrefix(c, "install:") {
			t.Errorf("the rollback issued an install, so it fetched rather than restored: %q", c)
		}
		if strings.HasPrefix(c, "restore:") && strings.HasSuffix(c, "@1.0.0") {
			t.Errorf("the rollback restored the release it was undoing: %q", c)
		}
	}
}

func TestARollbackDoesNotTouchANodeThatRefusedTheRelease(t *testing.T) {
	// A refused node was never changed. Asking it to remove would be
	// asking a node that never installed anything to take something away —
	// and on a node that already had the package, that would remove the
	// operator's own installation in the name of undoing ours.
	fleet := newFleet(nodes(20)...)
	var canary uint64
	// Find the canary by starting a throwaway plan.
	probe, _ := Start(context.Background(), newFleet(nodes(20)...), specFor("p", "1"), nodes(20), "rolling", nil)
	canary = probe.Plan().Wave()[0]
	fleet.install[canary] = Outcome{
		Status: StatusRefused,
		Reason: "this node's ceiling is L1 and the package is L2",
	}

	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(20), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	for _, c := range fleet.calls {
		if strings.HasPrefix(c, fmt.Sprintf("remove:%d:", canary)) {
			t.Errorf("the rollback removed from node %d, which refused the release and was never changed", canary)
		}
	}
}

func TestARollbackSaysWhichNodesItCouldNotPutBack(t *testing.T) {
	// A rollback that reports "done" while a node is still on the bad
	// version is the worst outcome available: the operator believes the
	// fleet is back and it is not.
	fleet := newFleet(nodes(20)...)
	for _, id := range nodes(20) {
		if id%2 == 0 {
			fleet.seed(id, ports.PluginInfo{Name: "acme-probe", Version: "0.9.0", Digest: "d-0.9.0"})
		}
	}
	fleet.failRemove[3] = true

	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(20), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.Advance(context.Background()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	err = r.Rollback(context.Background())
	if err == nil {
		t.Fatal("a rollback that could not remove a package reported success")
	}
	if !strings.Contains(err.Error(), "node 3") {
		t.Errorf("error = %q, want it to name the node it could not put back", err)
	}
}

func TestARollbackHappensOnce(t *testing.T) {
	// A second rollback would remove a package the first one already
	// removed, and on a node that has since been moved forward that means
	// removing somebody else's installation.
	fleet := newFleet(nodes(20)...)
	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(20), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.Advance(context.Background()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if err := r.Rollback(context.Background()); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	before := len(fleet.calls)
	if err := r.Rollback(context.Background()); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	if got := len(fleet.calls); got != before {
		t.Errorf("the second rollback sent %d more requests, want none", got-before)
	}
}

func TestAnAnswerFromANodeOutsideTheCurrentWaveIsIgnored(t *testing.T) {
	// The tunnel handler that receives an answer is not the goroutine that
	// dispatched it, so a slow node's answer can land after the release
	// has moved on. Recording it would confirm a node nobody is waiting
	// for, and the release would read as further along than it is.
	fleet := newFleet(nodes(50)...)
	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(50), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.Advance(context.Background()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	inWave := map[uint64]bool{}
	for _, id := range r.Plan().Wave() {
		inWave[id] = true
	}
	var stranger uint64
	for _, id := range nodes(50) {
		if !inWave[id] {
			stranger = id
			break
		}
	}
	if stranger == 0 {
		t.Fatal("no node outside the current wave to test with")
	}
	waveBefore := r.Plan().WaveNumber()
	r.Report(context.Background(), stranger, Outcome{Status: StatusInstalled})
	if r.Plan().WaveNumber() != waveBefore {
		t.Errorf("an answer from a node outside the wave moved the release on")
	}
}

func TestTheStatusAConsoleShowsNamesTheWaveAndTheHalt(t *testing.T) {
	fleet := newFleet(nodes(50)...)
	r, err := Start(context.Background(), fleet, specFor("acme-probe", "1.0.0"), nodes(50), "rolling", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.Halt("the canary's agent came up Degraded")
	st := r.Status()
	if st.Plugin != "acme-probe" || st.Version != "1.0.0" {
		t.Errorf("status names %s@%s, want acme-probe@1.0.0", st.Plugin, st.Version)
	}
	if st.Wave != 1 || st.Waves < 2 {
		t.Errorf("status says wave %d of %d, want the canary of several", st.Wave, st.Waves)
	}
	if !st.Halted || !strings.Contains(st.Reason, "Degraded") {
		t.Errorf("status does not carry the halt: %+v", st)
	}
	if st.Summary == "" {
		t.Error("status carries no summary line for the console to show")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
