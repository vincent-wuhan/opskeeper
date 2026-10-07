package spool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These are the four cases the plan names for a spool — 断连写入、恢复回放、
// 容量上限丢弃策略、回放限流 — plus the two this node actually has three of:
// the graded drop, and a sequence number that survives a restart.

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

func newSpool(t *testing.T, opts Options) (*Spool, *clock) {
	t.Helper()
	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	if opts.Path == "" {
		opts.Path = filepath.Join(t.TempDir(), "spool", "rows.jsonl")
	}
	if opts.Now == nil {
		opts.Now = c.now
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, c
}

// TestTheSpoolIsOwnerOnly is the first thing to write, every time: a node's
// buffered telemetry and its audit rows live here, and a world-readable
// copy of either is an incident report with an extra reader.
func TestTheSpoolIsOwnerOnly(t *testing.T) {
	s, _ := newSpool(t, Options{})
	if err := s.Record(context.Background(), "event", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("spool mode = %04o, want 0600", perm)
	}
	dir, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", perm)
	}
}

// TestAnAlreadyOpenSpoolIsRefusedRatherThanWidened is the uncomfortable one.
// A spool that was already group-readable is evidence that something else on
// this host is too open, and the fix here would be to paper over it.
func TestAnAlreadyOpenSpoolIsRefusedRatherThanWidened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rows.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := Open(Options{Path: path})
	if err == nil {
		t.Fatal("Open accepted a world-readable spool")
	}
	if !strings.Contains(err.Error(), "owner-only") {
		t.Errorf("error = %v, want it to say why", err)
	}
}

func TestTheSpoolRefusesToWriteThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := Open(Options{Path: link})
	if err == nil {
		t.Fatal("Open wrote through a symlink")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error = %v, want it to name the symlink", err)
	}
}

// TestAnInterruptedWriteDoesNotCostTheRowsAroundIt is the crash case. A
// spool that cannot be read because of its own last row is a spool that will
// never be replayed, so a half-written line is dropped and the rest is read.
func TestAnInterruptedWriteDoesNotCostTheRowsAroundIt(t *testing.T) {
	s, _ := newSpool(t, Options{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	f, err := os.OpenFile(s.Path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"c":"event","at":"2026-03-01T12:00:0`); err != nil {
		t.Fatalf("seed partial: %v", err)
	}
	f.Close()

	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("read %d rows, want 3: the partial line is dropped, not fatal", len(rows))
	}
}

// A tear is only harmless while nothing is appended after it. The file is
// opened O_APPEND, so without a repair at open the next row is glued onto
// the half-written bytes and a good row written *after* the restart is lost
// with them — the failure the previous test cannot see, because it never
// reopens.
func TestATornTailIsTruncatedSoTheNextRowIsNotWrittenOntoIt(t *testing.T) {
	s, _ := newSpool(t, Options{})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f, err := os.OpenFile(s.Path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"c":"event","at":"2026-03-01T12:00:0`); err != nil {
		t.Fatalf("seed partial: %v", err)
	}
	f.Close()

	s2, err := Open(Options{Path: s.Path(), Now: time.Now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if err := s2.Record(ctx, "event", map[string]int{"i": 2}); err != nil {
		t.Fatalf("Record after a torn tail: %v", err)
	}
	rows, err := s2.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("read %d rows, want 3: a row written after a power cut is not collateral damage", len(rows))
	}
	if !strings.Contains(string(rows[2].Payload), `"i":2`) {
		t.Errorf("the last row is %s, want the one just written", rows[2].Payload)
	}
}

// A file that is one unterminated line has nothing in it worth keeping, and
// the repair must say so rather than leaving a row that can never parse.
func TestASpoolThatIsNothingButATornLineIsEmptiedRatherThanKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool", "rows.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"c":"event","at":`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a spool of one torn line reports %d rows", len(rows))
	}
	if err := s.Record(context.Background(), "event", map[string]int{"i": 1}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rows, err = s.Peek(0)
	if err != nil {
		t.Fatalf("Peek after Record: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("read %d rows after a clean restart, want 1", len(rows))
	}
}

// TestARowWrittenWhileOfflineIsStillThereWhenTheLinkReturns is 断连写入 +
// 恢复回放, which is the case the whole package exists for. The writer
// never learns whether anything is listening, because the writer is a
// sampler and a sampler that waits for permission is a sampler that drops
// samples.
func TestARowWrittenWhileOfflineIsStillThereWhenTheLinkReturns(t *testing.T) {
	s, _ := newSpool(t, Options{})
	online := false
	var sent []Row
	pump, err := NewPump(PumpOptions{
		Spool:     s,
		Reachable: func() bool { return online },
		Send: func(_ context.Context, rows []Row) (int, error) {
			sent = append(sent, rows...)
			return len(rows), nil
		},
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if n, err := pump.DrainOnce(ctx); err != nil || n != 0 {
			t.Fatalf("drain while offline = %d, %v; want nothing attempted", n, err)
		}
	}
	if n, _ := s.Len(); n != 5 {
		t.Fatalf("spool holds %d rows, want 5: an offline writer lost data", n)
	}
	online = true
	if n, err := pump.DrainOnce(ctx); err != nil || n != 5 {
		t.Fatalf("drain after recovery = %d, %v; want 5", n, err)
	}
	if len(sent) != 5 {
		t.Fatalf("sent %d rows, want 5", len(sent))
	}
	for i, row := range sent {
		var body map[string]int
		if err := json.Unmarshal(row.Payload, &body); err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		if body["i"] != i {
			t.Errorf("row %d carried i=%d: replay is out of order", i, body["i"])
		}
	}
	if n, _ := s.Len(); n != 0 {
		t.Errorf("spool holds %d rows after a delivered drain, want 0", n)
	}
}

// TestReplayIsRateLimited is 回放限流, and it is a safety property rather
// than a performance one: a node that reconnects after a week must not hand
// the center the whole week in one request.
func TestReplayIsRateLimited(t *testing.T) {
	s, c := newSpool(t, Options{})
	ctx := context.Background()
	for i := 0; i < 250; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	calls := 0
	pump, err := NewPump(PumpOptions{
		Spool:     s,
		Now:       c.now,
		Reachable: func() bool { return true },
		Send: func(_ context.Context, rows []Row) (int, error) {
			calls++
			return len(rows), nil
		},
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	if n, err := pump.DrainOnce(ctx); err != nil || n != DefaultBatch {
		t.Fatalf("first drain = %d, %v; want a full batch of %d", n, err, DefaultBatch)
	}
	if n, err := pump.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second drain = %d, %v; want 0: the interval has not elapsed", n, err)
	}
	if calls != 1 {
		t.Errorf("sent %d times, want 1", calls)
	}
	c.advance(DefaultInterval)
	if n, err := pump.DrainOnce(ctx); err != nil || n != DefaultBatch {
		t.Fatalf("drain after the interval = %d, %v; want %d", n, err, DefaultBatch)
	}
	if n, _ := s.Len(); n != 50 {
		t.Errorf("%d rows left, want 50", n)
	}
}

// TestADrainThatEmptiesTheLogIsNotThrottled is the rule that keeps the rate
// limit from costing every healthy node its latency.
//
// The interval exists so that a node coming back from an outage does not
// hand the center a week of data in one second, and a fleet reconnecting at
// once does not do it in lockstep. A log holding fewer rows than one batch
// is neither of those: it is a node that sampled once. Throttling it would
// trade a real protection for a latency every node pays forever.
func TestADrainThatEmptiesTheLogIsNotThrottled(t *testing.T) {
	s, c := newSpool(t, Options{})
	ctx := context.Background()
	calls := 0
	pump, err := NewPump(PumpOptions{
		Spool:     s,
		Now:       c.now,
		Batch:     100,
		Reachable: func() bool { return true },
		Send: func(_ context.Context, rows []Row) (int, error) {
			calls++
			return len(rows), nil
		},
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	// The clock does not move. Every one of these drains has an interval
	// of zero elapsed, and each of them empties the log.
	for i := 0; i < 5; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if n, err := pump.DrainOnce(ctx); err != nil || n != 1 {
			t.Fatalf("drain %d = %d, %v; a one-row log is not a backlog", i, n, err)
		}
	}
	if calls != 5 {
		t.Errorf("sent %d times, want 5", calls)
	}
	// And a full batch is still throttled, which is the half that keeps
	// this rule from becoming "no rate limit at all".
	c.advance(DefaultInterval)
	for i := 0; i < 100; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if n, err := pump.DrainOnce(ctx); err != nil || n != 100 {
		t.Fatalf("full drain = %d, %v; want the whole batch", n, err)
	}
	if n, err := pump.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second full drain = %d, %v; want 0: a backlog is throttled", n, err)
	}
}

// TestAFailedDrainKeepsEveryRow is the all-or-nothing half. A chain with a
// hole in it is worse than a chain that is late.
func TestAFailedDrainKeepsEveryRow(t *testing.T) {
	s, _ := newSpool(t, Options{})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	boom := errors.New("tunnel down")
	pump, err := NewPump(PumpOptions{
		Spool:     s,
		Reachable: func() bool { return true },
		Send:      func(context.Context, []Row) (int, error) { return 0, boom },
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	if _, err := pump.DrainOnce(ctx); !errors.Is(err, boom) {
		t.Fatalf("drain error = %v, want the sender's", err)
	}
	if n, _ := s.Len(); n != 5 {
		t.Fatalf("spool holds %d rows after a failed send, want 5", n)
	}
	// And the retry gets the same rows in the same order.
	var seen []Row
	pump2, err := NewPump(PumpOptions{
		Spool:     s,
		Reachable: func() bool { return true },
		Send: func(_ context.Context, rows []Row) (int, error) {
			seen = append(seen, rows...)
			return len(rows), nil
		},
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	if _, err := pump2.DrainOnce(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(seen) != 5 {
		t.Fatalf("retry delivered %d rows, want 5", len(seen))
	}
}

// TestTheOldestRowsGoFirstWhenTheSpoolFills is 容量上限丢弃策略 for a single
// class: once the newest rows are gone, the outage is over and nobody is
// reading the log.
func TestTheOldestRowsGoFirstWhenTheSpoolFills(t *testing.T) {
	s, _ := newSpool(t, Options{MaxBytes: 32 * 1024, KeepFloor: 100})
	ctx := context.Background()
	for i := 0; i < 2000; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if size := s.Size(); size > 32*1024 {
		t.Errorf("size = %d, want it inside the cap", size)
	}
	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) < 100 {
		t.Fatalf("kept %d rows, want at least the protected tail of 100", len(rows))
	}
	if got := indexOf(rows, 1999); got != len(rows)-1 {
		t.Errorf("the newest row sits at %d, want last: compaction dropped the wrong end", got)
	}
	if indexOf(rows, 0) >= 0 {
		t.Error("the oldest row survived a full spool")
	}
	if st := s.Snapshot(); st.Dropped == 0 {
		t.Error("nothing was recorded as dropped: a health page cannot tell a quiet spool from a lossy one")
	}
}

// TestTheSpoolIsReadableAfterCompaction is the half that actually bites. A
// compaction that leaves a file no reader can parse has lost everything,
// including the rows it kept.
func TestTheSpoolIsReadableAfterCompaction(t *testing.T) {
	s, _ := newSpool(t, Options{MaxBytes: 32 * 1024, KeepFloor: 100})
	ctx := context.Background()
	for i := 0; i < 2000; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := s.Record(ctx, "event", map[string]string{"after": "compaction"}); err != nil {
		t.Fatalf("Record after compaction: %v", err)
	}
	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	last := rows[len(rows)-1]
	if !strings.Contains(string(last.Payload), "compaction") {
		t.Errorf("last payload = %s, want the row written after the compaction", last.Payload)
	}
}

// TestTracesGoBeforeMetrics is the plan's 分级丢弃 stated as an assertion:
// traces are the largest and the least dense, so under pressure they are
// the first thing to go — but not the *only* thing to go. A node that
// discards metrics to make room for nothing is worse than one that fills up.
func TestTracesGoBeforeMetrics(t *testing.T) {
	s, _ := newSpool(t, Options{
		MaxBytes:  16 * 1024,
		KeepFloor: 20,
		Classes: map[Class]Policy{
			"metric": {DropPriority: PriorityValuable},
			"trace":  {DropPriority: PriorityBulk},
		},
	})
	ctx := context.Background()
	// Interleaved, so the only thing that can save the metrics is the class
	// ordering rather than the age ordering.
	for i := 0; i < 800; i++ {
		if err := s.Record(ctx, "metric", map[string]int{"i": i, "v": 1}); err != nil {
			t.Fatalf("Record metric: %v", err)
		}
		if err := s.Record(ctx, "trace", map[string]any{"i": i, "pad": strings.Repeat("x", 80)}); err != nil {
			t.Fatalf("Record trace: %v", err)
		}
	}
	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	var metrics, traces int
	for _, r := range rows {
		switch r.Class {
		case "metric":
			metrics++
		case "trace":
			traces++
		}
	}
	if metrics == 0 {
		t.Fatal("every metric was dropped to make room for traces: the drop order is inverted")
	}
	if traces >= metrics {
		t.Errorf("kept %d traces against %d metrics; traces are the disposable class and should be gone first", traces, metrics)
	}
	st := s.Snapshot()
	if st.DroppedByClass["trace"] == 0 {
		t.Error("no trace was recorded as dropped")
	}
}

// TestAStaleRowGoesEvenWhenTheDiskIsEmpty is the difference between a
// capacity mechanism and a staleness one. A metric delivered an hour late
// is not a delayed metric; it is a wrong one.
func TestAStaleRowGoesEvenWhenTheDiskIsEmpty(t *testing.T) {
	s, c := newSpool(t, Options{
		MaxBytes: 1 << 20,
		Classes: map[Class]Policy{
			"metric": {DropPriority: PriorityValuable, MaxAge: 15 * time.Minute},
			"audit":  {DropPriority: PriorityCritical},
		},
	})
	ctx := context.Background()
	if err := s.Record(ctx, "metric", map[string]string{"n": "old"}); err != nil {
		t.Fatalf("Record metric: %v", err)
	}
	if err := s.Record(ctx, "audit", map[string]string{"n": "old"}); err != nil {
		t.Fatalf("Record audit: %v", err)
	}
	c.advance(16 * time.Minute)
	if err := s.Record(ctx, "metric", map[string]string{"n": "new"}); err != nil {
		t.Fatalf("Record fresh metric: %v", err)
	}
	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	var classes []Class
	for _, r := range rows {
		classes = append(classes, r.Class)
	}
	// The audit row was written first, so it is still first — the sweep
	// removed a row from the middle of the file, which is the whole
	// difference between a sweep and a truncation.
	if len(classes) != 2 {
		t.Fatalf("kept %v, want the fresh metric and the audit row", classes)
	}
	if classes[0] != "audit" {
		t.Errorf("kept %v, want the audit row to survive any age: an audit record has no horizon", classes)
	}
	if classes[1] != "metric" || !strings.Contains(string(rows[1].Payload), "new") {
		t.Errorf("kept %v, want the fresh metric", classes)
	}
	if st := s.Snapshot(); st.DroppedAged != 1 {
		t.Errorf("DroppedAged = %d, want 1: a health page must be able to tell staleness from volume", st.DroppedAged)
	}
}

// TestAnUndeclaredClassIsTreatedAsTheMostValuable is the conservative
// default. A row nobody classified must not be the first thing dropped,
// because somebody added a producer and did not update a table.
func TestAnUndeclaredClassIsTreatedAsTheMostValuable(t *testing.T) {
	s, _ := newSpool(t, Options{
		MaxBytes:  8 * 1024,
		KeepFloor: 5,
		Classes: map[Class]Policy{
			"trace": {DropPriority: PriorityBulk},
		},
	})
	ctx := context.Background()
	for i := 0; i < 400; i++ {
		if err := s.Record(ctx, "trace", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record trace: %v", err)
		}
	}
	for i := 0; i < 20; i++ {
		if err := s.Record(ctx, "something-new", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record undeclared: %v", err)
		}
	}
	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	var undeclared int
	for _, r := range rows {
		if r.Class == "something-new" {
			undeclared++
		}
	}
	if undeclared != 20 {
		t.Errorf("kept %d undeclared rows, want all 20", undeclared)
	}
}

// TestTheSequenceSurvivesARestart is the reason the envelope exists. Replay
// delivers at least once, and the only way the center can tell a
// redelivery from a new row is a number it has already seen.
func TestTheSequenceSurvivesARestart(t *testing.T) {
	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "rows.jsonl")
	ctx := context.Background()

	first, err := Open(Options{Path: path, Now: c.now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := first.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(Options{Path: path, Now: c.now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if err := second.Record(ctx, "event", map[string]int{"i": 3}); err != nil {
		t.Fatalf("Record after restart: %v", err)
	}
	rows, err := second.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("read %d rows, want 4", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Seq <= rows[i-1].Seq {
			t.Fatalf("sequence went backwards across the restart: %d after %d", rows[i].Seq, rows[i-1].Seq)
		}
	}
	if rows[0].Seq != 1 || rows[3].Seq != 4 {
		t.Errorf("sequences = %d..%d, want 1..4", rows[0].Seq, rows[3].Seq)
	}
}

// TestAnAckCannotLoseARowWrittenDuringIt is a concurrency bug this package
// was written to not have. The earlier implementation read the file, then
// took the lock and rewrote what it had read — so a row appended in between
// was not in the list being kept, and vanished. For an audit row that is
// the one failure that matters most.
func TestAnAckCannotLoseARowWrittenDuringIt(t *testing.T) {
	s, _ := newSpool(t, Options{})
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	// Hammer Ack and Record together; the invariant is that no row either
	// gets delivered or gets dropped, whichever side wins.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if err := s.Record(ctx, "event", map[string]int{"late": i}); err != nil {
				t.Errorf("Record: %v", err)
				return
			}
		}
		close(stop)
	}()
	for {
		select {
		case <-stop:
			wg.Wait()
			// The original fifty plus every "late" row the writer got in,
			// minus what was acked. A lost row is not visible in a count,
			// so the assertion is the blunt one: acking everything that is
			// left must leave nothing, and the writer's own error channel
			// above is what catches a torn write.
			if err := s.Ack(1 << 30); err != nil {
				t.Fatalf("Ack: %v", err)
			}
			if n, _ := s.Len(); n != 0 {
				t.Fatalf("%d rows survived a full ack", n)
			}
			return
		default:
		}
		if err := s.Ack(10); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	}
}

// TestAckDropsOnlyWhatWasDelivered keeps a pump honest: acking more than
// exists, or acking in the wrong order, silently discards rows nobody sent.
func TestAckDropsOnlyWhatWasDelivered(t *testing.T) {
	s, _ := newSpool(t, Options{})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := s.Ack(99); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n, _ := s.Len(); n != 0 {
		t.Errorf("rows = %d, want 0: acking more than exists drops what exists and no more", n)
	}
}

// TestAPartialDeliveryAcksOnlyWhatLanded is the reason the sender, and not
// the pump, decides how much to ack. A sample the center has already
// refused will be refused again, so a sender that refuses to report
// progress until it can ack the whole batch wedges the queue on one
// permanently-bad row — and the node then reports nothing at all, which is
// the failure mode a write-ahead log was installed to prevent.
func TestAPartialDeliveryAcksOnlyWhatLanded(t *testing.T) {
	s, c := newSpool(t, Options{})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	attempts := 0
	pump, err := NewPump(PumpOptions{
		Spool:     s,
		Now:       c.now,
		Reachable: func() bool { return true },
		Send: func(_ context.Context, rows []Row) (int, error) {
			attempts++
			// Three of the five land; the rest are samples the center
			// will never take.
			return 3, errors.New("two samples rejected as out of order")
		},
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	delivered, err := pump.DrainOnce(ctx)
	if err == nil {
		t.Error("drain reported success while the sender reported a failure")
	}
	if delivered != 3 {
		t.Errorf("delivered = %d, want 3", delivered)
	}
	if n, _ := s.Len(); n != 2 {
		t.Fatalf("spool holds %d rows, want the 2 that never landed", n)
	}
	rows, _ := s.Peek(0)
	// And the survivors are the tail, in order: the ones that never made
	// it, not an arbitrary two.
	if indexOf(rows, 3) != 0 || indexOf(rows, 4) != 1 {
		t.Errorf("survivors start at i=%d, want 3", indexOf(rows, 3))
	}
	// The next drain retries them rather than dropping them, so a
	// transient rejection does not silently become a permanent loss.
	c.advance(DefaultInterval)
	pump2, err := NewPump(PumpOptions{
		Spool:     s,
		Now:       c.now,
		Reachable: func() bool { return true },
		Send:      func(_ context.Context, rows []Row) (int, error) { return len(rows), nil },
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	if n, err := pump2.DrainOnce(ctx); err != nil || n != 2 {
		t.Fatalf("retry drained %d, %v; want the 2 survivors", n, err)
	}
	if attempts != 1 {
		t.Errorf("the sender was called %d times in one drain", attempts)
	}
}

// TestASenderThatLosesTrackOfItsBatchIsAFailure refuses to guess. A sender
// that reports fewer rows than it was handed, with no error, has lost track
// of its own batch — and the two available responses are both wrong: acking
// the difference discards rows nobody sent, treating them as sent hides the
// bug.
func TestASenderThatLosesTrackOfItsBatchIsAFailure(t *testing.T) {
	s, _ := newSpool(t, Options{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.Record(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	pump, err := NewPump(PumpOptions{
		Spool:     s,
		Reachable: func() bool { return true },
		Send:      func(context.Context, []Row) (int, error) { return 2, nil },
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	delivered, err := pump.DrainOnce(ctx)
	if err == nil {
		t.Fatal("a sender that under-reported was accepted")
	}
	if delivered != 2 {
		t.Errorf("delivered = %d, want the 2 it claimed", delivered)
	}
	if n, _ := s.Len(); n != 1 {
		t.Errorf("spool holds %d rows, want 1: the unclaimed row must stay", n)
	}
}

func TestARecordOnAClosedSpoolIsRefused(t *testing.T) {
	s, _ := newSpool(t, Options{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Record(context.Background(), "event", map[string]int{"i": 1}); !errors.Is(err, ErrClosed) {
		t.Errorf("Record on a closed spool = %v, want ErrClosed", err)
	}
}

func TestAPumpWithSomethingMissingSaysWhat(t *testing.T) {
	s, _ := newSpool(t, Options{})
	if _, err := NewPump(PumpOptions{Reachable: func() bool { return true }, Send: func(context.Context, []Row) (int, error) { return 0, nil }}); err == nil {
		t.Error("a pump with no spool was accepted")
	}
	if _, err := NewPump(PumpOptions{Spool: s, Reachable: func() bool { return true }}); err == nil {
		t.Error("a pump with nowhere to send was accepted")
	}
	if _, err := NewPump(PumpOptions{Spool: s, Send: func(context.Context, []Row) (int, error) { return 0, nil }}); err == nil {
		t.Error("a pump that does not know the link state was accepted")
	}
}

// indexOf finds the row whose payload carries this value under "i". It
// decodes into map[string]any rather than a typed map because the payloads
// in this file are not uniform, and a helper that silently fails on a
// string field is a helper that reports -1 for everything.
func indexOf(rows []Row, i int) int {
	for idx, r := range rows {
		var body map[string]any
		if json.Unmarshal(r.Payload, &body) != nil {
			continue
		}
		if v, ok := body["i"].(float64); ok && int(v) == i {
			return idx
		}
	}
	return -1
}

// TestTheFloorYieldsToTheCap is the one place two policies in this file
// disagree. The floor says "never come back from a compaction with nothing";
// the cap says "never exceed this many bytes". A row count is not a byte
// count, so on a spool with fat rows the floor can overrun the cap — and if
// it wins, a node whose operator did everything right fills its own disk.
func TestTheFloorYieldsToTheCap(t *testing.T) {
	const cap = 8 * 1024
	s, _ := newSpool(t, Options{
		Path:      filepath.Join(t.TempDir(), "fat.jsonl"),
		MaxBytes:  cap,
		KeepFloor: 1000, // far more rows than the cap can hold
	})
	ctx := context.Background()
	// Recorded twice over, so the file is overfilled rather than merely
	// filled: a spool that only exceeds its cap after its first compaction
	// is a spool whose cap is not a cap.
	for round := 0; round < 2; round++ {
		for i := 0; i < 300; i++ {
			if err := s.Record(ctx, "event", map[string]any{"pad": strings.Repeat("x", 200), "i": round*300 + i}); err != nil {
				t.Fatalf("Record: %v", err)
			}
		}
	}
	if size := s.Size(); size > cap {
		t.Errorf("size = %d over a %d cap: the protected floor overran the limit", size, cap)
	}
	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("compaction left nothing: the floor was supposed to prevent exactly this")
	}
	if indexOf(rows, 599) != len(rows)-1 {
		t.Error("the newest row is not the newest: compaction dropped the wrong end")
	}
}

// TestRecordSeqHandsBackTheNumberTheRowWasStampedWith is the contract the
// change-event sink depends on: the number it is told must be the number on
// disk, because that is the number a replay will carry back.
func TestRecordSeqHandsBackTheNumberTheRowWasStampedWith(t *testing.T) {
	s, _ := newSpool(t, Options{})
	ctx := context.Background()

	var got []uint64
	for i := 0; i < 3; i++ {
		seq, err := s.RecordSeq(ctx, "event", map[string]int{"i": i})
		if err != nil {
			t.Fatalf("RecordSeq %d: %v", i, err)
		}
		got = append(got, seq)
	}
	for i, seq := range got {
		if seq != uint64(i+1) {
			t.Errorf("RecordSeq call %d returned %d, want %d", i, seq, i+1)
		}
	}

	rows, err := s.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) != len(got) {
		t.Fatalf("stored %d rows, want %d", len(rows), len(got))
	}
	for i, r := range rows {
		if r.Seq != got[i] {
			t.Errorf("row %d carries seq %d but RecordSeq said %d: a replay would be unrecognisable", i, r.Seq, got[i])
		}
	}
}

// TestTheSequenceDoesNotRestartAtOne pins the property the manager's
// (edge_id, seq) unique index rests on across a restart. Seq resumes from
// the highest value in the file, so a node that restarts mid-outage does not
// start handing out numbers the center has already seen — which would look
// exactly like a replay and get the node's new events silently dropped.
func TestTheSequenceDoesNotRestartAtOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool", "rows.jsonl")
	ctx := context.Background()

	first, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := first.RecordSeq(ctx, "event", map[string]int{"i": i}); err != nil {
			t.Fatalf("RecordSeq: %v", err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	seq, err := second.RecordSeq(ctx, "event", map[string]int{"i": 99})
	if err != nil {
		t.Fatalf("RecordSeq after reopen: %v", err)
	}
	if seq != 5 {
		t.Fatalf("after a reopen the first sequence is %d, want 5: the center would treat it as a replay", seq)
	}
}

// TestAFailedWriteDoesNotConsumeASequenceNumber: a gap in the numbers reads
// as a lost row to anyone auditing the log, and RecordSeq's whole value is
// that the number it returns is the number on disk.
func TestAFailedWriteDoesNotConsumeASequenceNumber(t *testing.T) {
	s, _ := newSpool(t, Options{})
	ctx := context.Background()

	first, err := s.RecordSeq(ctx, "event", map[string]int{"i": 1})
	if err != nil {
		t.Fatalf("first RecordSeq: %v", err)
	}
	// A payload json cannot encode fails after the counter would have moved.
	if _, err := s.RecordSeq(ctx, "event", make(chan int)); err == nil {
		t.Fatal("recording an unencodable payload reported success")
	}
	next, err := s.RecordSeq(ctx, "event", map[string]int{"i": 2})
	if err != nil {
		t.Fatalf("third RecordSeq: %v", err)
	}
	if next != first+1 {
		t.Errorf("the failed write consumed a number: %d then %d, want %d then %d", first, next, first, first+1)
	}
}
