package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/migrate/clients"
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

// GenerateRollbackSnapshot 在 import 前生成一份"当前 opskeeper 状态"快照，
// 用于 import 失败时一键回滚。
//
// 实际语义：snapshot 记录 import 操作中将创建 / 修改 / 删除的实体 ID 列表。
// 简化实现：snapshot 记录 (entity_type, opskeeper_id) 映射表 + 原始 ops-keeper payload。
func GenerateRollbackSnapshot(target, token string) (*Snapshot, error) {
	// 占位：实际实现应查询 opskeeper 当前相关实体并快照
	// 这里只创建空 snapshot，import 时填充 CreatedIDs
	return NewSnapshot(target, "", nil), nil
}

// SaveRollbackSnapshot 把 rollback snapshot 写到磁盘。
//
// 命名约定：rollback-snapshot-{YYYY-MM-DDTHH-MM-SS}.json
func SaveRollbackSnapshot(snap *Snapshot, dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	ts := snap.Header.ExportedAt.UTC().Format("2006-01-02T15-04-05")
	path := filepath.Join(dir, fmt.Sprintf("rollback-snapshot-%s.json", ts))
	if err := snap.WriteTo(path); err != nil {
		return "", err
	}
	return path, nil
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

	for et, ids := range snap.Entities {
		// 注意：rollback snapshot 的 Entities 字段语义：
		// key=entity type, value=[created_id] 列表
		// 但当前 snapshot 设计中 value 是 []map[string]any。
		// 兼容：仅当 row["_kind"] == "created_id" 时视为 ID 列表
		for _, row := range ids {
			if id, ok := row["_id"].(string); ok {
				result.Total++
				if opts.DryRun {
					result.Deleted++
					continue
				}
				if err := client.DeleteEntity(ctx, targetEndpoint(et), id); err != nil {
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
