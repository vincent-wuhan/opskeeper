package autonomy

import (
	"context"
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/edge/spool"
)

// Sender delivers spooled decisions to the control plane.
//
// It is an interface because the tunnel is not this package's business: the
// pump's job is "when the link is back, hand the rows over, slowly", and
// the link's job is how to get them there. A node that has been offline for
// a week has an interesting story to tell, and a node that tells it in one
// request is a node the center cannot tell apart from an attack.
//
// The rate limit is not politeness. Replaying a week of decisions in a
// single burst lands on the same endpoint as every other node's reconnect at
// the same moment, and the audit chain that is supposed to record what the
// fleet did during the outage is exactly the thing that goes down under it.
//
// The pump itself is the shared one, for the same reason the spool is: the
// questions — does the link answer, may we drain now, is a partial delivery
// a delivery — have one set of answers, and this node has three spools that
// all need them.
const (
	// DefaultReplayBatch is how many rows go up per drain.
	DefaultReplayBatch = spool.DefaultBatch
	// DefaultReplayInterval is how often a drain may happen.
	DefaultReplayInterval = spool.DefaultInterval
)

// Sender is the control-plane side of a replay.
type Sender interface {
	// Send delivers rows in order. A partial failure is a failure: the
	// chain is ordered, and a hole in it is worse than a delay.
	Send(ctx context.Context, rows []Row) error
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, rows []Row) error

// Send implements Sender.
func (f SenderFunc) Send(ctx context.Context, rows []Row) error { return f(ctx, rows) }

// PumpOptions configures a Pump.
type PumpOptions struct {
	// Spool is the local record to drain. Required.
	Spool *Spool
	// Sender is where the rows go. Required.
	Sender Sender
	// Link decides when a drain may happen. Required: a pump that
	// replays into a closed tunnel is a pump that loses rows.
	Link Link
	// Batch bounds one drain. Default DefaultReplayBatch.
	Batch int
	// Interval bounds how often a drain may happen. Default
	// DefaultReplayInterval.
	Interval time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
	// Log is optional.
	Log *slog.Logger
}

// Pump replays the audit spool once the control plane is reachable again.
type Pump struct {
	inner *spool.Pump
	send  Sender
	log   *slog.Logger
}

// NewPump returns a Pump.
func NewPump(opts PumpOptions) (*Pump, error) {
	switch {
	case opts.Spool == nil:
		return nil, &ConfigError{Field: "Spool", Reason: "is required: a pump with nothing to drain is a pump that hides its own misconfiguration"}
	case opts.Sender == nil:
		return nil, &ConfigError{Field: "Sender", Reason: "is required: the rows have to go somewhere, and silently dropping them is not somewhere"}
	case opts.Link == nil:
		return nil, &ConfigError{Field: "Link", Reason: "is required: replaying into a closed tunnel loses rows"}
	}
	p := &Pump{send: opts.Sender, log: opts.Log}
	inner, err := spool.NewPump(spool.PumpOptions{
		Spool:     opts.Spool.Spool,
		Batch:     opts.Batch,
		Interval:  opts.Interval,
		Now:       opts.Now,
		Log:       opts.Log,
		Reachable: func() bool { return opts.Link.Reach().Online },
		// All-or-nothing, and deliberately: an audit chain with a hole in
		// it is worse than one that arrives late, so this sender never
		// reports partial progress even when the tunnel told it which half
		// it took. There is no "which half" — the sender is a tunnel call
		// whose failure is a failure.
		Send: func(ctx context.Context, raw []spool.Row) (int, error) {
			rows, err := decodeRows(raw)
			if err != nil {
				return 0, err
			}
			if err := p.send.Send(ctx, rows); err != nil {
				return 0, err
			}
			return len(rows), nil
		},
	})
	if err != nil {
		return nil, &ConfigError{Field: "Pump", Reason: err.Error()}
	}
	p.inner = inner
	return p, nil
}

// DrainOnce sends at most one batch, and only when the link is up and the
// interval has elapsed.
//
// It returns how many rows the center now has. A drain that is skipped —
// link down, interval not elapsed, nothing spooled — returns zero and is
// not an error, because the common case is a node that is simply not
// supposed to be talking to anyone.
func (p *Pump) DrainOnce(ctx context.Context) (int, error) { return p.inner.DrainOnce(ctx) }

// Run drains on a ticker until the context ends.
//
// The first drain is immediate rather than after one interval: a node that
// has just come back from an outage has rows the operator is waiting to
// read, and making them wait five seconds for no reason is the difference
// between a story and a mystery.
func (p *Pump) Run(ctx context.Context) error { return p.inner.Run(ctx) }

// Pending reports how many rows are waiting to go.
func (p *Pump) Pending() (int, error) { return p.inner.Pending() }
