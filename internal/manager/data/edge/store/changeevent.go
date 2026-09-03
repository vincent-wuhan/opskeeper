// Package store provides GORM-backed persistence for edge-domain
// entities. See also: internal/manager/data/<domain>/store/.
package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	edgemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// ChangeEventRepo persists ChangeEventRow records reported by edge
// agents over the tunnel. The repo is the production binding;
// tests inject an in-memory fake via the narrow Repo interface below.
type ChangeEventRepo struct {
	db *gorm.DB
}

// NewChangeEventRepo constructs the repo around an opened *gorm.DB.
func NewChangeEventRepo(db *gorm.DB) *ChangeEventRepo {
	return &ChangeEventRepo{db: db}
}

// ChangeEventRepoIface is the narrow seam callers (usecase, query tool)
// depend on, so tests can swap an in-memory implementation without
// standing up SQLite.
type ChangeEventRepoIface interface {
	BatchInsert(ctx context.Context, rows []edgemodel.ChangeEventRow) error
	ListByWindow(ctx context.Context, from, to time.Time, kind string, limit int) ([]edgemodel.ChangeEventRow, error)
	ListByEdge(ctx context.Context, edgeID uint64, from, to time.Time, limit int) ([]edgemodel.ChangeEventRow, error)
	DeleteOlderThan(ctx context.Context, ts time.Time) (int64, error)
}

// Compile-time check.
var _ ChangeEventRepoIface = (*ChangeEventRepo)(nil)

// BatchInsert writes a batch of rows in a single transaction. Empty
// input is a no-op (returns nil). All rows must have valid EdgeID /
// Source / Kind / Timestamp; otherwise the whole batch is rejected.
func (r *ChangeEventRepo) BatchInsert(ctx context.Context, rows []edgemodel.ChangeEventRow) error {
	if len(rows) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for i := range rows {
		if rows[i].EdgeID == 0 || rows[i].Source == "" || rows[i].Kind == "" || rows[i].Timestamp.IsZero() {
			return errs.ErrInvalid
		}
		if rows[i].CreatedAt.IsZero() {
			rows[i].CreatedAt = now
		}
	}
	return r.db.WithContext(ctx).CreateInBatches(rows, 100).Error
}

// ListByWindow returns events in [from, to] (inclusive), filtered by
// kind (empty = all), ordered by ts DESC, capped at limit.
// limit <= 0 means 200 (default cap).
func (r *ChangeEventRepo) ListByWindow(ctx context.Context, from, to time.Time, kind string, limit int) ([]edgemodel.ChangeEventRow, error) {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return nil, errs.ErrInvalid
	}
	if limit <= 0 {
		limit = 200
	}
	q := r.db.WithContext(ctx).Model(&edgemodel.ChangeEventRow{}).
		Where("ts >= ? AND ts <= ?", from, to)
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	var rows []edgemodel.ChangeEventRow
	if err := q.Order("ts DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListByEdge returns events for a specific edge in [from, to].
func (r *ChangeEventRepo) ListByEdge(ctx context.Context, edgeID uint64, from, to time.Time, limit int) ([]edgemodel.ChangeEventRow, error) {
	if edgeID == 0 {
		return nil, errs.ErrInvalid
	}
	if limit <= 0 {
		limit = 200
	}
	q := r.db.WithContext(ctx).Model(&edgemodel.ChangeEventRow{}).
		Where("edge_id = ?", edgeID)
	if !from.IsZero() {
		q = q.Where("ts >= ?", from)
	}
	if !to.IsZero() {
		q = q.Where("ts <= ?", to)
	}
	var rows []edgemodel.ChangeEventRow
	if err := q.Order("ts DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// DeleteOlderThan removes events with ts < cutoff. Returns rows deleted.
func (r *ChangeEventRepo) DeleteOlderThan(ctx context.Context, ts time.Time) (int64, error) {
	if ts.IsZero() {
		return 0, errs.ErrInvalid
	}
	res := r.db.WithContext(ctx).Where("ts < ?", ts).Delete(&edgemodel.ChangeEventRow{})
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, res.Error
	}
	return res.RowsAffected, nil
}
