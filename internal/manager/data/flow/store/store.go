// Package store is the GORM-backed implementation of biz/flow.Repo and
// biz/flow.RunRepo. Works against MySQL and SQLite alike — GORM hides
// the dialect at this level.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	biz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/flow"
	schedulerbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/scheduler"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/flow"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// Repo implements biz/flow.Repo.
type Repo struct{ db *gorm.DB }

// NewRepo constructs the definition repo.
func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

var _ biz.Repo = (*Repo)(nil)
var _ biz.ScheduleStateRepo = (*Repo)(nil)
var _ schedulerbiz.Repo = (*Repo)(nil)

func (r *Repo) Create(ctx context.Context, f *model.Flow) error {
	return r.db.WithContext(ctx).Create(f).Error
}

func (r *Repo) Update(ctx context.Context, f *model.Flow) error {
	return r.db.WithContext(ctx).Save(f).Error
}

func (r *Repo) Get(ctx context.Context, id uint64) (*model.Flow, error) {
	var f model.Flow
	if err := r.db.WithContext(ctx).First(&f, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &f, nil
}

func (r *Repo) List(ctx context.Context, limit, offset int) ([]*model.Flow, int64, error) {
	var total int64
	if err := r.db.WithContext(ctx).Model(&model.Flow{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []*model.Flow
	if err := r.db.WithContext(ctx).Order("id DESC").Limit(limit).Offset(offset).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (r *Repo) ListEnabled(ctx context.Context) ([]*model.Flow, error) {
	var out []*model.Flow
	if err := r.db.WithContext(ctx).Where("enabled = ?", true).Order("id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repo) Delete(ctx context.Context, id uint64) error {
	res := r.db.WithContext(ctx).Delete(&model.Flow{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

func (r *Repo) LoadScheduleStates(ctx context.Context) ([]*model.FlowScheduleNextFire, error) {
	var states []*model.FlowScheduleNextFire
	if err := r.db.WithContext(ctx).Where("status = ?", model.FlowScheduleStatusEnabled).
		Order("flow_id ASC, node_id ASC").Find(&states).Error; err != nil {
		return nil, fmt.Errorf("load flow schedule states: %w", err)
	}
	return states, nil
}

func (r *Repo) UpsertScheduleState(ctx context.Context, state *model.FlowScheduleNextFire) error {
	if state == nil {
		return errs.ErrInvalid
	}
	return r.db.WithContext(ctx).Save(state).Error
}

func (r *Repo) DeleteScheduleStatesNotIn(ctx context.Context, keys []biz.ScheduleStateKey) error {
	var states []*model.FlowScheduleNextFire
	if err := r.db.WithContext(ctx).Find(&states).Error; err != nil {
		return fmt.Errorf("list flow schedule states: %w", err)
	}
	keep := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keep[fmt.Sprintf("%d:%s", key.FlowID, key.NodeID)] = struct{}{}
	}
	for _, state := range states {
		if _, ok := keep[fmt.Sprintf("%d:%s", state.FlowID, state.NodeID)]; ok {
			continue
		}
		if err := r.db.WithContext(ctx).Delete(state).Error; err != nil {
			return fmt.Errorf("delete stale flow schedule state %d:%s: %w", state.FlowID, state.NodeID, err)
		}
	}
	return nil
}

// ListMissed implements schedulerbiz.Repo for boot-time compensation.
func (r *Repo) ListMissed(ctx context.Context, before time.Time) ([]schedulerbiz.MissedRunInfo, error) {
	var states []*model.FlowScheduleNextFire
	if err := r.db.WithContext(ctx).
		Where("status = ? AND next_fire_at < ?", model.FlowScheduleStatusEnabled, before.UTC()).
		Order("next_fire_at ASC").Find(&states).Error; err != nil {
		return nil, fmt.Errorf("list missed flow schedules: %w", err)
	}
	missed := make([]schedulerbiz.MissedRunInfo, 0, len(states))
	for _, state := range states {
		missed = append(missed, schedulerbiz.MissedRunInfo{
			FlowID:            fmt.Sprintf("%d", state.FlowID),
			NodeID:            state.NodeID,
			CronSpec:          state.CronSpec,
			ExpectedFireAt:    state.NextFireAt,
			MissedDurationSec: int64(before.UTC().Sub(state.NextFireAt).Seconds()),
		})
	}
	return missed, nil
}

// RecordMissedAudit implements schedulerbiz.Repo and is idempotent by the
// unique schedule/expected-fire pair.
func (r *Repo) RecordMissedAudit(ctx context.Context, missed schedulerbiz.MissedRunInfo) error {
	scheduleID := fmt.Sprintf("%s:%s", missed.FlowID, missed.NodeID)
	var existing model.MissedRunAudit
	err := r.db.WithContext(ctx).Where("schedule_id = ? AND expected_fire_at = ?", scheduleID, missed.ExpectedFireAt.UTC()).First(&existing).Error
	if err == nil {
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("find missed run audit: %w", err)
	}
	audit := &model.MissedRunAudit{
		ScheduleID:        scheduleID,
		ExpectedFireAt:    missed.ExpectedFireAt.UTC(),
		MissedDurationSec: missed.MissedDurationSec,
		DetectedAt:        time.Now().UTC(),
		AlertGenerated:    true,
	}
	return r.db.WithContext(ctx).Create(audit).Error
}

// RunRepo implements biz/flow.RunRepo.
type RunRepo struct{ db *gorm.DB }

// NewRunRepo constructs the run repo.
func NewRunRepo(db *gorm.DB) *RunRepo { return &RunRepo{db: db} }

var _ biz.RunRepo = (*RunRepo)(nil)

func (r *RunRepo) CreateRun(ctx context.Context, run *model.FlowRun) error {
	return r.db.WithContext(ctx).Create(run).Error
}

func (r *RunRepo) UpdateRun(ctx context.Context, run *model.FlowRun) error {
	return r.db.WithContext(ctx).Save(run).Error
}

func (r *RunRepo) GetRun(ctx context.Context, id string) (*model.FlowRun, error) {
	var run model.FlowRun
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &run, nil
}

func (r *RunRepo) ListRuns(ctx context.Context, flowID uint64, limit int) ([]*model.FlowRun, error) {
	var out []*model.FlowRun
	q := r.db.WithContext(ctx).Order("created_at DESC").Limit(limit)
	if flowID > 0 {
		q = q.Where("flow_id = ?", flowID)
	}
	if err := q.Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (r *RunRepo) CreateNode(ctx context.Context, n *model.FlowRunNode) error {
	return r.db.WithContext(ctx).Create(n).Error
}

func (r *RunRepo) UpdateNode(ctx context.Context, n *model.FlowRunNode) error {
	return r.db.WithContext(ctx).Save(n).Error
}

func (r *RunRepo) ListNodes(ctx context.Context, runID string) ([]*model.FlowRunNode, error) {
	var out []*model.FlowRunNode
	if err := r.db.WithContext(ctx).Where("run_id = ?", runID).Order("id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// PruneRuns deletes FINISHED runs created before `before`, plus their
// node rows, capping unbounded flow_runs / flow_run_nodes growth. Pending
// / running rows are never pruned (they may still be in flight). Node rows
// go first so a crash mid-prune can't orphan them. Returns runs deleted.
func (r *RunRepo) PruneRuns(ctx context.Context, before time.Time) (int64, error) {
	db := r.db.WithContext(ctx)
	var ids []string
	if err := db.Model(&model.FlowRun{}).
		Where("created_at < ? AND status NOT IN ?", before, []string{model.RunStatusPending, model.RunStatusRunning}).
		Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := db.Where("run_id IN ?", ids).Delete(&model.FlowRunNode{}).Error; err != nil {
		return 0, err
	}
	res := db.Where("id IN ?", ids).Delete(&model.FlowRun{})
	return res.RowsAffected, res.Error
}

func (r *RunRepo) SweepStaleRunning(ctx context.Context, reason string) (int64, error) {
	res := r.db.WithContext(ctx).Model(&model.FlowRun{}).
		Where("status IN ?", []string{model.RunStatusPending, model.RunStatusRunning}).
		Updates(map[string]any{"status": model.RunStatusFailed, "error": reason})
	return res.RowsAffected, res.Error
}
