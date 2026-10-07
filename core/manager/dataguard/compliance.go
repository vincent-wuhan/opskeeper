// Package dataguard — Data-Guard Phase 2 任务 2.1–2.2：ComplianceTag 结构 + 5 个预设框架。
//
// ComplianceTag 是「资源绑定的合规框架 / 控制项 / 是否强制执行」三元组。
// 通过把 ComplianceTag 数组序列化进 DataSensitivityLabel.ComplianceTags JSON 列，
// 资源可一次性声明所适用的合规体系（PCI-DSS / GDPR / 等保 2.0 三级 / HIPAA / SOC2）。
//
// **Enforced 这个字段今天不强制任何东西。** 它此前被写成"会由 cmdpolicy
// Sandbox 注入硬性约束（audit-log 保留期 1y、字段加密）"——那是设计，不是实现：
// 全仓没有任何代码读这个字段，cmdpolicy 里没有合规概念，而保留期与字段加密
// 两个控制项本身也没有实现可供注入。一个持久化的 `enforced: true` 因此是一枚
// 徽章，徽章不会让人更安全，只会让人以为更安全。真相登记在
// enforcement.go 的 `compliance.enforced-tag` 一行，由
// `make compliance-claims-check` 守着；把哪条控制项接上线，就从那一行里改状态。
package dataguard

import (
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

// 这里曾经有 MarshalComplianceTags / UnmarshalComplianceTags 两个函数，
// 解析与序列化 `[]ComplianceTag`（framework + controls + enforced）。
// **它们一端生产、一端读的不是同一列。**
//
// 写进 `DataSensitivityLabel.ComplianceTags` 的是 label.EncodeJSONTags 产出的
// `[]string`（框架名，字段自己的注释就是这么写的），而 Unmarshal 的是
// `[]ComplianceTag`。读路径那行是 `tags, _ := dataguard.UnmarshalComplianceTags(...)`
// ——错误被丢掉，于是**一个贴了 GDPR 标签的资源，在 effective 列表里永远显示
// 没有标签**，而控制台 POST 上去的那串框架名读回来就没了。
//
// 这一次缺陷是被减法审计挖出来的：MarshalComplianceTags 零生产调用方，
// 而它的孪生 UnmarshalComplianceTags 有一处调用——那一处读的形状和写的
// 形状对不上。**一个只有自己测试在用的写函数，与一个形状写错了的读函数，
// 是一对互相背书的死代码。**
//
// `ComplianceTag` 这个类型本身留下：它是词表的一部分，`controls` 与 `enforced`
// 至今没有任何写入方——那正是登记表 `compliance.enforced-tag` 这一行 declared
// 的内容。留下类型而不是留下两个错的编解码器，是因为**词表可以被登记，
// 错的实现不能**。

// DefaultFrameworkControls 给出 5 框架的推荐控制项（**不强制**）。
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

// Decision 368 deleted ComplianceOverride, which had no reference anywhere in
// the tree -- not even a test -- and a doc comment that said so outright:
// "留作 Phase 2 后续 sub-task". **A struct with no consumer is not a phase**;
// phases live in the ROADMAP, and a struct left in the package reads from the
// inside as a feature somebody can already configure and from the outside as
// nothing at all. This is the same shape as RaisedClass in the same decision,
// one layer down: both were a plan wearing a type's clothes.
