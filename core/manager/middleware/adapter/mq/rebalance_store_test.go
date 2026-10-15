package mq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The property the store exists for: a control plane that restarts mid-incident
// can still answer what the group did. That is a round trip through a NEW
// history object over the same directory, which is what a restart is.

func TestTheHistorySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	first, err := NewRebalanceHistoryStore(dir, 64, time.Hour)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	first.Record(state(member("m1", "orders", 0, 1)), now)
	first.Record(state(member("m1", "orders", 0), member("m2", "orders", 1)), now.Add(time.Minute))

	// A second process reads the same directory with a fresh object.
	second, err := NewRebalanceHistoryStore(dir, 64, time.Hour)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	events, ok := second.Query("orders", 0, 0)
	if !ok {
		t.Fatal("the restarted history does not know the group at all")
	}
	if len(events) != 2 {
		t.Fatalf("%d events after a restart, want 2", len(events))
	}
	if strings.Join(events[0].Joined, ",") != "m2" {
		t.Errorf("newest event joined %v, want [m2] — the order must survive too", events[0].Joined)
	}

	// And the state that produced it is still known, so the next sample
	// diffs against what the previous process saw rather than re-reporting
	// the whole membership as new.
	third, err := NewRebalanceHistoryStore(dir, 64, time.Hour)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if _, changed := third.Record(state(member("m1", "orders", 0), member("m2", "orders", 1)), now.Add(2*time.Minute)); changed {
		t.Error("an unchanged assignment was reported as a rebalance after a restart; the last state was not restored")
	}
}

// An unreadable old window and an empty one are different answers, and only
// one of them is a lie.
func TestAnUnreadableWindowIsRefusedRatherThanEmptied(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orders.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	h, err := NewRebalanceHistoryStore(dir, 64, time.Hour)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, ok := h.Query("orders", 0, 0); ok {
		t.Error("an unreadable history was reported as an empty one")
	}
	if h.LoadError("orders") == nil {
		t.Error("no load error recorded, so the refusal cannot name its cause")
	}
	// The damaged file is left where it is: deleting somebody's evidence to
	// make a query succeed is the wrong direction for a diagnostics tool.
	if _, err := os.Stat(filepath.Join(dir, "orders.json")); err != nil {
		t.Errorf("the unreadable file was removed: %v", err)
	}
}

// A group name comes from the broker, so it must not be able to name a file
// outside the directory the operator chose.
func TestAGroupNameCannotEscapeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	store, err := newFileHistory(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	path := store.path("../../etc/passwd")
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(dir)+string(filepath.Separator)) {
		t.Fatalf("group name escaped the directory: %s", path)
	}
}

// A directory an operator named but cannot use must fail loudly at
// construction, not fall back to memory.
func TestAnUnusableHistoryDirectoryIsAnError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := NewRebalanceHistoryStore(filepath.Join(blocker, "rebalance"), 64, time.Hour); err == nil {
		t.Error("a history directory that cannot be created was accepted; the connection would silently keep history in memory")
	}
}

func TestTheHistoryDirectoryIsOptional(t *testing.T) {
	t.Setenv(HistoryDirEnv, "")
	h, err := newHistory()
	if err != nil {
		t.Fatalf("newHistory with no directory configured: %v", err)
	}
	if h.store != nil {
		t.Error("an unconfigured deployment attached a store anyway")
	}

	t.Setenv(HistoryDirEnv, t.TempDir())
	h, err = newHistory()
	if err != nil {
		t.Fatalf("newHistory with a directory: %v", err)
	}
	if h.store == nil {
		t.Error("a configured directory left the history in memory")
	}
}

// The sampler's lifetime is the connection's lifetime. A goroutine that
// outlives it keeps calling a broker nobody is using, and a second Close on
// the same client must not close a closed channel.
func TestClosingAConnectionStopsItsSampler(t *testing.T) {
	c, err := newKafkaClient("kafka://127.0.0.1:1", time.Second)
	if err != nil {
		t.Fatalf("newKafkaClient: %v", err)
	}
	c.StartRebalanceSampler(10 * time.Millisecond)
	c.close()
	// A second Close is what an adapter teardown that runs twice does; it
	// must not panic on a channel that is already closed.
	c.close()
	c.StartRebalanceSampler(10 * time.Millisecond)
	c.close()
}

// Starting twice must not leave two tickers racing on one history.
func TestStartingTheSamplerTwiceKeepsOneTicker(t *testing.T) {
	c, err := newKafkaClient("kafka://127.0.0.1:1", time.Second)
	if err != nil {
		t.Fatalf("newKafkaClient: %v", err)
	}
	c.StartRebalanceSampler(time.Hour)
	first := c.samplerStop
	c.StartRebalanceSampler(time.Hour)
	if c.samplerStop != first {
		t.Error("a second Start replaced the running sampler's stop channel instead of being ignored")
	}
	c.close()
}
