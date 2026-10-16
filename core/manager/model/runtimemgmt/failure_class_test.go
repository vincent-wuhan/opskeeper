package runtimemgmt

import (
	"errors"
	"testing"
)

// TestFailureClass_Validate covers the closed-taxonomy gate. Only
// FailureClassValues members validate; empty / unknown / typo values
// surface ErrFailureClassInvalid.
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

// TestFailureClass_IsRetryable covers the auto-retry gate. The
// matrix is:
//
//	class\Safety               read_only  disposable  mutating
//	no_idempotency_contract       false      false       false
//	lease_expired                  true       true        true (idempotency required upstream)
//	runtime_lost                   true       true        true (idempotency required upstream)
//	admission_denied              false      false       false
//	workspace_unavailable          true       false       false (read-only may re-acquire)
//	credential_denied             false      false       false
//	plugin_incompatible           false      false       false
//	side_effect_duplicate         false      false       false
//	worker_error                   true       true        false (mutating needs idempotency)
//	cancelled                     false      false       false
//	internal                      false      false       false
func TestFailureClass_IsRetryable(t *testing.T) {
	cases := []struct {
		class FailureClass
		// expected per safety: readOnly, disposable, mutating
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
	// Unknown class defaults to false.
	bad := FailureClass("not_a_class")
	if bad.IsRetryable(TaskSafetyReadOnly) {
		t.Errorf("unknown class should default to not retryable")
	}
}

// TestIsFailureClass pins the membership check (mirrors IsTaskSafety
// pattern).
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

// TestTaskSafety_RequiresIdempotencyContract pins the safety→
// idempotency gate: only Mutating tasks require the contract; the
// others MAY carry one but it is not a precondition for retry.
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
