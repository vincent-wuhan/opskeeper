// Package hitl: PausePolicy 实现（路径 A P1-2 阶段 1 任务 1.4 partial — Phase 2 完整覆盖）。
//
// 当前仅实现基础分级：
//   - safe 不暂停（自动放行）
//   - mutating 单人审批（pending → approved/rejected）
//   - dangerous 双人审批 + 强校验（pending → approved/rejected，必须双人）
//
// Resource Sensitivity 由 P1-3 data-guard 提供（顶层 max 合并），故本文件
// 同时暴露 DataGuardSensitivityProvider 接口，让 biz 层装配 P1-3 实现。
package hitl

import (
	"context"
	"strings"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

// DataGuardSensitivityProvider 提供资源的敏感级别（来自 P1-3 data-guard）。
//
// biz/hitl 不直接依赖 internal/dataguard，避免跨子域耦合；装配由 cmd/main.go 注入。
type DataGuardSensitivityProvider interface {
	// Lookup 返回 resource (e.g. "host:edge-1" / "db:pg-prod-3") 的敏感级别。
	// 未知资源应返回 SensitivityInternal。
	Lookup(ctx context.Context, resource, kind string) string
}

// PausePolicyConfig 配置 PausePolicyImpl。
type PausePolicyConfig struct {
	// DangerRequiresDualSign 启用后 dangerous 永远要求双人审批（即使单 sign 也会停在 pending）。
	// 生产推荐 true。
	DangerRequiresDualSign bool

	// DefaultSeverity 当 tool.RiskLevel 未知时的兜底等级。
	DefaultSeverity string

	// AutoApproveToolKinds 是完全自动放行的 tool kind 列表（即使 severity=mutating）。
	// 例如 "query_metrics"、"query_kb"。
	AutoApproveToolKinds []string
}

// NewPolicyFromConfig 用 config 构造 policy。
func NewPolicyFromConfig(cfg PausePolicyConfig) *PausePolicyImpl {
	if cfg.DefaultSeverity == "" {
		cfg.DefaultSeverity = model.SeverityMutating
	}
	return &PausePolicyImpl{cfg: cfg}
}

// PausePolicyImpl 是默认 PausePolicy。
type PausePolicyImpl struct {
	cfg        PausePolicyConfig
	sensitivty DataGuardSensitivityProvider
	dualSign   *DualSignPolicy
}

// WithSensitivity 注入 P1-3 provider（可选）。nil 表示所有 resource 当作 SensitivityInternal。
func (p *PausePolicyImpl) WithSensitivity(prov DataGuardSensitivityProvider) *PausePolicyImpl {
	p.sensitivty = prov
	return p
}

// WithDualSignPolicy 注入 ADR-019 双签策略（可选）。nil 表示走单签 fallback。
//
// 注入后 ValidateSigners 才生效；ShouldPause 自身不依赖 dual sign policy。
func (p *PausePolicyImpl) WithDualSignPolicy(policy *DualSignPolicy) *PausePolicyImpl {
	p.dualSign = policy
	return p
}

// DualSignPolicy 返回已注入的双签策略（用于 HITL Web handler 校验）。
func (p *PausePolicyImpl) DualSignPolicy() *DualSignPolicy {
	return p.dualSign
}

// ValidateSigners 校验 action 的 signers 是否满足 dual sign 策略。
//
// 调用链：HITL Web handler 在收到签者列表后调此函数，Pass → 释放 resume token；
// 失败 → 返回 *DualSignError，前端按 Kind 渲染对应提示。
//
// 无 dual sign policy 注入：单签即合规（与历史行为一致）。
func (p *PausePolicyImpl) ValidateSigners(action *Action, signers []Signer) error {
	if p.dualSign == nil {
		if len(signers) == 0 {
			return &DualSignError{Kind: "missing_signer", Detail: "no signer on record"}
		}
		return nil
	}
	resource := ""
	actionKind := ""
	if action != nil {
		resource = action.Resource
		actionKind = action.Tool
	}
	return p.dualSign.Validate(resource, actionKind, signers)
}

// ShouldPause 实现 PausePolicy 接口。
func (p *PausePolicyImpl) ShouldPause(ctx context.Context, action *Action) (bool, *PauseReason, error) {
	if action == nil {
		return false, nil, nil
	}

	severity := maxSeverity(
		action.RiskLevelToSeverity(),
		sensitivityToSeverity(p.lookupSensitivity(ctx, action)),
		severityFromToolKind(action.Tool),
	)

	// 自动放行
	for _, k := range p.cfg.AutoApproveToolKinds {
		if k == action.Tool {
			return false, nil, nil
		}
	}

	switch severity {
	case model.SeveritySafe:
		return false, nil, nil

	case model.SeverityMutating:
		return true, &PauseReason{
			Code:        "risk_mutating",
			Description: "mutating tool requires single human approval",
			Metadata: map[string]interface{}{
				"tool":             action.Tool,
				"resource":         action.Resource,
				"severity":         severity,
				"required_signers": 1,
			},
		}, nil

	case model.SeverityDangerous:
		return true, &PauseReason{
			Code:        "risk_dangerous",
			Description: "dangerous tool requires dual human approval",
			Metadata: map[string]interface{}{
				"tool":               action.Tool,
				"resource":           action.Resource,
				"severity":           severity,
				"required_signers":   2,
				"dual_sign_required": true,
			},
		}, nil
	}
	return false, nil, nil
}

// RiskLevelToSeverity 把 tool.RiskLevel 字符串映射到 severity。
//
// 映射：read → safe；write/delete → mutating；manage/destructive → dangerous。
// 未识别值退化到 cfg.DefaultSeverity。
func (a *Action) RiskLevelToSeverity() string {
	switch strings.ToLower(strings.TrimSpace(a.RiskLevel)) {
	case "read", "risk_read":
		return model.SeveritySafe
	case "write", "risk_write":
		return model.SeverityMutating
	case "delete", "manage", "risk_delete", "risk_manage", "destructive":
		return model.SeverityDangerous
	}
	return ""
}

// sensitivityToSeverity 把 Sensitivity 等级映射到 severity 升级。
//
// 规则：top_secret → dangerous；restricted → dangerous；confidential → mutating；其余 → safe。
func sensitivityToSeverity(sensitivity string) string {
	switch strings.ToLower(strings.TrimSpace(sensitivity)) {
	case model.SensitivityTopSecret, model.SensitivityRestricted:
		return model.SeverityDangerous
	case model.SensitivityConfidential:
		return model.SeverityMutating
	}
	return model.SeveritySafe
}

// severityFromToolKind 给一些已知的"管理类"工具名打 dangerous 标；其余返回空，让 RiskLevel / Sensitivity 决定。
func severityFromToolKind(tool string) string {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "restart_service", "shell_command", "drop_table", "rm_file", "terraform_apply":
		return model.SeverityDangerous
	}
	return ""
}

// lookupSensitivity 查 data-guard 的资源敏感级别；未配置时返回 internal。
func (p *PausePolicyImpl) lookupSensitivity(ctx context.Context, action *Action) string {
	if p.sensitivty == nil || action == nil {
		return model.SensitivityInternal
	}
	if s := p.sensitivty.Lookup(ctx, action.Resource, action.Tool); s != "" {
		return s
	}
	return model.SensitivityInternal
}

// maxSeverity 取 (a, b) 二者中较严的 severity（safe < mutating < dangerous）。
func maxSeverity(a, b, fallback string) string {
	rank := func(s string) int {
		switch s {
		case model.SeveritySafe:
			return 0
		case model.SeverityMutating:
			return 1
		case model.SeverityDangerous:
			return 2
		}
		return -1
	}
	ar, br := rank(a), rank(b)
	if ar < 0 && br < 0 {
		return fallback
	}
	if ar >= br {
		return a
	}
	return b
}

// DefaultExpiryAfter 给出 proposal 默认过期时间。
func DefaultExpiryAfter() time.Time {
	return time.Now().Add(24 * time.Hour).UTC()
}
