// Package spool is the node's durable buffer: a bounded, owner-only,
// line-oriented file that accepts rows while nothing can be sent, and hands
// them back in order when something can.
//
// It exists because a node has three unrelated things that all discovered
// the same lesson independently, and the lesson is that an in-memory buffer
// is a buffer that loses everything at the moment losing things starts:
//
//   - autonomy writes an audit row before a self-heal runs, and a tunnel
//     that is down is exactly the case the row exists for
//   - the change watcher batches journald/dockerd events to keep a flapping
//     network from turning into a spin loop
//   - telemetry is sampled on a timer, and the timer does not care whether
//     the center answered
//
// Three implementations of "append a line, cap it, replay it in order" means
// three sets of answers to the same hard questions: what happens to a
// half-written line, what happens when a file grows, and which end gets
// dropped. They will not stay the same, and the one that is wrong will be
// whichever nobody was looking at. So this is the one implementation, and
// the difference between its three users is **policy**, declared per class
// and visible in one table rather than spread across three code paths.
//
// The properties are the ones the plan names for a spool — 断连写入、恢复回放、
// 容量上限丢弃策略、回放限流 — plus the two that came out of writing the first
// one for a node's audit trail:
//
//   - The file is 0600 and the directory 0700, and a file that is *already*
//     group- or world-readable is refused rather than quietly widened. A
//     buffer that was already too open is evidence that something else on
//     the host is too open, and the first thing this package should not do
//     is paper over it. A symlink at the path is refused for the same
//     reason: the path comes from configuration, and a node that writes
//     through a link somebody else planted has chosen its storage by
//     accident.
//   - A row that does not parse is skipped, not fatal. It is almost always
//     the tail of a write a power cut interrupted, and a spool that cannot
//     be read because of its own last row is a spool that will never be
//     replayed.
//
// It is deliberately not a general-purpose queue. There is one writer (the
// thing that produced the rows) and one reader (the replay pump on
// reconnect), and a design that permitted more would be a design with a
// locking protocol nobody had a reason to get right.
package spool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultMaxBytes is how large a spool may grow before it drops
	// something. It is a guard against a run loop, not a policy: a node
	// offline for a week at any rate it actually reaches stays well
	// inside it.
	DefaultMaxBytes = 8 << 20
	// DefaultSweepInterval is how often a spool with a class that has a
	// horizon re-reads itself looking for rows that aged out. A minute is
	// short enough that a row is never more than a minute past its
	// horizon, and long enough that the sweep is not a per-append cost.
	DefaultSweepInterval = time.Minute

	// DefaultKeepFloor is how many of the most valuable rows a compaction
	// always keeps.
	//
	// A node that has been alone long enough to fill a spool is the node
	// whose most recent rows matter most, and a compaction that kept
	// nothing would delete the evidence of the outage in progress in
	// order to save space. A hundred rows is a few tens of kilobytes.
	DefaultKeepFloor = 100

	dirMode  = 0o700
	fileMode = 0o600
)

// ErrClosed is returned by an operation on a closed spool.
var ErrClosed = errors.New("spool: the file is closed")

// Row is one stored line, with its envelope decoded.
//
// Seq is the whole reason the envelope exists beyond the class tag: a
// replay delivers rows at least once, and the only way the center can tell
// a redelivery from a new row is a number it has seen before.
type Row struct {
	// Class is the policy bucket this row was written under.
	Class Class
	// At is when the row was written, not when the thing it describes
	// happened. It is what age-based dropping measures, and it is
	// deliberately the write time: a policy that drops by event time
	// cannot be enforced by a buffer that only learns about the event
	// when it is already late.
	At time.Time
	// Seq is monotonic per spool and survives restarts.
	Seq uint64
	// Payload is the caller's JSON, kept raw so that a spool written by
	// one version of a caller is still readable by another.
	Payload json.RawMessage
}

// envelope is the on-disk shape.
//
// The keys are one character because this file is written on a timer by
// something with a small disk and a large opinion about what belongs on it,
// and telemetry payloads are the reason this package exists.
type envelope struct {
	Class   Class           `json:"c"`
	At      time.Time       `json:"at"`
	Seq     uint64          `json:"s"`
	Payload json.RawMessage `json:"p"`
}

// Options configures a Spool.
type Options struct {
	// Path is where the file lives. Required.
	Path string
	// MaxBytes caps the file. Default DefaultMaxBytes.
	MaxBytes int64
	// KeepFloor is how many of the most valuable rows a compaction always
	// keeps. Zero means DefaultKeepFloor; a negative number means no floor
	// at all, which is only sensible for a spool whose cap is small enough
	// that any floor would overrun it.
	KeepFloor int
	// Classes is the graded-drop policy. A class that is not named here
	// gets the default policy, which is "valuable, and never ages out" —
	// the conservative answer, because an unclassified row dropping by
	// default would be a row nobody chose to make disposable.
	Classes map[Class]Policy
	// Now defaults to time.Now. Tests inject a clock.
	Now func() time.Time
	// Label names this spool in error messages. Optional, and it exists
	// because a node has more than one of these and "the spool" is not
	// something anybody can act on at 04:00.
	Label string
	// SweepInterval is how often the file is re-examined for rows that
	// have aged out. Default DefaultSweepInterval.
	//
	// Aging is not a capacity mechanism, so it cannot wait for capacity
	// pressure to arrive: a metric an hour old is wrong whether or not
	// the disk is full, and a spool that only ages rows when it is full
	// would deliver every stale row it holds the moment it filled up.
	// It is also not free — it reads the file — so it runs on a timer
	// rather than on every append, and it rewrites nothing unless at
	// least one row actually aged out.
	SweepInterval time.Duration
}

// Spool is a bounded, classed, owner-only append-only file.
type Spool struct {
	path      string
	label     string
	maxBytes  int64
	keepFloor int
	classes   map[Class]Policy
	now       func() time.Time
	sweep     time.Duration
	// lastSweep is when the file was last examined for age. Zero means
	// never, so the first append sweeps.
	lastSweep time.Time
	// ages is set once a row of a class with a horizon lands, and cleared
	// never. A spool that has only ever held rows with no horizon should
	// not pay for a sweep it cannot benefit from.
	ages bool

	mu    sync.Mutex
	f     *os.File
	size  int64
	seq   uint64
	stats Stats
}

// Open opens or creates the spool described by opts.
func Open(opts Options) (*Spool, error) {
	if opts.Path == "" {
		return nil, errors.New("spool: a path is required")
	}
	label := opts.Label
	if label == "" {
		label = "the spool at " + opts.Path
	}
	s := &Spool{
		path:      opts.Path,
		label:     label,
		maxBytes:  opts.MaxBytes,
		keepFloor: opts.KeepFloor,
		classes:   opts.Classes,
		now:       opts.Now,
		sweep:     opts.SweepInterval,
	}
	if s.maxBytes <= 0 {
		s.maxBytes = DefaultMaxBytes
	}
	// Zero is the default rather than "none", because the floor is the
	// property that keeps a compaction from returning an empty file and a
	// caller who did not think about it should get it. A caller who really
	// wants no floor says so with a negative number, which is a decision
	// rather than an omission.
	if s.keepFloor == 0 {
		s.keepFloor = DefaultKeepFloor
	} else if s.keepFloor < 0 {
		s.keepFloor = 0
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.sweep <= 0 {
		s.sweep = DefaultSweepInterval
	}
	s.stats.WrittenByClass = map[Class]uint64{}
	s.stats.DroppedByClass = map[Class]uint64{}
	if err := s.open(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Spool) open() error {
	path := s.path
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("spool: %s is a symlink, and this host will not write through one", path)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return fmt.Errorf("spool: %s is mode %04o; owner-only is required, and this host will not widen a file it did not create", path, perm)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("spool: stat %s: %w", path, err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return fmt.Errorf("spool: create the directory for %s: %w", path, err)
		}
	}
	// O_RDWR because a replay reads the file this same handle owns, and
	// O_APPEND because the kernel — not whatever this process last read —
	// should choose the offset, so a row lands whole even if a second
	// writer ever appears.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, fileMode)
	if err != nil {
		return fmt.Errorf("spool: open %s: %w", path, err)
	}
	s.f = f
	info, err := f.Stat()
	if err != nil {
		f.Close()
		s.f = nil
		return fmt.Errorf("spool: stat %s: %w", path, err)
	}
	s.size = info.Size()
	if err := s.trimPartialTail(f, info.Size()); err != nil {
		f.Close()
		s.f = nil
		return err
	}
	// The sequence has to survive a restart, or every reconnect would start
	// numbering from one and the center's dedupe would treat a redelivery
	// of last week's rows as a week of new ones.
	rows, _, err := s.readLocked()
	if err != nil {
		f.Close()
		s.f = nil
		return err
	}
	for _, r := range rows {
		if r.Seq > s.seq {
			s.seq = r.Seq
		}
	}
	return nil
}

// trimPartialTail removes the tail of a write that was cut off mid-line.
//
// It runs once, at open, and it is here because of what readLocked already
// does. A half-written line fails to parse, so readLocked skips it and the
// rows around it are still readable — that is correct, and it is also not
// enough. The file is opened O_APPEND, so the next row lands *after* the
// torn bytes, producing one line that is "half of the last row plus all of
// the next one". The tear was already unreadable; now a row written after
// the restart is unreadable too, and it was never bad on its own.
//
// Truncating to the last newline discards exactly the bytes that could never
// parse and nothing else. The alternative — leaving them — is a spool that
// loses one good row for every power cut, on the one class of data where
// that is the failure that matters.
func (s *Spool) trimPartialTail(f *os.File, size int64) error {
	if size == 0 {
		return nil
	}
	if _, err := f.Seek(-1, io.SeekEnd); err != nil {
		return fmt.Errorf("spool: seek to the end of %s: %w", s.path, err)
	}
	var last [1]byte
	if _, err := io.ReadFull(f, last[:]); err != nil {
		return fmt.Errorf("spool: read the end of %s: %w", s.path, err)
	}
	if last[0] == '\n' {
		return nil
	}
	// Walk back in blocks rather than a byte at a time; a spool left with a
	// long torn tail would otherwise be reopened slowly, on every restart,
	// for the rest of its life.
	const block = 64 << 10
	buf := make([]byte, block)
	at := size
	for at > 0 {
		n := int64(block)
		if at < n {
			n = at
		}
		at -= n
		if _, err := f.Seek(at, io.SeekStart); err != nil {
			return fmt.Errorf("spool: rewind %s: %w", s.path, err)
		}
		if _, err := io.ReadFull(f, buf[:n]); err != nil {
			return fmt.Errorf("spool: read %s: %w", s.path, err)
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			cut := at + int64(i) + 1
			if err := f.Truncate(cut); err != nil {
				return fmt.Errorf("spool: truncate %s: %w", s.path, err)
			}
			s.size = cut
			return nil
		}
	}
	// No newline at all: the whole file is one unterminated line, so there
	// is nothing in it worth keeping.
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("spool: truncate %s: %w", s.path, err)
	}
	s.size = 0
	return nil
}

// Path is where the file lives, for a health page that wants to show it.
func (s *Spool) Path() string { return s.path }

// Record appends one row under a class.
//
// A full disk is not an error the caller has to handle: a node that has run
// out of room has bigger problems than a missing row, and refusing work
// because of it converts an availability problem into a correctness one.
// The row is still written when there is room for it, and the cap keeps
// "when there is room" true most of the time.
//
// Callers that need to name this row to somebody else want RecordSeq.
func (s *Spool) Record(ctx context.Context, class Class, payload any) error {
	_, err := s.RecordSeq(ctx, class, payload)
	return err
}

// RecordSeq appends one row and reports the sequence number the envelope was
// stamped with.
//
// The number matters to exactly one caller — the change-event sink, which
// sends the same row again after a lost ack and needs the center to be able
// to recognise it. That number is only useful if the sender knows it at the
// moment of the write, which is why this is a second entry point rather than
// a changed signature on Record: the other twenty-odd callers all write
// `if err := s.Record(...); err != nil`, and making them all unpack a
// sequence they do not use is how a useful return value becomes one nobody
// reads.
//
// The sequence is rolled back on a failed write, so a gap in the numbers
// means a row really was lost rather than an attempt that failed.
func (s *Spool) RecordSeq(_ context.Context, class Class, payload any) (uint64, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("spool: encode a %s row: %w", class, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return 0, ErrClosed
	}
	s.seq++
	line, err := json.Marshal(envelope{Class: class, At: s.now().UTC(), Seq: s.seq, Payload: body})
	if err != nil {
		// Only reachable if a payload contains something json.Marshal
		// accepted but cannot re-marshal inside a struct, which is not a
		// thing. The sequence is rolled back anyway so a failed record
		// does not leave a gap that looks like a lost row.
		s.seq--
		return 0, fmt.Errorf("spool: frame a %s row: %w", class, err)
	}
	line = append(line, '\n')
	if _, err := s.f.Write(line); err != nil {
		s.seq--
		return 0, fmt.Errorf("spool: append a %s row to %s: %w", class, s.path, err)
	}
	stamped := s.seq
	s.size += int64(len(line))
	s.stats.Written++
	s.stats.WrittenByClass[class]++
	if s.policy(class).MaxAge > 0 {
		s.ages = true
	}
	now := s.now()
	switch {
	case s.size > s.maxBytes:
		// Best effort, and deliberately so: a spool that cannot compact
		// is a spool that is full, and refusing the write would make a
		// disk problem look like a policy decision.
		_ = s.compactLocked()
	case s.ages && now.Sub(s.lastSweep) >= s.sweep:
		_ = s.compactLocked()
	}
	// stamped, not s.seq: compaction above can rewrite the file, and the
	// number this row was *written* with is the one a replay will carry
	// back, whatever happened to its neighbours afterwards.
	return stamped, nil
}

// Peek returns up to n of the oldest rows without removing them.
//
// It is what a pump reads: a drain that delivered rows and then failed to
// ack them must be able to look again, and a pump that read the file by
// removing from it could not be retried.
func (s *Spool) Peek(n int) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, _, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	if n > 0 && n < len(rows) {
		return rows[:n:n], nil
	}
	return rows, nil
}

// Ack drops the first n rows, which is how a reader says they arrived.
//
// It rewrites the file rather than truncating in place, because truncating
// the head of a file this process is still appending to would leave a hole
// where a row used to be. The replacement is written beside the spool and
// renamed over it, so a crash mid-ack leaves the whole old file rather than
// half of one.
//
// The read and the rewrite happen under one lock. Doing them under two is a
// lost-update bug with a long fuse: a row appended between the two is not
// in the list being kept, and it vanishes — which for an audit row is
// exactly the failure this package exists to prevent.
func (s *Spool) Ack(n int) error {
	if n <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, _, err := s.readLocked()
	if err != nil {
		return err
	}
	if n > len(rows) {
		n = len(rows)
	}
	if n == 0 {
		return nil
	}
	if err := s.rewriteLocked(rows[n:]); err != nil {
		return err
	}
	s.stats.Acked += uint64(n)
	return nil
}

// Replay hands every stored row to fn, oldest first, and reports how many
// were delivered.
func (s *Spool) Replay(fn func(Row) error) (int, error) {
	rows, err := s.Peek(0)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, row := range rows {
		if err := fn(row); err != nil {
			return delivered, err
		}
		delivered++
	}
	return delivered, nil
}

// Len reports how many rows the spool holds.
func (s *Spool) Len() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, _, err := s.readLocked()
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// Size reports the file's current length in bytes.
func (s *Spool) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// Snapshot returns the counters.
func (s *Spool) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.stats
	out.WrittenByClass = map[Class]uint64{}
	for k, v := range s.stats.WrittenByClass {
		out.WrittenByClass[k] = v
	}
	out.DroppedByClass = map[Class]uint64{}
	for k, v := range s.stats.DroppedByClass {
		out.DroppedByClass[k] = v
	}
	return out
}

// Close releases the file. Rows already written stay written; the file is
// not truncated, because a close is not a decision to discard.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// readLocked parses the whole spool, for a caller that already holds the
// lock. The split is not tidiness: Record compacts while holding the lock,
// so a read that took it again would deadlock the one path that runs during
// an outage.
func (s *Spool) readLocked() ([]Row, []int64, error) {
	if s.f == nil {
		return nil, nil, ErrClosed
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("spool: rewind %s: %w", s.path, err)
	}
	var rows []Row
	var sizes []int64
	sc := bufio.NewScanner(s.f)
	// A telemetry row is allowed to be large, and a scanner's 64 KiB
	// default would silently truncate it into a parse failure — which is
	// indistinguishable from a half-written line, i.e. a silently dropped
	// row. So the buffer is raised rather than left to surprise us.
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			// An interrupted write, or a file edited by hand. Either
			// way the rows around it are still worth having.
			continue
		}
		rows = append(rows, Row{Class: env.Class, At: env.At, Seq: env.Seq, Payload: env.Payload})
		sizes = append(sizes, int64(len(line))+1)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("spool: read %s: %w", s.path, err)
	}
	if _, err := s.f.Seek(0, io.SeekEnd); err != nil {
		return nil, nil, fmt.Errorf("spool: rewind %s: %w", s.path, err)
	}
	return rows, sizes, nil
}

// compactLocked applies the drop policy and rewrites what is left.
func (s *Spool) compactLocked() error {
	rows, sizes, err := s.readLocked()
	if err != nil {
		return err
	}
	s.lastSweep = s.now()
	drop, stats := s.planDrop(rows, sizes, s.size, s.lastSweep)
	if drop == nil {
		return nil
	}
	kept := make([]Row, 0, len(rows))
	for i, row := range rows {
		if !drop[i] {
			kept = append(kept, row)
		}
	}
	if err := s.rewriteLocked(kept); err != nil {
		return err
	}
	s.stats.merge(stats)
	return nil
}

// planDrop decides which rows go, and returns nil when nothing should.
//
// It has two stages and they answer different questions. Aging asks "is
// this row still worth having at any volume", and it is not a capacity
// mechanism — a class with a horizon drops past it whether or not the disk
// is full, because a six-hour-old metric is not a slow metric, it is a
// wrong one. Capacity then asks "given the disk is full, which rows are the
// ones to lose", and the answer is graded: the class the caller said was
// most disposable goes first, and within a class the oldest goes first.
func (s *Spool) planDrop(rows []Row, sizes []int64, total int64, now time.Time) ([]bool, Stats) {
	if len(rows) == 0 {
		return nil, Stats{}
	}
	drop := make([]bool, len(rows))
	var stats Stats

	// Stage 1: age. A row past its class's horizon is dropped whatever the
	// disk says, and the class with no horizon (zero) never ages out.
	for i, row := range rows {
		horizon := s.policy(row.Class).MaxAge
		if horizon > 0 && now.Sub(row.At) > horizon {
			drop[i] = true
			stats.drop(row, true)
		}
	}

	// Half the cap rather than all of it, so a spool sitting exactly at its
	// limit does not compact on every single append and turn a steady
	// stream into a steady stream of full rewrites.
	target := s.maxBytes / 2
	if total <= target {
		if stats.any() {
			return drop, stats
		}
		return nil, Stats{}
	}

	protected := s.protectedTail(rows, sizes)
	freed := int64(0)
	order := make([]int, 0, len(rows))
	for i := range rows {
		if !drop[i] && !protected[i] {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		ra, rb := rows[order[a]], rows[order[b]]
		pa, pb := s.policy(ra.Class), s.policy(rb.Class)
		if pa.DropPriority != pb.DropPriority {
			return pa.DropPriority > pb.DropPriority
		}
		return ra.Seq < rb.Seq
	})
	for _, i := range order {
		if total-freed <= target {
			break
		}
		drop[i] = true
		freed += sizes[i]
		stats.drop(rows[i], false)
	}
	if !stats.any() {
		return nil, Stats{}
	}
	return drop, stats
}

// protectedTail marks the rows a compaction may not take.
//
// "May not" is scoped to the most valuable class present rather than to the
// file as a whole, and the difference matters on a node holding three
// classes at once. A spool full of traces should be able to give up all of
// them; what it may not do is quietly delete the recent metric the operator
// is about to ask about because the traces were bigger.
//
// The floor is also bounded by the cap, which is the one place in this file
// where two policies disagree and the cap wins. A row count is not a byte
// count: a hundred audit rows are thirty-odd kilobytes today and would be a
// different number tomorrow, so a floor that is allowed to overrun MaxBytes
// is a disk that fills up on a node whose operator did everything right.
// Protecting "as many of the newest as fit, up to the floor" keeps the
// intent — never come back from a compaction with nothing — without turning
// the cap into a suggestion.
func (s *Spool) protectedTail(rows []Row, sizes []int64) []bool {
	protected := make([]bool, len(rows))
	if s.keepFloor <= 0 {
		return protected
	}
	best := Class("")
	found := false
	for _, row := range rows {
		p := s.policy(row.Class)
		switch {
		case !found:
			best, found = row.Class, true
		case p.DropPriority < s.policy(best).DropPriority:
			best = row.Class
		case p.DropPriority == s.policy(best).DropPriority && row.Class < best:
			// A tie is broken by name so that two runs of the same spool
			// protect the same rows; a compaction that alternated its
			// protected class would make the surviving set depend on map
			// iteration order.
			best = row.Class
		}
	}
	kept := 0
	var used int64
	for i := len(rows) - 1; i >= 0 && kept < s.keepFloor; i-- {
		if rows[i].Class != best {
			continue
		}
		if used+sizes[i] > s.maxBytes {
			// One more row would put the file over its own cap. Stop
			// here: the floor yields to the cap rather than the other
			// way round.
			break
		}
		protected[i] = true
		used += sizes[i]
		kept++
	}
	return protected
}

// rewriteLocked replaces the file's contents, atomically.
func (s *Spool) rewriteLocked(rows []Row) error {
	tmp := s.path + ".rewrite"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("spool: rewrite %s: %w", s.path, err)
	}
	w := bufio.NewWriter(f)
	var size int64
	for _, row := range rows {
		encoded, err := json.Marshal(envelope{Class: row.Class, At: row.At, Seq: row.Seq, Payload: row.Payload})
		if err != nil {
			f.Close()
			return fmt.Errorf("spool: encode a %s row: %w", row.Class, err)
		}
		encoded = append(encoded, '\n')
		n, err := w.Write(encoded)
		if err != nil {
			f.Close()
			return fmt.Errorf("spool: rewrite %s: %w", s.path, err)
		}
		size += int64(n)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return fmt.Errorf("spool: rewrite %s: %w", s.path, err)
	}
	// The sync is what makes the rename below a durability claim rather
	// than a tidiness one: without it a power cut can leave the old name
	// pointing at an empty file and the rows are gone.
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("spool: sync %s: %w", s.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("spool: rewrite %s: %w", s.path, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("spool: replace %s: %w", s.path, err)
	}
	// The old descriptor points at the unlinked original, so it is reopened
	// rather than reused: an append through it would write rows nobody can
	// read back.
	opened, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, fileMode)
	if err != nil {
		return fmt.Errorf("spool: reopen %s: %w", s.path, err)
	}
	if s.f != nil {
		_ = s.f.Close()
	}
	s.f = opened
	s.size = size
	return nil
}
