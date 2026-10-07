package migrate

import (
	"fmt"
	"strconv"
	"strings"
)

// TenantMapper 把 ops-keeper project_id 翻译成 opskeeper tenant_id。
//
// 多租户隔离的核心：禁止跨租户写入；同租户映射必须显式声明。
// 典型用法：
//
//	mapper, _ := ParseTenantMapping("42=1,100=2")  // ops-keeper 42 → opskeeper 1
//	tid := mapper.Map(42)                          // 1
//	tid = mapper.Map(99)                           // panic（未声明）
type TenantMapper struct {
	mapping map[int64]int64
	source  string
}

// ParseTenantMapping 解析 "ops=target,ops2=target2" 形式的映射。
//
// 接受：
//   - "42=1"               单条
//   - "42=1,100=2,200=3"   多条
//   - 空格容忍
//
// 拒绝：
//   - "abc=1"              源 ID 非数字
//   - "42=abc"             目标 ID 非数字
//   - "42=1,42=2"          重复源
//   - "42=1,"              末尾逗号
func ParseTenantMapping(s string) (*TenantMapper, error) {
	m := &TenantMapper{
		mapping: make(map[int64]int64),
		source:  s,
	}
	if strings.TrimSpace(s) == "" {
		return m, nil
	}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			return nil, fmt.Errorf("映射表达式包含空对（末尾多余逗号?）: %q", s)
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("映射表达式缺少 '=': %q", pair)
		}
		srcStr := strings.TrimSpace(parts[0])
		dstStr := strings.TrimSpace(parts[1])
		src, err := strconv.ParseInt(srcStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ops-keeper project_id 非数字: %q", srcStr)
		}
		dst, err := strconv.ParseInt(dstStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("opskeeper tenant_id 非数字: %q", dstStr)
		}
		if _, dup := m.mapping[src]; dup {
			return nil, fmt.Errorf("ops-keeper project_id 重复声明: %d", src)
		}
		m.mapping[src] = dst
	}
	return m, nil
}

// Map 翻译 ops-keeper project_id → opskeeper tenant_id。
// 未声明则返回错误（避免默认 0 = 跨租户）。
func (m *TenantMapper) Map(opsKeeperProjectID int64) (int64, error) {
	tid, ok := m.mapping[opsKeeperProjectID]
	if !ok {
		return 0, fmt.Errorf("ops-keeper project_id %d 未在 --tenant-mapping 中声明", opsKeeperProjectID)
	}
	return tid, nil
}

// MustMap 类似 Map，错误时 panic（用于已知有效场景）。
func (m *TenantMapper) MustMap(opsKeeperProjectID int64) int64 {
	tid, err := m.Map(opsKeeperProjectID)
	if err != nil {
		panic(err)
	}
	return tid
}

// Size 返回声明的映射条数。
func (m *TenantMapper) Size() int {
	return len(m.mapping)
}

// String 返回原始字符串（用于日志）。
func (m *TenantMapper) String() string {
	return m.source
}

// ValidateTenant 校验 opskeeper 写入时的 tenant_id 是否在白名单中。
//
// 用于防御性检查：即使 export 阶段没漏，import 阶段仍校验写入目标 tenant。
// 返回 true 表示允许写入。
func (m *TenantMapper) ValidateTenant(opskeeperTenantID int64) bool {
	for _, t := range m.mapping {
		if t == opskeeperTenantID {
			return true
		}
	}
	return false
}
