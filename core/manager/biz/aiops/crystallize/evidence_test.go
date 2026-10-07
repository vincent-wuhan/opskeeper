package crystallize

import (
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

// TestOutcomeOfReadsTheVerificationContract pins the one distinction a naive
// reader of the contract would lose: a pass that took two attempts is not a
// pass a runbook can be written from.
func TestOutcomeOfReadsTheVerificationContract(t *testing.T) {
	cases := []struct {
		name string
		vd   *loop.VerifiedDelta
		want Outcome
		ok   bool
	}{
		{"no verification", nil, "", false},
		{"first-try pass", &loop.VerifiedDelta{Passed: true}, OutcomeVerified, true},
		{"pass after a rollback", &loop.VerifiedDelta{Passed: true, RetryCount: 2}, OutcomeVerifiedWithRetry, true},
		{"failed", &loop.VerifiedDelta{Passed: false, FailedMetrics: []string{"host.cpu_usage"}}, OutcomeFailed, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := OutcomeOf(c.vd)
			if ok != c.ok || got != c.want {
				t.Fatalf("OutcomeOf = (%q, %t), want (%q, %t)", got, ok, c.want, c.ok)
			}
		})
	}
}

// TestFaultOfTakesTheKindAndTheTargetFamily: the contract carries a kind and
// no family, so the family comes from the target the fix was aimed at.
func TestFaultOfTakesTheKindAndTheTargetFamily(t *testing.T) {
	rc := &loop.RootCauseJSON{RootCauseObject: &loop.RootCauseObject{Kind: "pg.long_running_tx"}}
	f := FaultOf(rc, "postgres://prod-cluster-1/orders")
	if f.Kind != "pg.long_running_tx" || f.Family != "postgres" {
		t.Fatalf("fault = %+v, want the kind and the postgres family", f)
	}
	if got := FaultOf(nil, "host:i-0abc"); got.Kind != "" || got.Family != "host" {
		t.Fatalf("fault of a missing root cause = %+v, want the family only", got)
	}
	if got := FaultOf(&loop.RootCauseJSON{}, "host:i-0abc"); got.Kind != "" {
		t.Fatalf("a root cause with no object produced kind %q, want empty rather than invented", got.Kind)
	}
}

// TestFamilyOfTargetDoesNotGuess: a mis-grouped pattern is a runbook promoted
// on evidence from a different resource, so an unrecognised shape is its own
// family rather than a guess.
func TestFamilyOfTargetDoesNotGuess(t *testing.T) {
	cases := map[string]string{
		"postgres://prod/orders": "postgres",
		"host:i-0abc":            "host",
		"orders-api":             "orders-api",
		":leading-colon":         ":leading-colon",
		"":                       "",
	}
	for target, want := range cases {
		if got := FamilyOfTarget(target); got != want {
			t.Errorf("FamilyOfTarget(%q) = %q, want %q", target, got, want)
		}
	}
}

// TestTrialOfIgnoresARunThatNeverVerified: absence of evidence is not
// evidence, and recording it as a failure would retire a runbook because a run
// was interrupted.
func TestTrialOfIgnoresARunThatNeverVerified(t *testing.T) {
	exec := Execution{
		Tool: "host_restart_service", Class: domain.ClassDestructive,
		Argv:        []string{"systemctl", "restart", "orders-api"},
		Trigger:     domain.AutonomyTrigger{Kind: domain.TriggerMetricAbove, Metric: "node_disk_used_ratio", Threshold: 0.92},
		BlastRadius: domain.RadiusPod, TTL: 30 * time.Minute,
	}
	rem := loop.RemediationOption{Action: "restart_service", Target: "host:i-0abc", Risk: "mutating"}
	rc := &loop.RootCauseJSON{RootCauseObject: &loop.RootCauseObject{Kind: "host.disk_full"}}

	if _, ok := TrialOf(base, "inc-1", rc, nil, rem, exec); ok {
		t.Fatal("a run with no verification produced a trial, want none")
	}
	vd := &loop.VerifiedDelta{Passed: true}
	if _, ok := TrialOf(base, "inc-1", rc, vd, loop.RemediationOption{}, exec); ok {
		t.Fatal("a remediation with no target produced a trial, want none")
	}
	noArgv := exec
	noArgv.Argv = nil
	if _, ok := TrialOf(base, "inc-1", rc, vd, rem, noArgv); ok {
		t.Fatal("an execution with no argv produced a trial, want none")
	}
}

// TestTrialOfBuildsATrialTheLedgerAccepts is the seam end to end: the pieces
// the closed loop already has produce a record the ledger takes, and three of
// them promote a pattern.
func TestTrialOfBuildsATrialTheLedgerAccepts(t *testing.T) {
	rc := &loop.RootCauseJSON{RootCauseObject: &loop.RootCauseObject{Kind: "host.disk_full"}}
	rem := loop.RemediationOption{Action: "restart_service", Target: "host:i-0abc", Risk: "mutating"}
	exec := Execution{
		Tool: "host_restart_service", Class: domain.ClassDestructive,
		Argv:        []string{"systemctl", "restart", "orders-api"},
		Trigger:     domain.AutonomyTrigger{Kind: domain.TriggerMetricAbove, Metric: "node_disk_used_ratio", Threshold: 0.92},
		BlastRadius: domain.RadiusPod, TTL: 30 * time.Minute,
	}
	l := NewLedger(Policy{})
	var rep Report
	for i := 1; i <= 3; i++ {
		tr, ok := TrialOf(base.Add(time.Duration(i)*time.Minute), "inc-"+string(rune('a'+i)), rc, &loop.VerifiedDelta{Passed: true}, rem, exec)
		if !ok {
			t.Fatal("TrialOf refused a complete recovery")
		}
		var err error
		if rep, err = l.Record(tr); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if rep.Verdict != VerdictPromoted {
		t.Fatalf("verdict = %s, want promoted (%s)", rep.Verdict, rep.Reason)
	}
	// And the other half: the loop's own retry count retires it.
	tr, _ := TrialOf(base.Add(4*time.Minute), "inc-d", rc, &loop.VerifiedDelta{Passed: true, RetryCount: 1}, rem, exec)
	rep, err := l.Record(tr)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if rep.Verdict != VerdictRetired {
		t.Fatalf("a pass after a rollback = %s, want retired", rep.Verdict)
	}
}
