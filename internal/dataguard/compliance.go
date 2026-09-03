// Package dataguard — Data-Guard Phase 2 任务 2.1–2.2：ComplianceTag 结构 + 5 个预设框架。
//
// ComplianceTag 是「资源绑定的合规框架 / 控制项 / 是否强制执行」三元组。
// 通过把 ComplianceTag 数组序列化进 DataSensitivityLabel.ComplianceTags JSON 列，
// 资源可一次性声明所适用的合规体系（PCI-DSS / GDPR / 等保 2.0 三级 / HIPAA / SOC2）。
//
// Enforced=true 的 tag 会由 cmdpolicy Sandbox 注入硬性约束（如 audit-log
// 保留期 1y、字段加密）；Enforced=false 的 tag 仅作为元数据 / 报告指标。
package dataguard

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Framework 合规框架枚举。
type Framework string

const (
	FrameworkPCIDSS     Framework = "PCI-DSS"
	FrameworkGDPR       Framework = "GDPR"
	FrameworkDJCPLevel3 Framework = "等保-2.0-三级"
	FrameworkHIPAA      Framework = "HIPAA"
	FrameworkSOC2       Framework = "SOC2"
)

// AllFrameworks 内置 5 个预设，便于 UI 渲染与单元测试遍历。
var AllFrameworks = []Framework{
	FrameworkPCIDSS,
	FrameworkGDPR,
	FrameworkDJCPLevel3,
	FrameworkHIPAA,
	FrameworkSOC2,
}

// IsValidFramework 校验字符串是否为已知框架。
func IsValidFramework(s string) bool {
	for _, f := range AllFrameworks {
		if string(f) == s {
			return true
		}
	}
	return false
}

// ComplianceTag 单条合规 tag。
type ComplianceTag struct {
	Framework Framework `json:"framework"`
	Controls  []string  `json:"controls"`
	Enforced  bool      `json:"enforced"`
}

// Validate 检查框架名合法 + controls 非空 + framework 唯一不重复（外部约束）。
func (c ComplianceTag) Validate() error {
	if !IsValidFramework(string(c.Framework)) {
		return fmt.Errorf("dataguard: invalid framework %q", c.Framework)
	}
	if len(c.Controls) == 0 {
		return errors.New("dataguard: compliance tag requires at least 1 control")
	}
	return nil
}

// MarshalComplianceTags 把 []ComplianceTag 序列化为 JSON（写到
// DataSensitivityLabel.ComplianceTags）。
func MarshalComplianceTags(tags []ComplianceTag) (string, error) {
	if len(tags) == 0 {
		return "", nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "", fmt.Errorf("dataguard: marshal compliance tags: %w", err)
	}
	return string(b), nil
}

// UnmarshalComplianceTags 解析 JSON。
func UnmarshalComplianceTags(raw string) ([]ComplianceTag, error) {
	if raw == "" {
		return nil, nil
	}
	var tags []ComplianceTag
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil, fmt.Errorf("dataguard: unmarshal compliance tags: %w", err)
	}
	return tags, nil
}

// DefaultFrameworkControls 给出 5 框架的推荐控制项（不强制）。
//
// 用作 UI 提示 / 一键加载按钮；enforcement 在 Phase 3 SensitivityGate 接入。
func DefaultFrameworkControls() map[Framework][]string {
	return map[Framework][]string{
		FrameworkPCIDSS:     {"encryption-at-rest", "audit-log-retention-1y", "mfa-on-write", "geo-eu-only"},
		FrameworkGDPR:       {"subject-erasure", "purpose-limitation", "data-minimization"},
		FrameworkDJCPLevel3: {"encryption-at-rest", "access-control-rbac", "audit-log-retention-6mo", "incident-response-24h"},
		FrameworkHIPAA:      {"phi-encryption", "minimum-necessary", "audit-log-retention-6y"},
		FrameworkSOC2:       {"change-management", "access-review-quarterly", "logical-access-logging"},
	}
}

// ComplianceOverride 是 per-edge / per-resource / per-tenant 配置（Phase 2 任务 2.6）。
//
// 留作 Phase 2 后续 sub-task：当前仅定义结构 + 字段语义，不接入 cmdpolicy。
type ComplianceOverride struct {
	Scope       string    `json:"scope"`    // "edge" / "resource" / "tenant"
	ScopeID     string    `json:"scope_id"` // 对应 id
	Framework   Framework `json:"framework"`
	Action      string    `json:"action"` // "force_enforce" | "soften" | "remove"
	Reason      string    `json:"reason"`
	RequestedBy uint64    `json:"requested_by"`
}
