package runtimemgmt

import (
	"errors"
	"testing"
	"time"
)

// TestAdmissionDecision_Admitted_OK covers the spec scenario
// "No capacity is available" reverse: when admission passes, the
// Outcome is "admitted" and the Reason MUST be empty.
func TestAdmissionDecision_Admitted_OK(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	a := AdmissionDecision{
		TaskID:       "task-1",
		TenantID:     "tenant-1",
		Outcome:      AdmissionOutcomeAdmitted,
		Reason:       AdmissionReasonOK,
		ServerLimit:  16,
		RuntimeLimit: 4,
		DecidedAt:    now,
		DecidedBy:    "scheduler.v1",
	}
	if err := a.Validate(); err != nil {
		t.Errorf("expected admitted OK, got %v", err)
	}
}

// TestAdmissionDecision_Rejected covers all rejection reasons.
func TestAdmissionDecision_Rejected(t *testing.T) {
	now := time.Now().UTC()
	reasons := []string{
		AdmissionReasonNoCapacity,
		AdmissionReasonRuntimeNotReady,
		AdmissionReasonQuotaExceeded,
		AdmissionReasonSafetyNotAllowed,
		AdmissionReasonPolicyDenied,
	}
	for _, r := range reasons {
		a := AdmissionDecision{
			TaskID:    "task-1",
			TenantID:  "tenant-1",
			Outcome:   AdmissionOutcomeRejected,
			Reason:    r,
			DecidedAt: now,
		}
		if err := a.Validate(); err != nil {
			t.Errorf("rejected with reason %s should validate; got %v", r, err)
		}
	}
}

// TestAdmissionDecision_RejectedWithEmptyReason verifies the gate:
// rejection reasons MUST come from the closed list. Random strings
// or empty Reason produce ErrNoCapacity (no usable admit).
func TestAdmissionDecision_RejectedWithEmptyReason(t *testing.T) {
	a := AdmissionDecision{
		TaskID: "task-1", TenantID: "t",
		Outcome: AdmissionOutcomeRejected,
		Reason:  "",
	}
	if err := a.Validate(); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("empty reason err = %v, want ErrNoCapacity", err)
	}
	a.Reason = "freeform-typo"
	if err := a.Validate(); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("freeform reason err = %v, want ErrNoCapacity", err)
	}
}

// TestAdmissionDecision_AdmittedWithReason covers the inverse gate:
// if the Outcome is admitted, the Reason MUST be empty.
func TestAdmissionDecision_AdmittedWithReason(t *testing.T) {
	a := AdmissionDecision{
		TaskID: "task-1", TenantID: "t",
		Outcome: AdmissionOutcomeAdmitted,
		Reason:  AdmissionReasonNoCapacity, // contradicts Outcome
	}
	if err := a.Validate(); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("contradicted admit err = %v, want ErrNoCapacity", err)
	}
}

// TestAdmissionDecision_UnknownOutcome covers the catch-all rejection
// for unmapped outcomes — pinned so the orchestrator never silently
// accepts a malformed decision.
func TestAdmissionDecision_UnknownOutcome(t *testing.T) {
	a := AdmissionDecision{
		TaskID: "task-1", TenantID: "t",
		Outcome: AdmissionOutcome("maybe"),
	}
	if err := a.Validate(); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("unknown outcome err = %v, want ErrNoCapacity", err)
	}
}
