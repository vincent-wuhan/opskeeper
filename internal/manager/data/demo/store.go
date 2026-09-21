package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	alertmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/demo"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

type Repo struct {
	db *gorm.DB
}

func (r *Repo) UpdateStatusWithEvent(
	ctx context.Context, id uint64, status string, event *alertmodel.Event, allowedCurrent ...string,
) error {
	if !model.IsKnownStatus(status) || event == nil {
		return errs.ErrInvalid
	}
	allowed := make(map[string]struct{}, len(allowedCurrent))
	for _, current := range allowedCurrent {
		allowed[current] = struct{}{}
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run model.ScenarioRun
		if err := tx.First(&run, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errs.ErrNotFound
			}
			return err
		}
		if _, ok := allowed[run.Status]; !ok {
			return errs.ErrConflict
		}
		before := run.TargetFingerprint
		run.Status = status
		if run.TargetFingerprint != before || !model.IsKnownStatus(run.Status) {
			return errs.ErrConflict
		}
		if err := tx.Save(&run).Error; err != nil {
			return err
		}
		event.ID = 0
		event.IncidentID = run.IncidentID
		return tx.Create(event).Error
	})
}

func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&model.ScenarioRun{})
}

func (r *Repo) CreateOrUpdate(ctx context.Context, run *model.ScenarioRun) error {
	if run == nil || !model.IsKnownStatus(run.Status) {
		return errs.ErrInvalid
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing model.ScenarioRun
		err := tx.Where(
			"tenant_id = ? AND scenario_id = ? AND idempotency_key = ?",
			run.TenantID, run.ScenarioID, run.IdempotencyKey,
		).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(run).Error
		}
		if err != nil {
			return err
		}
		if existing.TargetFingerprint != run.TargetFingerprint {
			return errs.ErrConflict
		}
		run.ID = existing.ID
		run.CreatedAt = existing.CreatedAt
		return tx.Model(&existing).Select(
			"incident_id", "pool_manifest_id", "target", "target_fingerprint",
			"alert_fingerprint", "status", "expires_at", "updated_at",
		).Updates(run).Error
	})
}

func (r *Repo) GetByIdempotencyKey(ctx context.Context, tenantID uint64, scenarioID, key string) (*model.ScenarioRun, error) {
	var run model.ScenarioRun
	err := r.db.WithContext(ctx).Where(
		"tenant_id = ? AND scenario_id = ? AND idempotency_key = ?",
		tenantID, scenarioID, key,
	).First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errs.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *Repo) GetByIncident(ctx context.Context, tenantID uint64, scenarioID string, incidentID uint64) (*model.ScenarioRun, error) {
	var run model.ScenarioRun
	err := r.db.WithContext(ctx).Where(
		"tenant_id = ? AND scenario_id = ? AND incident_id = ?",
		tenantID, scenarioID, incidentID,
	).Order("created_at DESC, id DESC").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errs.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *Repo) ListExpiredAwaitingApproval(ctx context.Context, now time.Time, limit int) ([]model.ScenarioRun, error) {
	if limit <= 0 {
		limit = 100
	}
	var runs []model.ScenarioRun
	err := r.db.WithContext(ctx).Where(
		"status = ? AND expires_at <= ?", model.ScenarioStatusAwaitingApproval, now,
	).Order("expires_at ASC, id ASC").Limit(limit).Find(&runs).Error
	return runs, err
}

func (r *Repo) UpdateStatus(ctx context.Context, id uint64, status string, mutation func(*model.ScenarioRun) error) error {
	if !model.IsKnownStatus(status) {
		return errs.ErrInvalid
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run model.ScenarioRun
		if err := tx.First(&run, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errs.ErrNotFound
			}
			return err
		}
		before := run.TargetFingerprint
		run.Status = status
		if mutation != nil {
			if err := mutation(&run); err != nil {
				return err
			}
		}
		if !model.IsKnownStatus(run.Status) || run.TargetFingerprint != before {
			return errs.ErrConflict
		}
		return tx.Save(&run).Error
	})
}
