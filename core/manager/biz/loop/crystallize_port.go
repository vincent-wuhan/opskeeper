// Package loop — crystallize_port.go
//
// The seam the recovered phase uses to report a recovery that verified
// cleanly, so the platform can stop paying a model to reach a conclusion it
// has already reached before (the plan's item 7, arXiv 2607.07052).
//
// Why the seam is here and not inside the recovered worker. Deciding whether
// a pattern has earned a runbook is a question about repetition across runs,
// and a single phase worker sees one run. The ledger that counts the streak
// is a longer-lived object owned by whoever wires the control plane, so the
// worker's job is only to hand over what actually happened — the fault, the
// tool, the literal argv, the verification — and let that object decide.
//
// Three properties are load-bearing:
//
//   - The worker reports; it does not decide. A recovered phase that
//     promoted its own pattern would need the cross-run counters, and a
//     worker is reconstructed per phase with no memory between incidents.
//   - A crystallisation failure never fails the run. The fix worked and the
//     metric is back inside tolerance; a learning side-effect that could
//     flip that to phase_failed would make the platform refuse to admit it
//     fixed something because it could not write down that it had.
//   - Nothing is invented here. Every field below was observed on the run
//     (the argv the node executed, the fault the investigator diagnosed) or
//     declared by policy (the reach and window an operator will grant a
//     crystallised action). A field with no such source is absent, which
//     makes the record unusable rather than plausible.
package loop

import (
	"context"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// RecoveryEvidence is one clean recovery, in the shape the crystalliser
// needs. It is a value rather than a set of arguments so that a producer
// cannot fill in half of it and a consumer cannot read a field the producer
// never set.
type RecoveryEvidence struct {
	// At is when the verification completed. Required: a ledger with no
	// ordering cannot say which run a streak counts from.
	At time.Time

	// IncidentID and TenantID identify the run the evidence came from.
	// The incident id is what an approver reads on the emitted draft
	// ("promoted on these runs"); the tenant id scopes any lookup the
	// consumer makes.
	IncidentID string
	TenantID   string

	// FaultKind is the investigator's controlled root-cause kind
	// ("host.disk_full"). It comes from the RootCauseJSON the approval
	// phase read, not from a re-diagnosis here.
	FaultKind string

	// Target is the resource locator the fix acted on.
	Target string

	// Tool is the tool the fix ran through ("host.restart_service").
	Tool string

	// Argv is the literal vector the machine executed. It is the one field
	// no consumer may derive, and an empty one means there is nothing a
	// node could be asked to re-run.
	Argv []string

	// Trigger is the detection signal the incident had. It is the second
	// half of a runbook: the argv says what to run, the trigger says when.
	// It is carried rather than derived because the alert rule that fired
	// is the operator's own statement of the condition, and a value
	// invented from a metric name would fire on a different comparison
	// than the one that was reviewed.
	Trigger domain.AutonomyTrigger

	// Verified is the verification contract. Its Passed and RetryCount
	// together are what distinguish a first-try fix from one that needed a
	// rollback, and only the former is evidence about the fix.
	Verified *VerifiedDelta
}

// RecoveryCrystallizer is told that a recovery verified cleanly.
//
// The contract on an error is deliberately weak: the implementation may
// return an error to say it declined to record the run and why, but the
// caller must treat it as advisory. Losing a learning row must never
// un-verify a recovery, which is the same rule the postmortem's pattern
// learner obeys.
type RecoveryCrystallizer interface {
	Learn(ctx context.Context, ev RecoveryEvidence) error
}

// AutonomyTriggerSource names the detection signal that fired an incident,
// in the vocabulary a node can evaluate.
//
// It is a port rather than a value on RunOptions because the signal lives in
// the alert rule that fired, and the loop is handed an incident id, not a
// rule. A crystallised action carries both halves of a runbook — what to run
// and when to run it — and the "when" has to be the comparison the operator
// actually wrote.
//
// An implementation returns false when it has no rule for the incident. A
// missing trigger is not an error: it is a recovery this deployment cannot
// learn a self-heal from, and the caller records nothing rather than
// inventing a comparison.
type AutonomyTriggerSource interface {
	AutonomyTriggerFor(ctx context.Context, tenantID, incidentID string) (domain.AutonomyTrigger, bool)
}

// AutonomyTriggerSourceFunc adapts a function to AutonomyTriggerSource.
type AutonomyTriggerSourceFunc func(ctx context.Context, tenantID, incidentID string) (domain.AutonomyTrigger, bool)

func (f AutonomyTriggerSourceFunc) AutonomyTriggerFor(ctx context.Context, tenantID, incidentID string) (domain.AutonomyTrigger, bool) {
	return f(ctx, tenantID, incidentID)
}

// RecoveryCrystallizerFunc adapts a function to RecoveryCrystallizer.
type RecoveryCrystallizerFunc func(ctx context.Context, ev RecoveryEvidence) error

func (f RecoveryCrystallizerFunc) Learn(ctx context.Context, ev RecoveryEvidence) error {
	return f(ctx, ev)
}
