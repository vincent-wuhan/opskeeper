package telemetrywal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func newWAL(t *testing.T, opts Options) (*WAL, *clock) {
	t.Helper()
	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Now == nil {
		opts.Now = c.now
	}
	w, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w, c
}

func pointBatch(source string) Batch {
	return Batch{
		Source:    source,
		HostPoint: &tunnel.HostMetricPoint{CPUPct: 12.5, Ts: time.Now().Unix()},
		Samples:   []tunnel.PromSample{{Name: "node_cpu_pct", Value: 12.5}},
	}
}

func sampleBatch(source string, n int) Batch {
	b := Batch{Source: source}
	for i := 0; i < n; i++ {
		b.Samples = append(b.Samples, tunnel.PromSample{Name: "node_load", Value: float64(i), TsMs: time.Now().UnixMilli()})
	}
	return b
}

// TestSamplesWrittenWhileDisconnectedSurviveToBeSent is 断连写入 + 恢复回放
// at the level the plan means it, and it is the whole reason the package
// exists: the operator asks at 09:00 what the node looked like at 03:00,
// and the machine that knew is the machine that could not reach anybody.
func TestSamplesWrittenWhileDisconnectedSurviveToBeSent(t *testing.T) {
	w, _ := newWAL(t, Options{})
	ctx := context.Background()
	online := false
	var sent []Batch
	send := func(_ context.Context, batches []Batch) (int, error) {
		sent = append(sent, batches...)
		return len(batches), nil
	}
	reachable := func() bool { return online }

	for i := 0; i < 4; i++ {
		if err := w.Record(ctx, pointBatch(fmt.Sprintf("embedded-%d", i))); err != nil {
			t.Fatalf("Record: %v", err)
		}
		n, err := w.Drain(ctx, reachable, send)
		if err != nil || n != 0 {
			t.Fatalf("drain while disconnected = %d, %v; nothing should have been attempted", n, err)
		}
	}
	if len(sent) != 0 {
		t.Fatalf("%d batches went out while the link was down", len(sent))
	}
	if p, err := w.Pending(); err != nil || p != 4 {
		t.Fatalf("pending = %d, %v; want the 4 samples an outage should not have eaten", p, err)
	}

	online = true
	n, err := w.Drain(ctx, reachable, send)
	if err != nil || n != 4 {
		t.Fatalf("drain after recovery = %d, %v; want 4", n, err)
	}
	for i, b := range sent {
		if b.Source != fmt.Sprintf("embedded-%d", i) {
			t.Fatalf("batch %d came from %s: replay is out of order", i, b.Source)
		}
		if b.HostPoint == nil {
			t.Errorf("batch %d lost its host point: the drain re-issues a different call than the live path did", i)
		}
	}
	if p, _ := w.Pending(); p != 0 {
		t.Errorf("pending = %d after delivery, want 0", p)
	}
}

// TestATransportFailureKeepsEveryRow is the half of delivery that must not
// be traded away. A node that cannot reach the center has to keep what it
// sampled, because the next attempt is the only one that will work.
func TestATransportFailureKeepsEveryRow(t *testing.T) {
	w, _ := newWAL(t, Options{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := w.Record(ctx, sampleBatch("scrape:x", 1)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	boom := errors.New("tunnel refused the connection")
	if n, err := w.Drain(ctx, func() bool { return true }, func(context.Context, []Batch) (int, error) {
		return 0, boom
	}); err == nil || n != 0 {
		t.Fatalf("drain = %d, %v; want the sender's failure", n, err)
	}
	if p, _ := w.Pending(); p != 3 {
		t.Fatalf("pending = %d after a transport failure, want 3", p)
	}
}

// TestARefusedRowDoesNotWedgeTheQueue is the failure the sender-progress
// contract exists to prevent, and it is a failure of the *sender*, so this
// test models the sender rather than the pump.
//
// The node's drainBatches walks the batch it was handed: a transport error
// stops it and leaves everything on disk, and a rejection from the center
// does not — the center has already said it will not take those samples, so
// asking again at the drain interval is a node talking to itself, and a
// queue that stops on one permanently-refusable row is a node that has gone
// quiet for good, which is the exact outcome the log was installed to
// prevent.
func TestARefusedRowDoesNotWedgeTheQueue(t *testing.T) {
	w, _ := newWAL(t, Options{})
	ctx := context.Background()
	for _, src := range []string{"scrape:refused", "scrape:good", "scrape:also-good"} {
		if err := w.Record(ctx, sampleBatch(src, 1)); err != nil {
			t.Fatalf("Record %s: %v", src, err)
		}
	}
	refused := map[string]bool{"scrape:refused": true}
	var seen []string
	send := func(_ context.Context, batches []Batch) (int, error) {
		for _, b := range batches {
			seen = append(seen, b.Source)
			if refused[b.Source] {
				// The center said no. The agent counts it, says so loudly,
				// and moves on to the next batch rather than stalling.
				continue
			}
		}
		return len(batches), nil
	}
	if n, err := w.Drain(ctx, func() bool { return true }, send); err != nil || n != 3 {
		t.Fatalf("drain = %d, %v; want all 3 rows accounted for", n, err)
	}
	if len(seen) != 3 || seen[0] != "scrape:refused" || seen[2] != "scrape:also-good" {
		t.Fatalf("the drain saw %v; it must not skip past a refused row to reach a later one", seen)
	}
	if p, _ := w.Pending(); p != 0 {
		t.Errorf("pending = %d, want 0: a refused row is passed over, not retried forever", p)
	}
}

// TestATransportFailureStopsTheDrainWhereItIs is the other half of the
// sender's contract, and it is the half that keeps a network from turning
// into silent loss: everything from the failure onwards stays on disk, in
// order, for the next attempt.
func TestATransportFailureStopsTheDrainWhereItIs(t *testing.T) {
	w, _ := newWAL(t, Options{})
	ctx := context.Background()
	for _, src := range []string{"first", "second", "third"} {
		if err := w.Record(ctx, sampleBatch(src, 1)); err != nil {
			t.Fatalf("Record %s: %v", src, err)
		}
	}
	boom := errors.New("tunnel closed the stream")
	var seen []string
	// This is the shape of the agent's own drainBatches: walk the batch,
	// stop where the transport failed, and report how far it got.
	delivered, err := w.Drain(ctx, func() bool { return true }, func(_ context.Context, batches []Batch) (int, error) {
		for i, b := range batches {
			seen = append(seen, b.Source)
			if b.Source == "second" {
				return i, boom
			}
		}
		return len(batches), nil
	})
	if err == nil {
		t.Error("a transport failure was reported as success")
	}
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1: only the batch before the failure landed", delivered)
	}
	if len(seen) != 2 {
		t.Fatalf("the drain tried %v; it must stop at the failure rather than push the rest", seen)
	}
	rows, _ := w.Pending()
	if rows != 2 {
		t.Errorf("pending = %d, want the 2 that never landed", rows)
	}
}

// TestTheDrainIsRateLimited is 回放限流. A node that reconnects after a
// night must not hand the center the whole night in one request, and a
// fleet that all reconnects at once must not do it in lockstep either.
func TestTheDrainIsRateLimited(t *testing.T) {
	w, c := newWAL(t, Options{Batch: 10})
	ctx := context.Background()
	for i := 0; i < 35; i++ {
		if err := w.Record(ctx, sampleBatch("scrape:x", 1)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	calls := 0
	send := func(_ context.Context, batches []Batch) (int, error) {
		calls++
		return len(batches), nil
	}
	reachable := func() bool { return true }
	if n, err := w.Drain(ctx, reachable, send); err != nil || n != 10 {
		t.Fatalf("first drain = %d, %v; want a full batch of 10", n, err)
	}
	if n, err := w.Drain(ctx, reachable, send); err != nil || n != 0 {
		t.Fatalf("second drain = %d, %v; want 0 before the interval elapses", n, err)
	}
	if calls != 1 {
		t.Errorf("sent %d times, want 1", calls)
	}
	c.advance(DefaultInterval)
	if n, err := w.Drain(ctx, reachable, send); err != nil || n != 10 {
		t.Fatalf("drain after the interval = %d, %v; want 10", n, err)
	}
	if p, _ := w.Pending(); p != 15 {
		t.Errorf("pending = %d, want 15", p)
	}
}

// TestAMetricPastItsHorizonIsDropped is the staleness half of the graded
// drop. A metric half an hour late is not a delayed metric, it is a wrong
// one: the store either refuses it as out of order or accepts it and draws
// a spike in the wrong place. Dropping it locally is not a shortcut, it is
// the same refusal the center would have made, made here and for free.
func TestAMetricPastItsHorizonIsDropped(t *testing.T) {
	w, c := newWAL(t, Options{})
	ctx := context.Background()
	if err := w.Record(ctx, pointBatch("embedded")); err != nil {
		t.Fatalf("Record stale: %v", err)
	}
	c.advance(DefaultMetricHorizon + time.Minute)
	// One fresh write is what makes the sweep look at the file, and the
	// sweep is what removes the row that aged out.
	if err := w.Record(ctx, pointBatch("embedded")); err != nil {
		t.Fatalf("Record fresh: %v", err)
	}
	rows, err := w.spool.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("kept %d rows, want the fresh one", len(rows))
	}
	if st := w.Stats(); st.DroppedAged != 1 {
		t.Errorf("DroppedAged = %d, want 1: a health page has to be able to tell staleness from volume", st.DroppedAged)
	}
}

// TestTheDropOrderIsTheOneThePlanNames is a statement about the table
// rather than about a file, and it is the statement that makes the plan's
// 分级丢弃 checkable: traces first because they are the largest and least
// dense, then metrics, and a change event last because it is the answer to
// "what happened" and there is no version of that question where "too late
// to keep" helps.
func TestTheDropOrderIsTheOneThePlanNames(t *testing.T) {
	classes := DefaultClasses()
	event, ok := classes[ClassEvent]
	if !ok {
		t.Fatal("the event class is not in the policy table")
	}
	if event.MaxAge != 0 {
		t.Errorf("the event class has a %s horizon; a change event has no expiry", event.MaxAge)
	}
	if classes[ClassTrace].DropPriority <= classes[ClassHostPoint].DropPriority {
		t.Error("traces are not dropped before metrics")
	}
	if classes[ClassSamples].DropPriority != classes[ClassHostPoint].DropPriority {
		t.Error("a point and a sample are the same kind of thing and are graded differently")
	}
	if event.DropPriority >= classes[ClassHostPoint].DropPriority {
		t.Error("a change event is dropped before a metric")
	}
}

// TestTheLogIsOwnerOnly is the check the spill bug taught us to write
// first, and it applies twice over here: telemetry says what this host was
// doing, and an open file is a file the neighbours can read.
func TestTheLogIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: filepath.Join(dir, "wal")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()
	if err := w.Record(context.Background(), pointBatch("embedded")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	info, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log mode = %04o, want 0600", perm)
	}
}

// TestTheDrainLoopWakesOnANudge is the healthy path's whole latency story:
// the sampling loop nudges, the drain runs on the next loop turn, and the
// node is not waiting out an interval for every sample it took.
func TestTheDrainLoopWakesOnANudge(t *testing.T) {
	w, _ := newWAL(t, Options{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Record(ctx, pointBatch("embedded")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	drained := make(chan int, 4)
	send := func(_ context.Context, batches []Batch) (int, error) {
		drained <- len(batches)
		return len(batches), nil
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, func() bool { return true }, send) }()

	// The first drain is immediate, so this should not need the nudge.
	select {
	case n := <-drained:
		if n != 1 {
			t.Fatalf("drained %d, want the 1 row on disk", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the drain loop never made its first attempt")
	}

	// A fresh sample with an hour-long interval must still go out promptly:
	// that is the nudge doing its job.
	if err := w.Record(ctx, sampleBatch("scrape:x", 1)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	w.Nudge()
	select {
	case n := <-drained:
		if n != 1 {
			t.Fatalf("drained %d after a nudge, want 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a nudge did not wake the drain: every sample now waits out the interval")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the drain loop did not stop on context cancel")
	}
}

// TestARowThisBuildCannotReadIsNotAcked is the regression for a defect the
// first version of the drain shipped with. A row that does not parse was
// turned into an empty batch and handed to the sender; the sender is a
// no-op for an empty batch, so the row was acked and lost. The unreadable
// row is now carried as a short count, and a short count with no error is
// a failed drain, so every row — readable and unreadable alike — stays.
//
// The test writes at the spool level rather than through Record, because
// the only way to get an unreadable row is to write one this build cannot
// produce: a payload that is not a Batch.
func TestARowThisBuildCannotReadIsNotAcked(t *testing.T) {
	w, _ := newWAL(t, Options{})
	ctx := context.Background()

	// One good row, then one the Batch decoder will refuse.
	if err := w.Record(ctx, pointBatch("s1")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := w.spool.Record(ctx, ClassSamples, json.RawMessage(`"not a batch"`)); err != nil {
		t.Fatalf("Record unreadable: %v", err)
	}
	if err := w.Record(ctx, pointBatch("s2")); err != nil {
		t.Fatalf("Record: %v", err)
	}

	sent := 0
	_, err := w.Drain(ctx, func() bool { return true }, func(_ context.Context, batches []Batch) (int, error) {
		sent += len(batches)
		return len(batches), nil
	})
	if err == nil {
		t.Fatal("a drain holding a row this build cannot read succeeded; the row was acked and lost")
	}
	if sent != 0 {
		t.Errorf("sent %d batches from a batch that was partly unreadable; the readable rows must wait too", sent)
	}
	n, err := w.spool.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 3 {
		t.Errorf("spool holds %d rows after a failed drain, want 3 — nothing may be acked", n)
	}
}

func TestAWALWithoutADirectoryIsRefused(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Fatal("a log with nowhere to live was accepted")
	}
}

func TestADrainWithSomewhereMissingSaysWhat(t *testing.T) {
	w, _ := newWAL(t, Options{})
	ctx := context.Background()
	if _, err := w.Drain(ctx, nil, func(context.Context, []Batch) (int, error) { return 0, nil }); err == nil {
		t.Error("a drain that does not know the link state was accepted")
	}
	if _, err := w.Drain(ctx, func() bool { return true }, nil); err == nil {
		t.Error("a drain with nowhere to send was accepted")
	}
}
