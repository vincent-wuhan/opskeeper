package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	biz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/topology"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/topology"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// RelationRepo is the GORM-backed biz/topology.RelationRepo.
type RelationRepo struct{ db *gorm.DB }

// NewRelationRepo constructs the repo.
func NewRelationRepo(db *gorm.DB) *RelationRepo { return &RelationRepo{db: db} }

var _ biz.RelationRepo = (*RelationRepo)(nil)

func (r *RelationRepo) Create(ctx context.Context, rel *model.Relation) error {
	return r.db.WithContext(ctx).Create(rel).Error
}

func (r *RelationRepo) Update(ctx context.Context, id uint64, propsJSON string) error {
	res := r.db.WithContext(ctx).Model(&model.Relation{}).Where("id = ?", id).Update("props_jsonb", propsJSON)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

func (r *RelationRepo) Get(ctx context.Context, id uint64) (*model.Relation, error) {
	var rel model.Relation
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&rel).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &rel, nil
}

func (r *RelationRepo) List(ctx context.Context, f biz.RelationListFilter) ([]*model.Relation, error) {
	q := r.db.WithContext(ctx).Model(&model.Relation{})
	q = applyRelationFilter(q, f)
	q = q.Order("id DESC")
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	if f.Offset > 0 {
		q = q.Offset(f.Offset)
	}
	var out []*model.Relation
	if err := q.Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (r *RelationRepo) Count(ctx context.Context, f biz.RelationListFilter) (int64, error) {
	q := r.db.WithContext(ctx).Model(&model.Relation{})
	q = applyRelationFilter(q, f)
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

func (r *RelationRepo) Delete(ctx context.Context, id uint64) error {
	res := r.db.WithContext(ctx).Where("id = ?", id).Delete(&model.Relation{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// applyRelationFilter applies the optional filter clauses. SrcOrDstID
// is processed exclusively (mutually exclusive with SrcID/DstID — if
// the caller sets it, we ignore the more-specific endpoints to keep
// the SQL straightforward).
func applyRelationFilter(q *gorm.DB, f biz.RelationListFilter) *gorm.DB {
	if f.SrcOrDstID != 0 {
		q = q.Where("src_id = ? OR dst_id = ?", f.SrcOrDstID, f.SrcOrDstID)
	} else {
		if f.SrcID != 0 {
			q = q.Where("src_id = ?", f.SrcID)
		}
		if f.DstID != 0 {
			q = q.Where("dst_id = ?", f.DstID)
		}
	}
	if f.Type != "" {
		q = q.Where("type = ?", f.Type)
	}
	return q
}
