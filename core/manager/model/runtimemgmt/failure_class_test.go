package runtimemgmt

import (
	"errors"
	"testing"
)

// TestFailureClass_Validate 覆盖闭集分类校验：只有 FailureClassValues
// 成员通过；空值、未知值或拼写错误都返回 ErrFailureClassInvalid。
func TestFailureClass_Validate(t *testing.T) {
	for _, v := range FailureClassValues {
		if err := v.Validate(); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", v, err)
		}
	}
	var empty FailureClass
	if err := empty.Validate(); !errors.Is(err, ErrFailureClassInvalid) {
		t.Errorf("empty Validate() = %v, want ErrFailureClassInvalid", err)
	}
	bad := FailureClass("not_a_class")
	if err := bad.Validate(); !errors.Is(err, ErrFailureClassInvalid) {
		t.Errorf("bad Validate() = %v, want ErrFailureClassInvalid", err)
	}
}

// TestFailureClass_IsRetryable 覆盖自动重试闸门，矩阵如下：
//
//	class\Safety               read_only  disposable  mutating
//	no_idempotency_contract       false      false       false
//	lease_expired                  true       true        true（上游仍要求幂等）
//	runtime_lost                   true       true        true（上游仍要求幂等）
//	admission_denied              false      false       false
//	workspace_unavailable          true       false       false（只读可重新获取）
//	credential_denied             false      false       false
//	plugin_incompatible           false      false       false
//	side_effect_duplicate         false      false       false
//	worker_error                   true       true        false (mutating needs idempotency)
//	cancelled                     false      false       false
//	internal                      false      false       false
func TestFailureClass_IsRetryable(t *testing.T) {
	cases := []struct {
		class FailureClass
		// 期望值按安全级别排列：readOnly、disposable、mutating。
		readOnly   bool
		disposable bool
		mutating   bool
	}{
		{FailureClassNoIdempotencyContract, false, false, false},
		{FailureClassLeaseExpired, true, true, true},
		{FailureClassRuntimeLost, true, true, true},
		{FailureClassAdmissionDenied, false, false, false},
		{FailureClassWorkspaceUnavailable, true, false, false},
		{FailureClassCredentialDenied, false, false, false},
		{FailureClassPluginIncompatible, false, false, false},
		{FailureClassSideEffectDuplicate, false, false, false},
		{FailureClassWorkerError, true, true, false},
		{FailureClassCancelled, false, false, false},
		{FailureClassInternal, false, false, false},
	}
	for _, c := range cases {
		if got := c.class.IsRetryable(TaskSafetyReadOnly); got != c.readOnly {
			t.Errorf("%s.IsRetryable(read_only) = %v, want %v", c.class, got, c.readOnly)
		}
		if got := c.class.IsRetryable(TaskSafetyDisposable); got != c.disposable {
			t.Errorf("%s.IsRetryable(disposable) = %v, want %v", c.class, got, c.disposable)
		}
		if got := c.class.IsRetryable(TaskSafetyMutating); got != c.mutating {
			t.Errorf("%s.IsRetryable(mutating) = %v, want %v", c.class, got, c.mutating)
		}
	}
	// 未知分类默认不可重试。
	bad := FailureClass("not_a_class")
	if bad.IsRetryable(TaskSafetyReadOnly) {
		t.Errorf("unknown class should default to not retryable")
	}
}

// TestIsFailureClass 固定成员校验，模式与 IsTaskSafety 一致。
func TestIsFailureClass(t *testing.T) {
	for _, v := range FailureClassValues {
		if !IsFailureClass(v) {
			t.Errorf("IsFailureClass(%s) = false, want true", v)
		}
	}
	if IsFailureClass("") {
		t.Errorf("empty should NOT match")
	}
	if IsFailureClass("not_a_class") {
		t.Errorf("unknown should NOT match")
	}
}

// TestTaskSafety_RequiresIdempotencyContract 固定安全级别到幂等契约的
// 闸门：只有变更型任务必须携带契约；其他任务可以携带，但不是重试前置。
func TestTaskSafety_RequiresIdempotencyContract(t *testing.T) {
	cases := []struct {
		safety TaskSafety
		want   bool
	}{
		{TaskSafetyReadOnly, false},
		{TaskSafetyDisposable, false},
		{TaskSafetyMutating, true},
	}
	for _, c := range cases {
		if got := c.safety.RequiresIdempotencyContract(); got != c.want {
			t.Errorf("%s.RequiresIdempotencyContract() = %v, want %v",
				c.safety, got, c.want)
		}
	}
}

func TestIsTaskSafety(t *testing.T) {
	for _, v := range TaskSafetyValues {
		if !IsTaskSafety(v) {
			t.Errorf("IsTaskSafety(%s) = false, want true", v)
		}
	}
	if IsTaskSafety("") {
		t.Errorf("empty should NOT match")
	}
	if IsTaskSafety("harmful") {
		t.Errorf("unknown should NOT match")
	}
}
