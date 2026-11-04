// Package alert - L0 静态规则层（static-rules-v1）单测。
//
// 本文件原先与 L4 语义降噪的测试混装在 semantic_dedup_test.go 里。L4 语义降噪
// 服务已按删除裁决整体移除，但 L0 静态规则层仍在生产树里，它的覆盖不能跟着
// 一起消失——所以这里把这批用例从混装文件中拆出来独立成文件。
//
// 覆盖目标：
//   - DefaultStaticRulesV1 三条开箱规则的 ID / HarnessCaseID / OnHit 默认值
//   - Matcher 命中 / 不命中 / 按 ID 命中 / nil 入参防御
//   - YAML loader 在 Day 2 仍是 not implemented
//   - WithRules 覆盖默认规则集
package alert

import (
	"context"
	"strings"
	"testing"
)

func TestStaticRulesV1_Defaults(t *testing.T) {
	if got := len(DefaultStaticRulesV1); got != 3 {
		t.Fatalf("expected 3 default rules, got %d", got)
	}
	wantIDs := []string{"pg/long-running-tx", "redis/memory-burst", "host/cpu-spike"}
	for i, want := range wantIDs {
		if DefaultStaticRulesV1[i].ID != want {
			t.Errorf("rule[%d] ID = %q, want %q", i, DefaultStaticRulesV1[i].ID, want)
		}
		if DefaultStaticRulesV1[i].HarnessCaseID == "" {
			t.Errorf("rule[%d] missing HarnessCaseID", i)
		}
		if DefaultStaticRulesV1[i].OnHit != OnHitShortCircuitCorrelated {
			t.Errorf("rule[%d] OnHit = %v, want %v", i, DefaultStaticRulesV1[i].OnHit, OnHitShortCircuitCorrelated)
		}
	}
}

func TestStaticRulesV1_Match_Hit(t *testing.T) {
	m := NewStaticRulesV1Matcher()
	hit, err := m.Match(context.Background(), &RawAlert{
		ID:           "alt_1",
		TenantID:     "tenant_1",
		ResourceType: "pg",
		Message:      "pg idle in tx",
	})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if hit == nil {
		t.Fatalf("expected hit for pg alert")
	}
	if hit.Rule.ID != "pg/long-running-tx" {
		t.Errorf("hit rule ID = %q, want %q", hit.Rule.ID, "pg/long-running-tx")
	}
	if hit.AlertID != "alt_1" {
		t.Errorf("hit AlertID = %q, want alt_1", hit.AlertID)
	}
}

func TestStaticRulesV1_Match_Miss(t *testing.T) {
	m := NewStaticRulesV1Matcher()
	hit, err := m.Match(context.Background(), &RawAlert{
		ID:           "alt_2",
		ResourceType: "mq", // 不在 3 条规则里
	})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if hit != nil {
		t.Fatalf("expected nil hit for mq alert, got %+v", hit)
	}
}

func TestStaticRulesV1_MatchByID(t *testing.T) {
	m := NewStaticRulesV1Matcher()
	hit, err := m.MatchByID(context.Background(), &RawAlert{
		ID:           "alt_3",
		ResourceType: "redis",
	}, "redis/memory-burst")
	if err != nil {
		t.Fatalf("MatchByID: %v", err)
	}
	if hit == nil || hit.Rule.ID != "redis/memory-burst" {
		t.Fatalf("expected redis/memory-burst hit, got %+v", hit)
	}
}

func TestStaticRulesV1_NilAlert(t *testing.T) {
	m := NewStaticRulesV1Matcher()
	hit, err := m.Match(context.Background(), nil)
	if err != nil {
		t.Fatalf("Match nil: %v", err)
	}
	if hit != nil {
		t.Errorf("expected nil hit for nil alert")
	}
}

func TestStaticRulesV1_LoadYAML_NotImplemented(t *testing.T) {
	_, err := LoadStaticRulesFromYAML("/tmp/does-not-exist.yaml")
	if err == nil {
		t.Fatalf("expected not-implemented error from YAML loader in Day 2")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error %v does not mention 'not implemented'", err)
	}
}

func TestStaticRulesV1_WithRules_Override(t *testing.T) {
	custom := []StaticRule{{ID: "test/rule", ResourceType: "custom"}}
	m := NewStaticRulesV1Matcher().WithRules(custom)
	if got := len(m.ListRules()); got != 1 {
		t.Fatalf("WithRules: expected 1 rule, got %d", got)
	}
	hit, _ := m.Match(context.Background(), &RawAlert{ResourceType: "pg"})
	if hit != nil {
		t.Errorf("expected no hit for pg (not in custom ruleset), got %+v", hit)
	}
}
