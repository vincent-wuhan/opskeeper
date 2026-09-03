// chatdiagnose/store/mysql_pattern_meta.go — incident_pattern 表的 MySQL 持久化层。
//
// 设计：
//   - 不存 embedding（向量走 Qdrant；MySQL 仅 metadata）
//   - Save 走 INSERT ... ON DUPLICATE KEY UPDATE：保留 id + created_at，
//     更新业务字段；fingerprint 为空时转 NULL 绕过 UNIQUE 约束
//   - IncHitCount 走单 SQL 原子 UPDATE（Maxwell 建议，避免 read-modify-write race）
//   - 所有 SQL 必须 WHERE tenant_id=?（多租户强制）
//
// 多租户：tenant_id 复合 UNIQUE INDEX uniq_tenant_fingerprint 由 GORM AutoMigrate 建。

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// PatternMeta 是 incident_pattern 表的 MySQL metadata 持久化层。
// Composite PatternRepo 组合本接口 + QdrantPatternRepo。
type PatternMeta interface {
	Save(ctx context.Context, p *chatdiagnosemodel.IncidentPattern) error
	FindByFingerprint(ctx context.Context, tenantID, fingerprint string) (*chatdiagnosemodel.IncidentPattern, error)
	FindByIDs(ctx context.Context, tenantID string, ids []int64) ([]chatdiagnosemodel.IncidentPattern, error)
	FindTenantByPatternID(ctx context.Context, patternID int64) (string, error)
	IncHitCount(ctx context.Context, tenantID string, patternID int64) error
}

// PatternCandidateMeta is the optional tenant-scoped candidate source used
// by the business layer's pure-Go BM25 implementation.
type PatternCandidateMeta interface {
	SearchCandidates(ctx context.Context, tenantID string, terms []string, limit int) ([]chatdiagnosemodel.IncidentPattern, error)
}

// MySQLPatternMeta 是 GORM 实现。
type MySQLPatternMeta struct {
	db *gorm.DB
}

// NewMySQLPatternMeta 构造。db 不得为 nil。
func NewMySQLPatternMeta(db *gorm.DB) *MySQLPatternMeta {
	return &MySQLPatternMeta{db: db}
}

// Compile-time interface satisfaction check.
var _ PatternMeta = (*MySQLPatternMeta)(nil)
var _ PatternCandidateMeta = (*MySQLPatternMeta)(nil)

// Save UPSERT：(tenant_id, fingerprint) 唯一；fingerprint 为空时转 NULL。
//
// 保留 id + created_at：ON DUPLICATE KEY UPDATE 不触碰 id / created_at / hit_count。
// MySQL UPSERT 拿到 id 用 LastInsertId()，更新路径拿到原有 id。
func (m *MySQLPatternMeta) Save(ctx context.Context, p *chatdiagnosemodel.IncidentPattern) error {
	if p.TenantID == "" {
		return errors.New("chatdiagnose: PatternMeta.Save requires tenant_id")
	}
	if p.ResourceType == "" {
		return errors.New("chatdiagnose: PatternMeta.Save requires resource_type")
	}

	// fingerprint 为空 → NULL（兼容遗留 pattern 行）
	fp := normalizeFingerprint(p.Fingerprint)
	sym := normalizeStr(p.Symptom, 128)
	rco := normalizeStr(p.RootCauseObject, 128)
	sev := normalizeStr(p.Severity, 16)

	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now

	// embedding JSON 编码（如果提供）
	var embeddingVal interface{}
	if p.Embedding != "" {
		embeddingVal = p.Embedding
	} else {
		embeddingVal = nil
	}

	// GORM clause.OnConflict 提供跨方言 UPSERT：MySQL / SQLite / PostgreSQL 通用。
	// MySQL 用 ON DUPLICATE KEY UPDATE；SQLite/Postgres 用 ON CONFLICT ... DO UPDATE。
	// GORM 自动选择。
	conflictCols := []string{"tenant_id"}
	if fp != nil {
		conflictCols = append(conflictCols, "fingerprint")
	}
	updates := map[string]interface{}{
		"symptom":              sym,
		"root_cause_object":    rco,
		"signature":            p.Signature,
		"embedding":            embeddingVal,
		"last_hit_at":          p.LastHitAt,
		"source_postmortem_id": p.SourcePostmortemID,
		"severity":             sev,
		"confidence":           p.Confidence,
		"updated_at":           p.UpdatedAt,
	}
	tmpP := *p
	res := m.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "fingerprint"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&tmpP)
	if res.Error != nil {
		return fmt.Errorf("chatdiagnose: PatternMeta.Save: %w", res.Error)
	}
	// GORM 会在 INSERT 路径填 p.ID；UPSERT 路径保留 id。
	// 同步回原对象（p 是 pointer，tmpP 是 copy）。
	*p = tmpP
	return nil
}

// FindByFingerprint 按 (tenant_id, fingerprint) 查；fingerprint 为空 → 返回 nil, nil（无 fingerprint 行被隐藏）。
func (m *MySQLPatternMeta) FindByFingerprint(ctx context.Context, tenantID, fingerprint string) (*chatdiagnosemodel.IncidentPattern, error) {
	if tenantID == "" {
		return nil, errors.New("chatdiagnose: FindByFingerprint requires tenant_id")
	}
	if fingerprint == "" {
		return nil, nil
	}
	var p chatdiagnosemodel.IncidentPattern
	err := m.db.WithContext(ctx).
		Where("tenant_id = ? AND fingerprint = ?", tenantID, fingerprint).
		First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: FindByFingerprint: %w", err)
	}
	return &p, nil
}

// FindByIDs 批量查 metadata，按 tenant 隔离。
func (m *MySQLPatternMeta) FindByIDs(ctx context.Context, tenantID string, ids []int64) ([]chatdiagnosemodel.IncidentPattern, error) {
	if tenantID == "" {
		return nil, errors.New("chatdiagnose: FindByIDs requires tenant_id")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var patterns []chatdiagnosemodel.IncidentPattern
	err := m.db.WithContext(ctx).
		Where("tenant_id = ? AND id IN ?", tenantID, ids).
		Find(&patterns).Error
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: FindByIDs: %w", err)
	}
	return patterns, nil
}

// SearchCandidates returns tenant-owned rows matching at least one lexical
// term. The parameterized LIKE filter is portable across MySQL, PostgreSQL,
// and SQLite, and deliberately excludes the large embedding column.
func (m *MySQLPatternMeta) SearchCandidates(ctx context.Context, tenantID string, terms []string, limit int) ([]chatdiagnosemodel.IncidentPattern, error) {
	if tenantID == "" {
		return nil, errors.New("chatdiagnose: SearchCandidates requires tenant_id")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	normalizedTerms := make([]string, 0, len(terms))
	seen := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			continue
		}
		if _, duplicate := seen[term]; duplicate {
			continue
		}
		seen[term] = struct{}{}
		normalizedTerms = append(normalizedTerms, term)
	}
	if len(normalizedTerms) == 0 {
		return nil, nil
	}

	conditions := make([]string, 0, len(normalizedTerms))
	args := make([]interface{}, 0, len(normalizedTerms)*5+1)
	for _, term := range normalizedTerms {
		pattern := "%" + term + "%"
		conditions = append(conditions, "(LOWER(resource_type) LIKE ? OR LOWER(symptom) LIKE ? OR LOWER(root_cause_object) LIKE ? OR LOWER(signature) LIKE ? OR LOWER(severity) LIKE ?)")
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}
	matchingCondition := "tenant_id = ? AND (" + strings.Join(conditions, " OR ") + ")"
	args = append([]interface{}{tenantID}, args...)

	var patterns []chatdiagnosemodel.IncidentPattern
	err := m.db.WithContext(ctx).
		Select("id, tenant_id, resource_type, symptom, root_cause_object, signature, hit_count, last_hit_at, source_postmortem_id, fingerprint, severity, confidence, created_at, updated_at").
		Where(matchingCondition, args...).
		Order("updated_at DESC, id DESC").
		Limit(limit).
		Find(&patterns).Error
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: SearchCandidates: %w", err)
	}
	return patterns, nil
}

// FindTenantByPatternID resolves only the owning tenant needed to perform a
// tenant-scoped update. It never widens FindByIDs or returns cross-tenant rows.
func (m *MySQLPatternMeta) FindTenantByPatternID(ctx context.Context, patternID int64) (string, error) {
	if patternID <= 0 {
		return "", errors.New("chatdiagnose: FindTenantByPatternID requires patternID > 0")
	}
	var tenantID string
	err := m.db.WithContext(ctx).
		Model(&chatdiagnosemodel.IncidentPattern{}).
		Select("tenant_id").
		Where("id = ?", patternID).
		Scan(&tenantID).Error
	if err != nil {
		return "", fmt.Errorf("chatdiagnose: FindTenantByPatternID: %w", err)
	}
	return tenantID, nil
}

// IncHitCount 原子 +1；UPDATE 0 rows 视为成功（pattern 已被删 / 不存在）。
// 多租户：WHERE tenant_id=? 防止误改其他租户。
func (m *MySQLPatternMeta) IncHitCount(ctx context.Context, tenantID string, patternID int64) error {
	if tenantID == "" {
		return errors.New("chatdiagnose: IncHitCount requires tenant_id")
	}
	if patternID <= 0 {
		return errors.New("chatdiagnose: IncHitCount requires patternID > 0")
	}
	now := time.Now().UTC()
	res := m.db.WithContext(ctx).Model(&chatdiagnosemodel.IncidentPattern{}).
		Where("id = ? AND tenant_id = ?", patternID, tenantID).
		Updates(map[string]interface{}{
			"hit_count":   gorm.Expr("hit_count + 1"),
			"last_hit_at": now,
		})
	if res.Error != nil {
		return fmt.Errorf("chatdiagnose: IncHitCount: %w", res.Error)
	}
	return nil
}

// normalizeFingerprint "" → nil（让 UNIQUE INDEX 失效，兼容遗留数据）。
func normalizeFingerprint(fp string) interface{} {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return nil
	}
	if len(fp) > 64 {
		fp = fp[:64]
	}
	return fp
}

// normalizeStr 截断到 maxLen；空 → nil。
func normalizeStr(s string, maxLen int) interface{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}
