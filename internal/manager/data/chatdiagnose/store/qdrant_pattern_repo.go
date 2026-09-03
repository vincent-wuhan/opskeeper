// chatdiagnose/store/qdrant_pattern_repo.go — incident_pattern 向量搜索层（Qdrant）。
//
// 设计：
//   - 仅存向量 + payload；metadata 在 MySQL（PatternMeta），Search 拿到 pattern_id 后回查
//   - 多租户强制：Search MUST MustMatch={"tenant_id": T}
//   - Upsert 用 MySQL auto_increment id 转 uint64 作为 Point ID（同 pattern_id 重复 Upsert 覆盖）
//   - threshold 默认 0.85（spec §"KB 命中阈值"）

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/qdrantx"
)

// PatternHit 是 Qdrant Search 的语义化结果。
type PatternHit struct {
	Score         float64
	PatternID     int64
	Fingerprint   string
	PostmortemRef string
	Confidence    float64
	Severity      string
	ResourceType  string
}

// QdrantPatternRepo 是 incident_pattern collection 的 Qdrant 持久化层。
type QdrantPatternRepo struct {
	client *qdrantx.Client
	coll   *QdrantCollection
}

// NewQdrantPatternRepo 构造时 EnsureCollection + payload index（幂等）。
// dim 由 Embedder.Dim() 传入；Qdrant collection 自动按 dim 建。
func NewQdrantPatternRepo(ctx context.Context, c *qdrantx.Client, dim int) (*QdrantPatternRepo, error) {
	if c == nil {
		return nil, errors.New("chatdiagnose: NewQdrantPatternRepo requires qdrant client")
	}
	if dim <= 0 {
		return nil, errors.New("chatdiagnose: NewQdrantPatternRepo requires dim > 0")
	}
	coll := NewQdrantCollection(c, dim)
	if err := coll.Ensure(ctx); err != nil {
		return nil, fmt.Errorf("chatdiagnose: QdrantPatternRepo ensure: %w", err)
	}
	return &QdrantPatternRepo{client: c, coll: coll}, nil
}

// Upsert 写入向量 + payload。
// patternID > 0：用作 Qdrant Point ID（uint64）。
// payload MUST 包含 tenant_id（必填，多租户过滤）。
func (q *QdrantPatternRepo) Upsert(ctx context.Context, patternID int64, vector []float32, payload map[string]any) error {
	if patternID <= 0 {
		return errors.New("chatdiagnose: Upsert requires patternID > 0")
	}
	if len(vector) == 0 {
		return errors.New("chatdiagnose: Upsert requires non-empty vector")
	}
	if payload == nil {
		return errors.New("chatdiagnose: Upsert requires payload")
	}
	if _, ok := payload["tenant_id"]; !ok {
		return errors.New("chatdiagnose: Upsert payload MUST include tenant_id")
	}
	// pattern_id 注入 payload（反查用）
	payload["pattern_id"] = patternID
	pt := qdrantx.Point{
		ID:      uint64(patternID),
		Vector:  vector,
		Payload: payload,
	}
	if err := q.client.Upsert(ctx, q.coll.Name, []qdrantx.Point{pt}); err != nil {
		return fmt.Errorf("chatdiagnose: Qdrant Upsert: %w", err)
	}
	return nil
}

// Search cosine 搜索，按 tenant_id 强制过滤。
// topK ≤ 0 → 5；threshold ≤ 0 → 0.85（spec 默认值）。
func (q *QdrantPatternRepo) Search(ctx context.Context, tenantID string, query []float32, topK int, threshold float64) ([]PatternHit, error) {
	if tenantID == "" {
		return nil, errors.New("chatdiagnose: Search requires tenant_id")
	}
	if len(query) == 0 {
		return nil, errors.New("chatdiagnose: Search requires non-empty query")
	}
	if topK <= 0 {
		topK = 5
	}
	if threshold <= 0 {
		threshold = 0.85
	}
	hits, err := q.client.Search(ctx, q.coll.Name, query, qdrantx.SearchOpts{
		Limit:     topK,
		MustMatch: map[string]any{"tenant_id": tenantID},
	})
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: Qdrant Search: %w", err)
	}
	out := make([]PatternHit, 0, len(hits))
	for _, h := range hits {
		if h.Score < threshold {
			continue
		}
		ph := PatternHit{
			Score:         h.Score,
			PostmortemRef: stringFromPayload(h.Payload, "postmortem_ref"),
			Fingerprint:   stringFromPayload(h.Payload, "fingerprint"),
			Severity:      stringFromPayload(h.Payload, "severity"),
			ResourceType:  stringFromPayload(h.Payload, "resource_type"),
		}
		// pattern_id 在 Qdrant payload 里是 json number → float64
		if v, ok := h.Payload["pattern_id"]; ok {
			switch x := v.(type) {
			case float64:
				ph.PatternID = int64(x)
			case int64:
				ph.PatternID = x
			}
		}
		if v, ok := h.Payload["confidence"]; ok {
			if f, ok := v.(float64); ok {
				ph.Confidence = f
			}
		}
		out = append(out, ph)
	}
	return out, nil
}

// DeleteByPatternID 删除指定 pattern_id 的 Qdrant 点（postmortem 反悔路径）。
func (q *QdrantPatternRepo) DeleteByPatternID(ctx context.Context, patternID int64) error {
	if patternID <= 0 {
		return nil
	}
	if err := q.client.DeleteByFilter(ctx, q.coll.Name, map[string]any{"pattern_id": patternID}); err != nil {
		return fmt.Errorf("chatdiagnose: Qdrant DeleteByFilter: %w", err)
	}
	return nil
}

// stringFromPayload 安全的 payload 字符串读取。
func stringFromPayload(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
