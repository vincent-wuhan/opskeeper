// Package hitl — Phase 2 任务 2.4 + 2.6：PauseCoordinator 三处注入的对外 seam。
//
// 设计要点：
//   - Coordinator 暴露给 Agent ReAct loop / ReviewGate decorator / Flow engine
//     唯一的入口：ShouldPause(ctx, action) → (*Proposal, error)
//   - PausePolicy（policy.go）已能算 severity；此处补「payload 携带
//     data_sensitivity 时自动升级」（task 2.6 与 P1-3 联动）
//   - 暂停时不真正发起审批 / 走 IM，直接创建 Proposal row = pending；
//     IM 桥接（task 4.x）由 Phase 4 接管
//   - 返回 ErrProposalPending 让 caller 短路执行（避免重复副作用）
package hitl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

// ErrProposalPending 在 ShouldPause 返回非空 Proposal 时一并返回，caller 应
// 终止执行并把 Proposal 反馈到 SPA。
var ErrProposalPending = errors.New("hitl: proposal pending human decision")

// ProposalWriter 是 Coordinator 写入 Proposal 行的窄接口（Phase 2 简化为
// 直接调用 Service.CreateProposal；Phase 4 引入 IM bridge 后改为
// Service.Propose + IM 推送）。
type ProposalWriter interface {
	CreateProposal(ctx context.Context, p *model.Proposal) error
}

// PauseSeverityProvider 暴露给 Coordinator：基于 payload 评估 severity
// （task 2.6：data_sensitivity=TopSecret → severity=Dangerous）。
type PauseSeverityProvider interface {
	EscalateFromPayload(payload []byte) (string, string) // (severity, sensitivity)
}

// defaultSeverityProvider 是兜底实现 — 暴露协议，便于 Phase 2 接入。
type defaultSeverityProvider struct{}

// EscalateFromPayload 仅返回 UPGRADE 信号。若 payload 无 data_sensitivity 字段，
// 返回 ("", "") 让 Coordinator 沿用 policy 计算的 severity。
func (defaultSeverityProvider) EscalateFromPayload(payload []byte) (string, string) {
	if len(payload) == 0 {
		return "", ""
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return "", ""
	}
	sens, _ := raw["data_sensitivity"].(string)
	if sens == "" {
		sens, _ = raw["sensitivity"].(string)
	}
	// 大小写归一化（前端可能传 "TopSecret"，enum 是 "top_secret"）。
	canonicalSens := normalizeSensitivity(sens)
	switch canonicalSens {
	case model.SensitivityTopSecret, model.SensitivityRestricted:
		return model.SeverityDangerous, canonicalSens
	case model.SensitivityConfidential:
		return model.SeverityMutating, canonicalSens
	}
	// 未声明或未知 → 不升级，沿用 policy 计算的 severity
	return "", sens
}

// normalizeSensitivity 把任意大小写形式归一为 enum 常量值。
func normalizeSensitivity(s string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", "")) {
	case "public":
		return model.SensitivityPublic
	case "internal":
		return model.SensitivityInternal
	case "confidential":
		return model.SensitivityConfidential
	case "restricted":
		return model.SensitivityRestricted
	case "topsecret":
		return model.SensitivityTopSecret
	}
	return ""
}

// Coordinator 是 PausePoint 三处注入的统一入口。
type Coordinator struct {
	writer  ProposalWriter
	policy  PausePolicy
	payload PauseSeverityProvider
	resolve PauseTokenResolver
	now     func() time.Time
}

// PauseTokenResolver 把 action 转 ResumeToken，便于 paused 状态重放。
type PauseTokenResolver interface {
	BuildToken(ctx context.Context, a *Action, p *model.Proposal) ([]byte, error)
}

// NewCoordinator 构造 Coordinator。
func NewCoordinator(writer ProposalWriter, policy PausePolicy, payload PauseSeverityProvider) *Coordinator {
	if payload == nil {
		payload = defaultSeverityProvider{}
	}
	return &Coordinator{
		writer:  writer,
		policy:  policy,
		payload: payload,
		resolve: &defaultTokenResolver{policy: policy},
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithTokenResolver 注入自定义 token 构建器（Phase 3 接入）。
func (c *Coordinator) WithTokenResolver(r PauseTokenResolver) *Coordinator {
	c.resolve = r
	return c
}

// WithClock 注入当前时间（测试）。
func (c *Coordinator) WithClock(now func() time.Time) *Coordinator {
	c.now = now
	return c
}

// ShouldPause 是 PausePoint 的核心入口。
//
// 返回：
//   - proposal != nil → 写入数据库，caller 应中止执行并把 proposal id 反馈
//     上层（PausePoint / IM bridge）；返回 (nil, ErrProposalPending)
//   - proposal == nil → 放行，caller 继续执行
//   - err != nil → 数据/系统错误；caller 应 fail-fast（不重试）
func (c *Coordinator) ShouldPause(ctx context.Context, action *Action) (*model.Proposal, error) {
	// 先计算 payload 升级信号（task 2.6：sensitivity 高时强制 pause）
	var payload []byte
	if action != nil {
		payload, _ = json.Marshal(action.Payload)
	}
	escSev, escSens := c.payload.EscalateFromPayload(payload)

	should, reason, err := c.policy.ShouldPause(ctx, action)
	if err != nil {
		return nil, fmt.Errorf("hitl: policy: %w", err)
	}
	// payload 里声明了 data_sensitivity（含 Confidential+）时强制升级：
	// 即使 tool 是 read / unknown kind 也要走人工审批，对齐 design §2.3
	// "severity = max(tool.RiskLevel, resource.Sensitivity)" 语义。
	if !should && escSev != "" {
		should = true
		if reason == nil {
			reason = &PauseReason{
				Code:        "data_sensitivity_escalated",
				Description: "data_sensitivity (" + escSens + ") requires human approval",
			}
		}
	}
	if !should {
		return nil, nil
	}

	severity := ""
	if reason != nil {
		if v, ok := reason.Metadata["severity"].(string); ok {
			severity = v
		}
	}
	if escSev != "" && severityRank(escSev) > severityRank(severity) {
		severity = escSev
	}
	if severity == "" {
		severity = model.SeverityMutating
	}
	sensitivity := escSens
	if sensitivity == "" {
		sensitivity = string(model.SensitivityInternal)
	}

	now := c.now()
	p := &model.Proposal{
		Kind:        action.Tool,
		Title:       defaultTitle(action, reason),
		Summary:     reason.Description,
		PayloadJSON: string(payload),
		Source:      sourceFromAction(action),
		Severity:    severity,
		Sensitivity: sensitivity,
		State:       model.StatePending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if reason != nil {
		rs := reason.Description
		p.Reason = &rs
	}
	if action != nil && action.Resource != "" {
		p.SessionID = action.Resource
	}
	if err := c.writer.CreateProposal(ctx, p); err != nil {
		return nil, fmt.Errorf("hitl: create proposal: %w", err)
	}
	return p, ErrProposalPending
}

// defaultTitle 是 fallback title（当 action / reason 不足时）。
func defaultTitle(a *Action, r *PauseReason) string {
	if a != nil && a.Tool != "" {
		return "hitl: pause " + a.Tool
	}
	if r != nil && r.Code != "" {
		return "hitl: " + r.Code
	}
	return "hitl: pause"
}

// sourceFromAction 由 action 推断 source（agent / flow / harness）。
func sourceFromAction(a *Action) string {
	if a == nil {
		return model.SourceAgent
	}
	// 简化：将来由 caller 显式标注；这里 fallback agent
	return model.SourceAgent
}

// defaultTokenResolver 兜底 ResumeToken 构建器（把 UUID 哈希成 uint64）。
type defaultTokenResolver struct{ policy PausePolicy }

// BuildToken 序列化 action + proposal id 为最小 token。
func (d *defaultTokenResolver) BuildToken(_ context.Context, a *Action, p *model.Proposal) ([]byte, error) {
	tok := &ResumeToken{
		ProposalID:    hashProposalID(p.ID),
		LLMMessages:   nil,
		ToolCallStack: nil,
		CreatedAt:     time.Now().UTC(),
	}
	return tok.Serialize()
}

// severityRank 把 severity 映射到整数（higher = more severe）。
func severityRank(s string) int {
	switch s {
	case model.SeveritySafe:
		return 0
	case model.SeverityMutating:
		return 1
	case model.SeverityDangerous:
		return 2
	}
	return 0
}

// hashProposalID 把 UUID 字符串稳定哈希为 uint64（用于 ResumeToken 的数值 ID）。
func hashProposalID(id string) uint64 {
	var h uint64 = 1469598103934665603 // FNV-1a offset basis
	for i := 0; i < len(id); i++ {
		h ^= uint64(id[i])
		h *= 1099511628211 // FNV prime
	}
	return h
}
