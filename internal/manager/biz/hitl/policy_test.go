package hitl

import (
	"context"
	"errors"
	"testing"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

func TestPolicy_SafeSkipsPause(t *testing.T) {
	p := NewPolicyFromConfig(PausePolicyConfig{})
	got, reason, err := p.ShouldPause(context.Background(), &Action{
		Tool: "query_metrics", RiskLevel: "read",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Errorf("safe action should NOT pause")
	}
	if reason != nil {
		t.Errorf("reason = %+v, want nil", reason)
	}
}

func TestPolicy_MutatingRequiresSingleSign(t *testing.T) {
	p := NewPolicyFromConfig(PausePolicyConfig{})
	got, reason, err := p.ShouldPause(context.Background(), &Action{
		Tool: "restart_service_low", RiskLevel: "write", Resource: "host:edge-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("mutating should pause")
	}
	if reason.Metadata["required_signers"].(int) != 1 {
		t.Errorf("required_signers = %v, want 1", reason.Metadata["required_signers"])
	}
}

func TestPolicy_DangerousRequiresDualSign(t *testing.T) {
	p := NewPolicyFromConfig(PausePolicyConfig{DangerRequiresDualSign: true})
	got, reason, err := p.ShouldPause(context.Background(), &Action{
		Tool: "shell_command", RiskLevel: "manage", Resource: "host:prod-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("dangerous should pause")
	}
	signers, _ := reason.Metadata["required_signers"].(int)
	if signers != 2 {
		t.Errorf("required_signers = %d, want 2", signers)
	}
}

func TestPolicy_TopSecretSensitivityEscalatesToDangerous(t *testing.T) {
	p := NewPolicyFromConfig(PausePolicyConfig{}).WithSensitivity(&fakeDG{
		"host:prod-1": model.SensitivityTopSecret,
	})
	got, reason, err := p.ShouldPause(context.Background(), &Action{
		Tool: "any_read_tool", RiskLevel: "read", Resource: "host:prod-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("top-secret read should pause")
	}
	if reason.Metadata["severity"] != model.SeverityDangerous {
		t.Errorf("severity = %v, want dangerous", reason.Metadata["severity"])
	}
}

func TestPolicy_AutoApproveToolKinds(t *testing.T) {
	p := NewPolicyFromConfig(PausePolicyConfig{
		AutoApproveToolKinds: []string{"query_kb"},
	})
	got, _, err := p.ShouldPause(context.Background(), &Action{
		Tool: "query_kb", RiskLevel: "write",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("auto-approved tool should not pause")
	}
}

type fakeDG map[string]string

func (f fakeDG) Lookup(_ context.Context, resource, _ string) string {
	return f[resource]
}

func TestSensitivityToSeverity(t *testing.T) {
	cases := map[string]string{
		model.SensitivityPublic:       model.SeveritySafe,
		model.SensitivityInternal:     model.SeveritySafe,
		model.SensitivityConfidential: model.SeverityMutating,
		model.SensitivityRestricted:   model.SeverityDangerous,
		model.SensitivityTopSecret:    model.SeverityDangerous,
	}
	for in, want := range cases {
		if got := sensitivityToSeverity(in); got != want {
			t.Errorf("sensitivityToSeverity(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestMaxSeverity(t *testing.T) {
	cases := []struct {
		a, b, fb string
		want     string
	}{
		{"", "", model.SeverityMutating, model.SeverityMutating},
		{model.SeveritySafe, model.SeveritySafe, model.SeverityMutating, model.SeveritySafe},
		{model.SeveritySafe, model.SeverityMutating, model.SeverityMutating, model.SeverityMutating},
		{model.SeverityMutating, model.SeverityDangerous, model.SeveritySafe, model.SeverityDangerous},
		{"", model.SeveritySafe, "", model.SeveritySafe},
	}
	for _, c := range cases {
		got := maxSeverity(c.a, c.b, c.fb)
		if got != c.want {
			t.Errorf("maxSeverity(%q, %q, %q) = %q, want %q", c.a, c.b, c.fb, got, c.want)
		}
	}
}

func TestRiskLevelToSeverity(t *testing.T) {
	cases := map[string]string{
		"read":    model.SeveritySafe,
		"write":   model.SeverityMutating,
		"delete":  model.SeverityDangerous,
		"manage":  model.SeverityDangerous,
		"unknown": "",
	}
	for in, want := range cases {
		a := &Action{RiskLevel: in}
		if got := a.RiskLevelToSeverity(); got != want {
			t.Errorf("RiskLevelToSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateSigners_NoPolicy(t *testing.T) {
	p := NewPolicyFromConfig(PausePolicyConfig{})
	signers := []Signer{{UserID: 1, Role: "opskeeper-admin"}}
	if err := p.ValidateSigners(&Action{Tool: "approve", Resource: "tenant_wide"}, signers); err != nil {
		t.Errorf("no dual sign policy + 1 signer should pass, got %v", err)
	}
	err := p.ValidateSigners(&Action{Tool: "approve", Resource: "tenant_wide"}, nil)
	var dse *DualSignError
	if !errors.As(err, &dse) || dse.Kind != "missing_signer" {
		t.Errorf("expected missing_signer, got %v", err)
	}
}

func TestValidateSigners_PolicyHappy(t *testing.T) {
	dual := NewDualSignPolicy()
	dual.Add(DualSignRule{
		Resource: "tenant_wide", Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	p := NewPolicyFromConfig(PausePolicyConfig{}).WithDualSignPolicy(dual)
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
		{UserID: 2, Role: "opskeeper-observer"},
	}
	if err := p.ValidateSigners(&Action{Tool: "approve", Resource: "tenant_wide"}, signers); err != nil {
		t.Errorf("dual-sign via PausePolicyImpl failed: %v", err)
	}
}

func TestValidateSigners_PolicyUncovered(t *testing.T) {
	dual := NewDualSignPolicy()
	dual.Add(DualSignRule{
		Resource: "tenant_wide", Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	p := NewPolicyFromConfig(PausePolicyConfig{}).WithDualSignPolicy(dual)
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
		{UserID: 2, Role: "opskeeper-admin"},
	}
	err := p.ValidateSigners(&Action{Tool: "approve", Resource: "tenant_wide"}, signers)
	var dse *DualSignError
	if !errors.As(err, &dse) || dse.Kind != "role_groups_uncovered" {
		t.Errorf("expected role_groups_uncovered, got %v", err)
	}
}

func TestValidateSigners_NilAction(t *testing.T) {
	dual := NewDualSignPolicy()
	dual.Add(DualSignRule{
		Resource: "tenant_wide", Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	p := NewPolicyFromConfig(PausePolicyConfig{}).WithDualSignPolicy(dual)
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
		{UserID: 2, Role: "opskeeper-observer"},
	}
	// nil action → empty resource/action → no rule matches → single signer required.
	err := p.ValidateSigners(nil, signers)
	if err != nil {
		t.Errorf("nil action with signers should pass (no rule matches), got %v", err)
	}
}
