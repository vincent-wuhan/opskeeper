package spool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// The replay side: a pump that hands rows to the control plane once there
// is somebody there to hand them to, slowly.
//
// The rate limit is not politeness. A node that has been offline for a week
// has an interesting story to tell, and every node in the fleet comes back
// at the same moment after the same network event. Replaying in one burst
// lands all of it on the same endpoint simultaneously, and the audit chain
// that is supposed to record what the fleet did during the outage is
// exactly the thing that goes down under it. The limit is what turns a
// fleet-wide reconnect into a queue the center can absorb.
const (
	// DefaultBatch is how many rows go up per drain.
	DefaultBatch = 100
	// DefaultInterval is how often a drain may happen.
	DefaultInterval = 5 * time.Second
)

// PumpOptions configures a Pump.
type PumpOptions struct {
	// Spool is the local record to drain. Required.
	Spool *Spool
	// Send delivers rows in order, and reports how many of them the far
	// end now has for good. Required.
	//
	// The count is the whole design decision about acking, and it belongs
	// to the sender rather than to the pump because the two callers want
	// opposite things and neither is wrong:
	//
	//   - An ordered audit chain wants all-or-nothing. If the center got
	//     four of five decisions, a hole in the chain is worse than a
	//     late one, so the sender returns (0, err) and every row stays.
	//   - Telemetry cannot want that. A sample the center has already
	//     refused will be refused again, so a sender that refuses to make
	//     progress until it can ack everything wedges the queue on one
	//     permanently-bad row and the node stops reporting entirely. It
	//     returns what it got through and the pump acks that much.
	//
	// A count shorter than len(rows) with a nil error is a sender bug, and
	// the pump treats it as a failure rather than guessing.
	Send func(ctx context.Context, rows []Row) (int, error)
	// Reachable reports whether there is anybody to send to. Required: a
	// pump that replays into a closed tunnel is a pump that loses rows.
	Reachable func() bool
	// Batch bounds one drain. Default DefaultBatch.
	Batch int
	// Interval bounds how often a drain may happen. Default
	// DefaultInterval.
	Interval time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
	// Log is optional.
	Log *slog.Logger
}

// Pump replays a spool once the control plane is reachable again.
type Pump struct {
	spool   *Spool
	send    func(context.Context, []Row) (int, error)
	reach   func() bool
	batch   int
	every   time.Duration
	now     func() time.Time
	log     *slog.Logger
	lastRun time.Time
}

// NewPump returns a Pump, or a ConfigError naming what is missing.
func NewPump(opts PumpOptions) (*Pump, error) {
	switch {
	case opts.Spool == nil:
		return nil, fmt.Errorf("spool: a pump needs a spool: one with nothing to drain hides its own misconfiguration")
	case opts.Send == nil:
		return nil, fmt.Errorf("spool: a pump needs somewhere to send: silently dropping rows is not somewhere")
	case opts.Reachable == nil:
		return nil, fmt.Errorf("spool: a pump needs to know when the link is up: replaying into a closed tunnel loses rows")
	}
	p := &Pump{
		spool: opts.Spool,
		send:  opts.Send,
		reach: opts.Reachable,
		batch: opts.Batch,
		every: opts.Interval,
		now:   opts.Now,
		log:   opts.Log,
	}
	if p.batch <= 0 {
		p.batch = DefaultBatch
	}
	if p.every <= 0 {
		p.every = DefaultInterval
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p, nil
}

// DrainOnce sends at most one batch, and only when the link is up and the
// interval has elapsed.
//
// It returns how many rows the center now has. A drain that is skipped —
// link down, interval not elapsed, nothing spooled — returns zero and is
// not an error, because the common case is a node that is simply not
// supposed to be talking to anyone.
func (p *Pump) DrainOnce(ctx context.Context) (int, error) {
	if !p.reach() {
		return 0, nil
	}
	now := p.now()
	rows, err := p.spool.Peek(p.batch)
	if err != nil {
		return 0, err
	}
	// A drain that empties the log is not a burst, and the interval does
	// not apply to it.
	//
	// The interval exists for one reason: a node that has been offline
	// comes back holding a week, and a fleet that all reconnects at once
	// would hand the center all of it in the same second. A log with
	// fewer rows than one batch is not that — it is a node that sampled
	// once and is trying to deliver it — and making it wait out the
	// interval would add latency to every healthy node in the fleet in
	// order to protect the center from a problem only the backlogged ones
	// have.
	//
	// So the test is "did the batch come back full?", and it is asked
	// after the peek rather than before, because a full batch is the
	// evidence and a guess about the pending count is not.
	if len(rows) == p.batch {
		if !p.lastRun.IsZero() && now.Sub(p.lastRun) < p.every {
			return 0, nil
		}
	} else if len(rows) == 0 {
		// Still record the drain. Otherwise a node whose spool is empty
		// retries every tick forever, and the interval that was supposed
		// to protect the center from a reconnecting fleet is the one
		// thing the empty case skips.
		p.lastRun = now
		return 0, nil
	}
	delivered, err := p.send(ctx, rows)
	if err == nil && delivered != len(rows) {
		// Not a failure the pump can repair and not one it may paper
		// over: acking the difference would discard rows nobody sent, and
		// treating them as sent would hide a sender that lost track of
		// its own batch.
		err = fmt.Errorf("spool: the sender reported %d of %d rows delivered", delivered, len(rows))
	}
	// Whatever the far end has for good is acked, and only that. On the
	// all-or-nothing path the sender returns zero, so this is a no-op
	// there and the rows stay in the same order for the next attempt.
	if delivered > 0 {
		if ackErr := p.spool.Ack(delivered); ackErr != nil {
			return delivered, ackErr
		}
	}
	if err != nil {
		p.logf("replay failed", "rows", len(rows), "delivered", delivered, "error", err)
		return delivered, fmt.Errorf("spool: replay %d rows: %w", len(rows), err)
	}
	p.lastRun = now
	return delivered, nil
}

// Run drains on a ticker until the context ends.
//
// The first drain is immediate rather than after one interval: a node that
// has just come back from an outage has rows the operator is waiting to
// read, and making them wait five seconds for no reason is the difference
// between a story and a mystery.
func (p *Pump) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.every)
	defer ticker.Stop()
	for {
		if _, err := p.DrainOnce(ctx); err != nil && ctx.Err() == nil {
			// A failed drain is logged and retried; it is not fatal,
			// because the only thing that ends this loop is the node
			// stopping or the link going away again.
			if !errors.Is(err, context.Canceled) {
				p.logf("replay will be retried", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Pending reports how many rows are waiting to go.
func (p *Pump) Pending() (int, error) { return p.spool.Len() }

func (p *Pump) logf(msg string, args ...any) {
	if p.log != nil {
		p.log.Error(msg, args...)
	}
}
