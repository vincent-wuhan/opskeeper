package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// Repo 是 data_sensitivity_label 表的 GORM 仓库。
type Repo struct{ db *gorm.DB }

// NewRepo 构造仓库。
func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

// Create 插入/覆盖一条 label（PK 冲突时 upsert）。
//
// 设计：人工打标 + 自动打标 + 继承打标都走同一入口；
// Upsert 而不是 Create 让「重新打标」天然幂等。
func (r *Repo) Create(ctx context.Context, l *DataSensitivityLabel) error {
	return r.db.WithContext(ctx).Clauses().Save(l).Error
}

// Get 按 (resource_type, resource_id) 查询。
func (r *Repo) Get(ctx context.Context, resourceType, resourceID string) (*DataSensitivityLabel, error) {
	var l DataSensitivityLabel
	if err := r.db.WithContext(ctx).
		Where("resource_type = ? AND resource_id = ?", resourceType, resourceID).
		First(&l).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &l, nil
}

// List 按 sensitivity 过滤（可选）。
func (r *Repo) List(ctx context.Context, sensitivity string, source string, limit, offset int) ([]*DataSensitivityLabel, int64, error) {
	if limit <= 0 {
		limit = 100
	}
	q := r.db.WithContext(ctx).Model(&DataSensitivityLabel{})
	if sensitivity != "" {
		q = q.Where("sensitivity = ?", sensitivity)
	}
	if source != "" {
		q = q.Where("label_source = ?", source)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []*DataSensitivityLabel
	if err := q.Order("updated_at DESC").Limit(limit).Offset(offset).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// Delete 删除一条 label（admin 强制清理用）。
func (r *Repo) Delete(ctx context.Context, resourceType, resourceID string) error {
	res := r.db.WithContext(ctx).
		Where("resource_type = ? AND resource_id = ?", resourceType, resourceID).
		Delete(&DataSensitivityLabel{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// ListByResourceTypePrefix 按资源 ID 前缀查找子资源（用于父资源变更后批量刷继承）。
func (r *Repo) ListByResourceTypePrefix(ctx context.Context, resourceType, idPrefix string) ([]*DataSensitivityLabel, error) {
	var out []*DataSensitivityLabel
	if err := r.db.WithContext(ctx).
		Where("resource_type = ? AND resource_id LIKE ?", resourceType, idPrefix+"%").
		Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ListByResourceType 列出指定资源类型的所有 label（无 ID 前缀过滤）。
func (r *Repo) ListByResourceType(ctx context.Context, resourceType string, sensitivity string, limit, offset int) ([]*DataSensitivityLabel, error) {
	if limit <= 0 {
		limit = 100
	}
	q := r.db.WithContext(ctx).Where("resource_type = ?", resourceType)
	if sensitivity != "" {
		q = q.Where("sensitivity = ?", sensitivity)
	}
	var out []*DataSensitivityLabel
	if err := q.Order("updated_at DESC").Limit(limit).Offset(offset).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}
