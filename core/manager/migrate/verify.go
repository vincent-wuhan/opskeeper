package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/manager/migrate/clients"
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
	MatchedBySourceID map[EntityType]int              // 命中数（按 source_id）
	MissingInTarget   map[EntityType][]map[string]any // 目标缺失
	FieldDiffs        []FieldDiff                     // 字段值差异

	// Unchecked 记"因为查询失败而没能核对"的行数。
	//
	// 决策 293 之前这里只有一个 `continue`：查目标端失败与"目标端没有这条"
	// 走到同一个分支，而前者是一个网络错误。后者会让 MissingInTarget 长出
	// 一条"缺失"，前者什么都不长——**一份 verify 报告因此可以在一次都没
	// 核对成功的情况下印出「✅ 全部命中」**。
	Unchecked map[EntityType]int
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
		// 直接从两端拉，不落盘。
		//
		// 决策 293 之前这里先调了一次 Export 只是为了"重新拉取"，而 Export
		// 强制要求 --output，于是 verify --source <URL> 这条**文档里写的**
		// 用法必然以「--output 必填」失败。代码里那句"Export 写文件模式。
		// Verify 改为直接用客户端拉"的注释就写在这次调用的下面——**注释
		// 说出了要修什么，而修复没做，于是注释成了这段代码的病历**。
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
		Unchecked:         make(map[EntityType]int),
	}

	entities := opts.Entities
	if len(entities) == 0 {
		entities = MigrationOrder()
	}
	if err := requireImportable(entities); err != nil {
		return nil, err
	}

	for _, et := range entities {
		rows := snap.GetEntity(et)
		result.TotalSource += len(rows)
		endpoint, err := targetEndpoint(et)
		if err != nil {
			return nil, err
		}
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
			// 通过 by-source-id 取回目标端那一条：verify 要比的是内容，
			// 不是"在不在"。只问存在性的话，一行写错字段的记录照样算命中。
			fetched, err := client.FetchEntity(ctx, endpoint, srcIDStr)
			switch {
			case errors.Is(err, clients.ErrTargetEntityNotFound):
				result.MissingInTarget[et] = append(result.MissingInTarget[et], row)
				continue
			case err != nil:
				result.Unchecked[et]++
				continue
			}
			result.MatchedBySourceID[et]++
			result.FieldDiffs = append(result.FieldDiffs, diffFields(et, row, fetched, meta.FieldMap)...)
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
		Unchecked:         make(map[EntityType]int),
	}

	entities := opts.Entities
	if len(entities) == 0 {
		entities = MigrationOrder()
	}
	if err := requireImportable(entities); err != nil {
		return nil, err
	}

	for _, et := range entities {
		endpoint, err := targetEndpoint(et)
		if err != nil {
			return nil, err
		}
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
			meta := GetEntityMeta(et)
			fetched, err := dst.FetchEntity(ctx, endpoint, srcIDStr)
			switch {
			case errors.Is(err, clients.ErrTargetEntityNotFound):
				result.MissingInTarget[et] = append(result.MissingInTarget[et], row)
				continue
			case err != nil:
				result.Unchecked[et]++
				continue
			}
			result.MatchedBySourceID[et]++
			if meta != nil {
				result.FieldDiffs = append(result.FieldDiffs, diffFields(et, row, fetched, meta.FieldMap)...)
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

	out += "\n按实体类型：\n"
	for _, et := range MigrationOrder() {
		matched := r.MatchedBySourceID[et]
		missing := len(r.MissingInTarget[et])
		unchecked := r.Unchecked[et]
		if matched == 0 && missing == 0 && unchecked == 0 {
			continue
		}
		line := fmt.Sprintf("  %s: 命中 %d, 缺失 %d", et, matched, missing)
		if unchecked > 0 {
			line += fmt.Sprintf(", 未核对 %d", unchecked)
		}
		out += line + "\n"
	}
	out += skippedEntityReport()

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
	uncheckedTotal := sumMap(r.Unchecked)
	// 三个分支，不是两个。没核对过的行既不是命中也不是缺失，而把一份
	// 一次都没核对成功的报告印成「✅ 全部命中」，是这份报告能说出的最坏的
	// 一句话（决策 293）。
	switch {
	case missingTotal > 0:
		out += fmt.Sprintf("\n⚠️  缺失 %d 条\n", missingTotal)
	case uncheckedTotal > 0:
		out += fmt.Sprintf("\n⚠️  有 %d 条没能核对（查询目标端失败），"+
			"这份报告不构成「迁移成功」的结论\n", uncheckedTotal)
	default:
		out += "\n✅ 全部命中\n"
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

// skippedEntityReport 把"目标端不存在因而根本没查"的实体列出来。
//
// 不列的话，一份 9 类实体的 verify 报告会只印出查过的那几类，读起来
// 像"其余的都命中了"——被跳过的和核对过的在纸面上无法区分（决策 291）。
func skippedEntityReport() string {
	var missing []string
	for _, et := range MigrationOrder() {
		if meta := GetEntityMeta(et); !meta.IsImportable() {
			missing = append(missing, fmt.Sprintf("  %s → %s：%s", et, meta.Target, meta.TargetMissing))
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("\n未核对（opskeeper 里没有对应端点）：%d 类\n%s\n",
		len(missing), strings.Join(missing, "\n"))
}

// diffFields 逐字段比对一条源记录与它在目标端的那一条。
//
// 它只比 FieldMap 声明过的字段：源里还有 id / created_at 这类服务端自己
// 分配的字段，它们与目标端的值本来就不该相等，比了只会产出一份永远对不上的
// 差异清单。数字按数值比——JSON 解码出来的 5432 是 float64，而源里可能是
// int64，不归一化的话每一条都会假报差异。
func diffFields(et EntityType, src, dst map[string]any, fieldMap map[string]string) []FieldDiff {
	var out []FieldDiff
	sourceID := srcID(src)
	names := make([]string, 0, len(fieldMap))
	for from := range fieldMap {
		names = append(names, from)
	}
	sort.Strings(names)
	for _, from := range names {
		to := fieldMap[from]
		srcVal, ok := src[from]
		if !ok {
			continue
		}
		dstVal, ok := dst[to]
		if !ok || sameValue(srcVal, dstVal) {
			continue
		}
		out = append(out, FieldDiff{
			Entity: et, SourceID: sourceID, Field: to,
			SourceVal: srcVal, TargetVal: dstVal,
		})
	}
	return out
}

// sameValue 判断两个解码后的值是否相等，数字不区分 int64 与 float64。
func sameValue(a, b any) bool {
	an, aok := asNumber(a)
	bn, bok := asNumber(b)
	if aok && bok {
		return an == bn
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
