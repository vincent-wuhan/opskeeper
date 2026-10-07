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
//
// **这一段曾经是"以上是设计"，因为设计没有实现。决策 362 实现了它。**
//
// 决策 285 量到的实况（当时是 `TestDualSignCannotBeEnforcedBecauseNowhereStoresTwoSigners`，
// 今天改名了，原因见该文件）：
//   - `Validate` 的调用方是 **0 个**。启动时 `cmd/opskeeper` 载入规则文件、
//     校验语法、打一行 "dual sign policy loaded"，然后那个局部变量出作用域。
//   - `Service.Approve` 第一次调用就把 `StatusApproved` 写下去并返回。
//   - `model.Approval` 只有 `ApprovedBy *uint64`，`model.Proposal` 只有
//     `ApprovedBy *uint64` 与 `ResumedBy *uint64`。**三列都是单值。**
//
// 决策 362 补上的三件：
//  1. **存储**：`model.Approval` 新增 `signers_json`，一行放得下 N 个签名人；
//     同时新增 `risk_class` 与 `blast_radius` 两列——它们本来就以 payload 字节的
//     形式存在，而一条要按风险匹配的规则去解析它自己要批准的东西，
//     迟早会解析错。
//  2. **闸门**：`biz/approval` 的 `Sign` 累积签名、调用闸门、缺口未补齐时保持
//     pending（HTTP 202）而不是返回。装配在 `cmd/opskeeper/dualsigngate.go`。
//  3. **声明**：启动日志与本段同时改成"已生效"，并说明怎么关掉。
//
// 验证器本身在同一次决策里修了两个洞，两者都曾经让"双签"成为一句空话：
//   - **按 UserID 去重**。此前 `Validate` 接受同一个人的两条签名记录，理由写的是
//     "调用方负责去重"——而一个把控制交给周围代码记得去做的规则，等于没有规则。
//   - **真的去读 `rule.Role`**。此前 `collectRequires` 只看 resource/action/effect，
//     于是规则文件里最显眼的那个字段从未被读过。
//
// 仍然没有实现的是 `sensitivity.escalates-severity`：敏感度把审批升级成 dangerous
// 那条路径的生产者仍然是零，见 dataguard 登记表。
//
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

	// Role 是 iam 的系统角色（"admin" / "user" / "viewer"），
	// 来自 tenantctx，即签名人**当时以什么身份登录**。
	//
	// 决策 362 之前这里写的是 Casbin role 并举例 "opskeeper-admin"——
	// 一个系统里从来没有人持有的名字。规则文件当时也在用这个名字，
	// 于是整份配置没有一条规则可能被满足。
	Role string

	// ApprovedAt 仅用于审计与限速；不影响校验逻辑。
	ApprovedAt int64 // unix seconds
}

// DualSignRule 一条 Casbin policy 规则。
//
// JSON 形态：
//
//	{"role":"admin","resource":"destructive","action":"approve",
//	 "effect":"allow","requires":["admin"]}
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
	// One person is one signature. The rule exists to stop a single operator
	// from being the whole control, and a validator that counts a repeated
	// signer twice hands that control back to exactly the person it was
	// written for. Deduplicating here rather than in the caller is a
	// deliberate choice: every caller that forgets is a caller that has just
	// disabled the control it called.
	signers = dedupeSigners(signers)
	// The `role` on a rule was never checked by Validate: collectRequires
	// reads only resource, action and effect, so a rule file that named
	// "opskeeper-admin" on every line was a file whose most prominent field
	// did nothing. It is checked here, where the rest of the rule is, and a
	// row of rules that names a role is now a statement about who may sign.
	if !p.roleSigned(resource, action, signers) {
		return &DualSignError{
			Kind:    "role_not_signed",
			Detail:  fmt.Sprintf("no signer holds a role the rule requires for %s/%s", resource, action),
			Missing: p.collectRoles(resource, action),
		}
	}
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

// roleSigned reports whether some signer holds one of the roles the matching
// rules name. A rule with no role names none, which is the "anybody may sign
// this one" case and is why the field is optional.
func (p *DualSignPolicy) roleSigned(resource, action string, signers []Signer) bool {
	need := p.collectRoles(resource, action)
	if len(need) == 0 {
		return true
	}
	for _, s := range signers {
		for _, role := range need {
			if strings.EqualFold(s.Role, role) {
				return true
			}
		}
	}
	return false
}

// collectRoles 取所有匹配 resource+action 的 allow rules 的 Role 并集。
func (p *DualSignPolicy) collectRoles(resource, action string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, r := range p.rules {
		if !strings.EqualFold(r.Effect, "allow") {
			continue
		}
		if !matchResource(r.Resource, resource) || !matchAction(r.Action, action) {
			continue
		}
		if r.Role != "" {
			seen[r.Role] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// dedupeSigners keeps the first record per user and drops the rest. "First"
// is the earliest signature on the row, which is the one the audit chain
// already names.
func dedupeSigners(ss []Signer) []Signer {
	seen := make(map[uint64]struct{}, len(ss))
	out := make([]Signer, 0, len(ss))
	for _, s := range ss {
		if _, ok := seen[s.UserID]; ok {
			continue
		}
		seen[s.UserID] = struct{}{}
		out = append(out, s)
	}
	return out
}

// RequiresFor 返回 resource+action 匹配到的角色组要求。导出的原因是 approve
// 路径需要把"还差什么"讲给人听，而那正是 collectRequires 在算的东西。
func (p *DualSignPolicy) RequiresFor(resource, action string) []string {
	return p.collectRequires(resource, action)
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
