package auditlog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The tests here are about what the node keeps when things go wrong, because
// that is the only thing a ledger on a node is for. A row that reached the
// chain is unremarkable; a row that was dropped because the disk was full,
// or because a drain half-succeeded, is the failure this whole package
// exists to be measured by.

func openSink(t *testing.T) *Sink {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ledger.jsonl"), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func entry(action ports.AuditAction, target string) ports.AuditEntry {
	return ports.AuditEntry{
		At:      time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		Actor:   "operator@example.invalid",
		Action:  action,
		Target:  target,
		Outcome: "allowed",
		Class:   "read",
	}
}

func TestARowSurvivesTheRoundTripToDisk(t *testing.T) {
	s := openSink(t)
	in := entry(ports.ActionToolBlocked, "host_bash")
	in.Detail = []byte(`{"reason":"not on the allow list"}`)

	if err := s.Record(context.Background(), in); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := s.Peek(10)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("peeked %d rows, want 1", len(got))
	}
	if got[0].Action != in.Action || got[0].Target != in.Target {
		t.Errorf("row came back as %+v, want %+v", got[0], in)
	}
	if !got[0].At.Equal(in.At) {
		t.Errorf("At = %v, want %v", got[0].At, in.At)
	}
	if string(got[0].Detail) != string(in.Detail) {
		t.Errorf("Detail = %s, want %s", got[0].Detail, in.Detail)
	}
}

// The ledger is a list of things an AI did to a host. It is written 0600 and
// the assertion is here because the permission is the only thing standing
// between this file and every other account on the machine.
func TestTheLedgerIsNotReadableByAnyoneElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	s, err := Open(path, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Record(context.Background(), entry(ports.ActionToolCall, "host_bash")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("ledger mode is %o; a node's tool calls are not a world-readable file", perm)
	}
}

func TestOpenRefusesAnEmptyPath(t *testing.T) {
	if _, err := Open("  ", 0); err == nil {
		t.Error("Open accepted an empty path")
	}
}

// Verify is the one method of the port a node cannot honour, and the reason
// matters: a nil here would be read as "verified" by any caller that checks
// the error and logs the outcome.
func TestVerifySaysTheChainIsNotHereRatherThanClaimingItIsIntact(t *testing.T) {
	s := openSink(t)
	err := s.Verify(context.Background())
	if !errors.Is(err, ErrNoChainHere) {
		t.Fatalf("Verify = %v, want ErrNoChainHere", err)
	}
	if !strings.Contains(err.Error(), "control plane") {
		t.Errorf("the refusal does not say where verification lives: %v", err)
	}
}

func TestAnUnopenedSinkSaysSoRatherThanPanicking(t *testing.T) {
	var s *Sink
	if err := s.Record(context.Background(), entry(ports.ActionToolCall, "x")); err == nil {
		t.Error("Record on a nil Sink returned nil")
	}
	if _, err := s.Peek(1); err == nil {
		t.Error("Peek on a nil Sink returned nil")
	}
}

func TestThePumpNamesTheFieldThatIsMissing(t *testing.T) {
	s := openSink(t)
	good := func(o PumpOptions) PumpOptions {
		o.Sink = s
		o.Sender = SenderFunc(func(context.Context, []ports.AuditEntry) error { return nil })
		o.Reachable = func() bool { return true }
		return o
	}
	for _, tc := range []struct {
		name string
		want string
		opts PumpOptions
	}{
		{"no sink", "Sink", PumpOptions{}},
		{"no sender", "Sender", PumpOptions{Sink: s}},
		{"no reachability", "Reachable", PumpOptions{Sink: s,
			Sender: SenderFunc(func(context.Context, []ports.AuditEntry) error { return nil })}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPump(tc.opts)
			if err == nil {
				t.Fatalf("NewPump accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Errorf("error is %T, want *ConfigError", err)
			}
		})
	}
	if _, err := NewPump(good(PumpOptions{})); err != nil {
		t.Fatalf("NewPump on a complete wiring: %v", err)
	}
}

func TestADrainHandsTheRowsOverAndForgetsThem(t *testing.T) {
	s := openSink(t)
	for i := 0; i < 3; i++ {
		if err := s.Record(context.Background(), entry(ports.ActionToolCall, "t")); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	var sent []ports.AuditEntry
	p, err := NewPump(PumpOptions{
		Sink:      s,
		Reachable: func() bool { return true },
		Sender: SenderFunc(func(_ context.Context, rows []ports.AuditEntry) error {
			sent = append(sent, rows...)
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	n, err := p.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if n != 3 || len(sent) != 3 {
		t.Fatalf("drained %d rows and sent %d, want 3 and 3", n, len(sent))
	}
	if pending, err := p.Pending(); err != nil || pending != 0 {
		t.Errorf("after a good drain the node still holds %d rows (err %v)", pending, err)
	}
}

// The one that must not be got wrong. The chain is an ordered append-only
// ledger with no dedupe key: if a drain reports progress it did not make,
// the next retry writes the same rows again, and a reader cannot tell which
// of the two copies of a tool call actually happened.
func TestAFailedDrainKeepsEveryRowOnTheNode(t *testing.T) {
	s := openSink(t)
	for i := 0; i < 3; i++ {
		if err := s.Record(context.Background(), entry(ports.ActionToolCall, "t")); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	boom := errors.New("the tunnel is down")
	p, err := NewPump(PumpOptions{
		Sink:      s,
		Reachable: func() bool { return true },
		Sender:    SenderFunc(func(context.Context, []ports.AuditEntry) error { return boom }),
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	if n, err := p.DrainOnce(context.Background()); err == nil || n != 0 {
		t.Fatalf("DrainOnce = (%d, %v), want (0, %v)", n, err, boom)
	}
	if pending, err := p.Pending(); err != nil || pending != 3 {
		t.Errorf("after a failed drain the node holds %d rows, want 3", pending)
	}
}

// A node that is not connected is not misbehaving, and its ledger filling up
// is exactly what the cap is for. The pump must not try.
func TestNothingIsSentWhileTheLinkIsDown(t *testing.T) {
	s := openSink(t)
	if err := s.Record(context.Background(), entry(ports.ActionToolCall, "t")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	called := false
	p, err := NewPump(PumpOptions{
		Sink:      s,
		Reachable: func() bool { return false },
		Sender: SenderFunc(func(context.Context, []ports.AuditEntry) error {
			called = true
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	n, err := p.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce on a down link: %v", err)
	}
	if n != 0 || called {
		t.Fatalf("a down link still drained: n=%d called=%v", n, called)
	}
	if pending, _ := p.Pending(); pending != 1 {
		t.Errorf("the row was lost while the link was down: %d pending", pending)
	}
}

func TestReplaySkipsALineAPowerCutLeftHalfWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	s, err := Open(path, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Record(context.Background(), entry(ports.ActionToolCall, "a")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The tail of an interrupted write.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := f.WriteString("{\"at\":\"2026"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = f.Close()

	s2, err := Open(path, 0)
	if err != nil {
		t.Fatalf("reopen ledger: %v", err)
	}
	defer func() { _ = s2.Close() }()
	// A drain that refuses to read the file because of its own last row is a
	// drain that never runs again. The good row written before the tear is
	// still readable, and the tear is not mistaken for a row.
	rows, err := s2.Peek(10)
	if err != nil {
		t.Fatalf("Peek refused a file with one half-written line: %v", err)
	}
	if len(rows) != 1 || rows[0].Target != "a" {
		t.Fatalf("Peek returned %d rows (%+v), want the one good row", len(rows), rows)
	}

	// And a good row appended after the tear stays reachable, because a bad
	// line in the middle must not become a hole in the delivery. That is the
	// whole reason a tear is skipped rather than propagated: the chain is
	// ordered, and a refusal would hand the center a permanent gap.
	if err := s2.Record(context.Background(), entry(ports.ActionToolCall, "b")); err != nil {
		t.Fatalf("Record after a bad line: %v", err)
	}
	rows, err = s2.Peek(10)
	if err != nil {
		t.Fatalf("Peek after a good row was appended: %v", err)
	}
	if len(rows) != 2 || rows[1].Target != "b" {
		t.Fatalf("Peek returned %d rows (%+v), want two, the second b", len(rows), rows)
	}

	// Replay, which is what a pump actually calls, agrees with Peek.
	var targets []string
	n, err := s2.Replay(func(e ports.AuditEntry) error {
		targets = append(targets, e.Target)
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n != 2 || len(targets) != 2 || targets[0] != "a" || targets[1] != "b" {
		t.Fatalf("Replay delivered %d rows %v, want [a b]", n, targets)
	}
}
