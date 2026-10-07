package autonomy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The spool is what the plan calls "本地审计 spool", and its tests are the
// four cases it names: 断连写入、恢复回放、容量上限丢弃策略、回放限流.

func newSpool(t *testing.T, maxBytes int64) *Spool {
	t.Helper()
	s, err := OpenSpool(filepath.Join(t.TempDir(), "audit", "autonomy.jsonl"), maxBytes)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func row(key, result string) Row {
	return Row{
		At:      time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		Action:  "restart-orders-on-disk-full",
		Package: "opskeeper-sre-autonomy",
		Tool:    "host_restart_service",
		Target:  "orders-api-7d9",
		Argv:    []string{"systemctl", "restart", "orders-api"},
		Key:     key,
		Verdict: Run.String(),
		Phase:   PhaseDecided,
		Result:  result,
	}
}

// TestTheSpoolIsOwnerOnly is the check the spill bug taught us to write
// first: a node's autonomy decisions are a list of things it did to itself
// without asking, and a world-readable copy of that is an incident report
// with an extra reader.
func TestTheSpoolIsOwnerOnly(t *testing.T) {
	s := newSpool(t, 0)
	if err := s.Record(context.Background(), row("k1", ResultOK)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600: the spool lists what this node did to itself", perm)
	}
	dir, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("directory mode = %04o, want owner-only", perm)
	}
}

// TestAnAlreadyOpenSpoolIsRefusedRatherThanWidened is the judgement behind
// it: a spool that was already too open is evidence that something else on
// the host is too open, and the arbiter's first act should not be to paper
// over it.
func TestAnAlreadyOpenSpoolIsRefusedRatherThanWidened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autonomy.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := OpenSpool(path, 0); err == nil {
		t.Fatal("a world-readable spool was adopted")
	} else if !strings.Contains(err.Error(), "owner-only") {
		t.Errorf("error = %v, want it to say why", err)
	}
}

// TestTheSpoolRefusesToWriteThroughASymlink is the other half: the path
// comes from configuration, and a node that writes its audit trail through
// a link somebody planted has an audit trail that ends up somewhere its
// owner did not choose.
func TestTheSpoolRefusesToWriteThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "somebody-elses-file")
	if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(dir, "autonomy.jsonl")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := OpenSpool(link, 0); err == nil {
		t.Fatal("the spool followed a symlink")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error = %v, want it to name the symlink", err)
	}
}

// TestAnInterruptedWriteDoesNotCostTheRowsAroundIt is the crash case. A
// spool that cannot be read because of its own last row is a spool that
// will never be replayed, so a half-written line is dropped and the rest is
// read.
func TestAnInterruptedWriteDoesNotCostTheRowsAroundIt(t *testing.T) {
	s := newSpool(t, 0)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.Record(ctx, row(fmt.Sprintf("k%d", i), ResultOK)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	// A power cut mid-write leaves a partial line and nothing after it.
	f, err := os.OpenFile(s.Path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"at":"2026-03-01T12:00:0`); err != nil {
		t.Fatalf("seed partial: %v", err)
	}
	f.Close()

	got, err := s.Replay(func(Row) error { return nil })
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got != 3 {
		t.Errorf("replayed %d rows, want 3: the partial line is dropped, not fatal", got)
	}
}

// TestDrainingAnEmptySpoolIsNotWork covers the common case, which is a node
// that has no rows to send and is being asked every few seconds anyway.
func TestDrainingAnEmptySpoolIsNotWork(t *testing.T) {
	s := newSpool(t, 0)
	p, err := NewPump(PumpOptions{Spool: s, Sender: SenderFunc(func(context.Context, []Row) error {
		t.Error("the sender was called with nothing to send")
		return nil
	}), Link: LinkFunc(func() Reach { return Reach{Online: true} })})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	n, err := p.DrainOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("DrainOnce = %d, %v, want 0, nil", n, err)
	}
}

// TestReplayingWhileOfflineLosesNothing: the spool's whole reason. A node
// whose tunnel is down must not drain into it, and what it wrote while it
// was down must still be there when the tunnel comes back.
func TestReplayingWhileOfflineLosesNothing(t *testing.T) {
	s := newSpool(t, 0)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.Record(ctx, row(fmt.Sprintf("k%d", i), ResultOK)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	var sent [][]Row
	var mu sync.Mutex
	online := false
	p, err := NewPump(PumpOptions{
		Spool: s,
		Sender: SenderFunc(func(_ context.Context, rows []Row) error {
			mu.Lock()
			sent = append(sent, rows)
			mu.Unlock()
			return nil
		}),
		Link: LinkFunc(func() Reach { return Reach{Online: online} }),
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}

	if n, err := p.DrainOnce(ctx); n != 0 || err != nil {
		t.Fatalf("an offline drain = %d, %v, want nothing", n, err)
	}
	if n, err := p.Pending(); n != 5 || err != nil {
		t.Fatalf("pending = %d, want 5: a drain that could not happen must not consume anything", n)
	}

	online = true
	n, err := p.DrainOnce(ctx)
	if err != nil || n != 5 {
		t.Fatalf("the drain after reconnect = %d, %v, want 5", n, err)
	}
	mu.Lock()
	got := len(sent)
	mu.Unlock()
	if got != 1 {
		t.Errorf("the sender was called %d times, want 1", got)
	}
	if n, err := p.Pending(); n != 0 || err != nil {
		t.Errorf("pending after the drain = %d, want 0", n)
	}
}

// TestReplayIsRateLimited is the "回放限流，避免冲击中心" line, and it is
// not politeness: every node's tunnel comes back at the same moment after a
// center restart, and the audit chain is the endpoint that must survive it.
func TestReplayIsRateLimited(t *testing.T) {
	s := newSpool(t, 0)
	ctx := context.Background()
	for i := 0; i < 250; i++ {
		if err := s.Record(ctx, row(fmt.Sprintf("k%d", i), ResultOK)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	clock := &clock{at: installedAt}
	var sent []int
	p, err := NewPump(PumpOptions{
		Spool:    s,
		Batch:    100,
		Interval: 5 * time.Second,
		Now:      clock.now,
		Sender: SenderFunc(func(_ context.Context, rows []Row) error {
			sent = append(sent, len(rows))
			return nil
		}),
		Link: LinkFunc(func() Reach { return Reach{Online: true} }),
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}

	// The first drain goes out immediately: an operator waiting on a story
	// from an outage should not wait an interval for it.
	if n, _ := p.DrainOnce(ctx); n != 100 {
		t.Fatalf("first drain = %d, want a full batch", n)
	}
	// The second, one second later, is held back.
	clock.advance(time.Second)
	if n, _ := p.DrainOnce(ctx); n != 0 {
		t.Fatalf("a drain inside the interval = %d, want 0", n)
	}
	// And the row count is untouched, so holding it back costs nothing.
	if n, _ := p.Pending(); n != 150 {
		t.Fatalf("pending = %d, want 150: a held drain must not consume rows", n)
	}
	clock.advance(4 * time.Second)
	if n, _ := p.DrainOnce(ctx); n != 100 {
		t.Fatalf("drain after the interval = %d, want 100", n)
	}
	clock.advance(5 * time.Second)
	if n, _ := p.DrainOnce(ctx); n != 50 {
		t.Fatalf("final drain = %d, want the remainder", n)
	}
	want := []int{100, 100, 50}
	if len(sent) != len(want) {
		t.Fatalf("batch sizes = %v, want %v", sent, want)
	}
	for i, n := range sent {
		if n != want[i] {
			t.Errorf("batch %d = %d, want %d", i, n, want[i])
		}
	}
}

// TestAFailedReplayKeepsItsRows is why the ack is all-or-nothing: the
// center's chain is ordered, and a hole in it is worse than a delay.
func TestAFailedReplayKeepsItsRows(t *testing.T) {
	s := newSpool(t, 0)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.Record(ctx, row(fmt.Sprintf("k%d", i), ResultOK)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	failing := true
	clock := &clock{at: installedAt}
	p, err := NewPump(PumpOptions{
		Spool: s, Batch: 10, Interval: time.Second, Now: clock.now,
		Sender: SenderFunc(func(context.Context, []Row) error {
			if failing {
				return errors.New("tunnel is down")
			}
			return nil
		}),
		Link: LinkFunc(func() Reach { return Reach{Online: true} }),
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	if n, err := p.DrainOnce(ctx); n != 0 || err == nil {
		t.Fatalf("a failing drain = %d, %v, want an error and no rows moved", n, err)
	}
	if n, _ := p.Pending(); n != 3 {
		t.Fatalf("pending = %d, want 3: a failed delivery consumed rows", n)
	}
	failing = false
	clock.advance(time.Second)
	if n, err := p.DrainOnce(ctx); n != 3 || err != nil {
		t.Fatalf("the retry = %d, %v, want 3", n, err)
	}
}

// TestTheSpoolDropsItsOldestRowsWhenItFills is the capacity policy, and
// which end it drops from is the whole decision: once the newest rows are
// gone, the outage is over and nobody is reading the log.
func TestTheSpoolDropsItsOldestRowsWhenItFills(t *testing.T) {
	// A cap small enough that a few hundred rows overflow it, and large
	// enough that the floor of a hundred rows still fits inside it.
	//
	// The second half of that sentence is load-bearing, and it was learned
	// the hard way. The cap used to be 32 KiB, which happened to fit 100
	// rows of the old line format to the byte — and stopped fitting the
	// moment the row grew an envelope. A floor that overruns the cap is
	// not a generous floor, it is a full disk, so the cap is now the hard
	// bound and this test uses a number with room in it rather than a
	// number that happens to work today.
	s := newSpool(t, 64*1024)
	ctx := context.Background()
	for i := 0; i < 2000; i++ {
		if err := s.Record(ctx, row(fmt.Sprintf("key-%04d", i), ResultOK)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > 64*1024 {
		t.Errorf("size = %d, want it inside the cap", info.Size())
	}
	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) < 100 {
		t.Fatalf("kept %d rows, want at least the floor of 100", len(rows))
	}
	// The newest survive; the oldest are what got dropped.
	last := rows[len(rows)-1]
	if last.Key != "key-1999" {
		t.Errorf("newest key = %s, want key-1999: compaction dropped the wrong end", last.Key)
	}
	if rows[0].Key == "key-0000" {
		t.Error("the oldest row survived a full spool: the cap dropped the wrong end")
	}
}

// TestTheSpoolIsReadableAfterCompaction is the boring half of the previous
// test and the one that actually bites: a compaction that leaves a file no
// reader can parse is a spool that has silently lost everything, including
// the rows it kept.
func TestTheSpoolIsReadableAfterCompaction(t *testing.T) {
	s := newSpool(t, 64*1024)
	ctx := context.Background()
	for i := 0; i < 2000; i++ {
		if err := s.Record(ctx, row(fmt.Sprintf("key-%04d", i), ResultOK)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	// A write after the compaction must land in the replacement file, not
	// in the unlinked original the old descriptor still points at.
	if err := s.Record(ctx, row("key-after-compaction", ResultOK)); err != nil {
		t.Fatalf("Record after compaction: %v", err)
	}
	var keys []string
	n, err := s.Replay(func(r Row) error {
		keys = append(keys, r.Key)
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(keys) != n {
		t.Errorf("replayed %d rows but the callback saw %d", n, len(keys))
	}
	if keys[len(keys)-1] != "key-after-compaction" {
		t.Errorf("last key = %s, want the row written after the compaction", keys[len(keys)-1])
	}
}

// TestAckDropsOnlyWhatWasDelivered keeps the replay pump honest: acking a
// count larger than the spool holds, or acking in the wrong order, would
// silently discard rows nobody ever sent.
func TestAckDropsOnlyWhatWasDelivered(t *testing.T) {
	s := newSpool(t, 0)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.Record(ctx, row(fmt.Sprintf("k%d", i), ResultOK)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := s.Ack(99); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n, _ := s.Len(); n != 0 {
		t.Errorf("rows = %d, want 0: acking more than exists drops what exists and no more", n)
	}
	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}
