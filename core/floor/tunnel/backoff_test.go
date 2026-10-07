package tunnel

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// identity is the jitter that makes a schedule observable: it returns the
// ceiling, so the test sees the raw exponential curve with the jitter
// factored out. Anything else would assert on a random number.
func identity(ceiling time.Duration) time.Duration { return ceiling }

// recording captures the ceilings a schedule asked for, which is the only
// way to see the curve without also seeing a draw.
type recording struct{ ceilings []time.Duration }

func (r *recording) jitter(ceiling time.Duration) time.Duration {
	r.ceilings = append(r.ceilings, ceiling)
	return ceiling
}

func TestTheCeilingDoublesAndStopsAtTheCap(t *testing.T) {
	rec := &recording{}
	b := newDialBackoff(rec.jitter)

	// Twelve attempts is enough to reach the cap and then sit on it for a
	// while; a schedule that kept doubling would need eleven to pass 60s,
	// and one that stopped short of it would never arrive here.
	for i := 0; i < 12; i++ {
		b.next()
	}

	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second,
		60 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second,
	}
	if len(rec.ceilings) != len(want) {
		t.Fatalf("recorded %d ceilings, want %d", len(rec.ceilings), len(want))
	}
	for i, got := range rec.ceilings {
		if got != want[i] {
			t.Fatalf("ceiling %d = %v, want %v; full schedule %v", i, got, want[i], rec.ceilings)
		}
	}
}

func TestAnUnjitteredScheduleIsTheTextbookCurve(t *testing.T) {
	// The exponential behaviour has to survive the extraction. Pinning it
	// with identity jitter keeps this test from being about randomness,
	// and it is the assertion that a future "simplify" of next() breaks
	// first.
	b := newDialBackoff(identity)
	if got := b.next(); got != time.Second {
		t.Errorf("first wait = %v, want 1s", got)
	}
	if got := b.next(); got != 2*time.Second {
		t.Errorf("second wait = %v, want 2s", got)
	}
}

// The regression that matters. A dial schedule that returns its ceiling is a
// correct-looking exponential backoff and a fleet-wide thundering herd: every
// edge computes the same wait from the same failure, so the broker takes
// every reconnect at once, and at the 60s cap it keeps taking them at once
// forever.
//
// The assertion goes through next() rather than calling fullJitter directly,
// and that is the whole difficulty. A test of the jitter function on its own
// passes whether or not the schedule uses it — it tests a helper, not the
// behaviour — so it stayed green through the exact mutation it exists to
// catch. Reaching the ceiling is possible, so this draws repeatedly; a draw
// landing exactly on a nanosecond-resolution ceiling is not a thing that
// happens, and replacing the jittered draw with the bare ceiling fails on the
// first iteration.
func TestTheWaitIsDrawnFromTheCeilingRatherThanEqualToIt(t *testing.T) {
	b := newDialBackoff(nil)
	for i := 0; i < 32; i++ {
		ceiling := b.ceiling
		got := b.next()
		if got < 0 {
			t.Fatalf("draw %d returned %v, want a non-negative wait", i, got)
		}
		if got >= ceiling {
			t.Fatalf("draw %d returned %v, which is its own ceiling of %v: every node in "+
				"the fleet retries in lockstep", i, got, ceiling)
		}
	}
}

func TestFullJitterOnADegenerateCeilingIsZeroNotAPanic(t *testing.T) {
	for _, ceiling := range []time.Duration{0, -time.Second} {
		if got := fullJitter(ceiling); got != 0 {
			t.Errorf("fullJitter(%v) = %v, want 0", ceiling, got)
		}
	}
}

// A schedule handed a nil jitter gets full jitter, because the alternative is
// a struct whose zero value silently means "never back off at all". Proved
// through next() for the same reason as the test above: a non-nil field is
// not the same claim as a jittered wait.
func TestANilJitterMeansFullJitter(t *testing.T) {
	b := newDialBackoff(nil)
	if b.jitter == nil {
		t.Fatal("newDialBackoff(nil) left the jitter unset")
	}
	if got := b.next(); got >= dialBackoffBase {
		t.Errorf("the default schedule's first wait was %v, which is the %v ceiling itself; "+
			"a zero jitter is not a backoff", got, dialBackoffBase)
	}
}

// The ceiling has to keep meaning something after a long run of small draws.
// Drawing from the *previous wait* rather than from the ceiling looks like an
// equivalent implementation and is not: a streak of short draws walks the
// schedule back down to 1s and the node retries the broker at 1s forever,
// which is the rate the cap exists to prevent.
func TestTheCeilingDoesNotShrinkAfterASmallDraw(t *testing.T) {
	// A jitter that always returns almost nothing: the adversarial case.
	b := newDialBackoff(func(time.Duration) time.Duration { return 0 })
	for i := 0; i < 40; i++ {
		b.next()
	}
	if b.ceiling != dialBackoffMax {
		t.Errorf("after 40 zero draws the ceiling is %v, want %v: a schedule whose ceiling "+
			"can shrink has no floor under its retry rate", b.ceiling, dialBackoffMax)
	}
}

// --- the wiring, not just the schedule ---------------------------------
//
// Everything above tests dialBackoff as a value. These two drive Dial and
// read the waits back out, because "the schedule is jittered" and "Dial uses
// the schedule" are different claims and only one of them is about this
// file. A loop that kept its own time.After would satisfy every test above.
//
// The broker is a port nothing listens on, so each attempt is refused in
// about a millisecond and the whole loop runs in milliseconds. The wait is
// not really taken — sleep records it and returns — which is the only reason
// this can assert on a 16-second ceiling inside a unit test.

const refusedAddr = "127.0.0.1:1"

// dialWaits runs Dial against a dead broker and returns the waits it asked
// for, stopping after n of them.
func dialWaits(t *testing.T, jitter backoffJitter, n int) []time.Duration {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waits := make([]time.Duration, 0, n)
	c := &geminioClient{
		cfg:      ClientConfig{ServerAddr: refusedAddr, AccessKey: "ak", SecretKey: "sk"},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		jitter:   jitter,
		handlers: map[string]Handler{},
		sleep: func(_ context.Context, wait time.Duration) error {
			waits = append(waits, wait)
			if len(waits) >= n {
				// Cancelling rather than returning an error keeps Dial's own
				// exit path under test: it has to notice the cancellation at
				// the top of the loop, not be handed an error from below.
				cancel()
			}
			return nil
		},
	}

	if err := c.Dial(ctx); err == nil {
		t.Fatal("Dial returned nil against a dead broker")
	}
	return waits
}

func TestDialTakesAJitteredWaitOnEveryRetry(t *testing.T) {
	ceilings := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
	}
	waits := dialWaits(t, fullJitter, len(ceilings))
	if len(waits) != len(ceilings) {
		t.Fatalf("recorded %d waits, want %d: %v", len(waits), len(ceilings), waits)
	}
	for i, got := range waits {
		if got < 0 || got >= ceilings[i] {
			t.Fatalf("retry %d waited %v against a %v ceiling; a wait at or above its "+
				"ceiling is the lockstep schedule this exists to prevent", i, got, ceilings[i])
		}
	}
	// Two nodes that failed at the same instant must not pick the same
	// wait. Comparing the recorded waits against a second client's is not
	// possible here, so the weaker but sufficient form: a schedule that
	// returned its ceiling would put every wait exactly on ceilings[i],
	// which the loop above already rejects.
}

// The other half of the same claim, and the one that would catch a ceiling
// that stopped doubling: with the jitter pinned to identity, Dial has to
// produce the textbook curve exactly.
func TestDialFollowsTheCeilingCurveWhenTheJitterIsPinned(t *testing.T) {
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, dialBackoffMax, dialBackoffMax,
	}
	waits := dialWaits(t, identity, len(want))
	if len(waits) != len(want) {
		t.Fatalf("recorded %d waits, want %d: %v", len(waits), len(want), waits)
	}
	for i, got := range waits {
		if got != want[i] {
			t.Fatalf("retry %d waited %v, want %v; full schedule %v", i, got, want[i], waits)
		}
	}
}
