package plugin

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Advancing a release without a person pressing the button.
//
// A release today only moves when somebody calls Advance. That is the
// right default — a wave gate nobody opened is a gate — but it is not a
// plan: an operator who starts a rolling release and then goes to lunch
// has a package sitting on the canary and a fleet that will not hear about
// it until they come back. The wave gate is a rule about evidence, not
// about who is watching.
//
// This is that missing piece, and it is deliberately narrow. It advances a
// release only when the current wave is **fully confirmed**, and it halts
// on anything else. Three decisions, and the two negative ones are the
// point:
//
//   - pending — somebody has not answered yet. Wait. Not "assume ok":
//     a blank answer is exactly the state Rollout holds a release on, and
//     a driver that treated it as success would be the fleet-wide rollout
//     wearing the canary's name.
//   - failed — at least one node refused or could not carry it out. Halt.
//     Not advance: Rollout.Advance would happily move on (its gate is
//     "accounted for", confirmed *or* failed), so an automatic driver
//     built on Advance alone walks a broken package out to every node in
//     the fleet with nobody watching. Halting instead of rolling back is
//     deliberate too: whether to take the canary back is a decision the
//     existing Halt doc already reserves for a human, and a driver that
//     rolls back unasked removes that pause.
//   - stalled — pending, and the set has not changed for longer than the
//     stall budget. Halt, naming the nodes. A node that never answers is
//     a node the operator needs to be told about, and a driver that waits
//     forever reports a release that looks like it is still working.

// Action is what the driver decided for one release in one pass.
type Action string

const (
	// ActionIdle means the release needs nothing: it is done, halted,
	// rolled back, or waiting on somebody.
	ActionIdle Action = "idle"
	// ActionAdvanced means a wave was sent.
	ActionAdvanced Action = "advanced"
	// ActionHalted means the driver stopped the release.
	ActionHalted Action = "halted"
	// ActionWaited means nodes have not answered yet and the driver is
	// leaving the release alone.
	ActionWaited Action = "waited"
	// ActionStalled means pending nodes stopped changing and the driver
	// halted the release rather than wait forever.
	ActionStalled Action = "stalled"
)

// Decision is one release's fate in one pass.
type Decision struct {
	Plugin string
	Action Action
	// Wave is the wave number the decision was about.
	Wave int
	// Pending and Failed are the node ids behind a Waited, Halted or
	// Stalled decision. They are carried because "the driver halted it"
	// without saying which nodes is a log line nobody can act on.
	Pending []uint64
	Failed  []uint64
	// Reason is the sentence a halt carries, and the only thing that says
	// why. Empty for a decision that did not halt.
	Reason string
}

// Clock is the driver's only source of time, so a test can drive a stall
// without sleeping through it.
type Clock func() time.Time

// Options configure a Driver.
type Options struct {
	// Interval is how often Run ticks. Zero means the default.
	Interval time.Duration
	// StallAfter is how long a wave's pending set may stay unchanged
	// before the driver gives up on it.
	//
	// Zero means the default, and that is the safe reading: a wiring that
	// forgets to set this gets a stall budget rather than none. The first
	// version of this file had zero *disable* the check, and the wiring
	// passed zero — so the shipped control plane silently had no stall
	// budget, which is most of the reason the driver exists. Turning the
	// check off is now spelled as a negative duration, so it has to be
	// written down deliberately.
	StallAfter time.Duration
	// Now is the clock. Nil means time.Now.
	Now Clock
	// Log is where the driver's own decisions go. Nil means slog.Default.
	Log *slog.Logger
}

// The two defaults are deliberately unexported. A caller passes zero and
// gets these; exporting them would invite a second copy of "15s" and
// "10m" into a wiring file, and a second copy is a second thing to forget
// to change. It would also be a symbol no production code references —
// the deadcode ratchet is right to call that out, and it was.
const (
	defaultInterval = 15 * time.Second
	// How long a wave may stay stuck before the driver halts it. Long
	// enough that a node restarting is not a stall, short enough that
	// "still releasing" is not a status an operator learns about from the
	// fleet's own incident.
	defaultStallAfter = 10 * time.Minute
)

// Driver advances releases nobody is advancing.
type Driver struct {
	mgr        *Manager
	interval   time.Duration
	stallAfter time.Duration
	now        Clock
	log        *slog.Logger

	mu sync.Mutex
	// stuck records, per package, the pending set the last pass saw and
	// when it first saw it. It is the driver's own state and deliberately
	// not the rollout's: a stalled wave is a fact about *watching*, and
	// putting it on the Rollout would make it look like part of the plan.
	stuck map[string]stallRecord
}

type stallRecord struct {
	pending []uint64
	since   time.Time
}

// NewDriver builds a Driver over a Manager. A nil Manager is refused here
// rather than at the first tick, because a driver that is wired to
// nothing is a goroutine that looks like automation.
func NewDriver(mgr *Manager, opts Options) (*Driver, error) {
	if mgr == nil {
		return nil, errors.New("a driver needs a release manager to drive")
	}
	d := &Driver{
		mgr:        mgr,
		interval:   opts.Interval,
		stallAfter: opts.StallAfter,
		now:        opts.Now,
		log:        opts.Log,
		stuck:      map[string]stallRecord{},
	}
	if d.interval <= 0 {
		d.interval = defaultInterval
	}
	if d.stallAfter == 0 {
		d.stallAfter = defaultStallAfter
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	return d, nil
}

// Step runs one pass and reports what it did to every release.
//
// It is exported so the control plane can drive it from its own loop, and
// so a test can advance the world one pass at a time instead of racing a
// ticker. Run is the only thing that knows about time passing.
func (d *Driver) Step(ctx context.Context) []Decision {
	var out []Decision
	d.mgr.each(func(plan *Rollout) {
		out = append(out, d.stepOne(ctx, plan))
	})
	return out
}

func (d *Driver) stepOne(ctx context.Context, plan *Rollout) Decision {
	st := plan.Status()
	dec := Decision{Plugin: st.Plugin, Wave: st.Wave}

	// Terminal releases are left exactly as they are. Advancing a done
	// release would be a no-op at best; halting one would overwrite the
	// reason an operator or an earlier halt wrote, which is the only
	// record of why a release stopped.
	if st.Halted || st.RolledBack || st.Done {
		return dec.withAction(ActionIdle)
	}
	if len(st.Pending) > 0 {
		return d.waiting(plan, dec, st)
	}
	if len(st.Failed) > 0 {
		dec.Failed = st.Failed
		dec.Reason = failureReason(st)
		plan.Halt(dec.Reason)
		d.forget(st.Plugin)
		return dec.withAction(ActionHalted)
	}

	// Every node in the wave confirmed. This is the one case the driver
	// acts on, and the gate is Rollout's, not this file's: Advance still
	// refuses on a halt and still refuses a wave that is not accounted
	// for, so a release the operator halted a moment ago cannot be
	// stepped over here.
	moved, err := plan.Advance(ctx)
	if err != nil {
		// A halt arrived between the status snapshot and here. Report it
		// as idle: the release is stopped, and that is not a failure of
		// the driver.
		if plan.Halted() {
			return dec.withAction(ActionIdle)
		}
		dec.Reason = err.Error()
		return dec.withAction(ActionIdle)
	}
	d.forget(st.Plugin)
	if !moved {
		return dec.withAction(ActionIdle)
	}
	return dec.withAction(ActionAdvanced)
}

// waiting handles a wave with nodes that have not answered, including the
// case where they never will.
func (d *Driver) waiting(plan *Rollout, dec Decision, st Status) Decision {
	dec.Pending = st.Pending
	// Negative is the only way to switch the check off; see
	// Options.StallAfter for why zero is not that.
	if d.stallAfter < 0 {
		return dec.withAction(ActionWaited)
	}

	now := d.now()
	same, since := d.observe(st.Plugin, st.Pending, now)
	if !same {
		return dec.withAction(ActionWaited)
	}
	if now.Sub(since) < d.stallAfter {
		return dec.withAction(ActionWaited)
	}

	// The pending set has not moved for the whole budget. Halt, and say
	// which nodes: an operator's next question is always "which ones",
	// and a reason string without ids makes them go and diff two status
	// snapshots to answer it.
	dec.Reason = "no answer from " + describe(st.Pending) + " for " +
		d.stallAfter.String() + "; wave " + itoa(st.Wave+1) + " is stuck"
	plan.Halt(dec.Reason)
	d.forget(st.Plugin)
	return dec.withAction(ActionStalled)
}

// observe records the pending set and reports whether it is the same one
// as last time, with the moment it was first seen.
func (d *Driver) observe(name string, pending []uint64, now time.Time) (same bool, since time.Time) {
	key := append([]uint64(nil), pending...)
	sort.Slice(key, func(i, j int) bool { return key[i] < key[j] })

	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.stuck[name]
	if ok && equalIDs(rec.pending, key) {
		return true, rec.since
	}
	d.stuck[name] = stallRecord{pending: key, since: now}
	return false, now
}

func (d *Driver) forget(name string) {
	d.mu.Lock()
	delete(d.stuck, name)
	d.mu.Unlock()
}

func equalIDs(a, b []uint64) bool {
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

func describe(ids []uint64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, itoa(int(id)))
	}
	return strings.Join(parts, ", ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// failureReason is the sentence a failure-driven halt carries.
//
// It names the count and the wave, and it deliberately does not list the
// node ids: Rollout keeps the node's own reason per node in Failed(), and
// a summary that repeats ids without reasons reads like it has more
// information than it does.
func failureReason(st Status) string {
	return "wave " + itoa(st.Wave+1) + " reported " + itoa(len(st.Failed)) +
		" node(s) that did not take the package; the release was stopped rather than advanced"
}

func (d Decision) withAction(a Action) Decision {
	d.Action = a
	return d
}

// Run ticks until ctx is done.
//
// It returns ctx.Err() so a caller wiring it into a process's shutdown
// path can tell "stopped because we asked" from "stopped because it
// broke". A driver that returns nil on cancellation cannot be told apart
// from one that finished its work, and the work is never finished.
func (d *Driver) Run(ctx context.Context) error {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			for _, dec := range d.Step(ctx) {
				if dec.Action == ActionIdle || dec.Action == ActionWaited {
					continue
				}
				d.log.Info("plugin release driver acted on a release",
					slog.String("plugin", dec.Plugin),
					slog.String("action", string(dec.Action)),
					slog.Int("wave", dec.Wave+1),
					slog.Any("pending", dec.Pending),
					slog.Any("failed", dec.Failed),
					slog.String("reason", dec.Reason))
			}
		}
	}
}
