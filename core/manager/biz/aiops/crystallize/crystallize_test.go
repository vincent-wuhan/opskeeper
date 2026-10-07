package crystallize

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

var base = time.Date(2026, 10, 3, 4, 0, 0, 0, time.UTC)

// trialAt is one clean verification of the same pattern, N minutes after base.
func trialAt(minutes int) Trial {
	at := base.Add(time.Duration(minutes) * time.Minute)
	return Trial{
		At: at,
		Pattern: Pattern{
			Fault: Fault{Kind: "host.disk_full", Family: "host"},
			Action: Action{
				Tool:        "host_restart_service",
				Class:       domain.ClassDestructive,
				Argv:        []string{"systemctl", "restart", "orders-api"},
				Target:      "host:i-0abc123",
				Trigger:     domain.AutonomyTrigger{Kind: domain.TriggerMetricAbove, Metric: "node_disk_used_ratio", Threshold: 0.92},
				BlastRadius: domain.RadiusPod,
				TTL:         30 * time.Minute,
			},
		},
		Outcome:  OutcomeVerified,
		Evidence: "incident-" + string(rune('a'+minutes)),
	}
}

func (t Trial) withOutcome(o Outcome) Trial { t.Outcome = o; return t }

func (t Trial) withTarget(target string) Trial { t.Pattern.Action.Target = target; return t }

func (t Trial) withArgv(argv ...string) Trial {
	t.Pattern.Action.Argv = argv
	return t
}

func (t Trial) withClass(c domain.ToolClass) Trial { t.Pattern.Action.Class = c; return t }

func (t Trial) withRadius(r domain.BlastRadius) Trial { t.Pattern.Action.BlastRadius = r; return t }

func (t Trial) withMetric(metric string) Trial { t.Pattern.Action.Trigger.Metric = metric; return t }

func mustRecord(t *testing.T, l *Ledger, tr Trial) Report {
	t.Helper()
	rep, err := l.Record(tr)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	return rep
}

// TestThreeCleanVerificationsPromoteAPattern pins the threshold and the
// verdict the crossing produces.
func TestThreeCleanVerificationsPromoteAPattern(t *testing.T) {
	l := NewLedger(Policy{})
	if rep := mustRecord(t, l, trialAt(1)); rep.Verdict != VerdictHeld {
		t.Fatalf("first verification = %s, want held: one working fix is an anecdote", rep.Verdict)
	}
	if rep := mustRecord(t, l, trialAt(2)); rep.Verdict != VerdictHeld {
		t.Fatalf("second verification = %s, want held: two is a coincidence", rep.Verdict)
	}
	rep := mustRecord(t, l, trialAt(3))
	if rep.Verdict != VerdictPromoted {
		t.Fatalf("third verification = %s, want promoted (%s)", rep.Verdict, rep.Reason)
	}
	if !rep.Run.Promoted || rep.Run.PromotedAt.IsZero() {
		t.Fatalf("promoted run = %+v, want the promotion stamped", rep.Run)
	}
	if got := len(l.Promoted()); got != 1 {
		t.Fatalf("promoted = %d, want 1", got)
	}
}

// TestTwoCleanVerificationsAreNotEnough is the other half of the threshold:
// the number is a floor, and it is a floor on *clean* runs.
func TestTwoCleanVerificationsAreNotEnough(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	mustRecord(t, l, trialAt(2))
	if got := len(l.Promoted()); got != 0 {
		t.Fatalf("promoted = %d after two verifications, want 0", got)
	}
}

// TestTheStreakCountsConsecutiveVerificationsNotTotals is the rule that makes
// a runbook a runbook: a pattern that failed in between has not been shown to
// work every time, it has been shown to work sometimes.
func TestTheStreakCountsConsecutiveVerificationsNotTotals(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	mustRecord(t, l, trialAt(2))
	if rep := mustRecord(t, l, trialAt(3).withOutcome(OutcomeFailed)); rep.Verdict != VerdictHeld {
		t.Fatalf("failure = %s, want held", rep.Verdict)
	} else if rep.Run.Streak != 0 {
		t.Fatalf("streak after a failure = %d, want 0", rep.Run.Streak)
	}
	mustRecord(t, l, trialAt(4))
	mustRecord(t, l, trialAt(5))
	if got := len(l.Promoted()); got != 0 {
		t.Fatalf("promoted = %d after two verifications since the failure, want 0", got)
	}
	if rep := mustRecord(t, l, trialAt(6)); rep.Verdict != VerdictPromoted {
		t.Fatalf("third clean verification after the failure = %s, want promoted", rep.Verdict)
	}
}

// TestARollbackRetiresAPromotedPattern is the downgrade half of
// "evidence-driven upgrade and downgrade". A runbook that stops working is
// taken back by the same record that promoted it — nobody has to remember.
func TestARollbackRetiresAPromotedPattern(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	mustRecord(t, l, trialAt(2))
	mustRecord(t, l, trialAt(3))

	rep := mustRecord(t, l, trialAt(4).withOutcome(OutcomeRolledBack))
	if rep.Verdict != VerdictRetired {
		t.Fatalf("rollback on a promoted pattern = %s, want retired", rep.Verdict)
	}
	if !rep.Run.Retired || rep.Run.RetireReason == "" {
		t.Fatalf("retired run = %+v, want a reason", rep.Run)
	}
	if got := len(l.Promoted()); got != 0 {
		t.Fatalf("promoted = %d after retirement, want 0", got)
	}
}

// TestAVerificationThatTookTwoAttemptsIsContradictingEvidence pins the reading
// of the loop's RetryCount. A pass after a rollback means the fix did not
// restore the service on its own, and an autonomy declaration has no second
// attempt in it.
func TestAVerificationThatTookTwoAttemptsIsContradictingEvidence(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	mustRecord(t, l, trialAt(2))
	if rep := mustRecord(t, l, trialAt(3).withOutcome(OutcomeVerifiedWithRetry)); rep.Run.Streak != 0 {
		t.Fatalf("streak after a retried pass = %d, want 0: a retried pass is not a first-try pass", rep.Run.Streak)
	}
	mustRecord(t, l, trialAt(4))
	mustRecord(t, l, trialAt(5))
	mustRecord(t, l, trialAt(6))
	if got := len(l.Promoted()); got != 1 {
		t.Fatalf("promoted = %d, want the pattern promoted on three *first-try* passes", got)
	}
	if rep := mustRecord(t, l, trialAt(7).withOutcome(OutcomeVerifiedWithRetry)); rep.Verdict != VerdictRetired {
		t.Fatalf("a retried pass on a promoted pattern = %s, want retired", rep.Verdict)
	}
}

// TestARefusalMovesNoCounter: a human saying "not now" is not a fact about the
// fix, and a ledger that read it as one would let a cautious reviewer destroy
// a pattern's record.
func TestARefusalMovesNoCounter(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	mustRecord(t, l, trialAt(2))
	rep := mustRecord(t, l, trialAt(3).withOutcome(OutcomeRejected))
	if rep.Verdict != VerdictIgnored {
		t.Fatalf("refusal = %s, want ignored", rep.Verdict)
	}
	if rep.Run.Streak != 2 || rep.Run.Attempts != 2 || rep.Run.Rejections != 1 {
		t.Fatalf("run after a refusal = %+v, want streak 2, attempts 2, rejections 1", rep.Run)
	}
	if rep := mustRecord(t, l, trialAt(4)); rep.Verdict != VerdictPromoted {
		t.Fatalf("next clean verification = %s, want promoted: the refusal did not break the streak", rep.Verdict)
	}
}

// TestARetiredPatternIsNotReissuedByMoreEvidence: retirement is a human's
// decision to make again, not something the same evidence can undo.
func TestARetiredPatternIsNotReissuedByMoreEvidence(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	mustRecord(t, l, trialAt(2))
	mustRecord(t, l, trialAt(3))
	mustRecord(t, l, trialAt(4).withOutcome(OutcomeFailed))
	for i := 5; i <= 8; i++ {
		rep := mustRecord(t, l, trialAt(i))
		if rep.Verdict == VerdictPromoted {
			t.Fatalf("run %d re-promoted a retired pattern", i)
		}
	}
	if got := len(l.Promoted()); got != 0 {
		t.Fatalf("promoted = %d, want 0: retirement is terminal until a human re-admits the package", got)
	}
}

// TestADifferentTargetIsADifferentRunbook: argv is literal, so a runbook is
// for the target it was proved on.
func TestADifferentTargetIsADifferentRunbook(t *testing.T) {
	l := NewLedger(Policy{})
	for i := 1; i <= 3; i++ {
		mustRecord(t, l, trialAt(i))
	}
	mustRecord(t, l, trialAt(4).withTarget("host:i-0def456"))
	if got := len(l.Promoted()); got != 1 {
		t.Fatalf("promoted = %d, want 1: the second target has one verification, not three", got)
	}
	if got := len(l.Runs()); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
}

// TestADifferentArgvIsADifferentRunbook: the same fault fixed by a different
// program is a different declaration.
func TestADifferentArgvIsADifferentRunbook(t *testing.T) {
	l := NewLedger(Policy{})
	for i := 1; i <= 3; i++ {
		mustRecord(t, l, trialAt(i))
	}
	mustRecord(t, l, trialAt(4).withArgv("systemctl", "restart", "orders-worker"))
	if got := len(l.Promoted()); got != 1 {
		t.Fatalf("promoted = %d, want 1", got)
	}
}

// TestADifferentTriggerIsADifferentRunbook: a declaration fires on one
// signal, and a run that fired on another is evidence for another runbook.
func TestADifferentTriggerIsADifferentRunbook(t *testing.T) {
	l := NewLedger(Policy{})
	for i := 1; i <= 3; i++ {
		mustRecord(t, l, trialAt(i))
	}
	mustRecord(t, l, trialAt(4).withMetric("node_memory_used_ratio"))
	if got := len(l.Promoted()); got != 1 {
		t.Fatalf("promoted = %d, want 1", got)
	}
	if got := len(l.Runs()); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
}

// TestAnUnusableTrialChangesNothing is the difference between a producer bug
// and evidence. Half a pattern is not a pattern, and counting it either way
// would corrupt the record the promotion rests on.
func TestAnUnusableTrialChangesNothing(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	mustRecord(t, l, trialAt(2))

	cases := []struct {
		name string
		mut  func(Trial) Trial
	}{
		{"no timestamp", func(tr Trial) Trial { tr.At = time.Time{}; return tr }},
		{"no fault kind", func(tr Trial) Trial { tr.Pattern.Fault.Kind = ""; return tr }},
		{"no tool", func(tr Trial) Trial { tr.Pattern.Action.Tool = ""; return tr }},
		{"no target", func(tr Trial) Trial { tr.Pattern.Action.Target = ""; return tr }},
		{"no argv", func(tr Trial) Trial { tr.Pattern.Action.Argv = nil; return tr }},
		{"unknown class", func(tr Trial) Trial { tr.Pattern.Action.Class = "sideways"; return tr }},
		{"unusable trigger", func(tr Trial) Trial {
			tr.Pattern.Action.Trigger = domain.AutonomyTrigger{Kind: "vibes_are_bad"}
			return tr
		}},
		{"no radius", func(tr Trial) Trial { tr.Pattern.Action.BlastRadius = domain.RadiusNone; return tr }},
		{"unknown radius", func(tr Trial) Trial { tr.Pattern.Action.BlastRadius = "galaxy"; return tr }},
		{"no ttl", func(tr Trial) Trial { tr.Pattern.Action.TTL = 0; return tr }},
		{"shell argv", func(tr Trial) Trial { return tr.withArgv("sh", "-c", "systemctl restart orders-api; rm -rf /") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := l.Record(c.mut(trialAt(9))); !errors.Is(err, ErrUnusable) {
				t.Fatalf("Record(%s) error = %v, want ErrUnusable", c.name, err)
			}
		})
	}
	// Three trials were recorded in total (1, 2 and 10); the eleven refused
	// ones added nothing to any counter.
	if got := mustRecord(t, l, trialAt(10)).Run; got.Streak != 3 || got.Attempts != 3 || got.Verified != 3 || got.Rejections != 0 {
		t.Fatalf("run after eleven refused trials = %+v, want three trials and no rejections", got)
	}
	if got := len(l.Runs()); got != 1 {
		t.Fatalf("runs = %d, want 1: a refused trial must not create a pattern", got)
	}
}

// TestTheLedgerSurvivesConcurrentRecording: the ledger is read by a console
// and written by the loop at the same time.
func TestTheLedgerSurvivesConcurrentRecording(t *testing.T) {
	l := NewLedger(Policy{MinCleanStreak: 1000})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := l.Record(trialAt(i*20 + j)); err != nil {
					t.Errorf("Record: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	run := l.Runs()[0]
	if run.Attempts != 160 || run.Verified != 160 || run.Streak != 160 {
		t.Fatalf("run = %+v, want 160 attempts and verifications", run)
	}
	if len(run.Evidence) > maxEvidence {
		t.Fatalf("evidence list = %d entries, want it bounded at %d", len(run.Evidence), maxEvidence)
	}
}

// TestRunsAreListedInAStableOrder: a listing that reorders between calls is a
// listing nobody can diff.
func TestRunsAreListedInAStableOrder(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1).withTarget("host:i-0zzz"))
	mustRecord(t, l, trialAt(2).withTarget("host:i-0aaa"))
	first := l.Runs()
	second := l.Runs()
	if len(first) != 2 {
		t.Fatalf("runs = %d, want 2", len(first))
	}
	for i := range first {
		if first[i].Pattern.Key() != second[i].Pattern.Key() {
			t.Fatalf("run %d reordered between calls: %q then %q", i, first[i].Pattern.Key(), second[i].Pattern.Key())
		}
	}
}

// TestTheRunCarriesTheEvidenceItPromotedOn: the count is not the evidence, and
// an approver has to be able to read the runs.
func TestTheRunCarriesTheEvidenceItPromotedOn(t *testing.T) {
	l := NewLedger(Policy{})
	for i := 1; i <= 3; i++ {
		mustRecord(t, l, trialAt(i))
	}
	run := l.Promoted()[0]
	if len(run.Evidence) != 3 {
		t.Fatalf("evidence = %v, want the three runs", run.Evidence)
	}
	if run.FirstSeen != trialAt(1).At || run.LastSeen != trialAt(3).At {
		t.Fatalf("window = %s..%s, want the first and last trials", run.FirstSeen, run.LastSeen)
	}
}

// TestAReadToolIsHeldWithAReason: the streak can say a pattern works and still
// leave nothing to write down, because autonomy may not drive a read.
func TestAReadToolIsHeldWithAReason(t *testing.T) {
	l := NewLedger(Policy{})
	var rep Report
	for i := 1; i <= 3; i++ {
		rep = mustRecord(t, l, trialAt(i).withClass(domain.ClassRead))
	}
	if rep.Verdict != VerdictHeld {
		t.Fatalf("verdict = %s, want held", rep.Verdict)
	}
	if !strings.Contains(rep.Reason, "read") {
		t.Fatalf("reason = %q, want it to name the read", rep.Reason)
	}
	if got := len(l.Promoted()); got != 0 {
		t.Fatalf("promoted = %d, want 0", got)
	}
}
