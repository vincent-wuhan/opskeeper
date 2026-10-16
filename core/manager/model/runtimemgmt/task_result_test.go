package runtimemgmt

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestTaskResult_Validate 覆盖结果持久化闸门：必须有终态结果与脱敏状态；
// 失败结果必须有已识别 FailureClass；跳过脱敏时必须携带理由。
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

	// 必填终态。失败的额外分类要求在下方负例单独检查；这里每个标准终态
	// 都带脱敏状态，失败行也带有效分类。
	cases := []TaskResult{
		{TaskID: "t", Status: TaskStateSucceeded, SanitizerStatus: SanitizerStatusApplied},
		{TaskID: "t", Status: TaskStateFailed, SanitizerStatus: SanitizerStatusApplied, FailureClass: FailureClassWorkerError},
		{TaskID: "t", Status: TaskStateCancelled, SanitizerStatus: SanitizerStatusApplied},
	}
	for _, r := range cases {
		if err := r.Validate(); err != nil {
			t.Errorf("terminal %s should validate: %v", r.Status, err)
		}
	}
	// 非终态必须拒绝。
	nt := TaskResult{TaskID: "t", Status: TaskStateRunning, SanitizerStatus: SanitizerStatusApplied}
	if err := nt.Validate(); !errors.Is(err, ErrResultNotReady) {
		t.Errorf("non-terminal err = %v, want ErrResultNotReady", err)
	}
	// 空脱敏状态必须拒绝。
	noSan := TaskResult{TaskID: "t", Status: TaskStateSucceeded}
	if err := noSan.Validate(); !errors.Is(err, ErrSanitizerMissing) {
		t.Errorf("no sanitizer err = %v, want ErrSanitizerMissing", err)
	}
	// 未知脱敏状态必须拒绝。
	unkSan := TaskResult{TaskID: "t", Status: TaskStateSucceeded, SanitizerStatus: SanitizerStatus("unclassified")}
	if err := unkSan.Validate(); !errors.Is(err, ErrSanitizerMissing) {
		t.Errorf("unknown sanitizer err = %v, want ErrSanitizerMissing", err)
	}
	// 跳过脱敏但没有理由必须拒绝。
	skip := TaskResult{TaskID: "t", Status: TaskStateSucceeded, SanitizerStatus: SanitizerStatusSkipped}
	if err := skip.Validate(); !errors.Is(err, ErrSanitizerMissing) {
		t.Errorf("skipped without notes err = %v, want ErrSanitizerMissing", err)
	}
	// 跳过脱敏且携带理由可通过。
	skipOk := TaskResult{
		TaskID: "t", Status: TaskStateSucceeded,
		SanitizerStatus: SanitizerStatusSkipped,
		SanitizerNotes:  "no credentials in output",
	}
	if err := skipOk.Validate(); err != nil {
		t.Errorf("skipped+notes should validate: %v", err)
	}
	// 失败但缺少失败分类必须拒绝。
	failedNoClass := TaskResult{
		TaskID: "t", Status: TaskStateFailed,
		SanitizerStatus: SanitizerStatusApplied,
	}
	if err := failedNoClass.Validate(); !errors.Is(err, ErrFailureClassInvalid) {
		t.Errorf("failed w/o class err = %v, want ErrFailureClassInvalid", err)
	}
	// 失败但使用伪分类必须拒绝。
	failedBadClass := TaskResult{
		TaskID: "t", Status: TaskStateFailed,
		SanitizerStatus: SanitizerStatusApplied,
		FailureClass:    FailureClass("not_a_class"),
	}
	if err := failedBadClass.Validate(); !errors.Is(err, ErrFailureClassInvalid) {
		t.Errorf("failed w/ bogus class err = %v, want ErrFailureClassInvalid", err)
	}
	// 空 TaskID 始终拒绝，作为纵深防御。
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

// TestTaskResult_JSON_RoundTrip 是序列化冒烟测试：所有公开字段必须经过
// JSON 编解码保持不变，保证 API 与 Dashboard 看到与编排器一致的标识、
// 脱敏裁决和失败分类。
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
