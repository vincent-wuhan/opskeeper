package tunnel

import (
	"context"
	"math/rand/v2"
	"time"
)

// A fleet of nodes does not lose its tunnel because the broker restarted. It
// loses it because the broker restarted, and every node noticed at the same
// instant.
//
// This file is the answer to that. A plain exponential backoff — 1s, 2s, 4s,
// ... capped at 60s — is the textbook shape and it is exactly wrong for a
// fleet: every node computes the same wait from the same failure, so a
// control plane that comes back after a blip receives every edge's reconnect
// in the same millisecond, and if it is still not ready they all do it again
// 60 seconds later, in lockstep, forever. The cap that exists to bound the
// retry rate is what makes it permanent: below the cap the herd spreads out
// on its own as the intervals diverge, and at the cap they are all equal
// again.
//
// So the wait is drawn rather than computed. What follows keeps the
// exponential ceiling — it is the bound on how long a single node may stay
// away — and randomises everything under it.

const (
	// dialBackoffBase is the first wait, before any failure has told us
	// anything about the broker.
	dialBackoffBase = time.Second

	// dialBackoffMax is the ceiling the ceiling itself converges to. It
	// bounds the worst case for one node; it says nothing about how the
	// nodes are distributed within it, which is the jitter's job.
	dialBackoffMax = 60 * time.Second
)

// backoffJitter maps a ceiling onto the wait actually taken.
//
// It is a named type rather than a plain func so the dial loop can be handed
// a schedule that does not consult a random source at all, which is the
// only way to assert on a sequence of waits without asserting on a seed.
type backoffJitter func(ceiling time.Duration) time.Duration

// fullJitter draws uniformly from [0, ceiling).
//
// Full jitter rather than half jitter because the problem being solved is
// collision, not latency. Equal jitter — ceiling/2 plus a random half —
// halves the spread and leaves a floor that every node clears in lockstep at
// the low end; full jitter lets two nodes that failed at the same instant
// come back at genuinely unrelated times, which is the entire point. The
// zero draw is not a busy loop: the draw only skips the sleep, and the dial
// attempt it precedes is a network operation that takes real time.
func fullJitter(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling)))
}

// dialBackoff is the wait schedule for one client's first dial.
//
// It is a type rather than three locals inside Dial for a reason that is not
// tidiness: the schedule is the thing worth testing, and the only way to
// test a sequence of waits is to be able to produce one without a socket.
type dialBackoff struct {
	jitter backoffJitter
	// ceiling is what the next wait is drawn from, and it is the value that
	// doubles. It is deliberately *not* the previous wait: jittering the
	// ceiling instead of the wait is what keeps the cap meaningful instead
	// of letting a long streak of small draws collapse the schedule back
	// to 1s and retry the broker forever at a rate the cap was written to
	// prevent.
	ceiling time.Duration
}

// newDialBackoff starts a schedule. A nil jitter means full jitter, because
// the zero value of this struct is never what a caller wants to say.
func newDialBackoff(jitter backoffJitter) *dialBackoff {
	if jitter == nil {
		jitter = fullJitter
	}
	return &dialBackoff{jitter: jitter, ceiling: dialBackoffBase}
}

// next returns the wait before the following attempt and advances the
// schedule.
func (b *dialBackoff) next() time.Duration {
	ceiling := b.ceiling
	if ceiling > dialBackoffMax {
		ceiling = dialBackoffMax
	}
	wait := b.jitter(ceiling)
	if b.ceiling < dialBackoffMax {
		b.ceiling *= 2
		if b.ceiling > dialBackoffMax {
			b.ceiling = dialBackoffMax
		}
	}
	return wait
}

// sleepWithContext is the real wait between dial attempts.
//
// The zero-wait case is not special-cased for jitter's benefit: a draw of
// zero is a legal schedule value, and allocating a timer that fires
// immediately is a small enough waste that a branch to avoid it would cost
// more in the reader than it saves in the allocator. The context arm is not
// optional, though — a client cancelled mid-backoff has to return, and a
// plain time.After would keep the goroutine asleep for up to a minute after
// the operator asked it to stop.
func sleepWithContext(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
