package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The tests below are about the one thing the driver adds: a release that
// moves with nobody watching it. Every assertion is on what a node ended
// up holding or on what the release was told — never on the driver's own
// bookkeeping, because a driver that advanced the fleet while reporting
// nothing would pass a test written against its internals.

func newDriverFor(t *testing.T, n *scriptedNode, opts Options) *Driver {
	t.Helper()
	d, err := NewDriver(newManager(t, n), opts)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	return d
}

// canaryFor is the first wave's node ids, read out of the planner itself.
//
// The tests need these before Start, because a node has to be scripted
// silent *before* the wave is dispatched at it — a node marked afterwards
// is a node nobody was ever asked, which is a different and much easier
// thing to get right. Reading the ids from the planner rather than
// hardcoding node 1 also matters: the canary is chosen by hashing the
// package identity, so a test that assumed an index was testing a
// different release than the one it thought it was.
func canaryFor(t *testing.T, name, version string, fleet []uint64) []uint64 {
	t.Helper()
	plan, err := pluginmanifest.PlanRollout(name, version, fleet, "rolling")
	if err != nil {
		t.Fatalf("PlanRollout: %v", err)
	}
	out := plan.Wave()
	if len(out) == 0 {
		t.Fatal("the planner produced a first wave with no nodes in it")
	}
	return append([]uint64(nil), out...)
}

// startAndScript marks the canary's first node per the fixture's script,
// then starts the release so the wave is actually dispatched at it.
func startAndScript(t *testing.T, d *Driver, n *scriptedNode, mark func(node uint64)) {
	t.Helper()
	canary := canaryFor(t, "acme", "1.0.0", fleet25())
	mark(canary[0])
	if _, err := d.mgr.Start(context.Background(), releaseRequest("acme", "1.0.0")); err != nil {
		t.Fatalf("start: %v", err)
	}
}

// The reason the driver exists: a rolling release started and then left
// alone still reaches the fleet.
func TestTheDriverCarriesARollingReleaseToTheFleetWithNobodyPressingAdvance(t *testing.T) {
	n := newFleet(fleet25()...)
	d := newDriverFor(t, n, Options{})
	startAndScript(t, d, n, func(uint64) {})

	canary := canaryFor(t, "acme", "1.0.0", fleet25())
	if got := len(reached(n, "acme")); got != len(canary) {
		t.Fatalf("after Start the package is on %d nodes, want only the canary's %d", got, len(canary))
	}

	// One pass per wave, bounded well past the plan's wave count so a
	// driver that stopped early fails loudly rather than by timeout.
	for pass := 0; pass < 10; pass++ {
		d.Step(context.Background())
	}

	st, err := d.mgr.Status("acme")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Done {
		t.Fatalf("release is not done: %+v", st)
	}
	if got := len(reached(n, "acme")); got != 25 {
		t.Fatalf("package reached %d nodes, want the whole 25-node fleet", got)
	}
}

// The safety property. Rollout.Advance deliberately walks past a node that
// *failed* — its gate is "accounted for", confirmed or failed — so a
// driver built on Advance alone would carry a broken package out to every
// node in the fleet at three in the morning.
func TestTheDriverHaltsOnAFailedNodeInsteadOfAdvancingPastIt(t *testing.T) {
	n := newFleet(fleet25()...)
	d := newDriverFor(t, n, Options{})
	startAndScript(t, d, n, func(id uint64) {
		n.install[id] = Outcome{Status: StatusRefused, Reason: "signature key unknown"}
	})

	canary := canaryFor(t, "acme", "1.0.0", fleet25())
	// The decision is looked for across the passes rather than in the
	// last one: once halted, every later pass is correctly idle, so
	// asserting on the final pass would be asserting the halt *did not*
	// stick.
	var acted Decision
	for pass := 0; pass < 10; pass++ {
		for _, dec := range d.Step(context.Background()) {
			if dec.Action != ActionIdle {
				acted = dec
			}
		}
		if acted.Action == ActionHalted {
			break
		}
	}
	if acted.Action != ActionHalted {
		t.Fatalf("driver's only decision was %q, want it to halt on the refusal", acted.Action)
	}

	st, _ := d.mgr.Status("acme")
	if !st.Halted {
		t.Fatalf("release is not halted: %+v", st)
	}
	// The refusal has to be visible. A halt whose reason does not say what
	// went wrong sends the operator back to a status endpoint.
	if !strings.Contains(st.Reason, "1 node") {
		t.Errorf("halt reason = %q, want it to name how many nodes refused", st.Reason)
	}
	if got := len(reached(n, "acme")); got > len(canary) {
		t.Fatalf("package reached %d nodes, want at most the %d canary nodes", got, len(canary))
	}

	// A later pass must not step over the halt either.
	before := len(reached(n, "acme"))
	d.Step(context.Background())
	if after := len(reached(n, "acme")); after != before {
		t.Fatalf("a halted release reached %d more nodes on the next pass", after-before)
	}
}

// A node that answers nothing is exactly the state Rollout holds a
// release on for. The driver must not be the thing that overrides it:
// treating a blank answer as "probably fine" is how a driver becomes a
// fleet-wide rollout wearing a canary's name.
func TestTheDriverWaitsOnANodeThatHasNotAnsweredAndNeverCallsItInstalled(t *testing.T) {
	n := newFleet(fleet25()...)
	d := newDriverFor(t, n, Options{})
	startAndScript(t, d, n, func(id uint64) { n.silent[id] = true })

	canary := canaryFor(t, "acme", "1.0.0", fleet25())
	st, _ := d.mgr.Status("acme")
	if len(st.Pending) != 1 || st.Pending[0] != canary[0] {
		t.Fatalf("pending = %v, want exactly the silent canary node %d", st.Pending, canary[0])
	}

	for pass := 0; pass < 5; pass++ {
		for _, dec := range d.Step(context.Background()) {
			if dec.Action != ActionWaited {
				t.Fatalf("pass %d: action = %q, want the driver to keep waiting", pass, dec.Action)
			}
		}
	}
	if st, _ = d.mgr.Status("acme"); st.Wave != 1 {
		t.Fatalf("wave = %d after five passes, want it held at 1", st.Wave)
	}
	if got := len(reached(n, "acme")); got != len(canary)-1 {
		t.Fatalf("package is on %d nodes, want only the %d canary nodes that answered", got, len(canary)-1)
	}
}

// A node that never answers is a node the operator has to hear about. A
// driver that waits forever reports a release that looks like it is still
// working, which is the failure mode this whole package is about.
func TestTheDriverHaltsAStuckWaveAndNamesTheNodes(t *testing.T) {
	n := newFleet(fleet25()...)
	now := time.Now()
	d := newDriverFor(t, n, Options{
		StallAfter: 10 * time.Minute,
		Now:        func() time.Time { return now },
	})
	startAndScript(t, d, n, func(id uint64) { n.silent[id] = true })
	canary := canaryFor(t, "acme", "1.0.0", fleet25())

	// The driver has to have *seen* the pending set before elapsed time
	// means anything. A budget measured from a wave's dispatch rather
	// than from the driver's first observation would stall a release that
	// started ten minutes ago and has been watched for one second.
	d.Step(context.Background())

	// Inside the budget: waiting, and time alone is not yet a reason.
	now = now.Add(9 * time.Minute)
	for _, dec := range d.Step(context.Background()) {
		if dec.Action != ActionWaited {
			t.Fatalf("action = %q at 9 minutes, want it still waiting", dec.Action)
		}
	}

	now = now.Add(2 * time.Minute) // past 10 minutes of an unchanged pending set
	var last Decision
	for _, dec := range d.Step(context.Background()) {
		last = dec
	}
	if last.Action != ActionStalled {
		t.Fatalf("action = %q after the stall budget, want a stall", last.Action)
	}
	st, _ := d.mgr.Status("acme")
	if !st.Halted {
		t.Fatalf("a stalled wave did not halt the release: %+v", st)
	}
	// The operator's next question is always "which nodes", so the ids
	// have to be in the reason and not only in the status fields.
	if !strings.Contains(st.Reason, itoa(int(canary[0]))) {
		t.Errorf("stall reason = %q, want it to name node %d", st.Reason, canary[0])
	}
	if !strings.Contains(st.Reason, "10m0s") {
		t.Errorf("stall reason = %q, want it to say how long it waited", st.Reason)
	}
}

// The stall budget is about a pending set that stops *changing*. A node
// that answers late must restart the clock, or a slow-but-alive fleet
// gets halted by a timer that never looked at whether anything improved.
func TestALateAnswerRestartsTheStallClock(t *testing.T) {
	n := newFleet(fleet25()...)
	now := time.Now()
	d := newDriverFor(t, n, Options{
		StallAfter: 10 * time.Minute,
		Now:        func() time.Time { return now },
	})
	canary := canaryFor(t, "acme", "1.0.0", fleet25())
	startAndScript(t, d, n, func(id uint64) { n.silent[id] = true })
	d.Step(context.Background()) // observe the pending set, starting the budget

	now = now.Add(8 * time.Minute)
	for _, dec := range d.Step(context.Background()) {
		if dec.Action != ActionWaited {
			t.Fatalf("action = %q at 8 minutes, want it still waiting", dec.Action)
		}
	}

	// The answer arrives late, the way a pushed report would. Un-marking
	// the node silent would not do this: the wave was dispatched once and
	// that dispatch is gone, so nothing would re-ask it.
	d.mgr.Report(canary[0], Outcome{
		Plugin: "acme", Status: StatusInstalled, Digest: "d-1.0.0",
	})

	now = now.Add(4 * time.Minute) // 12 minutes since the wave started
	for _, dec := range d.Step(context.Background()) {
		if dec.Action == ActionStalled || dec.Action == ActionHalted {
			t.Fatalf("a release that was still making progress was halted: %+v", dec)
		}
	}
	if st, _ := d.mgr.Status("acme"); st.Halted {
		t.Fatalf("release halted even though a node answered late: %+v", st)
	}
	// The late answer let the wave through: the release is genuinely
	// further along, not merely un-halted.
	if st, _ := d.mgr.Status("acme"); st.Wave < 2 {
		t.Fatalf("wave = %d, want the late answer to have advanced the release", st.Wave)
	}

	// And the budget really is running from each observation rather than
	// from the release's start: a second release, watched from scratch,
	// still stalls after one full stretch of nothing changing.
	betaCanary := canaryFor(t, "beta", "2.0.0", fleet25())
	n.silent[betaCanary[0]] = true
	if _, err := d.mgr.Start(context.Background(), releaseRequest("beta", "2.0.0")); err != nil {
		t.Fatalf("start beta: %v", err)
	}
	d.Step(context.Background()) // beta's budget starts here, at this instant

	now = now.Add(9 * time.Minute)
	for _, dec := range d.Step(context.Background()) {
		if dec.Plugin == "beta" && dec.Action != ActionWaited {
			t.Fatalf("beta: action = %q at 9 minutes, want it still waiting", dec.Action)
		}
	}
	now = now.Add(2 * time.Minute) // a full stretch past the observation

	acted := Decision{}
	for _, dec := range d.Step(context.Background()) {
		if dec.Action == ActionStalled {
			acted = dec
		}
	}
	if acted.Action != ActionStalled {
		t.Fatalf("action = %q, want a stall once nothing changed for the whole budget", acted.Action)
	}
	if acted.Plugin != "beta" {
		t.Fatalf("stalled %q, want the release that was actually stuck", acted.Plugin)
	}
}

// A wiring that forgets to set a stall budget must get one. The first
// version of the driver read zero as "off", and the control plane's wiring
// passed zero — so the shipped binary had no stall budget, which is most
// of the reason the driver exists. The deadcode ratchet is what caught it:
// the default constant had no caller at all.
func TestAZeroStallBudgetMeansTheDefaultRatherThanNoStallCheck(t *testing.T) {
	n := newFleet(fleet25()...)
	now := time.Now()
	// No StallAfter at all: exactly what cmd/opskeeper passes.
	d := newDriverFor(t, n, Options{Now: func() time.Time { return now }})
	startAndScript(t, d, n, func(id uint64) { n.silent[id] = true })
	d.Step(context.Background())

	now = now.Add(24 * time.Hour)
	var acted Decision
	for _, dec := range d.Step(context.Background()) {
		if dec.Action == ActionStalled {
			acted = dec
		}
	}
	if acted.Action != ActionStalled {
		t.Fatalf("action = %q after a day, want the default stall budget to have fired", acted.Action)
	}
}

// Switching the check off is still possible, but it has to be written down:
// a negative duration, which no wiring sets by accident.
func TestANegativeStallBudgetDisablesTheCheckOnPurpose(t *testing.T) {
	n := newFleet(fleet25()...)
	now := time.Now()
	d := newDriverFor(t, n, Options{
		StallAfter: -1,
		Now:        func() time.Time { return now },
	})
	startAndScript(t, d, n, func(id uint64) { n.silent[id] = true })

	now = now.Add(365 * 24 * time.Hour)
	for _, dec := range d.Step(context.Background()) {
		if dec.Action != ActionWaited {
			t.Fatalf("action = %q, want the check to be off and the driver still waiting", dec.Action)
		}
	}
}

// A halt the operator wrote is the record of why a release stopped. The
// driver must not overwrite it with its own idle pass, and it must not
// walk past it either.
func TestTheDriverLeavesAnOperatorsHaltAlone(t *testing.T) {
	n := newFleet(fleet25()...)
	d := newDriverFor(t, n, Options{})
	startAndScript(t, d, n, func(uint64) {})
	canary := canaryFor(t, "acme", "1.0.0", fleet25())

	if _, err := d.mgr.Halt("acme", "vendor asked for a 30 minute window"); err != nil {
		t.Fatalf("halt: %v", err)
	}
	for pass := 0; pass < 5; pass++ {
		for _, dec := range d.Step(context.Background()) {
			if dec.Action != ActionIdle {
				t.Fatalf("pass %d: action = %q, want the driver to leave a halted release alone", pass, dec.Action)
			}
		}
	}
	st, _ := d.mgr.Status("acme")
	if st.Reason != "vendor asked for a 30 minute window" {
		t.Fatalf("reason = %q, want the operator's own sentence preserved", st.Reason)
	}
	if got := len(reached(n, "acme")); got != len(canary) {
		t.Fatalf("a halted release reached %d nodes, want only the %d canary nodes", got, len(canary))
	}
}

// Rollback forgets the release, and a driver left holding a record of it
// must not resurrect it or act on it later.
func TestTheDriverDoesNotResurrectARolledBackRelease(t *testing.T) {
	n := newFleet(fleet25()...)
	d := newDriverFor(t, n, Options{})
	startAndScript(t, d, n, func(uint64) {})

	if _, err := d.mgr.Rollback(context.Background(), "acme"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if decs := d.Step(context.Background()); len(decs) != 0 {
		t.Fatalf("driver acted on %d releases after the only one was rolled back", len(decs))
	}
	if got := reached(n, "acme"); len(got) != 0 {
		t.Fatalf("rollback left the package on %d nodes", len(got))
	}
}

// A driver wired to nothing is a goroutine that looks like automation.
func TestADriverWithNoManagerIsRefusedAtConstruction(t *testing.T) {
	if _, err := NewDriver(nil, Options{}); err == nil {
		t.Fatal("NewDriver(nil) succeeded; a driver that drives nothing must not start")
	}
}

// Run must be able to say why it stopped: a driver that returns nil on
// cancellation cannot be told apart from one that finished, and its work
// is never finished.
func TestRunStopsWithTheContextErrorSoShutdownCanTellWhyItStopped(t *testing.T) {
	n := newFleet(fleet25()...)
	d := newDriverFor(t, n, Options{Interval: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil after cancellation; that reads as 'finished'")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}
}

// Two drivers over one manager must not skip or double a wave. The lock
// is per-Rollout, so this is a real interleaving question rather than a
// bookkeeping one, and it is the shape a horizontally scaled control
// plane actually has.
func TestTwoDriversOverOneManagerDoNotSkipAWave(t *testing.T) {
	n := newFleet(fleet25()...)
	mgr := newManager(t, n)
	a, _ := NewDriver(mgr, Options{})
	b, _ := NewDriver(mgr, Options{})
	if _, err := mgr.Start(context.Background(), releaseRequest("acme", "1.0.0")); err != nil {
		t.Fatalf("start: %v", err)
	}
	for pass := 0; pass < 10; pass++ {
		a.Step(context.Background())
		b.Step(context.Background())
	}
	st, _ := mgr.Status("acme")
	if !st.Done {
		t.Fatalf("release not done: %+v", st)
	}
	if got := len(reached(n, "acme")); got != 25 {
		t.Fatalf("package reached %d nodes, want 25 — a wave was skipped or doubled", got)
	}
}

// A stalled wave that then finishes must not be halted on the strength of
// time that passed while the release was moving. This is the case that
// separates "pending set stopped changing" from "release has been running
// a long time".
func TestAReleaseThatKeepsMovingIsNeverStalledForTakingTooLong(t *testing.T) {
	n := newFleet(fleet25()...)
	now := time.Now()
	d := newDriverFor(t, n, Options{
		StallAfter: time.Second,
		Now:        func() time.Time { return now },
	})
	startAndScript(t, d, n, func(uint64) {})

	// Far more than the stall budget, one wave at a time.
	for pass := 0; pass < 10; pass++ {
		now = now.Add(time.Hour)
		for _, dec := range d.Step(context.Background()) {
			if dec.Action == ActionStalled || dec.Action == ActionHalted {
				t.Fatalf("pass %d: a release that was advancing every wave was halted: %+v", pass, dec)
			}
		}
	}
	st, _ := d.mgr.Status("acme")
	if !st.Done {
		t.Fatalf("release did not finish despite advancing every pass: %+v", st)
	}
	if got := len(reached(n, "acme")); got != 25 {
		t.Fatalf("package reached %d nodes, want 25", got)
	}
}
