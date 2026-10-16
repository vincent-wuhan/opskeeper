package runtimemgmt

import (
	"errors"
	"testing"
	"time"
)

// TestAdmissionDecision_Admitted_OK 覆盖“无可用容量”的反向场景：
// 准入通过时 Outcome 必须是 admitted，Reason 必须为空。
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

// TestAdmissionDecision_Rejected 覆盖所有拒绝原因。
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

// TestAdmissionDecision_RejectedWithEmptyReason 验证拒绝原因必须来自
// 闭集；随机字符串或空 Reason 返回 ErrNoCapacity，不得被当作可用准入。
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

// TestAdmissionDecision_AdmittedWithReason 覆盖反向闸门：
// Outcome 为 admitted 时 Reason 必须为空。
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

// TestAdmissionDecision_UnknownOutcome 覆盖未知 Outcome 的兜底拒绝，
// 防止编排器静默接受畸形决策。
func TestAdmissionDecision_UnknownOutcome(t *testing.T) {
	a := AdmissionDecision{
		TaskID: "task-1", TenantID: "t",
		Outcome: AdmissionOutcome("maybe"),
	}
	if err := a.Validate(); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("unknown outcome err = %v, want ErrNoCapacity", err)
	}
}
