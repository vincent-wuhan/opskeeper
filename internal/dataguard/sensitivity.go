// Package dataguard 实现业务层数据敏感度分级与合规防护。
//
// 路径 A P1-3 阶段 1 任务 1.2 启动 — 5 级 sensitivity enum 定义。
//
// 设计要点：
//   - 5 级 sensitivity（Public / Internal / Confidential / Restricted / TopSecret）
//   - 每级绑定允许的 cmdpolicy 类别与 Casbin action 子集
//   - 公开 API 用 Parse() / IsValid() 防止未识别字符串入库
//   - 比较函数 Compare() 用于与 subject clearance 比对
package dataguard

import (
	"fmt"
	"strings"
)

// Sensitivity 数据敏感度等级（5 级）。
type Sensitivity string

const (
	// Public 公开数据 — 所有 subject 可访问
	Public Sensitivity = "Public"

	// Internal 内部数据 — 所有 employee 可访问
	Internal Sensitivity = "Internal"

	// Confidential 机密数据 — 需要 confidential-reader role
	Confidential Sensitivity = "Confidential"

	// Restricted 受限数据 — 需要 restricted-reader role；写操作需 override
	Restricted Sensitivity = "Restricted"

	// TopSecret 绝密数据 — 默认无人；写操作强制 override + 双人审批
	TopSecret Sensitivity = "TopSecret"
)

// rank 将 sensitivity 映射到整数等级（用于 Compare）。
var rank = map[Sensitivity]int{
	Public:       0,
	Internal:     1,
	Confidential: 2,
	Restricted:   3,
	TopSecret:    4,
}

// Parse 解析字符串为 Sensitivity，未知值返回错误。
func Parse(s string) (Sensitivity, error) {
	v := Sensitivity(strings.TrimSpace(s))
	if _, ok := rank[v]; !ok {
		return "", fmt.Errorf("dataguard: unknown sensitivity %q", s)
	}
	return v, nil
}

// MustParse 解析失败时 panic（仅用于测试或硬编码常量）。
func MustParse(s string) Sensitivity {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// IsValid 检查字符串是否为合法的 sensitivity。
func IsValid(s string) bool {
	_, err := Parse(s)
	return err == nil
}

// Compare 返回 s 与 other 的等级差（s - other）。
//   - s < other → 负值（更敏感）
//   - s == other → 0
//   - s > other → 正值（更宽松）
func (s Sensitivity) Compare(other Sensitivity) int {
	return rank[s] - rank[other]
}

// IsZeroTamper 判断该 sensitivity 是否触发 zero-tamper 模式。
func (s Sensitivity) IsZeroTamper() bool {
	return s == Restricted || s == TopSecret
}

// String 字符串表示。
func (s Sensitivity) String() string {
	return string(s)
}
