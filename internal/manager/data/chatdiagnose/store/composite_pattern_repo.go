// chatdiagnose/store/composite_pattern_repo.go — 组合 MySQL metadata + Qdrant 向量。
//
// 设计：
//   - FindSimilar 走 Qdrant cosine 搜 topK → 拿 pattern_ids → MySQL 拿 metadata → 按 qdrant 顺序合并
//   - Save 双写（MySQL UPSERT + Qdrant Upsert），任一失败 slog warn + 继续
//   - IncHitCount 走 MySQL（atomic UPDATE）；chat latency 不被写影响
//   - FindSimilar 失败 MUST slog warn + 返回 nil（KB miss 不阻塞 chat）
//
// 多租户强制：所有方法 tenant_id 隔离。

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"time"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// CompositePatternRepo 组合 PatternMeta（metadata）+ QdrantPatternRepo（向量）。
// 实现 chatdiagnose.PatternRepo 风格接口（FindSimilar + IncHitCount + Save）。
type CompositePatternRepo struct {
	meta   PatternMeta
	qdrant *QdrantPatternRepo
	log    *slog.Logger
}

// NewCompositePatternRepo 构造。meta 和 qdrant 不得为 nil。
func NewCompositePatternRepo(meta PatternMeta, qdrant *QdrantPatternRepo, log *slog.Logger) *CompositePatternRepo {
	if log == nil {
		log = slog.Default()
	}
	return &CompositePatternRepo{
		meta:   meta,
		qdrant: qdrant,
		log:    log.With(slog.String("comp", "chatdiagnose.composite_repo")),
	}
}

// FindSimilar cosine 搜索 → metadata join → 合并返回。
// 匹配 chatdiagnose.PatternRepo.FindSimilar(ctx, tenantID, vec []float64, topK)。
// vec 由 chatdiagnose.KBLookupImpl 用 embedder.Embed(signature) 算出。
func (c *CompositePatternRepo) FindSimilar(ctx context.Context, tenantID string, queryVec []float64, topK int) ([]chatdiagnosemodel.IncidentPattern, error) {
	if tenantID == "" {
		return nil, nil
	}
	if c.qdrant == nil {
		return nil, nil
	}
	if len(queryVec) == 0 {
		return nil, nil
	}
	if topK <= 0 {
		topK = 5
	}

	// float64 → float32 for Qdrant
	vec32 := make([]float32, len(queryVec))
	for i, v := range queryVec {
		vec32[i] = float32(v)
	}

	// The business layer applies KBLookupRequest.Threshold. Returning every
	// scored neighbor here avoids silently losing relevant results when the
	// caller requests a lower threshold than the historical default.
	hits, err := c.qdrant.Search(ctx, tenantID, vec32, topK, 0)
	if err != nil {
		c.log.Warn("composite_repo: qdrant search failed",
			slog.String("tenant_id", tenantID), slog.Any("err", err))
		return nil, nil
	}
	if len(hits) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(hits))
	scoreByID := make(map[int64]float64, len(hits))
	for _, h := range hits {
		if h.PatternID <= 0 {
			continue
		}
		ids = append(ids, h.PatternID)
		scoreByID[h.PatternID] = h.Score
	}
	if len(ids) == 0 {
		return nil, nil
	}
	metas, err := c.meta.FindByIDs(ctx, tenantID, ids)
	if err != nil {
		c.log.Warn("composite_repo: meta FindByIDs failed",
			slog.String("tenant_id", tenantID), slog.Any("err", err))
		return nil, nil
	}
	// 按 Qdrant 顺序合并
	out := make([]chatdiagnosemodel.IncidentPattern, 0, len(metas))
	used := make(map[int64]bool, len(metas))
	for _, h := range hits {
		for _, m := range metas {
			if m.ID == h.PatternID && !used[m.ID] {
				m.Relevance = scoreByID[m.ID]
				out = append(out, m)
				used[m.ID] = true
				break
			}
		}
	}
	return out, nil
}

// SearchCandidates delegates to the optional metadata candidate source while
// preserving the caller's tenant scope.
func (c *CompositePatternRepo) SearchCandidates(ctx context.Context, tenantID string, terms []string, limit int) ([]chatdiagnosemodel.IncidentPattern, error) {
	if c.meta == nil {
		return nil, nil
	}
	searcher, ok := c.meta.(PatternCandidateMeta)
	if !ok {
		return nil, nil
	}
	return searcher.SearchCandidates(ctx, tenantID, terms, limit)
}

// Save UPSERT 双写：MySQL metadata + Qdrant 向量。任一失败 slog warn + 继续。
//   - embedding float64 转 float32 给 Qdrant
//   - patternID 从 MySQL UPSERT 拿到（last insert id 或反查）
//   - payload 必须含 tenant_id
func (c *CompositePatternRepo) Save(ctx context.Context, p *chatdiagnosemodel.IncidentPattern) error {
	if c.meta == nil {
		return nil
	}
	if err := c.meta.Save(ctx, p); err != nil {
		c.log.Warn("composite_repo: meta Save failed",
			slog.String("tenant_id", p.TenantID), slog.Any("err", err))
		return err
	}
	// 写 Qdrant
	if c.qdrant != nil && p.ID > 0 && len(p.Embedding) > 0 {
		vec, err := decodeEmbeddingJSON(p.Embedding)
		if err != nil {
			c.log.Warn("composite_repo: decode embedding failed", slog.Any("err", err))
		} else {
			payload := map[string]any{
				"tenant_id":      p.TenantID,
				"fingerprint":    p.Fingerprint,
				"severity":       p.Severity,
				"resource_type":  p.ResourceType,
				"confidence":     p.Confidence,
				"postmortem_ref": p.SourcePostmortemID,
				"hit_count":      p.HitCount,
			}
			if err := c.qdrant.Upsert(ctx, p.ID, vec, payload); err != nil {
				c.log.Warn("composite_repo: qdrant upsert failed",
					slog.String("tenant_id", p.TenantID), slog.Any("err", err))
				// 不返回 error — MySQL 已写，Qdrant 失败视为 KB miss
			}
		}
	}
	return nil
}

// IncHitCount 走 MySQL（atomic UPDATE）；不触 Qdrant（hit_count 不存 Qdrant payload）。
func (c *CompositePatternRepo) IncHitCount(ctx context.Context, patternID int64) error {
	// PatternRepo's public contract carries only patternID. Resolve the owner
	// through a minimal tenant projection, then keep the UPDATE tenant-scoped.
	if c.meta == nil {
		return nil
	}
	tenantID, err := c.meta.FindTenantByPatternID(ctx, patternID)
	if err != nil {
		return err
	}
	if tenantID == "" {
		return nil
	}
	return c.meta.IncHitCount(ctx, tenantID, patternID)
}

// ComputeFingerprint 计算 fingerprint（暴露给 postmortem_worker）。
// sha256(resource_type + ":" + root_cause_object + ":" + severity)[:16] hex。
func ComputeFingerprint(resourceType, rootCauseObject, severity string) string {
	h := sha256.New()
	h.Write([]byte(resourceType))
	h.Write([]byte(":"))
	h.Write([]byte(rootCauseObject))
	h.Write([]byte(":"))
	h.Write([]byte(severity))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// decodeEmbeddingJSON 把 IncidentPattern.Embedding (JSON 字符串) 解码成 float32 切片。
// 接受 "[1.0,2.0,...]" 或 "[1,2,3]" 两种格式。
func decodeEmbeddingJSON(s string) ([]float32, error) {
	s = s[1 : len(s)-1] // strip '[' ']'
	if s == "" {
		return nil, nil
	}
	parts := splitFloatList(s)
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(p, 32)
		if err != nil {
			return nil, err
		}
		out = append(out, float32(f))
	}
	return out, nil
}

// splitFloatList 简单的逗号分割（支持负数、空白）。
func splitFloatList(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		if r == ' ' {
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// silence unused imports in some build configs
var _ = time.Now
