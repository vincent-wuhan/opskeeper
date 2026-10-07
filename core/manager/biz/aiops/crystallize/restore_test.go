package crystallize

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// history builds a ledger whose four patterns each hold a different piece of
// state a restore could drop.
//
// Every pattern is here to probe one field, and the probe is always the same
// one: what does the ledger DECIDE next. A restore that loses a field is not
// caught by comparing the restored runs to the original runs on their own —
// the two can be equal in every field the comparison looked at while the
// decision differs — so each pattern is set up so that the lost field changes
// a verdict that comes after.
func history(t *testing.T) *Ledger {
	t.Helper()
	l := NewLedger(Policy{})

	// held: two clean runs, one short of promotion. The streak is the field.
	for _, m := range []int{1, 2} {
		mustRecord(t, l, trialAt(m))
	}

	// promoted: three clean runs, already holding a runbook. A later failure
	// has to retire it, which is only reachable through promoted.
	for _, m := range []int{3, 4, 5} {
		mustRecord(t, l, trialAt(m).withTarget("host:i-promoted"))
	}

	// disagreeing: two clean runs that granted different reaches. The pattern
	// is otherwise identical to held's, so a restore that forgets the
	// disagreement reads as one clean run short of a promotion it may never
	// have — and the crystallizable gate is exactly what refuses it.
	for _, m := range []int{6, 7} {
		tr := trialAt(m).withTarget("host:i-disagree")
		mustRecord(t, l, tr)
	}
	// Re-grant the same pattern at a wider reach so noteGrant records a
	// disagreement. The streak keeps rising; the reach does not agree.
	wider := trialAt(8).withTarget("host:i-disagree")
	wider.Pattern.Action.BlastRadius = domain.RadiusSingleNS
	mustRecord(t, l, wider)

	// reset: one clean run then one failure. The streak is zero and the
	// pattern is nowhere near promotion, so a restore that dropped the
	// failure would let one clean run reach "2 of 3".
	mustRecord(t, l, trialAt(9).withTarget("host:i-reset"))
	mustRecord(t, l, trialAt(10).withTarget("host:i-reset").withOutcome(OutcomeFailed))

	// retired: promoted, then contradicted, and taken back. The retirement
	// and the reason for it are the fields, and the reason is what an
	// operator reads to decide whether the pattern is worth trying again —
	// so a restore that kept the flag but dropped the sentence would leave
	// a runbook nobody can argue with.
	for _, m := range []int{11, 12, 13} {
		mustRecord(t, l, trialAt(m).withTarget("host:i-retired"))
	}
	mustRecord(t, l, trialAt(14).withTarget("host:i-retired").withOutcome(OutcomeFailed))

	// refused: two refusals and nothing else. A refusal returns before
	// noteGrant, so this is the one shape of run with attempts but no grant
	// ever observed — and it is the shape that makes radiusKnown load-bearing
	// rather than decorative. Restored as "a grant was seen", the first real
	// trial would compare its own reach against an empty one, find them
	// different, and record a disagreement that never happened — which
	// fails the crystallizable gate for good.
	mustRecord(t, l, trialAt(15).withTarget("host:i-refused").withOutcome(OutcomeRejected))
	mustRecord(t, l, trialAt(16).withTarget("host:i-refused").withOutcome(OutcomeRejected))

	return l
}

// TestRestoreIsInvisibleToTheNextVerdict is the property that makes the file
// store safe: restoring a ledger and then running the same trials through it
// must produce the same verdicts, for the same reasons, as never having
// restored at all.
//
// It is written as a differential test over a scripted future rather than a
// field comparison on purpose. The fields a restore could get wrong are
// exactly the ones a run-by-run report does not show: radius agreement, the
// promotion flag, the streak arithmetic. Reading them back off a struct proves
// the copy; running a trial through and reading a verdict proves the ledger.
func TestRestoreIsInvisibleToTheNextVerdict(t *testing.T) {
	original := history(t)

	restored := NewLedger(original.Policy())
	if err := restored.Restore(original.Runs()); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// A future that exercises every branch a restored field reaches.
	future := []Trial{
		// held: this is the run that promotes it.
		trialAt(20),
		// held again, now promoted: held must not cross twice.
		trialAt(21),
		// promoted, and kept: the reason names the day it was promoted, so
		// the promotion time has to have survived to be readable.
		trialAt(29).withTarget("host:i-promoted"),
		// promoted: a failure then retires the runbook it just kept.
		trialAt(22).withTarget("host:i-promoted").withOutcome(OutcomeFailed),
		// disagreeing: still not crystallizable, however clean it gets.
		trialAt(23).withTarget("host:i-disagree"),
		trialAt(24).withTarget("host:i-disagree"),
		// reset: back to counting from one.
		trialAt(25).withTarget("host:i-reset"),
		// retired: more of the same evidence must not reissue it. The
		// reason is the tell — a restore that lost the retirement answers
		// "already promoted" here, and one that lost the reason answers
		// with a blank the operator is expected to read.
		trialAt(27).withTarget("host:i-retired"),
		trialAt(28).withTarget("host:i-retired").withOutcome(OutcomeFailed),
		// refused, and then earned: three clean runs have to carry it to a
		// runbook, which they only can if the restored ledger knows it has
		// never seen a reach for this pattern.
		trialAt(30).withTarget("host:i-refused"),
		trialAt(31).withTarget("host:i-refused"),
		trialAt(32).withTarget("host:i-refused"),
		// a pattern the restored ledger has never seen.
		trialAt(26).withTarget("host:i-fresh"),
	}

	for i, tr := range future {
		want, err := original.Record(tr)
		if err != nil {
			t.Fatalf("original Record %d: %v", i, err)
		}
		got, err := restored.Record(tr)
		if err != nil {
			t.Fatalf("restored Record %d: %v", i, err)
		}
		if got.Verdict != want.Verdict {
			t.Fatalf("trial %d (%s): verdict %q, want %q (reason: %s / %s)",
				i, tr.Pattern.Action.Target, got.Verdict, want.Verdict, got.Reason, want.Reason)
		}
		if got.Reason != want.Reason {
			t.Fatalf("trial %d (%s): reason %q, want %q", i, tr.Pattern.Action.Target, got.Reason, want.Reason)
		}
		if !reflect.DeepEqual(got.Run, want.Run) {
			t.Fatalf("trial %d (%s): run %+v, want %+v", i, tr.Pattern.Action.Target, got.Run, want.Run)
		}
	}

	if a, b := len(original.Runs()), len(restored.Runs()); a != b {
		t.Fatalf("restored ledger has %d patterns, want %d", b, a)
	}
	for i := range original.Runs() {
		if !reflect.DeepEqual(original.Runs()[i], restored.Runs()[i]) {
			t.Fatalf("pattern %d differs after the same future: %+v vs %+v", i, original.Runs()[i], restored.Runs()[i])
		}
	}
}

// TestRestoreRejectsTwoStatesForOnePattern: a document that says the same
// pattern is in two states at once is a document this build cannot honour, and
// the alternative — picking one — is picking for the operator which of their
// own decisions to keep.
func TestRestoreRejectsTwoStatesForOnePattern(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	runs := l.Runs()
	shadowed := runs[0]
	shadowed.Streak = 99

	err := NewLedger(Policy{}).Restore([]Run{runs[0], shadowed})
	if err == nil {
		t.Fatal("Restore accepted two states for one pattern, want a refusal")
	}
	if !errors.Is(err, ErrUnusableRun) {
		t.Fatalf("Restore error %v, want it to wrap ErrUnusableRun", err)
	}
}

// TestRestoreIsAllOrNothing: a load that applies the good runs and drops the
// bad one is a ledger holding part of a snapshot's decisions and inventing the
// streak arithmetic for the rest, which no later Record can reconcile.
func TestRestoreIsAllOrNothing(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	mustRecord(t, l, trialAt(2).withTarget("host:i-second"))
	good := l.Runs()

	poisoned := append([]Run(nil), good...)
	broken := good[0]
	broken.Streak = -1
	poisoned = append(poisoned, broken)

	target := NewLedger(Policy{})
	if err := target.Restore(poisoned); err == nil {
		t.Fatal("Restore accepted a negative streak, want a refusal")
	}
	if len(target.Runs()) != 0 {
		t.Fatalf("a refused Restore left %d patterns behind, want 0", len(target.Runs()))
	}
}

// TestRestoreRefusesARunTheLedgerCouldNotHaveMade: the file is a file. A hand
// -edited, truncated or older-build document must not be able to put a pattern
// in front of an approver that Record would have refused to mint.
func TestRestoreRefusesARunTheLedgerCouldNotHaveMade(t *testing.T) {
	source := NewLedger(Policy{})
	mustRecord(t, source, trialAt(1))
	run := source.Runs()[0]

	cases := map[string]func(*Run){
		"argv carrying a shell metacharacter": func(r *Run) {
			r.Pattern.Action.Argv = []string{"systemctl", "restart; rm -rf /"}
		},
		"no target":          func(r *Run) { r.Pattern.Action.Target = "" },
		"unknown tool class": func(r *Run) { r.Pattern.Action.Class = domain.ToolClass("L9") },
		"a streak longer than the verified attempts behind it": func(r *Run) {
			r.Streak, r.Verified = 9, 1
		},
		"more verified attempts than trials": func(r *Run) {
			r.Verified, r.Attempts = 9, 1
		},
		"a negative rejection count": func(r *Run) { r.Rejections = -1 },
		"first seen after last seen": func(r *Run) {
			r.FirstSeen = base.Add(2 * time.Hour)
			r.LastSeen = base
		},
		"promoted with no promotion time": func(r *Run) { r.Promoted = true },
		"retired with no reason": func(r *Run) {
			r.Retired, r.RetiredAt = true, base
		},
		"a retirement reason while not retired": func(r *Run) { r.RetireReason = "contradicted" },
		"attempts with no granted reach": func(r *Run) {
			r.Attempts, r.Verified, r.Streak = 2, 2, 2
			r.GrantedRadius = domain.RadiusNone
		},
		"attempts with no granted window": func(r *Run) {
			r.Attempts, r.Verified, r.Streak = 2, 2, 2
			r.GrantedTTL = 0
		},
	}

	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			bad := run
			corrupt(&bad)
			if err := NewLedger(Policy{}).Restore([]Run{bad}); err == nil {
				t.Fatalf("Restore accepted %s, want a refusal", name)
			}
		})
	}
}

// TestRestoreKeepsTheDeduplicationOfEvidence: seen is the set behind the
// evidence list, and rebuilding it is the one derived field that can be done
// wrong without any counter moving.
func TestRestoreKeepsTheDeduplicationOfEvidence(t *testing.T) {
	l := NewLedger(Policy{})
	for _, m := range []int{1, 2, 3} {
		mustRecord(t, l, trialAt(m))
	}
	runs := l.Runs()
	if len(runs[0].Evidence) != 3 {
		t.Fatalf("evidence list has %d ids, want 3", len(runs[0].Evidence))
	}

	restored := NewLedger(Policy{})
	if err := restored.Restore(runs); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// incident-a is already in the list; recording it again must not append.
	mustRecord(t, restored, trialAt(1))
	if got := len(restored.Runs()[0].Evidence); got != 3 {
		t.Fatalf("re-recording a known evidence id grew the list to %d, want 3", got)
	}
}

// TestRestoreReplacesRatherThanMerges: Restore is how a boot loads a snapshot,
// so a second call has to be the last word. Merging would resurrect runs the
// operator deleted.
func TestRestoreReplacesRatherThanMerges(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	survivor := l.Runs()

	full := NewLedger(Policy{})
	mustRecord(t, full, trialAt(1).withTarget("host:i-kept"))
	mustRecord(t, full, trialAt(2).withTarget("host:i-dropped"))
	snapshot := full.Runs()

	if err := l.Restore(snapshot); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(l.Runs()) != 2 {
		t.Fatalf("ledger has %d patterns after Restore, want the snapshot's 2", len(l.Runs()))
	}
	if err := l.Restore(survivor); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(l.Runs()) != 1 {
		t.Fatalf("ledger has %d patterns after a second Restore, want 1 — Restore replaced, it did not merge", len(l.Runs()))
	}
}

// TestRestoreOfNothingIsAnEmptyLedger: the file not existing is a state, and
// it has to reach the ledger as one rather than as an error.
func TestRestoreOfNothingIsAnEmptyLedger(t *testing.T) {
	l := NewLedger(Policy{})
	if err := l.Restore(nil); err != nil {
		t.Fatalf("Restore(nil): %v", err)
	}
	if len(l.Runs()) != 0 {
		t.Fatalf("ledger has %d patterns after Restore(nil), want 0", len(l.Runs()))
	}
	// And it still counts: an empty restore must not have wedged the ledger.
	rep := mustRecord(t, l, trialAt(1))
	if rep.Verdict != VerdictHeld {
		t.Fatalf("verdict after Restore(nil) is %q, want %q — the ledger stopped counting", rep.Verdict, VerdictHeld)
	}
}

// TestRestoreKeepsTheNarrowestObservedGrant: the window a draft carries is the
// shortest one the evidence actually granted, and the policy ceiling is only
// a fallback. A restore that dropped GrantedTTL would not lose a counter —
// it would widen every emitted declaration from the reach its operator
// approved to the policy maximum, and nothing in the ledger would say so.
func TestRestoreKeepsTheNarrowestObservedGrant(t *testing.T) {
	short := trialAt(1)
	short.Pattern.Action.TTL = 5 * time.Minute
	long := trialAt(2)
	long.Pattern.Action.TTL = 25 * time.Minute

	l := NewLedger(Policy{MaxTTL: 30 * time.Minute})
	mustRecord(t, l, short)
	mustRecord(t, l, long)

	if got := l.Runs()[0].GrantedTTL; got != 5*time.Minute {
		t.Fatalf("granted ttl = %s, want the shortest observed grant of 5m", got)
	}

	restored := NewLedger(Policy{MaxTTL: 30 * time.Minute})
	if err := restored.Restore(l.Runs()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := restored.Runs()[0].GrantedTTL; got != 5*time.Minute {
		t.Fatalf("granted ttl after Restore = %s, want 5m — the draft would carry the policy ceiling instead", got)
	}

	// And the draft has to be the one an approver would have read before the
	// restart, which is the only reading that makes the restored TTL matter.
	for _, ledger := range []*Ledger{l, restored} {
		mustRecord(t, ledger, trialAt(3))
		mustRecord(t, ledger, trialAt(4))
		drafts, err := ledger.Drafts()
		if err != nil {
			t.Fatalf("Drafts: %v", err)
		}
		if len(drafts) != 1 {
			t.Fatalf("drafts: %d, want 1", len(drafts))
		}
		if got := time.Duration(drafts[0].Manifest.Spec.Autonomy.Actions[0].TTL); got != 5*time.Minute {
			t.Fatalf("emitted action ttl = %s, want the observed 5m, not the policy ceiling", got)
		}
	}
}

// TestRestoreKeepsTheRadiusAGrantDisagreedOn: RadiusDisagreement is stored
// inverted, and the inversion is the kind of detail that survives a round trip
// through a struct unnoticed. It is also the one that keeps a runbook from
// being minted on evidence that granted two different reaches.
func TestRestoreKeepsTheRadiusAGrantDisagreedOn(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	wider := trialAt(2)
	wider.Pattern.Action.BlastRadius = domain.RadiusSingleNS
	mustRecord(t, l, wider)

	run := l.Runs()[0]
	if !run.RadiusDisagreement {
		t.Fatal("two grants at different radii did not record a disagreement")
	}

	restored := NewLedger(Policy{})
	if err := restored.Restore([]Run{run}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !restored.Runs()[0].RadiusDisagreement {
		t.Fatal("Restore dropped the radius disagreement")
	}
	// Two more clean runs must not promote it: the gate is the disagreement.
	mustRecord(t, restored, trialAt(3))
	mustRecord(t, restored, trialAt(4))
	if restored.Runs()[0].Promoted {
		t.Fatal("a pattern whose grants disagreed was promoted after a restore")
	}
}

// TestRestoreRoundTripsEveryField is the test the differential one cannot be.
//
// The verdict test above is the stronger property — it checks the restored
// ledger DECIDES the same things — and it is blind to a whole class of field.
// lastSeen is the example: Record keeps the latest timestamp, so a restore
// that dropped lastSeen to zero would have it overwritten by the next trial
// to the same value the original ended up with, and every verdict, reason and
// counter would still match. A field whose only effect is "what time was
// this last" is invisible to any test that runs a trial afterwards.
//
// So the round trip is also checked directly, before anything is recorded.
// It is a weaker assertion and it is here precisely because the stronger one
// cannot see these fields.
func TestRestoreRoundTripsEveryField(t *testing.T) {
	original := history(t)
	before := original.Runs()
	if len(before) == 0 {
		t.Fatal("history built no runs to compare")
	}

	restored := NewLedger(original.Policy())
	if err := restored.Restore(before); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	after := restored.Runs()
	if len(after) != len(before) {
		t.Fatalf("restored %d runs, want %d", len(after), len(before))
	}
	for i := range before {
		if !reflect.DeepEqual(before[i], after[i]) {
			t.Fatalf("run %d did not round trip:\n  before %+v\n  after  %+v", i, before[i], after[i])
		}
	}
}
