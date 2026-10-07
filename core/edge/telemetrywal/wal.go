// Package telemetrywal is the node's write-ahead log for telemetry.
//
// The plan's line is "遥测与变更事件先落盘再上报", and the half that matters
// is the "先". Today the node samples on a timer and pushes straight at the
// tunnel, and a push that fails is a sample that does not exist: the loop
// logs it and the next tick produces a fresh one. For a dashboard that is
// invisible, because dashboards are mostly flat lines and a missing point
// is indistinguishable from a quiet host. For an investigation it is not —
// the question an operator asks at 09:00 is "what did this node look like
// at 03:00", and today the honest answer is a shrug, because the machine
// that knew is the machine that could not reach anybody.
//
// The WAL changes the order and nothing else about the wire. A sample is
// written to disk first, the drain sends it, and the row is acked. Three
// properties make that safe rather than merely different:
//
//   - **The drain is the only sender.** There is no "fast path" that
//     pushes the fresh batch directly and a replay that pushes the
//     backlog, because two senders over one queue is how a row gets
//     delivered twice or acked before it was sent. A node that is
//     healthy pays for this as up to one drain interval of latency, which
//     is a trade worth naming: it is bounded, it is configurable, and
//     the alternative is a queue that can lose its own tail.
//   - **Delivery is at least once, and says so.** Every row carries a
//     sequence number that survives a restart, and the center is expected
//     to dedupe on it. A WAL without that is a WAL that trades silent
//     loss for silent duplication, which is a worse trade during an
//     incident.
//   - **Rows expire, classes do not.** A metric pushed an hour late is
//     not a delayed metric, it is a wrong one: the store either rejects
//     it as out of order or accepts it and draws a spike in the wrong
//     place. So the metric classes carry a horizon and an audit-shaped
//     class does not. This is the one place where "keep everything" is
//     the wrong answer, and it is the reason the spool has a policy table
//     instead of a single cap.
package telemetrywal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/edge/spool"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The classes, and what each one is worth.
const (
	// ClassHostPoint is the legacy fast-path point that backs the
	// dashboard cards and the alert rules.
	ClassHostPoint = spool.Class("host_point")
	// ClassSamples is the open-set Prometheus push.
	ClassSamples = spool.Class("samples")
	// ClassTrace is high-volume, low-density telemetry.
	//
	// No producer on this node writes it yet — the collector produces
	// points and samples, not spans. The class exists because the plan's
	// drop order names it, and a drop order that is only expressed in a
	// comment is not a drop order: declaring it here means the ordering
	// is a table that can be tested, and the day a tracer lands it
	// inherits a policy instead of a debate.
	ClassTrace = spool.Class("trace")
	// ClassEvent is a change-watcher event: a service restarted, a
	// package was installed, a log line matched a filter.
	//
	// Nothing in this package writes it — the change watcher keeps its own
	// log, because its rows are events and its sender is a different call.
	// The class lives here so that both logs are graded by one table
	// rather than by two.
	ClassEvent = spool.Class("event")
)

// Horizons. These are policy numbers, not measurements, and the reasoning
// is the same for all of them: how late is too late for the center to
// still use the row for something.
const (
	// DefaultMetricHorizon is how long a metric row stays worth
	// sending.
	//
	// It is bounded by what the receiving store tolerates rather than by
	// how interesting the data is. Prometheus-style remote write rejects
	// samples older than its out-of-order window, so a row that arrives
	// later than this is not a delayed row, it is a rejected one — and a
	// row that costs a full replay round trip to be refused is a row that
	// was displacing rows that would have been accepted.
	DefaultMetricHorizon = 30 * time.Minute
	// DefaultTraceHorizon is the same for spans, and shorter, because the
	// density argument cuts the other way: a trace row is worth far less
	// per byte than a metric row and there are always more of them.
	DefaultTraceHorizon = 10 * time.Minute
	// DefaultWALBytes caps the file.
	//
	// It is sized for a node that is off the network for a day at its
	// own sampling rate with room to spare, which is deliberate: a cap
	// that is routinely hit is a cap that has already started deciding
	// what the fleet gets to see, and a decision made by a byte counter
	// on a disconnected node is one nobody reviewed.
	DefaultWALBytes = 16 << 20
	// DefaultBatch bounds one drain.
	DefaultBatch = 100
	// DefaultInterval bounds how often a drain may happen.
	DefaultInterval = 5 * time.Second
)

// DefaultClasses is the node's telemetry drop policy, as one table.
//
// It is exported because it is more than this package's business: the
// change watcher keeps its own log — its rows are events and its sender is
// a different call — and a node that graded those two logs by two different
// tables would be making the "what do we lose first" decision twice, in two
// places, with no way to see both answers at once. Two files, one policy.
//
// The table is the plan's 分级丢弃 as numbers: traces have the highest
// drop priority because they are the largest and the least dense, metrics
// are next, and a change event is never dropped while anything else is
// still on disk.
func DefaultClasses() map[spool.Class]spool.Policy {
	return map[spool.Class]spool.Policy{
		ClassHostPoint: {DropPriority: spool.PriorityValuable, MaxAge: DefaultMetricHorizon},
		ClassSamples:   {DropPriority: spool.PriorityValuable, MaxAge: DefaultMetricHorizon},
		ClassTrace:     {DropPriority: spool.PriorityBulk, MaxAge: DefaultTraceHorizon},
		// No MaxAge, and that is the whole point of the class. The
		// question "is it too late to record that the service restarted
		// at 03:12" has no useful answer, and a horizon here would delete
		// the record of an outage because the outage outlasted somebody's
		// retention policy.
		ClassEvent: {DropPriority: spool.PriorityCritical},
	}
}

// Batch is one collection result, as it will be handed to the tunnel on
// the way out.
//
// It mirrors what the two push methods already take rather than inventing a
// third shape, because the replay has to re-issue the *same* calls the live
// path issues. A WAL that introduced its own envelope on the way back
// would need a new wire method, and a new wire method is a thing the
// manager has to be able to roll back.
type Batch struct {
	// Source is the collector's label for this series.
	Source string `json:"source"`
	// HostPoint is set when this batch carried the legacy fast path.
	HostPoint *tunnel.HostMetricPoint `json:"host_point,omitempty"`
	// Samples is the open-set push, if any.
	Samples []tunnel.PromSample `json:"samples,omitempty"`
}

// Sender is the node's side of a drain: it takes the batches the log is
// holding and hands them to the center.
//
// It reports how many of them the center now has for good, and that count
// is the difference between this log and the audit log next door. An audit
// chain would rather lose the whole batch than have a hole in it. Telemetry
// cannot: a sample the center has already refused will be refused again, so
// a sender that refuses to make progress until it can ack everything wedges
// the queue on one permanently-bad row and the node stops reporting for
// good. Progress, not perfection, is what a log full of samples wants.
type Sender func(ctx context.Context, batches []Batch) (delivered int, err error)

// Options configures a WAL.
type Options struct {
	// Dir is where the file lives. Required.
	Dir string
	// MaxBytes caps the file. Default DefaultWALBytes.
	MaxBytes int64
	// Batch bounds one drain. Default DefaultBatch.
	Batch int
	// Interval bounds how often a drain may happen. Default
	// DefaultInterval.
	Interval time.Duration
	// Now defaults to time.Now. Tests inject a clock.
	Now func() time.Time
	// Log is optional.
	Log *slog.Logger
}

// WAL is the node's telemetry write-ahead log and the drain that empties
// it.
type WAL struct {
	spool   *spool.Spool
	pump    *spool.Pump
	log     *slog.Logger
	now     func() time.Time
	batch   int
	every   time.Duration
	nudged  chan struct{}
	pending func() (int, error)
}

// FileName is the WAL's file inside its directory.
const FileName = "telemetry-wal.jsonl"

// Open creates the WAL in dir.
//
// A node whose directory is not writable gets a WAL that refuses to open
// rather than a node that silently keeps losing telemetry, because the
// second failure looks exactly like the first: no rows, no errors, and an
// operator who concludes the host is quiet.
func Open(opts Options) (*WAL, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("telemetrywal: the write-ahead log needs a directory to live in")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	w := &WAL{
		log:    log,
		now:    opts.Now,
		batch:  opts.Batch,
		every:  opts.Interval,
		nudged: make(chan struct{}, 1),
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.batch <= 0 {
		w.batch = DefaultBatch
	}
	if w.every <= 0 {
		w.every = DefaultInterval
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("telemetrywal: create %s: %w", opts.Dir, err)
	}
	sp, err := spool.Open(spool.Options{
		Path:     filepath.Join(opts.Dir, FileName),
		MaxBytes: opts.MaxBytes,
		Label:    "the telemetry write-ahead log",
		Classes:  DefaultClasses(),
		// The same clock the drain uses. Handing the log time.Now while
		// the pump gets an injected one is how a test ends up proving
		// that rows written in the test's year do not age out, and how a
		// production node ends up with a sweep that never fires because
		// its two clocks disagree about which century it is in.
		Now: w.now,
	})
	if err != nil {
		return nil, fmt.Errorf("telemetrywal: %w", err)
	}
	w.spool = sp
	return w, nil
}

// Path is where the file lives, for a health page.
func (w *WAL) Path() string { return w.spool.Path() }

// Pending reports how many rows are waiting to go.
func (w *WAL) Pending() (int, error) { return w.spool.Len() }

// Stats is the spool's counters, for the same health page that shows the
// autonomy spool: a node quietly dropping telemetry looks exactly like a
// node with nothing to report.
func (w *WAL) Stats() spool.Stats { return w.spool.Snapshot() }

// Record writes one collection result to disk.
//
// It is called on the sampling tick, before anything is sent, and it does
// not care whether anything is listening. That is the whole point: a
// sampler that waits for permission is a sampler that drops samples, and
// the permission is exactly what is missing during the outage the WAL
// exists for.
//
// A write that fails does not stop the loop. The next tick samples again
// and the health counters say what happened, because a node that refuses
// to sample because its disk is full is a node that has turned a
// telemetry problem into an availability one.
func (w *WAL) Record(ctx context.Context, batch Batch) error {
	return w.spool.Record(ctx, classOf(batch), batch)
}

// classOf picks the bucket from what the batch actually carries.
//
// It is derived rather than passed in, because the caller is the sampling
// loop and a caller that chose the class would be a second place for the
// policy to be wrong. A batch with both halves is a point row: the point
// is the smaller of the two and the one the dashboards read, so the row
// that survives a compaction is the one carrying it.
func classOf(b Batch) spool.Class {
	if b.HostPoint != nil {
		return ClassHostPoint
	}
	return ClassSamples
}

// Drain sends up to one batch, and only when the link is up and the
// interval has elapsed.
func (w *WAL) Drain(ctx context.Context, reachable func() bool, send Sender) (int, error) {
	pump, err := w.ensurePump(reachable, send)
	if err != nil {
		return 0, err
	}
	return pump.DrainOnce(ctx)
}

// Nudge asks the drain loop to try sooner than its next tick.
//
// This is the healthy path's whole latency story: the sampling tick
// nudges, and the drain runs, so a node with an empty log sends its fresh
// batch on the next loop turn rather than waiting out the interval. The
// interval still bounds the *rate*; the nudge only removes the wait.
func (w *WAL) Nudge() {
	select {
	case w.nudged <- struct{}{}:
	default:
	}
}

func (w *WAL) ensurePump(reachable func() bool, send Sender) (*spool.Pump, error) {
	if w.pump != nil {
		return w.pump, nil
	}
	if reachable == nil {
		return nil, fmt.Errorf("telemetrywal: the drain needs to know whether the link is up")
	}
	if send == nil {
		return nil, fmt.Errorf("telemetrywal: the drain needs somewhere to send")
	}
	pump, err := spool.NewPump(spool.PumpOptions{
		Spool:     w.spool,
		Batch:     w.batch,
		Interval:  w.every,
		Now:       w.now,
		Log:       w.log,
		Reachable: reachable,
		Send: func(ctx context.Context, rows []spool.Row) (int, error) {
			batches, unreadable := decode(rows)
			if unreadable > 0 {
				// Leave every row — readable or not — for the next
				// drain. The honest answer to "how many did the center
				// get" is zero when part of the batch was never
				// offered, and the count-short-of-batch rule makes
				// that a failed drain rather than a partial ack.
				//
				// This is a wedge, and it is survivable rather than
				// permanent: the unreadable row sits at the head of
				// the log and the drain cannot get past it, but its
				// class horizon still applies — the sweep drops it
				// once the class ages out — so the node resumes on
				// its own. A long loud stall that clears itself beats
				// a short quiet one that loses the row.
				w.log.Warn("telemetry: the write-ahead log holds rows this build cannot read; leaving them for a reader that can",
					slog.Int("unreadable", unreadable),
					slog.Int("rows", len(rows)))
				return 0, fmt.Errorf("telemetrywal: %d of %d rows are unreadable by this build", unreadable, len(rows))
			}
			return send(ctx, batches)
		},
	})
	if err != nil {
		return nil, err
	}
	w.pump = pump
	return pump, nil
}

// Run drains until the context ends, waking on a nudge or on the interval.
//
// The nudge channel is a hint, not a queue: a full channel already means
// "there is a pending wake-up", so dropping a second one loses no work.
func (w *WAL) Run(ctx context.Context, reachable func() bool, send Sender) error {
	if _, err := w.ensurePump(reachable, send); err != nil {
		return err
	}
	ticker := time.NewTicker(w.every)
	defer ticker.Stop()
	for {
		if _, err := w.pump.DrainOnce(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("telemetry drain failed; the rows stay on disk",
				slog.Any("err", err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.nudged:
		case <-ticker.C:
		}
	}
}

// Close releases the file.
func (w *WAL) Close() error { return w.spool.Close() }

// decode turns spool rows back into batches, and says which rows it could
// not read.
//
// The second return value is the whole point. A row that does not parse is
// a row from a build that is not this one — a downgrade, or a format the
// caller changed. It must not be sent, because there is nothing to send,
// but it must also not be acked: an unreadable row is still a row, and
// discarding it turns "this node has telemetry I cannot read" into "this
// node sent me nothing", which is precisely the silent loss the WAL exists
// to prevent.
//
// The earlier shape of this function returned a sentinel `Batch{Source:
// "unreadable"}` and relied on the sender failing on it. That reliance was
// never real: the sender is `pushBatch`, and a batch with no HostPoint and
// no Samples is a no-op that returns nil, so the row was acked and lost.
// The comment said the rows stay on disk; the code acked them. Carrying the
// count here makes the two agree, and the pump's rule — a count short of
// len(rows) with a nil error is a sender bug — turns a short count into a
// failed drain that leaves every row in place.
func decode(rows []spool.Row) ([]Batch, int) {
	out := make([]Batch, 0, len(rows))
	unreadable := 0
	for _, r := range rows {
		var b Batch
		if err := json.Unmarshal(r.Payload, &b); err != nil {
			unreadable++
			continue
		}
		out = append(out, b)
	}
	return out, unreadable
}
