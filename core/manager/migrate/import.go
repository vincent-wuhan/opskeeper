package migrate

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/manager/migrate/clients"
)

// ImportOptions 控制导入行为。
type ImportOptions struct {
	// Snapshot snapshot 文件路径。
	Snapshot string
	// Target opskeeper base URL（必填）。
	Target string
	// Token opskeeper 认证 token。
	Token string
	// TenantMapping ops-keeper project_id → opskeeper tenant_id 映射字符串。
	// 例："42=1,100=2"。必填。
	TenantMapping string
	// Entities 限定导入实体类型；nil = 全部。
	Entities []EntityType
	// DryRun true 时只校验 + 报告，不实际写入。
	DryRun bool
	// RatePerSec 限速（默认 1000 行/秒）。
	RatePerSec int
	// RollbackDir 回滚快照的输出目录；空 = 写在源 snapshot 旁边。
	//
	// 决策 292 起这不是可选项：一次导入如果写了几百行却没留下删除清单，
	// 就没有任何办法把它们撤掉。import 每次真的写入之后都会落一份。
	RollbackDir string
}

// ImportResult 描述一次导入的统计。
type ImportResult struct {
	Total      int                     // 总记录数
	Imported   int                     // 新建数
	Skipped    int                     // 跳过（幂等命中）
	Failed     int                     // 失败数
	Failures   []ImportFailure         // 失败详情（前 100 条）
	CreatedIDs map[EntityType][]string // 新建 ID 列表（rollback 用）

	// RollbackSnapshot 是本次导入写下的回滚快照路径；dry-run 与
	// 一行未写时为空。**它为空就意味着这次导入没法撤。**
	RollbackSnapshot string

	// Unmigratable 按实体类型记下"目标端不存在、因此迁不过去"的行数。
	// 只有 dry-run 会填——真跑的时候这类实体在写入前就被整体拒绝，
	// 不会走到逐行统计那一步。**它为空的 dry-run 报告是可信的；
	// 它非空而报告说"全部可导入"，那份报告在说谎。**
	Unmigratable map[EntityType]int
}

// ImportFailure 描述一次失败。
type ImportFailure struct {
	Entity   EntityType
	SourceID string
	Reason   error
}

// Import 执行导入：读 snapshot → 应用 FieldMap → 写入 opskeeper。
//
// 流程：
//  1. 读 snapshot
//  2. 解析 TenantMapping（必填，缺则失败）
//  3. 按依赖顺序遍历实体
//  4. 对每条记录：
//     a. 应用 FieldMap（ops-keeper → opskeeper）
//     b. 翻译 tenant_id
//     c. 校验目标 tenant 在白名单
//     d. 限速 → 幂等查询 → 创建
//  5. 累计统计
func Import(ctx context.Context, opts ImportOptions) (*ImportResult, error) {
	if opts.Target == "" {
		return nil, fmt.Errorf("--target 必填")
	}
	if opts.Snapshot == "" {
		return nil, fmt.Errorf("--source snapshot 必填")
	}
	if opts.TenantMapping == "" {
		return nil, fmt.Errorf("--tenant-mapping 必填（多租户隔离）")
	}
	if opts.RatePerSec <= 0 {
		opts.RatePerSec = 1000
	}

	snap, err := ReadSnapshot(opts.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("读 snapshot 失败: %w", err)
	}

	mapper, err := ParseTenantMapping(opts.TenantMapping)
	if err != nil {
		return nil, fmt.Errorf("解析 tenant-mapping 失败: %w", err)
	}
	if mapper.Size() == 0 {
		return nil, fmt.Errorf("--tenant-mapping 不能为空（多租户隔离）")
	}

	rateLimiter := NewRateLimiter(opts.RatePerSec)
	defer rateLimiter.Stop()

	client := clients.NewTargetClient(opts.Target, opts.Token)
	result := &ImportResult{
		CreatedIDs: make(map[EntityType][]string),
	}

	entities := opts.Entities
	if len(entities) == 0 {
		entities = MigrationOrder()
	}
	// dry-run 不因为目标端缺失而失败，但**必须把它说出来**：dry-run 的全部
	// 意义就是"这一步会不会成"，而"这一类根本没有目标端"是最该被 dry-run
	// 报出来的一条。决策 293 之前这里直接跳过检查，于是 pg_connections 的
	// 每一行都被记进 result.Imported，CLI 打印「假设可导入: 1」——
	// 一个把必然失败的迁移报成"假设会成功"的 dry-run，比不 dry 更坏。
	if opts.DryRun {
		result.Unmigratable = unmigratableCounts(snap, entities, mapper)
	} else if err := requireImportable(entities); err != nil {
		return nil, err
	}

	for _, et := range entities {
		meta := GetEntityMeta(et)
		if meta == nil {
			return nil, fmt.Errorf("未知实体: %s", et)
		}
		endpoint, endpointErr := targetEndpoint(et)
		if endpointErr != nil && !opts.DryRun {
			return nil, endpointErr
		}
		rows := snap.GetEntity(et)
		for _, row := range rows {
			result.Total++

			// 限速
			if err := rateLimiter.Take(ctx); err != nil {
				return result, err
			}

			// 翻译 tenant_id
			tenantID, err := translateTenant(row, mapper)
			if err != nil {
				result.Failed++
				if len(result.Failures) < 100 {
					result.Failures = append(result.Failures, ImportFailure{
						Entity: et, SourceID: srcID(row), Reason: err,
					})
				}
				continue
			}

			// 应用 FieldMap
			translated := applyFieldMap(row, meta.FieldMap)

			// Dry-run: 不实际写入。目标端不存在的实体不计入 Imported——
			// 它不是"假设会成功"，是"确定不会成功"，而 Unmigratable 里已经
			// 按实体计好数了（决策 293）。
			if opts.DryRun {
				if endpointErr == nil {
					result.Imported++
				}
				continue
			}

			// 幂等校验
			exists, err := client.EntityExists(ctx, endpoint, srcID(row))
			if err != nil {
				result.Failed++
				if len(result.Failures) < 100 {
					result.Failures = append(result.Failures, ImportFailure{
						Entity: et, SourceID: srcID(row), Reason: err,
					})
				}
				continue
			}
			if exists {
				result.Skipped++
				continue
			}

			// 写入 opskeeper
			createdID, err := client.CreateEntity(ctx, endpoint, tenantID, translated)
			if err != nil {
				result.Failed++
				if len(result.Failures) < 100 {
					result.Failures = append(result.Failures, ImportFailure{
						Entity: et, SourceID: srcID(row), Reason: err,
					})
				}
				continue
			}
			result.Imported++
			result.CreatedIDs[et] = append(result.CreatedIDs[et], createdID)
		}
	}

	// 落回滚快照。放在循环之外、返回之前：哪怕导入只成功了一部分，已经写
	// 进去的那些行也必须有一份删除清单——最需要回滚的恰恰是"导到一半"
	// 这种情形。
	if len(result.CreatedIDs) > 0 {
		dir := opts.RollbackDir
		if dir == "" {
			dir = filepath.Dir(opts.Snapshot)
		}
		path, err := SaveRollbackSnapshot(BuildRollbackSnapshot(opts.Target, result.CreatedIDs), dir)
		if err != nil {
			return result, fmt.Errorf("写回滚快照失败（导入已完成，但撤不回去）: %w", err)
		}
		result.RollbackSnapshot = path
	}
	return result, nil
}

// translateTenant 从记录中提取 ops-keeper project_id 并翻译成 opskeeper tenant_id。
func translateTenant(row map[string]any, mapper *TenantMapper) (int64, error) {
	pidAny, ok := row["project_id"]
	if !ok {
		return 0, fmt.Errorf("记录缺少 project_id 字段")
	}
	pid, ok := pidAny.(float64) // JSON 数字默认解码为 float64
	if !ok {
		// 兼容 int64 等
		if pid2, ok := pidAny.(int64); ok {
			pid = float64(pid2)
		} else {
			return 0, fmt.Errorf("project_id 类型非数字: %T", pidAny)
		}
	}
	return mapper.Map(int64(pid))
}

// applyFieldMap 把 ops-keeper 字段翻译成 opskeeper 字段。
func applyFieldMap(row map[string]any, fieldMap map[string]string) map[string]any {
	out := make(map[string]any, len(fieldMap))
	for srcField, dstField := range fieldMap {
		if v, ok := row[srcField]; ok {
			out[dstField] = v
		}
	}
	// 保留原始 source_id 字段（幂等键）
	if id, ok := row["id"]; ok {
		out["source_id"] = id
	}
	return out
}

// srcID 提取记录 source id。
func srcID(row map[string]any) string {
	if v, ok := row["id"]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// targetEndpoint 返回 opskeeper 的写入端点，形如 "/v1/users"。
//
// 它读的是 TargetRoute 而不是 Target。两个字段曾经只有一个，Target 是
// 自由文本，于是 "tenants" / "schedules" / "middleware_resources" 被
// 原样拼进 URL——这三个名字在 manager 的路由表里都没有对应物，import
// 会为每一行记一条 "失败: 404"，而 404 被记成一行数据错误，不是一个
// 端点不存在的事实（决策 291）。
func targetEndpoint(et EntityType) (string, error) {
	meta := GetEntityMeta(et)
	if meta == nil {
		return "", fmt.Errorf("未知实体: %s", et)
	}
	if !meta.IsImportable() {
		return "", fmt.Errorf("%s 无法导入 opskeeper：%s", et, meta.TargetMissing)
	}
	return meta.TargetRoute, nil
}

// requireImportable 在动任何一行数据之前先问一遍端点存不存在。
//
// 一个迁移工具对"目标端没有这个实体"say 出来的必须是这件事本身，而不是
// 把它摊成 N 条逐行的失败——后者读起来像数据脏，前者才是真的。
func requireImportable(entities []EntityType) error {
	var missing []string
	for _, et := range entities {
		meta := GetEntityMeta(et)
		if meta == nil {
			return fmt.Errorf("未知实体: %s", et)
		}
		if !meta.IsImportable() {
			missing = append(missing, fmt.Sprintf("%s → %s（%s）", et, meta.Target, meta.TargetMissing))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("以下实体的目标端在 opskeeper 里不存在，未执行任何写入:\n  - %s\n"+
		"用 --entity 只选可导入的实体（%s），或先决定这些实体的落点。",
		strings.Join(missing, "\n  - "), strings.Join(importableEntityStrings(), ", "))
}

// ImportableEntities 返回当前真的能导入的实体类型（依赖顺序）。
func ImportableEntities() []EntityType {
	var out []EntityType
	for _, et := range MigrationOrder() {
		if GetEntityMeta(et).IsImportable() {
			out = append(out, et)
		}
	}
	return out
}

func importableEntityStrings() []string {
	var out []string
	for _, et := range ImportableEntities() {
		out = append(out, string(et))
	}
	return out
}

// unmigratableCounts 数出 dry-run 里那些"目标端不存在"的实体各有多少行。
//
// 判据与真跑时一致：TargetRoute 为空。租户映射不成立的行不计入——
// 那是另一个原因（源数据引用了一个没声明的 project），混进来会把这个
// 事实藏起来。
func unmigratableCounts(snap *Snapshot, entities []EntityType, mapper *TenantMapper) map[EntityType]int {
	out := map[EntityType]int{}
	for _, et := range entities {
		if meta := GetEntityMeta(et); meta.IsImportable() {
			continue
		}
		n := 0
		for _, row := range snap.GetEntity(et) {
			if _, err := translateTenant(row, mapper); err != nil {
				continue
			}
			n++
		}
		if n > 0 {
			out[et] = n
		}
	}
	return out
}
