package auditlog

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/edge/spool"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The pump, typed.
//
// The work is the shared one in core/edge/spool, for the reason it is shared
// there: "does the link answer, may we drain now, is a partial delivery a
// delivery" have one set of answers on this node, and there are two spools
// that need them. What this file adds is the type in the signature and
// nothing else — and the type is the whole point, because a Pump whose
// Send takes []spool.Row would let a caller hand it autonomy rows by
// accident, and the two would be indistinguishable on the wire.

// Sender delivers ledger rows to the control plane.
//
// It is an interface for the same reason it is one in autonomy: the tunnel
// is not this package's business. The pump's job is "when the link is back,
// hand the rows over, slowly", and the link's job is how to get them there.
// A node that has been offline for a week has an interesting story to tell,
// and a node that tells it in one request is a node the center cannot tell
// apart from an attack.
type Sender interface {
	// Send delivers rows in order. A partial failure is a failure: the
	// chain is ordered, and a hole in it is worse than a delay.
	Send(ctx context.Context, rows []ports.AuditEntry) error
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, rows []ports.AuditEntry) error

// Send implements Sender.
func (f SenderFunc) Send(ctx context.Context, rows []ports.AuditEntry) error { return f(ctx, rows) }

// ConfigError is a wiring mistake, said in the words of the field that is
// wrong.
//
// A typed error rather than a formatted one because a pump that is missing
// its sink and a pump that is missing its link want different fixes, and
// "auditlog: pump: is required" sends an operator to neither.
type ConfigError struct{ Field, Reason string }

func (e *ConfigError) Error() string { return "auditlog: " + e.Field + " " + e.Reason }

// PumpOptions configures a Pump.
type PumpOptions struct {
	// Sink is the node's ledger to drain. Required.
	Sink *Sink
	// Sender is where the rows go. Required.
	Sender Sender
	// Reachable reports whether there is anybody to send to. Required: a
	// pump that drains into a closed tunnel is a pump that loses rows.
	//
	// A function rather than autonomy's richer Link, because the only
	// question this pump asks is whether the tunnel is up, and autonomy's
	// Reach also carries how long it has been down — a threshold its own
	// backoff consults. Duplicating that struct here would be a second
	// place to forget to fill it in.
	Reachable func() bool
	// Batch bounds one drain. Default DefaultReplayBatch.
	Batch int
	// Interval bounds how often a drain may happen. Default
	// DefaultReplayInterval.
	Interval time.Duration
	// Now defaults to time.Now. Tests inject a clock.
	Now func() time.Time
	// Log is optional.
	Log *slog.Logger
}

// Pump hands the node's ledger rows to the control plane when the link is
// back.
type Pump struct{ inner *spool.Pump }

// NewPump returns a Pump.
func NewPump(opts PumpOptions) (*Pump, error) {
	switch {
	case opts.Sink == nil:
		return nil, &ConfigError{Field: "Sink", Reason: "is required: a pump with nothing to drain is a pump that hides its own misconfiguration"}
	case opts.Sender == nil:
		return nil, &ConfigError{Field: "Sender", Reason: "is required: the rows have to go somewhere, and silently dropping them is not somewhere"}
	case opts.Reachable == nil:
		return nil, &ConfigError{Field: "Reachable", Reason: "is required: draining into a closed tunnel loses rows"}
	}
	inner, err := spool.NewPump(spool.PumpOptions{
		Spool:     opts.Sink.Spool,
		Batch:     opts.Batch,
		Interval:  opts.Interval,
		Now:       opts.Now,
		Log:       opts.Log,
		Reachable: opts.Reachable,
		// All-or-nothing, and deliberately so. A chain is an ordered
		// append-only ledger with no dedupe key, so a sender that made
		// partial progress would let the next retry write the same rows a
		// second time. A duplicated tool call in the chain is worse than a
		// tool call that arrives late, because a reader cannot tell which
		// of the two actually happened.
		Send: func(ctx context.Context, raw []spool.Row) (int, error) {
			rows, err := decode(raw)
			if err != nil {
				return 0, err
			}
			if err := opts.Sender.Send(ctx, rows); err != nil {
				return 0, err
			}
			return len(rows), nil
		},
	})
	if err != nil {
		return nil, &ConfigError{Field: "Pump", Reason: err.Error()}
	}
	return &Pump{inner: inner}, nil
}

// DrainOnce sends at most one batch, and only when the link is up and the
// interval has elapsed.
//
// It returns how many rows the center now has. A drain that is skipped —
// link down, interval not elapsed, nothing spooled — returns zero and is not
// an error, because the common case is a node that is simply not supposed
// to be talking to anyone.
func (p *Pump) DrainOnce(ctx context.Context) (int, error) {
	if p == nil || p.inner == nil {
		return 0, errors.New("auditlog: drain on an unbuilt pump")
	}
	return p.inner.DrainOnce(ctx)
}

// Run drains on a ticker until the context ends.
//
// The first drain is immediate rather than after one interval: a node that
// has just come back from an outage has rows an operator is waiting to read,
// and making them wait a full interval for no reason is the difference
// between a story and a mystery.
func (p *Pump) Run(ctx context.Context) error {
	if p == nil || p.inner == nil {
		return errors.New("auditlog: run an unbuilt pump")
	}
	return p.inner.Run(ctx)
}

// Pending reports how many rows are waiting to go.
func (p *Pump) Pending() (int, error) {
	if p == nil || p.inner == nil {
		return 0, errors.New("auditlog: pending on an unbuilt pump")
	}
	return p.inner.Pending()
}
