package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
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

// StrictestForResourceID 返回某个 id 在**所有**资源类型下生效标签里最严格的一条。
//
// 为什么需要跨类型：审批行上带着的是一个裸 id（`web-1`），不是
// `pod:web-1`。调用方知道工具打到了某个东西，却不知道它被登记成哪种资源，
// 而敏感度标签是按 (类型, id) 存的——于是"这个 id 危不危险"这个问题在没有
// 类型的输入下无法被回答。
//
// 跨类型会撞名：一个叫 `web-1` 的 Pod 和一个叫 `web-1` 的 Service 是两行。
// 所以这里取**最严格**的一条而不是第一条：撞名时答案偏向更严的那一侧，
// 漏判的方向因此永远是安全的。排序由调用方按 dataguard 的等级序做，
// 存储层不复制那份表。
func (r *Repo) StrictestForResourceID(ctx context.Context, resourceID string) ([]*DataSensitivityLabel, error) {
	var rows []*DataSensitivityLabel
	if err := r.db.WithContext(ctx).
		Where("resource_id = ?", resourceID).
		Order("confidence DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
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
