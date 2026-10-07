package pluginmanifest

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// errTest is the failure a node reports when the install did not work. It
// never gets compared — the rollout records that a node failed, not why —
// so the simplest possible error is the right one.
var errTest = errors.New("install failed")

// The rollout's tests are about one thing: a wave does not end until
// everybody in it has answered.
//
// Everything else — how the canary is chosen, how the waves are cut — is
// a convenience. The property that keeps a bad package off most of a
// fleet is the gate, and it is the one that is easy to write in a way
// that looks right and is not.

func fleet(n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = uint64(i + 1)
	}
	return out
}

func TestTheFirstWaveIsACanaryAndItIsSmall(t *testing.T) {
	// On a hundred nodes the canary is ten. Large enough to include a node
	// with the kernel version or the filesystem that the package breaks on,
	// small enough that a mistake costs a tenth of the fleet.
	r, err := PlanRollout("acme-probe", "1.0.0", fleet(100), domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	canary := r.Wave()
	if want := 100 * CanaryFractionTenths / 10; len(canary) != want {
		t.Errorf("canary holds %d nodes, want %d", len(canary), want)
	}
	if r.WaveNumber() != 1 {
		t.Errorf("the rollout starts at wave %d, want 1", r.WaveNumber())
	}
	if r.WaveCount() < 2 {
		t.Errorf("a hundred nodes planned into %d wave(s); the canary is not a canary if it is the fleet",
			r.WaveCount())
	}
}

func TestAFleetOfOneStillGetsACanary(t *testing.T) {
	// The degenerate case that a size calculation gets wrong: a canary of
	// ten percent of one node rounds to zero, and a zero-node first wave
	// means the rollout starts by installing to everybody.
	r, err := PlanRollout("acme-probe", "1.0.0", []uint64{7}, domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	if got := r.Wave(); len(got) != 1 || got[0] != 7 {
		t.Errorf("wave = %v, want the single node", got)
	}
	if r.WaveCount() != 1 {
		t.Errorf("a one-node fleet planned into %d waves", r.WaveCount())
	}
}

func TestTheCanaryIsTheSameNodesEveryTimeForOneRelease(t *testing.T) {
	// Determinism is what makes a canary discussable. A random canary
	// means a retried rollout tests a different fleet, so a package that
	// broke ten percent of nodes never retests the ten percent that broke,
	// and "the canary passed" means nothing on the next attempt.
	first, err := PlanRollout("acme-probe", "1.0.0", fleet(50), domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	// The same release, presented in a different order, must plan the same
	// rollout — otherwise the result depends on how the node list happened
	// to be assembled.
	reversed := fleet(50)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	second, err := PlanRollout("acme-probe", "1.0.0", reversed, domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	if strings.Join(fmtIDs(first.Wave()), ",") != strings.Join(fmtIDs(second.Wave()), ",") {
		t.Errorf("the same release planned two different canaries: %v vs %v",
			first.Wave(), second.Wave())
	}
}

func TestAReleaseReachesADifferentCanaryThanTheOneBefore(t *testing.T) {
	// The other half of determinism. A canary that is always the same
	// nodes re-tests the machines that were just proven, and a package
	// that only breaks on some hardware passes every release.
	v1, err := PlanRollout("acme-probe", "1.0.0", fleet(200), domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	v2, err := PlanRollout("acme-probe", "1.1.0", fleet(200), domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	same := 0
	for _, id := range v1.Wave() {
		for _, other := range v2.Wave() {
			if id == other {
				same++
			}
		}
	}
	if same == len(v1.Wave()) {
		t.Errorf("two releases canaried exactly the same %d nodes; the canary is not rotating",
			same)
	}
}

func TestTheNextWaveDoesNotStartWhileTheCurrentOneIsWaiting(t *testing.T) {
	// The property the whole type exists for.
	r, err := PlanRollout("acme-probe", "1.0.0", fleet(100), domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	canary := r.Wave()
	if len(canary) < 2 {
		t.Fatalf("the canary holds %d nodes, too few to test the gate", len(canary))
	}

	// Nobody has answered.
	if r.Advance() {
		t.Fatal("the rollout advanced to the rest of the fleet before the canary reported anything")
	}
	// Most of them answer.
	for i, id := range canary {
		if i == len(canary)-1 {
			continue // one node is left silent
		}
		r.Confirm(id)
	}
	if r.Advance() {
		t.Fatal("the rollout advanced while one canary node was still unaccounted for")
	}
	if r.WaveNumber() != 1 {
		t.Errorf("wave = %d, want the rollout to still be on the canary", r.WaveNumber())
	}

	// The last one answers, and only now does it move.
	r.Confirm(canary[len(canary)-1])
	if !r.Advance() {
		t.Fatal("the rollout did not advance once the canary was fully accounted for")
	}
	if r.WaveNumber() != 2 {
		t.Errorf("wave = %d, want 2", r.WaveNumber())
	}
}

func TestAFailedNodeCountsAsAnsweredButIsStillVisible(t *testing.T) {
	// A rollout that refuses to move because one node never reported would
	// stall a whole fleet on a machine that is down for unrelated reasons.
	// So a failure unblocks the wave — and is impossible to miss, because
	// the caller has to look at Failed() to find out why it moved early.
	r, err := PlanRollout("acme-probe", "1.0.0", fleet(100), domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	canary := r.Wave()
	for _, id := range canary {
		r.Fail(id, errTest)
	}
	if got := len(r.Failed()); got != len(canary) {
		t.Errorf("Failed() returned %d nodes, want %d", got, len(canary))
	}
	if !r.Advance() {
		t.Fatal("a fully-failed canary blocked the rollout forever; the operator needs to be able to stop it themselves")
	}
	if !strings.Contains(r.Progress(), "failed") {
		t.Errorf("progress = %q, want it to say nodes failed", r.Progress())
	}
}

func TestAFailedNodeIsNotAlsoConfirmed(t *testing.T) {
	// A node that reported success and then reported failure has failed.
	// Recording both would let a canary look healthy in a summary while
	// the reason it moved on was a failure.
	r, err := PlanRollout("acme-probe", "1.0.0", fleet(100), domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	id := r.Wave()[0]
	r.Confirm(id)
	r.Fail(id, errTest)

	if len(r.Pending()) != len(r.Wave())-1 {
		t.Errorf("pending = %d, want the failed node excluded", len(r.Pending()))
	}
	// The summary has to agree with the two maps. A node that both
	// confirmed and failed would be counted once as success, and a canary
	// that "passed" while every node in it reported a problem is exactly
	// the summary that gets believed.
	if got := r.Progress(); !strings.Contains(got, "0 ok") || !strings.Contains(got, "1 failed") {
		t.Errorf("progress = %q, want the node counted only as failed", got)
	}
}

func TestEveryNodeEndsUpInExactlyOneWave(t *testing.T) {
	// A node in two waves would be installed twice, the second time over
	// the first without waiting for it. The plan is also the whole of what
	// a dry run would print, so a node that appears nowhere is a node that
	// silently never gets the package.
	nodes := fleet(97)
	r, err := PlanRollout("acme-probe", "1.0.0", nodes, domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	seen := map[uint64]int{}
	for wave := 0; wave < r.WaveCount(); wave++ {
		for _, id := range r.Wave() {
			seen[id]++
		}
		if wave == r.WaveCount()-1 {
			break
		}
		for _, id := range r.Wave() {
			r.Confirm(id)
		}
		r.Advance()
	}
	if len(seen) != len(nodes) {
		t.Errorf("the plan covers %d of %d nodes", len(seen), len(nodes))
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("node %d appears in %d waves", id, times)
		}
	}
}

func TestAPinnedPackageGoesOutInOneWave(t *testing.T) {
	// A pinned package is one an operator chose deliberately and that will
	// not be auto-upgraded, so canary-ing it is theatre: the next release
	// will not be canaried either.
	r, err := PlanRollout("acme-repair", "1.0.0", fleet(100), domain.InstallPin)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	if r.WaveCount() != 1 {
		t.Errorf("a pinned package was planned into %d waves, want 1", r.WaveCount())
	}
	if len(r.Wave()) != 100 {
		t.Errorf("the single wave holds %d nodes, want all 100", len(r.Wave()))
	}
}

func TestTheRolloutIsFinishedWhenTheLastWaveIsAccountedFor(t *testing.T) {
	r, err := PlanRollout("acme-probe", "1.0.0", fleet(12), domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	for r.WaveCount() > 0 {
		for _, id := range r.Wave() {
			r.Confirm(id)
		}
		if r.Done() {
			break
		}
		if !r.Advance() {
			t.Fatalf("the rollout stalled at wave %d with nothing pending: %s", r.WaveNumber(), r.Progress())
		}
	}
	if !r.Done() {
		t.Errorf("the rollout is not done after every wave was confirmed: %s", r.Progress())
	}
	if r.Advance() {
		t.Error("a finished rollout advanced past its last wave")
	}
}

func TestANodeListedTwiceIsRefusedRatherThanInstalledTwice(t *testing.T) {
	// Two entries for one node means the second wave installs over the
	// first without waiting for it. Caught at plan time, because catching
	// it during the rollout means it already happened somewhere.
	_, err := PlanRollout("acme-probe", "1.0.0", []uint64{1, 2, 2, 3}, domain.InstallRolling)
	if err == nil {
		t.Fatal("a node listed twice was planned into two waves")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("error = %q, want it to say the node appears twice", err)
	}
}

func TestAnEmptyFleetIsRefused(t *testing.T) {
	if _, err := PlanRollout("acme-probe", "1.0.0", nil, domain.InstallRolling); err == nil {
		t.Error("a rollout to no nodes was planned")
	}
}

func TestAnUnknownStrategyIsTreatedAsRolling(t *testing.T) {
	// An unrecognised strategy must not be the permissive one. It falls
	// through to the rolling plan, which canaried — and a manifest whose
	// strategy is misspelled therefore gets the safer of the two plans
	// rather than the fleet-sized one.
	r, err := PlanRollout("acme-probe", "1.0.0", fleet(100), "rolliing")
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	if len(r.Wave()) == 100 {
		t.Error("a misspelled install strategy skipped the canary")
	}
}

func TestProgressNamesThePackageAndTheWave(t *testing.T) {
	// This line is what an operator watches during a rollout. It has to
	// answer "which package, how far, is anything wrong".
	r, err := PlanRollout("acme-repair", "2.1.0", fleet(50), domain.InstallRolling)
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	got := r.Progress()
	for _, want := range []string{"acme-repair", "2.1.0", "1/", "awaiting"} {
		if !strings.Contains(got, want) {
			t.Errorf("progress = %q, want it to mention %q", got, want)
		}
	}
}

func TestANilRolloutIsInertRatherThanAPanic(t *testing.T) {
	// A caller that built no rollout — because planning failed — will
	// still ask it things. None of them may panic.
	var r *Rollout
	r.Confirm(1)
	r.Fail(1, errTest)
	if r.Wave() != nil || r.Done() || r.Advance() {
		t.Error("a nil rollout reported progress")
	}
	if r.WaveNumber() != 0 || r.WaveCount() != 0 {
		t.Error("a nil rollout reported waves")
	}
	if got := r.Progress(); got == "" {
		t.Error("a nil rollout has no progress line")
	}
	if len(r.Pending()) != 0 || len(r.Failed()) != 0 {
		t.Error("a nil rollout reported nodes")
	}
}

// fmtIDs renders a wave so two plans can be compared as strings in a
// failure message, which is easier to read than two integer slices.
func fmtIDs(ids []uint64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatUint(id, 10)
	}
	return out
}
