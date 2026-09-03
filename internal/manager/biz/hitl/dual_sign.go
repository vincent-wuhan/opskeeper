// Package hitl dual-sign validator for tenant_wide / blast_radius 高风险动作。
//
// ADR-019（docs/superpowers/decisions/2026-08-19-tenant-wide-dual-approval.md）：
//   - 高风险（blast_radius ∈ {cluster, tenant_wide} / data-guard destructive）
//     写操作需要双人审批，两签必须来自不同角色组（避免单点滥用）
//   - 角色与"必须组合"由 policy/opskeeper/casbin/tenant_wide.json 定义，
//     启动时载入；cmdpolicy 9 类策略 + Casbin RBAC + DualSignPolicy 三重门控
//
// 与 PausePolicyImpl.ShouldPause 的关系：
//   - ShouldPause 决定"要不要停"（输出 PauseReason.Metadata.dual_sign_required）
//   - DualSignPolicy.Validate 决定"签得够不够"（按角色组覆盖校验）
//   - HITL Web 通道在签齐时调用 Validate，validate 通过 → resume token 释放
package hitl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Signer 表示一个审批人。
type Signer struct {
	// UserID 在审计日志里作为 approved_by 写入。
	UserID uint64

	// Role 是 Casbin role（"opskeeper-admin" / "opskeeper-observer" / 等）。
	// 来自 iam/biz/authz HydrateMemberships 同步。
	Role string

	// ApprovedAt 仅用于审计与限速；不影响校验逻辑。
	ApprovedAt int64 // unix seconds
}

// DualSignRule 一条 Casbin policy 规则。
//
// JSON 形态：
//
//	{"role":"opskeeper-admin","resource":"tenant_wide","action":"approve",
//	 "effect":"allow","requires":["opskeeper-admin","opskeeper-observer"]}
type DualSignRule struct {
	// Role 主签角色（policy.sub）；为空表示该 rule 不绑定主签角色，
	// 只看 resource+action+requires。
	Role string

	// Resource policy.obj。
	Resource string

	// Action policy.act。
	Action string

	// Effect "allow" / "deny"。
	Effect string

	// Requires 双签必须覆盖的角色组列表（每个元素至少出现一次）。
	// 空 → 单签即够；非空 → 任意两签只要覆盖所有 Requires 即合规。
	Requires []string
}

// DualSignPolicy 加载自 policy/opskeeper/casbin/tenant_wide.json 的内存形态。
type DualSignPolicy struct {
	mu    sync.RWMutex
	rules []DualSignRule
}

// NewDualSignPolicy 构造空 policy（用于单元测试 + 默认 fallback）。
func NewDualSignPolicy() *DualSignPolicy {
	return &DualSignPolicy{}
}

// LoadDualSignPolicies 从 JSON 文件载入策略。
//
// 文件不存在 → 返回空 policy + nil error（生产允许双签降级为单签，
// 需配合 cmdpolicy 风险等级共同判定）。
//
// 文件存在但解析失败 → 返回 error。
func LoadDualSignPolicies(path string) (*DualSignPolicy, error) {
	p := NewDualSignPolicy()
	if path == "" {
		return p, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
		return nil, fmt.Errorf("dual_sign: read %s: %w", path, err)
	}
	var doc struct {
		Policies []DualSignRule `json:"policies"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("dual_sign: parse %s: %w", path, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = doc.Policies
	return p, nil
}

// Add 运行时追加 rule（用于测试 + HotReload 预留）。
func (p *DualSignPolicy) Add(r DualSignRule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, r)
}

// Rules 返回当前规则的快照（用于审计 / UI 渲染）。
func (p *DualSignPolicy) Rules() []DualSignRule {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]DualSignRule, len(p.rules))
	copy(out, p.rules)
	return out
}

// Validate 检查 signers 是否满足 resource+action 对应的双签要求。
//
// 返回 nil → 合规。
// 返回 DualSignError → 不合规，ErrKind 区分失败原因。
//
// 判定流程：
//  1. 找匹配 resource+action+effect=allow 的 rules
//  2. 取所有命中 rules 的 Requires 集合并集
//  3. 若 Requires 为空 → 单签即合规
//  4. 若 Requires 非空 → signers 必须 ≥ 2，且并集覆盖所有 Requires
//
// 角色组覆盖算法：
//   - 每个 signer.role 与 Requires 元素做精确匹配（大小写敏感）
//   - 同一 role 出现多次只算一组
//   - signers 顺序无关（验证后审计日志按 ApprovedAt 排序输出）
func (p *DualSignPolicy) Validate(resource, action string, signers []Signer) error {
	requires := p.collectRequires(resource, action)
	if len(requires) == 0 {
		// 无双签规则：单签即合规（调用方负责其它层校验）。
		if len(signers) == 0 {
			return &DualSignError{Kind: "missing_signer", Detail: "no signer on record"}
		}
		return nil
	}
	if len(signers) < 2 {
		return &DualSignError{
			Kind:   "insufficient_signers",
			Detail: fmt.Sprintf("requires %d signers, got %d", 2, len(signers)),
			Need:   requires,
		}
	}
	covered := map[string]struct{}{}
	for _, s := range signers {
		for _, req := range requires {
			if s.Role == req {
				covered[req] = struct{}{}
			}
		}
	}
	missing := []string{}
	for _, r := range requires {
		if _, ok := covered[r]; !ok {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return &DualSignError{
			Kind:    "role_groups_uncovered",
			Detail:  fmt.Sprintf("signers do not cover required role groups: %v", missing),
			Need:    requires,
			Have:    signerRoles(signers),
			Missing: missing,
		}
	}
	return nil
}

// collectRequires 取所有匹配 resource+action 的 allow rules 的 Requires 并集。
func (p *DualSignPolicy) collectRequires(resource, action string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, r := range p.rules {
		if !strings.EqualFold(r.Effect, "allow") {
			continue
		}
		if !matchResource(r.Resource, resource) {
			continue
		}
		if !matchAction(r.Action, action) {
			continue
		}
		for _, req := range r.Requires {
			seen[req] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// matchResource "*" 通配；否则精确匹配。
func matchResource(rule, req string) bool {
	if rule == "*" || rule == "" {
		return true
	}
	return rule == req
}

// matchAction "*" 通配；否则大小写不敏感精确匹配。
func matchAction(rule, req string) bool {
	if rule == "*" || rule == "" {
		return true
	}
	return strings.EqualFold(rule, req)
}

func signerRoles(ss []Signer) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Role)
	}
	return out
}

// DualSignError 校验失败原因。
//
// ErrKind 取值：
//   - missing_signer: 没有任何签者
//   - insufficient_signers: 签者不足 2
//   - role_groups_uncovered: 签者角色未覆盖必需组
type DualSignError struct {
	Kind    string
	Detail  string
	Need    []string
	Have    []string
	Missing []string
}

func (e *DualSignError) Error() string {
	if e.Kind == "" {
		return "dual_sign: unknown error"
	}
	return fmt.Sprintf("dual_sign: %s: %s", e.Kind, e.Detail)
}
