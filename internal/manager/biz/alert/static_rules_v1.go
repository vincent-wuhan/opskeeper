// Package alert - L0 静态规则层（static-rules-v1 移植）
//
// 本文件实现 zero-manual-ops-loop Day 2 Task 2.2：借鉴 v1 static-rules-v1 的
// "静态规则"模式（schema + precompiled match + severity），应用到 alert pipeline
// 维度做命中判断。
//
// 3 条开箱规则（设计 D12）：
//   - pg/long-running-tx       : PG idle in transaction 会话数 > 5 持续 2 分钟
//   - redis/memory-burst        : Redis used memory 占比 > 90% 持续 1 分钟
//   - host/cpu-spike            : 主机 CPU > 90% 持续 5 分钟（production tag）
//
// Day 2 行为：schema + 简单 ResourceType 匹配（mock），真实 PromQL 查询留后续 Day。
// 命中 OnHit="short_circuit_correlated" 直接短路进入闭环 correlated 阶段。
//
// Day 11+ 路线：外部 YAML loader 在 LoadStaticRulesFromYAML 预留；Day 2 不引入。
package alert

import (
	"context"
	"fmt"
	"time"
)

// OnHitBehavior 静态规则命中后的处置策略。
//
// Day 2：3 条规则全部 hardcode 为 ShortCircuitCorrelated。
// 留 schema 便于 Day 11+ 扩展（如新增 QueueForSemanticDedup / MarkOnly 等）。
type OnHitBehavior string

const (
	// OnHitShortCircuitCorrelated 命中后直接短路进入 correlated 阶段，
	// 跳过 L1-L4 抑制与降噪（因为已经判定为已知根因）。
	OnHitShortCircuitCorrelated OnHitBehavior = "short_circuit_correlated"

	// OnHitQueueForSemanticDedup 命中后仅打标，仍走 L4 语义降噪。
	// Day 11+ 引入，目前 schema 预留。
	OnHitQueueForSemanticDedup OnHitBehavior = "queue_for_semantic_dedup"

	// OnHitMarkOnly 命中后仅打 static_rule_id，不影响主流程。
	// Day 11+ 引入，目前 schema 预留。
	OnHitMarkOnly OnHitBehavior = "mark_only"
)

// Severity 静态规则严重度等级，对齐 alert model 的 Severity 枚举。
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// StaticRule L0 静态规则 schema。
//
// YAML 字段命名对齐 v1 static-rules-v1，便于 Day 11+ 外部加载复用。
type StaticRule struct {
	ID           string        `json:"id"          yaml:"id"`
	Description  string        `json:"description" yaml:"description"`
	ResourceType string        `json:"resource_type" yaml:"resource_type"` // pg / redis / host / k8s / mq
	PromQL       string        `json:"promql"      yaml:"promql"`
	Threshold    float64       `json:"threshold"   yaml:"threshold"`
	WindowSec    int           `json:"window_sec"  yaml:"window_sec"`
	Severity     Severity      `json:"severity"    yaml:"severity"`
	OnHit        OnHitBehavior `json:"on_hit"      yaml:"on_hit"`

	// HarnessCaseID 关联 Harness 黄金事故 case ID（Day 7 跑通）。
	// 与 internal/harness/cases/schema.json 中的 id 对齐。
	HarnessCaseID string `json:"harness_case_id" yaml:"harness_case_id"`
}

// DefaultStaticRulesV1 3 条开箱静态规则（设计 D12）。
//
// 严禁在此处修改 PromQL 或阈值——所有调整走 YAML loader（Day 11+）+ Harness case 重测。
var DefaultStaticRulesV1 = []StaticRule{
	{
		ID:            "pg/long-running-tx",
		Description:   "PG idle in transaction 会话数持续 > 5 达 2 分钟",
		ResourceType:  "pg",
		PromQL:        `pg_stat_activity_count{state="idle in transaction"}`,
		Threshold:     5,
		WindowSec:     120,
		Severity:      SeverityCritical,
		OnHit:         OnHitShortCircuitCorrelated,
		HarnessCaseID: "pg/long-running-tx",
	},
	{
		ID:            "redis/memory-burst",
		Description:   "Redis used memory 占比 > 90% 达 1 分钟",
		ResourceType:  "redis",
		PromQL:        `redis_used_memory_bytes / redis_max_memory_bytes`,
		Threshold:     0.9,
		WindowSec:     60,
		Severity:      SeverityWarning,
		OnHit:         OnHitShortCircuitCorrelated,
		HarnessCaseID: "redis/memory-burst",
	},
	{
		ID:            "host/cpu-spike",
		Description:   "主机 CPU 利用率 > 90% 达 5 分钟（production tag）",
		ResourceType:  "host",
		PromQL:        `host_cpu_utilization{tag="production"}`,
		Threshold:     0.9,
		WindowSec:     300,
		Severity:      SeverityCritical,
		OnHit:         OnHitShortCircuitCorrelated,
		HarnessCaseID: "host/cpu-spike",
	},
}

// RawAlert L0 输入：recordFiring 之前的原始 alert 摘要。
//
// Day 2：仅取 alert incident 的关键字段；真实 PromQL 查询由 alert pipeline 上游
// 完成，L0 只做"已知规则命中"判定（短路判定，不查指标）。
type RawAlert struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	Message      string    `json:"message"`
	CreatedAt    time.Time `json:"created_at"`
}

// StaticRuleHit L0 命中结果。
type StaticRuleHit struct {
	Rule     StaticRule `json:"rule"`
	AlertID  string     `json:"alert_id"`
	TenantID string     `json:"tenant_id"`
	HitAt    time.Time  `json:"hit_at"`
}

// StaticRulesV1Matcher L0 命中检测器。
//
// Day 2：仅做 ResourceType 字符串匹配（mock），命中即返回 DefaultStaticRulesV1 中
// 同 ResourceType 的第一条规则。
//
// Day 11+：替换为 YAML loader + 真实 PromQL evaluator。
// 接口签名 Match / MatchByID 保持稳定，调用方（alert pipeline 的
// RecordFiring 前置钩子）不需改。
type StaticRulesV1Matcher struct {
	// rules 是已加载的规则集合。Day 2 默认填充 DefaultStaticRulesV1；
	// Day 11+ YAML loader 替换之。
	rules []StaticRule

	// now 用于测试时注入固定时钟；nil 时取 time.Now。
	now func() time.Time
}

// NewStaticRulesV1Matcher 构造默认 matcher。
func NewStaticRulesV1Matcher() *StaticRulesV1Matcher {
	rules := make([]StaticRule, len(DefaultStaticRulesV1))
	copy(rules, DefaultStaticRulesV1)
	return &StaticRulesV1Matcher{
		rules: rules,
		now:   time.Now,
	}
}

// WithRules 注入自定义规则集合（测试 / Day 11+ YAML loader）。
func (m *StaticRulesV1Matcher) WithRules(rules []StaticRule) *StaticRulesV1Matcher {
	m.rules = make([]StaticRule, len(rules))
	copy(m.rules, rules)
	return m
}

// WithClock 注入固定时钟（测试用）。
func (m *StaticRulesV1Matcher) WithClock(now func() time.Time) *StaticRulesV1Matcher {
	m.now = now
	return m
}

// Match 按 alert.ResourceType 匹配第一条规则。
//
// Day 2 mock：仅比对 ResourceType。真实 PromQL 评估留 Day 11+。
// 返回 (hit, nil) 命中 / (nil, nil) 未命中 / (nil, err) 异常。
func (m *StaticRulesV1Matcher) Match(ctx context.Context, alert *RawAlert) (*StaticRuleHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if alert == nil {
		return nil, nil
	}
	for i := range m.rules {
		rule := &m.rules[i]
		if rule.ResourceType != alert.ResourceType {
			continue
		}
		return m.makeHit(rule, alert), nil
	}
	return nil, nil
}

// MatchByID 按规则 ID 精确匹配（用于 source linker 反查）。
func (m *StaticRulesV1Matcher) MatchByID(ctx context.Context, alert *RawAlert, ruleID string) (*StaticRuleHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if alert == nil {
		return nil, nil
	}
	for i := range m.rules {
		rule := &m.rules[i]
		if rule.ID != ruleID {
			continue
		}
		return m.makeHit(rule, alert), nil
	}
	return nil, nil
}

// ListRules 返回当前加载的规则集合（只读副本）。
func (m *StaticRulesV1Matcher) ListRules() []StaticRule {
	out := make([]StaticRule, len(m.rules))
	copy(out, m.rules)
	return out
}

func (m *StaticRulesV1Matcher) makeHit(rule *StaticRule, alert *RawAlert) *StaticRuleHit {
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	return &StaticRuleHit{
		Rule:     *rule,
		AlertID:  alert.ID,
		TenantID: alert.TenantID,
		HitAt:    now().UTC(),
	}
}

// LoadStaticRulesFromYAML 从 YAML 文件加载外部规则。
//
// Day 11+ 引入；Day 2 显式返回 not-implemented 错误以避免误用。
// 接口签名先落，确保上游 schema 稳定。
func LoadStaticRulesFromYAML(path string) ([]StaticRule, error) {
	return nil, fmt.Errorf("YAML loader not implemented in Day 2; rules are hardcoded in DefaultStaticRulesV1 (path=%s)", path)
}
