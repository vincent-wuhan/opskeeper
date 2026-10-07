package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/migrate/clients"
)

// RollbackOptions 控制回滚行为。
type RollbackOptions struct {
	// SnapshotPath 回滚快照路径。
	// 命名约定：rollback-snapshot-{YYYY-MM-DDTHH-MM-SS}.json
	SnapshotPath string
	// Target opskeeper base URL。
	Target string
	// Token opskeeper 认证 token。
	Token string
	// DryRun true 时只报告将删除的实体，不实际删除。
	DryRun bool
}

// RollbackResult 描述一次回滚的统计。
type RollbackResult struct {
	Total     int
	Deleted   int
	Skipped   int
	Failed    int
	Failures  []RollbackFailure
	DeletedAt time.Time
}

// RollbackFailure 描述一次回滚失败。
type RollbackFailure struct {
	Entity EntityType
	ID     string
	Reason error
}

// BuildRollbackSnapshot 把一次导入真正新建出来的 ID 变成一份可回滚的快照。
//
// 决策 292 之前这条链是断的：Rollback 从快照里读每行的 "_id"，
// GenerateRollbackSnapshot 却只返回一份空快照，而 import 从不把它拿到的
// CreatedIDs 写进任何文件——**所以 `opskeeper-migrate rollback` 永远读到 0 行，
// 报一句"总计: 0"然后成功退出**。一个永远删不掉任何东西的回滚命令，
// 与没有回滚命令的区别只在于它让人以为有。
//
// 快照的形状是 entity type → [{"_id": "<opskeeper 里的 ID>"}]，也就是 Rollback
// 读的那一种。这里显式说明这个约定，而不是让两边各自猜：它是这个文件格式里
// 唯一一处"行里只有一个下划线开头字段"的地方。
func BuildRollbackSnapshot(target string, created map[EntityType][]string) *Snapshot {
	snap := NewSnapshot(target, "", nil)
	// 按 EntityType 的字母序写，让同一份结果每次产生同一个文件内容。
	types := make([]EntityType, 0, len(created))
	for et := range created {
		types = append(types, et)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	for _, et := range types {
		rows := make([]map[string]any, 0, len(created[et]))
		for _, id := range created[et] {
			rows = append(rows, map[string]any{"_id": id})
		}
		snap.PutEntity(et, rows)
	}
	return snap
}

// SaveRollbackSnapshot 把 rollback snapshot 写到磁盘。
//
// 命名约定：rollback-snapshot-{YYYY-MM-DDTHH-MM-SS}.json
//
// 秒级的时间戳在两次导入落在同一秒时会互相覆盖——而"覆盖掉上一份回滚快照"
// 正是这个工具最不能做的事：它删掉的是刚刚写进生产的数据的删除清单。文件名
// 冲突时追加一个递增后缀，宁可名字长得难看。
func SaveRollbackSnapshot(snap *Snapshot, dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	ts := snap.Header.ExportedAt.UTC().Format("2006-01-02T15-04-05")
	base := fmt.Sprintf("rollback-snapshot-%s", ts)
	path := filepath.Join(dir, base+".json")
	for i := 2; ; i++ {
		if _, err := os.Stat(path); err != nil {
			break
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.json", base, i))
	}
	if err := snap.WriteTo(path); err != nil {
		return "", err
	}
	return path, nil
}

// entitiesIn 列出一份快照里出现过的实体类型（字母序）。
func entitiesIn(snap *Snapshot) []EntityType {
	out := make([]EntityType, 0, len(snap.Entities))
	for et := range snap.Entities {
		out = append(out, et)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// rollbackOrder 把快照里的实体按**导入顺序的逆序**排好。
//
// 导入先建被依赖的、再建依赖它的，回滚必须反过来：先删 orgs 再删 users。
// 快照里出现的实体按 MigrationOrder 定位，取不到位置的排在最后——它们不在
// 注册表里，端点解析那一步会负责报错。
func rollbackOrder(snap *Snapshot) []EntityType {
	order := MigrationOrder()
	rank := make(map[EntityType]int, len(order))
	for i, et := range order {
		rank[et] = i
	}
	present := entitiesIn(snap)
	sort.Slice(present, func(i, j int) bool {
		ri, oki := rank[present[i]]
		rj, okj := rank[present[j]]
		switch {
		case oki && okj:
			return ri > rj
		case oki:
			return true
		case okj:
			return false
		default:
			return present[i] < present[j]
		}
	})
	return present
}

// Rollback 执行回滚：从 rollback snapshot 中读取 created IDs，逐个删除。
//
// 流程：
//  1. 读 rollback snapshot
//  2. 对每个 entity type 的 created ID 列表
//  3. 调用 TargetClient.DeleteEntity
//  4. 累计统计
func Rollback(ctx context.Context, opts RollbackOptions) (*RollbackResult, error) {
	if opts.SnapshotPath == "" {
		return nil, fmt.Errorf("--rollback-snapshot 必填")
	}
	if opts.Target == "" {
		return nil, fmt.Errorf("--target 必填")
	}

	snap, err := ReadSnapshot(opts.SnapshotPath)
	if err != nil {
		return nil, fmt.Errorf("读 rollback snapshot 失败: %w", err)
	}

	client := clients.NewTargetClient(opts.Target, opts.Token)
	result := &RollbackResult{
		DeletedAt: time.Now().UTC(),
	}

	// 回滚只删得掉当初真的写进去过的东西，所以它按导入端的同一份
	// TargetRoute 解析端点：目标端不存在的实体在导入时就被挡住了，
	// 这里同样不拼一个必然 404 的 URL（决策 291）。
	//
	// 判据取自快照里实际出现的实体类型，而不是注册表的全体——一份
	// 只含 users 的回滚快照不该因为 pg_connections 没有落点而拒绝回滚。
	if err := requireImportable(entitiesIn(snap)); err != nil {
		return nil, err
	}

	// 删除顺序 = 导入顺序的逆序。导入先建 users 再建依赖它们的 orgs，
	// 回滚就先把 orgs 删掉；反过来删会撞上外键，或者留下一个指向已删用户的
	// 组织。map 的遍历顺序是随机的，所以这里显式按 MigrationOrder 排。
	for _, et := range rollbackOrder(snap) {
		ids := snap.Entities[et]
		endpoint, eerr := targetEndpoint(et)
		if eerr != nil {
			return nil, eerr
		}
		for _, row := range ids {
			if id, ok := row["_id"].(string); ok {
				result.Total++
				if opts.DryRun {
					result.Deleted++
					continue
				}
				if err := client.DeleteEntity(ctx, endpoint, id); err != nil {
					result.Failed++
					if len(result.Failures) < 100 {
						result.Failures = append(result.Failures, RollbackFailure{
							Entity: et, ID: id, Reason: err,
						})
					}
					continue
				}
				result.Deleted++
			}
		}
	}
	return result, nil
}

// IsRollbackSnapshotPath 判定路径是否符合 rollback snapshot 命名约定。
func IsRollbackSnapshotPath(path string) bool {
	base := filepath.Base(path)
	return len(base) > 18 && base[:18] == "rollback-snapshot-"
}

// ListRollbackSnapshots 列出目录下所有 rollback snapshot（按时间倒序）。
func ListRollbackSnapshots(dir string) ([]SnapshotMeta, error) {
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []SnapshotMeta
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !IsRollbackSnapshotPath(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		meta, err := InspectSnapshot(path)
		if err != nil {
			continue // 跳过损坏文件
		}
		meta.IsRollback = true
		out = append(out, *meta)
	}
	// 按 ExportedAt 倒序
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Header.ExportedAt.After(out[i].Header.ExportedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}
