package runtimemgmt

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestTaskResult_Validate covers the result-persistence gate. A
// terminal result is required; the sanitizer status must be set;
// failed results require a recognized FailureClass; a "skipped"
// sanitizer MUST carry rationale.
func TestTaskResult_Validate(t *testing.T) {
	now := time.Now().UTC()
	good := TaskResult{
		TaskID: "t", TenantID: "tn", AttemptNumber: 1,
		Status:          TaskStateSucceeded,
		EndedAt:         now,
		SanitizerStatus: SanitizerStatusApplied,
	}
	if err := good.Validate(); err != nil {
		t.Errorf("succeeded OK: %v", err)
	}

	// Required status terminals.
	for _, status := range []TaskState{TaskStateSucceeded, TaskStateFailed, TaskStateCancelled} {
		r := TaskResult{TaskID: "t", SanitizerStatus: SanitizerStatusApplied, Status: status}
		if err := r.Validate(); err != nil {
			t.Errorf("terminal %s should validate: %v", status, err)
		}
	}
	// Non-terminal status rejected.
	nt := TaskResult{TaskID: "t", Status: TaskStateRunning, SanitizerStatus: SanitizerStatusApplied}
	if err := nt.Validate(); !errors.Is(err, ErrResultNotReady) {
		t.Errorf("non-terminal err = %v, want ErrResultNotReady", err)
	}
	// Empty sanitizer rejected.
	noSan := TaskResult{TaskID: "t", Status: TaskStateSucceeded}
	if err := noSan.Validate(); !errors.Is(err, ErrSanitizerMissing) {
		t.Errorf("no sanitizer err = %v, want ErrSanitizerMissing", err)
	}
	// Unknown sanitizer rejected.
	unkSan := TaskResult{TaskID: "t", Status: TaskStateSucceeded, SanitizerStatus: SanitizerStatus("unclassified")}
	if err := unkSan.Validate(); !errors.Is(err, ErrSanitizerMissing) {
		t.Errorf("unknown sanitizer err = %v, want ErrSanitizerMissing", err)
	}
	// Skipped without notes rejected.
	skip := TaskResult{TaskID: "t", Status: TaskStateSucceeded, SanitizerStatus: SanitizerStatusSkipped}
	if err := skip.Validate(); !errors.Is(err, ErrSanitizerMissing) {
		t.Errorf("skipped without notes err = %v, want ErrSanitizerMissing", err)
	}
	// Skipped with notes OK.
	skipOk := TaskResult{
		TaskID: "t", Status: TaskStateSucceeded,
		SanitizerStatus: SanitizerStatusSkipped,
		SanitizerNotes:  "no credentials in output",
	}
	if err := skipOk.Validate(); err != nil {
		t.Errorf("skipped+notes should validate: %v", err)
	}
	// Failed without a failure class rejected.
	failedNoClass := TaskResult{
		TaskID: "t", Status: TaskStateFailed,
		SanitizerStatus: SanitizerStatusApplied,
	}
	if err := failedNoClass.Validate(); !errors.Is(err, ErrFailureClassInvalid) {
		t.Errorf("failed w/o class err = %v, want ErrFailureClassInvalid", err)
	}
	// Failed with bogus failure class rejected.
	failedBadClass := TaskResult{
		TaskID: "t", Status: TaskStateFailed,
		SanitizerStatus: SanitizerStatusApplied,
		FailureClass:    FailureClass("not_a_class"),
	}
	if err := failedBadClass.Validate(); !errors.Is(err, ErrFailureClassInvalid) {
		t.Errorf("failed w/ bogus class err = %v, want ErrFailureClassInvalid", err)
	}
	// Empty TaskID always rejected (defence in depth).
	empty := TaskResult{Status: TaskStateSucceeded, SanitizerStatus: SanitizerStatusApplied}
	if err := empty.Validate(); !errors.Is(err, ErrResultNotReady) {
		t.Errorf("empty task id err = %v, want ErrResultNotReady", err)
	}
}

func TestIsSanitizerStatus(t *testing.T) {
	for _, v := range SanitizerStatusValues {
		if !IsSanitizerStatus(v) {
			t.Errorf("IsSanitizerStatus(%s) = false, want true", v)
		}
	}
	if IsSanitizerStatus("") {
		t.Errorf("empty should NOT match")
	}
	if IsSanitizerStatus("partial") {
		t.Errorf("unknown should NOT match")
	}
}

// TestTaskResult_JSON_RoundTrip is the serialization smoke test.
// All public-field values MUST survive a JSON encode/decode cycle.
// Pinned here so that the eventual API/Dashboard client sees the
// same identifiers, sanitizer verdicts, and failure classes the
// orchestrator wrote.
func TestTaskResult_JSON_RoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	in := TaskResult{
		TaskID:                    "t-1",
		AttemptNumber:             1,
		TenantID:                  "tenant-1",
		Status:                    TaskStateFailed,
		EndedAt:                   now,
		FailureClass:              FailureClassLeaseExpired,
		ErrorMessage:              "worker x timed out at prepare",
		OutputRef:                 "oss://key/42",
		OutputSize:                4096,
		SanitizerStatus:           SanitizerStatusApplied,
		SanitizerNotes:            "",
		OutputDigest:              "deadbeef",
		CompletedBy:               "runtime:edge:abc",
		PodIdempotencyFingerprint: "cafef00d",
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out TaskResult
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.TaskID != in.TaskID ||
		out.TenantID != in.TenantID ||
		out.Status != in.Status ||
		out.FailureClass != in.FailureClass ||
		out.SanitizerStatus != in.SanitizerStatus ||
		out.OutputDigest != in.OutputDigest ||
		out.OutputRef != in.OutputRef ||
		out.OutputSize != in.OutputSize ||
		out.CompletedBy != in.CompletedBy ||
		out.PodIdempotencyFingerprint != in.PodIdempotencyFingerprint {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
	if !out.EndedAt.Equal(in.EndedAt) {
		t.Errorf("EndedAt drift: got %v, want %v", out.EndedAt, in.EndedAt)
	}
}
