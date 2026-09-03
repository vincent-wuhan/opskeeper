package hitl

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return p
}

func TestLoadDualSignPolicies_Missing(t *testing.T) {
	p, err := LoadDualSignPolicies("/nonexistent/should/not/exist.json")
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(p.Rules()) != 0 {
		t.Errorf("missing file → empty policy; got %d rules", len(p.Rules()))
	}
}

func TestLoadDualSignPolicies_Valid(t *testing.T) {
	body := `{
  "policies": [
    {"role":"opskeeper-admin","resource":"tenant_wide","action":"approve","effect":"allow","requires":["opskeeper-admin","opskeeper-observer"]},
    {"role":"opskeeper-observer","resource":"tenant_wide","action":"approve","effect":"allow","requires":["opskeeper-admin","opskeeper-observer"]}
  ]
}`
	p, err := LoadDualSignPolicies(writePolicy(t, body))
	if err != nil {
		t.Fatalf("load valid: %v", err)
	}
	rules := p.Rules()
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if rules[0].Role != "opskeeper-admin" || rules[0].Requires[0] != "opskeeper-admin" {
		t.Errorf("rule[0] mismatch: %+v", rules[0])
	}
}

func TestLoadDualSignPolicies_BadJSON(t *testing.T) {
	body := `not json at all`
	_, err := LoadDualSignPolicies(writePolicy(t, body))
	if err == nil {
		t.Fatalf("expected parse error")
	}
}

func TestValidate_NoRule_NoSigner(t *testing.T) {
	p := NewDualSignPolicy()
	err := p.Validate("tenant_wide", "approve", nil)
	var dse *DualSignError
	if !errors.As(err, &dse) || dse.Kind != "missing_signer" {
		t.Errorf("expected missing_signer, got %v", err)
	}
}

func TestValidate_NoRule_SingleSigner(t *testing.T) {
	p := NewDualSignPolicy()
	err := p.Validate("tenant_wide", "approve", []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
	})
	if err != nil {
		t.Errorf("no rule + 1 signer should pass, got %v", err)
	}
}

func TestValidate_DualSign_HappyPath(t *testing.T) {
	p := NewDualSignPolicy()
	p.Add(DualSignRule{
		Role: "opskeeper-admin", Resource: "tenant_wide",
		Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
		{UserID: 2, Role: "opskeeper-observer"},
	}
	if err := p.Validate("tenant_wide", "approve", signers); err != nil {
		t.Errorf("dual-sign happy path failed: %v", err)
	}
}

func TestValidate_DualSign_Insufficient(t *testing.T) {
	p := NewDualSignPolicy()
	p.Add(DualSignRule{
		Role: "opskeeper-admin", Resource: "tenant_wide",
		Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
	}
	err := p.Validate("tenant_wide", "approve", signers)
	var dse *DualSignError
	if !errors.As(err, &dse) || dse.Kind != "insufficient_signers" {
		t.Errorf("expected insufficient_signers, got %v", err)
	}
}

func TestValidate_DualSign_SameRoleGroup(t *testing.T) {
	p := NewDualSignPolicy()
	p.Add(DualSignRule{
		Role: "opskeeper-admin", Resource: "tenant_wide",
		Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	// 两个 admin → 缺 observer 组
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
		{UserID: 2, Role: "opskeeper-admin"},
	}
	err := p.Validate("tenant_wide", "approve", signers)
	var dse *DualSignError
	if !errors.As(err, &dse) || dse.Kind != "role_groups_uncovered" {
		t.Errorf("expected role_groups_uncovered, got %v", err)
	}
	if len(dse.Missing) != 1 || dse.Missing[0] != "opskeeper-observer" {
		t.Errorf("missing=%v, want [opskeeper-observer]", dse.Missing)
	}
}

func TestValidate_DualSign_SameUserDifferentRole(t *testing.T) {
	p := NewDualSignPolicy()
	p.Add(DualSignRule{
		Resource: "tenant_wide", Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	// 同 UserID 在不同角色下签 → 角色覆盖，但调用方应禁止（test 验证逻辑不禁止）。
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
		{UserID: 1, Role: "opskeeper-observer"},
	}
	if err := p.Validate("tenant_wide", "approve", signers); err != nil {
		t.Errorf("policy should pass (caller enforces distinct userids), got %v", err)
	}
}

func TestValidate_DualSign_WildcardAction(t *testing.T) {
	p := NewDualSignPolicy()
	p.Add(DualSignRule{
		Resource: "tenant_wide", Action: "*", Effect: "allow",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
		{UserID: 2, Role: "opskeeper-observer"},
	}
	if err := p.Validate("tenant_wide", "execute_destructive", signers); err != nil {
		t.Errorf("wildcard action should match, got %v", err)
	}
}

func TestValidate_DualSign_WildcardResource(t *testing.T) {
	p := NewDualSignPolicy()
	p.Add(DualSignRule{
		Resource: "*", Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
		{UserID: 2, Role: "opskeeper-observer"},
	}
	if err := p.Validate("cluster_wide", "approve", signers); err != nil {
		t.Errorf("wildcard resource should match, got %v", err)
	}
}

func TestValidate_DualSign_ResourceMismatch(t *testing.T) {
	p := NewDualSignPolicy()
	p.Add(DualSignRule{
		Resource: "tenant_wide", Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
		{UserID: 2, Role: "opskeeper-observer"},
	}
	// 不匹配的 resource → 没规则匹配 → 单签即合规（保守降级）
	if err := p.Validate("host:edge-1", "approve", signers); err != nil {
		t.Errorf("resource mismatch should fall through to single-signer rule, got %v", err)
	}
}

func TestValidate_DualSign_DenyRuleIgnored(t *testing.T) {
	p := NewDualSignPolicy()
	p.Add(DualSignRule{
		Resource: "tenant_wide", Action: "approve", Effect: "deny",
		Requires: []string{"opskeeper-admin", "opskeeper-observer"},
	})
	// deny rules don't contribute Requires.
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
	}
	if err := p.Validate("tenant_wide", "approve", signers); err != nil {
		t.Errorf("deny-only rule should not require dual sign, got %v", err)
	}
}

func TestValidate_DualSign_MultipleRulesUnion(t *testing.T) {
	p := NewDualSignPolicy()
	p.Add(DualSignRule{
		Role: "opskeeper-admin", Resource: "tenant_wide",
		Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-admin"},
	})
	p.Add(DualSignRule{
		Role: "opskeeper-observer", Resource: "tenant_wide",
		Action: "approve", Effect: "allow",
		Requires: []string{"opskeeper-observer"},
	})
	signers := []Signer{
		{UserID: 1, Role: "opskeeper-admin"},
		{UserID: 2, Role: "opskeeper-observer"},
	}
	if err := p.Validate("tenant_wide", "approve", signers); err != nil {
		t.Errorf("union of requires should be covered, got %v", err)
	}
}

func TestDualSignError_Message(t *testing.T) {
	e := &DualSignError{Kind: "x", Detail: "y"}
	if got := e.Error(); got != "dual_sign: x: y" {
		t.Errorf("Error() = %q", got)
	}
	e2 := &DualSignError{}
	if got := e2.Error(); got != "dual_sign: unknown error" {
		t.Errorf("empty Error() = %q", got)
	}
}
