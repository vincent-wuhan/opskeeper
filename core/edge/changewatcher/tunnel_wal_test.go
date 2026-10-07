package changewatcher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// These are the plan's 1.1 cases for the change-event half: an event is on
// disk before it is reported, a flush that fails loses nothing, and the
// backlog goes up in batches rather than all at once when the tunnel comes
// back.

func newWALSink(t *testing.T, fc *fakeClient, cfg TunnelSinkSinkConfig) *TunnelSink {
	t.Helper()
	cfg.WALDir = filepath.Join(t.TempDir(), "changes")
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.BufferSize == 0 {
		cfg.BufferSize = cfg.BatchSize * 2
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = time.Hour
	}
	sink := NewTunnelSink(fc, newTestLogger(t), cfg)
	t.Cleanup(func() { _ = sink.CloseLog() })
	return sink
}

func changeEvent(subject string) ChangeEvent {
	return ChangeEvent{
		Source:    SourceJournald,
		Kind:      KindServiceRestart,
		Subject:   subject,
		Action:    "restarted",
		Timestamp: time.Date(2026, 3, 1, 3, 12, 0, 0, time.UTC),
		Severity:  SeverityInfo,
	}
}

// TestAFlushedEventIsOnDiskBeforeItIsReported is the whole of 1.1 for this
// sink, and it is a statement about order: the row is written before the
// call that can fail. A sink that only records on the success path records
// nothing at all during the outage it exists for.
func TestAFlushedEventIsOnDiskBeforeItIsReported(t *testing.T) {
	fc := newFakeClient()
	sink := newWALSink(t, fc, TunnelSinkSinkConfig{})
	ctx := context.Background()

	if err := sink.Push(ctx, changeEvent("orders-api")); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// The tunnel is down for this one, so the event cannot go out.
	fc.failNext.Store(true)
	sink.flushBatch(ctx)

	if n, err := sink.Pending(); err != nil || n != 1 {
		t.Fatalf("pending = %d, %v; want the event on disk: a failed flush used to lose it", n, err)
	}
	if fc.callCount() != 0 {
		t.Fatalf("%d calls went out, want 0", fc.callCount())
	}
	rows, err := sink.wal.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	var ev ChangeEvent
	if err := json.Unmarshal(rows[0].Payload, &ev); err != nil {
		t.Fatalf("the row is not a change event: %v", err)
	}
	if ev.Subject != "orders-api" {
		t.Errorf("subject = %q, want orders-api", ev.Subject)
	}
}

// TestAReconnectReplaysTheBacklogInOrder is 恢复回放. The events that piled
// up while the tunnel was down go up oldest first, and they go *before* the
// event that arrived after the link came back — otherwise a node's timeline
// reads backwards exactly when someone is trying to reconstruct what
// happened.
func TestAReconnectReplaysTheBacklogInOrder(t *testing.T) {
	fc := newFakeClient()
	sink := newWALSink(t, fc, TunnelSinkSinkConfig{BatchSize: 100, FlushInterval: time.Hour})
	ctx := context.Background()

	for _, subject := range []string{"first", "second", "third"} {
		if err := sink.Push(ctx, changeEvent(subject)); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
	fc.failNext.Store(true)
	sink.flushBatch(ctx)
	if n, _ := sink.Pending(); n != 3 {
		t.Fatalf("pending = %d, want 3", n)
	}

	// A new event arrives while the backlog is still on disk. It is in the
	// channel, and the channel is newer than the log.
	if err := sink.Push(ctx, changeEvent("fourth")); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// One flush carries one batch, so the replay tick is spent on the
	// backlog and "fourth" waits. That ordering is the point, not an
	// accident of the loop.
	sink.flushBatch(ctx)
	if n, _ := sink.Pending(); n != 0 {
		t.Fatalf("pending = %d after the replay, want 0", n)
	}
	sink.flushBatch(ctx)

	fc.mu.Lock()
	delivered := append([]*tunnel.PushChangeEventsRequest(nil), fc.calls...)
	fc.mu.Unlock()

	var got []string
	for _, c := range delivered {
		for _, e := range c.Events {
			got = append(got, e.Subject)
		}
	}
	want := []string{"first", "second", "third", "fourth"}
	if len(got) != len(want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivered %v, want %v: the backlog must go before what arrived after it", got, want)
		}
	}
}

// TestTheBacklogGoesUpInBatches is 回放限流 for this sink: one flush carries
// one batch, so a node that reconnects after an hour hands the center an
// hour's worth of events gradually rather than in a single call — and a
// fleet that all reconnects at once does not do it in lockstep either.
func TestTheBacklogGoesUpInBatches(t *testing.T) {
	const batch = 2
	fc := newFakeClient()
	sink := newWALSink(t, fc, TunnelSinkSinkConfig{BatchSize: batch, BufferSize: 64, FlushInterval: time.Hour})
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		if err := sink.Push(ctx, changeEvent("e")); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
	// The tunnel is down, so the first flush lands on disk and stays there.
	fc.failNext.Store(true)
	sink.flushBatch(ctx)
	if n, _ := sink.Pending(); n != batch {
		t.Fatalf("pending = %d after a failed flush, want %d: a failed flush must ack nothing", n, batch)
	}

	// Healthy again. Each flush moves at most one batch, so six events
	// take three ticks rather than one. The measurement is the wire, not
	// the log: Pending() counts the disk and cannot see what is still in
	// the channel, so counting rows would measure the wrong thing.
	for i := 0; i < 4; i++ {
		sink.flushBatch(ctx)
	}
	fc.mu.Lock()
	delivered := append([]*tunnel.PushChangeEventsRequest(nil), fc.calls...)
	fc.mu.Unlock()
	if len(delivered) < 3 {
		t.Fatalf("the backlog took %d calls, want at least 3: one batch per flush is the rate limit", len(delivered))
	}
	total := 0
	for i, c := range delivered {
		if len(c.Events) > batch {
			t.Errorf("call %d carried %d events, more than the batch size of %d", i, len(c.Events), batch)
		}
		total += len(c.Events)
	}
	if total != 6 {
		t.Errorf("delivered %d events, want 6", total)
	}
}

// TestASinkWithNoDirectoryKeepsTheOldBehaviour is the compatibility
// statement: a development node with no state directory still delivers
// change events, it just forgets them when a flush fails, which is what it
// has always done.
func TestASinkWithNoDirectoryKeepsTheOldBehaviour(t *testing.T) {
	fc := newFakeClient()
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{BatchSize: 10, BufferSize: 10, FlushInterval: time.Hour})
	defer sink.CloseLog()
	ctx := context.Background()
	if sink.wal != nil {
		t.Fatal("a sink with no directory opened a log")
	}
	if err := sink.Push(ctx, changeEvent("orders-api")); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if n, err := sink.Pending(); err != nil || n != 0 {
		t.Errorf("pending = %d, %v; there is no log to be pending on", n, err)
	}
	sink.flushBatch(ctx)
	if fc.callCount() != 1 {
		t.Errorf("%d calls, want 1", fc.callCount())
	}
}

// TestTheEventLogIsOwnerOnly is the spill bug's lesson applied to a second
// file: a change event says what this host did, and a world-readable list
// of that is an inventory.
func TestTheEventLogIsOwnerOnly(t *testing.T) {
	fc := newFakeClient()
	dir := filepath.Join(t.TempDir(), "changes")
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{BatchSize: 10, BufferSize: 10, FlushInterval: time.Hour, WALDir: dir})
	defer sink.CloseLog()
	if err := sink.Push(context.Background(), changeEvent("orders-api")); err != nil {
		t.Fatalf("Push: %v", err)
	}
	sink.flushBatch(context.Background())
	info, err := os.Stat(filepath.Join(dir, WALFileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log mode = %04o, want 0600", perm)
	}
}
