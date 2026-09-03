// chatdiagnose/store/qdrant_collection.go — incident_pattern collection 生命周期管理。
//
// 启动时 EnsureCollection(dim) + EnsurePayloadIndex(tenant_id, fingerprint)；
// 多租户 + dedup 查询必需这两个 payload index（无 index 时 qdrant 全扫）。

package store

import (
	"context"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/qdrantx"
)

// QdrantCollection 是 incident_pattern collection 的启动期生命周期包装。
type QdrantCollection struct {
	Client *qdrantx.Client
	Name   string
	Dim    int
}

// NewQdrantCollection 构造 collection wrapper。dim 来自 Embedder.Dim()。
func NewQdrantCollection(c *qdrantx.Client, dim int) *QdrantCollection {
	return &QdrantCollection{Client: c, Name: "incident_pattern", Dim: dim}
}

// Ensure 幂等创建 collection + payload index。启动时调用一次。
// payload 索引字段：
//   - tenant_id   (keyword): 多租户强制过滤
//   - fingerprint (keyword): dedup 查询
//   - pattern_id  (integer): MySQL 反查
func (q *QdrantCollection) Ensure(ctx context.Context) error {
	if err := q.Client.EnsureCollection(ctx, q.Name, q.Dim); err != nil {
		return fmt.Errorf("chatdiagnose: ensure collection %s: %w", q.Name, err)
	}
	if err := q.Client.EnsurePayloadIndex(ctx, q.Name, "tenant_id", "keyword"); err != nil {
		return fmt.Errorf("chatdiagnose: ensure index tenant_id: %w", err)
	}
	if err := q.Client.EnsurePayloadIndex(ctx, q.Name, "fingerprint", "keyword"); err != nil {
		return fmt.Errorf("chatdiagnose: ensure index fingerprint: %w", err)
	}
	if err := q.Client.EnsurePayloadIndex(ctx, q.Name, "pattern_id", "integer"); err != nil {
		return fmt.Errorf("chatdiagnose: ensure index pattern_id: %w", err)
	}
	return nil
}
