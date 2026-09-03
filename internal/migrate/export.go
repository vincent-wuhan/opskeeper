package migrate

import (
	"context"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/internal/migrate/clients"
)

// ExportOptions 控制导出行为。
type ExportOptions struct {
	// Entities 指定要导出的实体类型；nil = 全部 9 类。
	Entities []EntityType
	// Output 快照输出路径（.json 或 .json.gz）。
	Output string
	// Source ops-keeper base URL（必填）。
	Source string
	// Token ops-keeper 认证 token。
	Token string
}

// Export 执行导出：拉取 ops-keeper 数据 → 写 snapshot 文件。
//
// 流程：
//  1. 创建空 Snapshot（带 header）
//  2. 按依赖顺序遍历实体类型
//  3. 对每个实体调用 OpsKeeperClient.ListAll
//  4. 累积到 Snapshot.Entities
//  5. 写文件
func Export(ctx context.Context, opts ExportOptions) (*Snapshot, error) {
	if opts.Source == "" {
		return nil, fmt.Errorf("--source 必填")
	}
	if opts.Output == "" {
		return nil, fmt.Errorf("--output 必填")
	}

	entities := opts.Entities
	if len(entities) == 0 {
		entities = MigrationOrder()
	} else {
		// 校验 + 按依赖顺序排序
		order := MigrationOrder()
		set := make(map[EntityType]bool, len(entities))
		for _, e := range entities {
			set[e] = true
		}
		entities = nil
		for _, e := range order {
			if set[e] {
				entities = append(entities, e)
			}
		}
	}

	client := clients.NewOpsKeeperClient(opts.Source, opts.Token)
	snap := NewSnapshot(opts.Source, "", nil)

	for _, et := range entities {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := client.ListAll(ctx, string(et))
		if err != nil {
			return nil, fmt.Errorf("导出 %s 失败: %w", et, err)
		}
		snap.PutEntity(et, rows)
	}

	if err := snap.WriteTo(opts.Output); err != nil {
		return nil, fmt.Errorf("写 snapshot 失败: %w", err)
	}
	return snap, nil
}
