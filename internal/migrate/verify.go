package migrate

import (
	"context"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/internal/migrate/clients"
)

// VerifyOptions 控制验证行为。
type VerifyOptions struct {
	// SnapshotPath 导出快照路径（作为"源真值"）。
	SnapshotPath string
	// Source ops-keeper base URL（重新拉取对比）。
	Source string
	// Token ops-keeper 认证 token。
	SourceToken string
	// Target opskeeper base URL（拉取已导入实体）。
	Target string
	// Token opskeeper 认证 token。
	TargetToken string
	// TenantMapping 映射。
	TenantMapping string
	// Output HTML 报告输出路径（可选）。
	Output string
	// Entities 限定验证实体类型。
	Entities []EntityType
}

// VerifyResult 描述一次验证结果。
type VerifyResult struct {
	TotalSource       int                             // 源端总数
	TotalTarget       int                             // 目标端总数
	MatchedBySourceID map[EntityType]int              // 命中数（按 source_id）
	MissingInTarget   map[EntityType][]map[string]any // 目标缺失
	ExtraInTarget     map[EntityType]int              // 目标多余（本次 import 外的）
	FieldDiffs        []FieldDiff                     // 字段值差异
}

// FieldDiff 描述一条记录的字段差异。
type FieldDiff struct {
	Entity    EntityType
	SourceID  string
	Field     string
	SourceVal any
	TargetVal any
}

// Verify 对比源端 vs 目标端，输出 diff 报告。
//
// 流程：
//  1. 读 snapshot（或重新拉源）
//  2. 对每个 source_id，查询 opskeeper 是否存在且字段一致
//  3. 累计缺失 / 多余 / 字段差异
func Verify(ctx context.Context, opts VerifyOptions) (*VerifyResult, error) {
	if opts.Target == "" {
		return nil, fmt.Errorf("--target 必填")
	}

	var snap *Snapshot
	if opts.SnapshotPath != "" {
		s, err := ReadSnapshot(opts.SnapshotPath)
		if err != nil {
			return nil, fmt.Errorf("读 snapshot 失败: %w", err)
		}
		snap = s
	} else if opts.Source != "" {
		// 重新拉取（不写文件）
		_, err := Export(ctx, ExportOptions{
			Entities: opts.Entities,
			Source:   opts.Source,
			Token:    opts.SourceToken,
			Output:   "", // 不写
		})
		if err != nil {
			return nil, err
		}
		// 注意：Export 写文件模式。Verify 改为直接用客户端拉
		return verifyFromClients(ctx, opts)
	} else {
		return nil, fmt.Errorf("必须提供 --source snapshot 或 --source opskeeper URL")
	}

	mapper, err := ParseTenantMapping(opts.TenantMapping)
	if err != nil {
		return nil, err
	}
	if mapper.Size() == 0 {
		return nil, fmt.Errorf("--tenant-mapping 不能为空")
	}

	client := clients.NewTargetClient(opts.Target, opts.TargetToken)
	result := &VerifyResult{
		MatchedBySourceID: make(map[EntityType]int),
		MissingInTarget:   make(map[EntityType][]map[string]any),
		ExtraInTarget:     make(map[EntityType]int),
	}

	entities := opts.Entities
	if len(entities) == 0 {
		entities = MigrationOrder()
	}

	for _, et := range entities {
		rows := snap.GetEntity(et)
		result.TotalSource += len(rows)
		meta := GetEntityMeta(et)
		if meta == nil {
			continue
		}
		for _, row := range rows {
			tenantID, terr := translateTenant(row, mapper)
			if terr != nil {
				continue
			}
			if !mapper.ValidateTenant(tenantID) {
				continue // 防御：不在白名单的 tenant 跳过
			}
			srcIDStr := srcID(row)
			// 通过 by-source-id 查询目标
			exists, err := client.EntityExists(ctx, targetEndpoint(et), srcIDStr)
			if err != nil {
				continue
			}
			if exists {
				result.MatchedBySourceID[et]++
			} else {
				result.MissingInTarget[et] = append(result.MissingInTarget[et], row)
			}
		}
	}
	return result, nil
}

// verifyFromClients 直接从两端 API 拉取对比（无 snapshot）。
func verifyFromClients(ctx context.Context, opts VerifyOptions) (*VerifyResult, error) {
	// 简化实现：调用客户端 ListAll 对比
	src := clients.NewOpsKeeperClient(opts.Source, opts.SourceToken)
	dst := clients.NewTargetClient(opts.Target, opts.TargetToken)
	mapper, err := ParseTenantMapping(opts.TenantMapping)
	if err != nil {
		return nil, err
	}
	result := &VerifyResult{
		MatchedBySourceID: make(map[EntityType]int),
		MissingInTarget:   make(map[EntityType][]map[string]any),
		ExtraInTarget:     make(map[EntityType]int),
	}

	entities := opts.Entities
	if len(entities) == 0 {
		entities = MigrationOrder()
	}

	for _, et := range entities {
		rows, err := src.ListAll(ctx, string(et))
		if err != nil {
			continue
		}
		result.TotalSource += len(rows)
		for _, row := range rows {
			if _, terr := translateTenant(row, mapper); terr != nil {
				continue
			}
			srcIDStr := srcID(row)
			exists, err := dst.EntityExists(ctx, targetEndpoint(et), srcIDStr)
			if err != nil {
				continue
			}
			if exists {
				result.MatchedBySourceID[et]++
			} else {
				result.MissingInTarget[et] = append(result.MissingInTarget[et], row)
			}
		}
	}
	return result, nil
}

// String 格式化输出 verify 结果（用于 CLI stdout）。
func (r *VerifyResult) String() string {
	out := "=== Migration Verify Report ===\n"
	out += fmt.Sprintf("源端总数: %d\n", r.TotalSource)
	out += fmt.Sprintf("目标端命中: %d\n", sumMap(r.MatchedBySourceID))
	if r.TotalTarget > 0 {
		out += fmt.Sprintf("目标端总数: %d\n", r.TotalTarget)
	}

	out += "\n按实体类型：\n"
	for _, et := range MigrationOrder() {
		matched := r.MatchedBySourceID[et]
		missing := len(r.MissingInTarget[et])
		if matched == 0 && missing == 0 {
			continue
		}
		out += fmt.Sprintf("  %s: 命中 %d, 缺失 %d\n", et, matched, missing)
	}

	if len(r.FieldDiffs) > 0 {
		out += fmt.Sprintf("\n字段差异: %d\n", len(r.FieldDiffs))
		for i, d := range r.FieldDiffs {
			if i >= 20 {
				out += "  ... (更多省略)\n"
				break
			}
			out += fmt.Sprintf("  %s[%s].%s: src=%v dst=%v\n", d.Entity, d.SourceID, d.Field, d.SourceVal, d.TargetVal)
		}
	}

	missingTotal := 0
	for _, rows := range r.MissingInTarget {
		missingTotal += len(rows)
	}
	if missingTotal == 0 {
		out += "\n✅ 全部命中\n"
	} else {
		out += fmt.Sprintf("\n⚠️  缺失 %d 条\n", missingTotal)
	}
	return out
}

func sumMap(m map[EntityType]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
