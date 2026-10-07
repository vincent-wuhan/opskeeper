package crystallize

import (
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

// OutcomeOf reads the closed loop's verification contract and says what it is
// as evidence.
//
// The distinction it preserves is the one the contract already draws and a
// naive reader would throw away: VerifiedDelta.Passed is true both for a fix
// that worked and for a fix that worked after the loop rolled back and tried
// again, and RetryCount is the only field that tells them apart. Reading
// Passed alone would crystallize both, and the second one is not a runbook —
// a runbook has no second attempt in it.
func OutcomeOf(vd *loop.VerifiedDelta) (Outcome, bool) {
	if vd == nil {
		return "", false
	}
	switch {
	case vd.Passed && vd.RetryCount == 0:
		return OutcomeVerified, true
	case vd.Passed:
		return OutcomeVerifiedWithRetry, true
	default:
		return OutcomeFailed, true
	}
}

// FaultOf names the fault the investigator diagnosed.
//
// The kind is the loop's controlled root-cause kind. The family comes from
// the target the fix was aimed at, because the contract carries a kind and no
// resource family, and the family is half of what tells two runbooks with the
// same kind apart: a disk-full on a host and a disk-full on a database are
// two different argv.
func FaultOf(rc *loop.RootCauseJSON, target string) Fault {
	f := Fault{Family: FamilyOfTarget(target)}
	if rc != nil && rc.RootCauseObject != nil {
		f.Kind = rc.RootCauseObject.Kind
	}
	return f
}

// FamilyOfTarget reads the resource family off a remediation target
// ("postgres://prod-cluster-1" → "postgres", "host:i-0abc123" → "host").
//
// A target with no scheme is its own family. Guessing one would make the
// ledger's grouping depend on a convention nobody declared, and a mis-grouped
// pattern is a runbook promoted on evidence from a different resource.
func FamilyOfTarget(target string) string {
	if i := strings.Index(target, ":"); i > 0 {
		return target[:i]
	}
	return target
}

// Execution is what actually ran, and what a human granted for it.
//
// These are the fields the ledger cannot derive from the loop's own
// contracts: the argv the node will re-run, the trigger it will evaluate, and
// the reach and window the approver agreed to. They arrive from the execution
// record rather than from the diagnosis, and a caller that cannot fill them
// in has no pattern to record yet.
type Execution struct {
	Tool        string
	Class       domain.ToolClass
	Argv        []string
	Trigger     domain.AutonomyTrigger
	BlastRadius domain.BlastRadius
	TTL         time.Duration
}

// TrialOf assembles the trial for one closed-loop recovery.
//
// It returns false when the verification has nothing to say — no
// VerifiedDelta means the loop did not get far enough to have an opinion, and
// recording that as a failure would retire a runbook because a run was
// interrupted. Absence of evidence is not evidence here, which is the same
// reason Record refuses an unusable trial rather than counting it.
func TrialOf(at time.Time, incidentID string, rc *loop.RootCauseJSON, vd *loop.VerifiedDelta, rem loop.RemediationOption, exec Execution) (Trial, bool) {
	outcome, ok := OutcomeOf(vd)
	if !ok {
		return Trial{}, false
	}
	if strings.TrimSpace(rem.Target) == "" || exec.Tool == "" || len(exec.Argv) == 0 {
		return Trial{}, false
	}
	return Trial{
		At: at,
		Pattern: Pattern{Fault: FaultOf(rc, rem.Target), Action: Action{
			Tool: exec.Tool, Class: exec.Class, Argv: append([]string(nil), exec.Argv...),
			Target: rem.Target, Trigger: exec.Trigger, BlastRadius: exec.BlastRadius, TTL: exec.TTL,
		}},
		Outcome:  outcome,
		Evidence: incidentID,
	}, true
}
