package proposal

import "testing"

func TestSafeValidate(t *testing.T) {
	if err := SeverityTier(SeveritySafe).Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestMutatingValidate(t *testing.T) {
	if err := SeverityTier(SeverityMutating).Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestDangerousValidate(t *testing.T) {
	if err := SeverityTier(SeverityDangerous).Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestBogusValidate(t *testing.T) {
	if err := SeverityTier("bogus").Validate(); err == nil {
		t.Errorf("expected error for unknown tier")
	}
}

func TestRequiresOperatorConfirm(t *testing.T) {
	if RequiresOperatorConfirm(SeveritySafe) {
		t.Error("safe should not require confirm")
	}
	if !RequiresOperatorConfirm(SeverityMutating) {
		t.Error("mutating should require confirm")
	}
	if !RequiresOperatorConfirm(SeverityDangerous) {
		t.Error("dangerous should require confirm")
	}
}

func TestRequiresTwoPerson(t *testing.T) {
	if RequiresTwoPerson(SeveritySafe) {
		t.Error("safe should not require two-person")
	}
	if RequiresTwoPerson(SeverityMutating) {
		t.Error("mutating should not require two-person")
	}
	if !RequiresTwoPerson(SeverityDangerous) {
		t.Error("dangerous should require two-person")
	}
}

func TestAutoApprove(t *testing.T) {
	if !AutoApprove(SeveritySafe) {
		t.Error("safe should auto-approve")
	}
	if AutoApprove(SeverityMutating) {
		t.Error("mutating should not auto-approve")
	}
	if AutoApprove(SeverityDangerous) {
		t.Error("dangerous should not auto-approve")
	}
}
