package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// Repo 是 hitl/store 的统一仓库入口。
//
// 它对应 biz/hitl 需要的所有 I/O 操作：
//   - Proposal：Create / Get / List / Decide / SetResult / Transition
//   - ProposalState：UpsertState / LoadState / DeleteState
//   - 迁移验证工具：CountByState / CountByLegacyKind
type Repo struct{ db *gorm.DB }

// NewRepo 构造仓库。
func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

// ErrStateConflict 在 optimistic-locked 状态迁移失败时返回。
var ErrStateConflict = errors.New("hitl/store: state conflict (proposal not in expected state)")

// Create 插入新 proposal。
func (r *Repo) Create(ctx context.Context, p *model.Proposal) error {
	return r.db.WithContext(ctx).Create(p).Error
}

func (r *Repo) CreateAgentTeamsIdempotent(ctx context.Context, p *model.Proposal) error {
	if p == nil || p.IdempotencyKey == nil || *p.IdempotencyKey == "" {
		return errs.ErrInvalid
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing model.Proposal
		queryErr := tx.Where("idempotency_key = ?", *p.IdempotencyKey).First(&existing).Error
		if queryErr == nil {
			if err := assertSameAgentTeamsIdempotentInsert(p, &existing); err != nil {
				return err
			}
			*p = existing
			return nil
		}
		if !errors.Is(queryErr, gorm.ErrRecordNotFound) {
			return queryErr
		}
		return tx.Create(p).Error
	})
	if err == nil {
		return nil
	}
	existing, getErr := r.getByIdempotencyKey(ctx, *p.IdempotencyKey)
	if getErr == nil {
		if err := assertSameAgentTeamsIdempotentInsert(p, existing); err != nil {
			return err
		}
		*p = *existing
		return nil
	}
	return err
}

func assertSameAgentTeamsIdempotentInsert(insert, existing *model.Proposal) error {
	if insert.Kind != existing.Kind || insert.SessionID != existing.SessionID ||
		insert.MessageID != existing.MessageID || insert.PayloadJSON != existing.PayloadJSON {
		return errs.ErrConflict
	}
	return nil
}

// Get 按 ID 查找。
func (r *Repo) Get(ctx context.Context, id string) (*model.Proposal, error) {
	var p model.Proposal
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

func (r *Repo) getByIdempotencyKey(ctx context.Context, key string) (*model.Proposal, error) {
	var p model.Proposal
	if err := r.db.WithContext(ctx).Where("idempotency_key = ?", key).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

// List 列出 proposal，按 created_at DESC；可按 state 过滤。
func (r *Repo) List(ctx context.Context, state string, limit, offset int) ([]*model.Proposal, int64, error) {
	if limit <= 0 {
		limit = 100
	}
	q := r.db.WithContext(ctx).Model(&model.Proposal{})
	if state != "" {
		q = q.Where("state = ?", state)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []*model.Proposal
	if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// CountByState 统计指定 state 的行数。
func (r *Repo) CountByState(ctx context.Context, state string) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&model.Proposal{}).
		Where("state = ?", state).Count(&n).Error
	return n, err
}

// CountByLegacyKind 统计 legacy_kind 的行数（迁移验证用）。
func (r *Repo) CountByLegacyKind(ctx context.Context, legacyKind string) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&model.Proposal{}).
		Where("legacy_kind = ?", legacyKind).Count(&n).Error
	return n, err
}

// Transition 乐观锁状态迁移：仅当 proposal 当前 state == expectedFrom 时更新。
//
// 返回 ErrStateConflict 表示 proposal 已被并发决策迁走，调用方应 Get 一次重试。
//
// 校验：先过 model.IsValidTransition(expectedFrom, toState) 检查静态合法迁移表，
// 再 WHERE state=expectedFrom 防止并发 race。
func (r *Repo) Transition(ctx context.Context, id, expectedFrom string, f model.TransitionFields) error {
	if !model.IsValidTransition(expectedFrom, f.ToState) {
		return errors.New("hitl/store: invalid state transition " + expectedFrom + " -> " + f.ToState)
	}
	updates := map[string]any{
		"state":      f.ToState,
		"updated_at": time.Now().UTC(),
	}
	if f.ApprovedBy != nil {
		updates["approved_by"] = *f.ApprovedBy
	}
	if f.RejectedBy != nil {
		updates["rejected_by"] = *f.RejectedBy
	}
	if f.PausedBy != nil {
		updates["paused_by"] = *f.PausedBy
	}
	if f.ResumedBy != nil {
		updates["resumed_by"] = *f.ResumedBy
	}
	if f.Reason != nil {
		updates["reason"] = *f.Reason
	}
	if f.ResultJSON != nil {
		updates["result_json"] = *f.ResultJSON
	}
	if f.ExpiresAt != nil {
		updates["expires_at"] = *f.ExpiresAt
	}
	if f.DecidedAt != nil {
		updates["decided_at"] = *f.DecidedAt
	}
	if f.ExecutedAt != nil {
		updates["executed_at"] = *f.ExecutedAt
	}
	if f.PausedAt != nil {
		updates["paused_at"] = *f.PausedAt
	}
	if f.ResumedAt != nil {
		updates["resumed_at"] = *f.ResumedAt
	}
	if f.IMThreadID != nil {
		updates["im_thread_id"] = *f.IMThreadID
	}
	if f.MatrixEventID != nil {
		updates["matrix_event_id"] = *f.MatrixEventID
	}
	if f.DryRunDiffURL != nil {
		updates["dry_run_diff_url"] = *f.DryRunDiffURL
	}
	if f.ExecutionLeaseExpiresAt != nil {
		updates["execution_lease_expires_at"] = *f.ExecutionLeaseExpiresAt
	}
	if f.IncrementPauseStateVersion {
		updates["pause_state_version"] = gorm.Expr("pause_state_version + 1")
	}
	res := r.db.WithContext(ctx).Model(&model.Proposal{}).
		Where("id = ? AND state = ?", id, expectedFrom).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrStateConflict
	}
	return nil
}

// SetResult 写回执行结果（在 approved/executing 阶段调用，不做状态迁移）。
func (r *Repo) SetResult(ctx context.Context, id, state, resultJSON string, executedAt time.Time) error {
	return r.db.WithContext(ctx).Model(&model.Proposal{}).Where("id = ?", id).
		Updates(map[string]any{
			"state":       state,
			"result_json": resultJSON,
			"executed_at": executedAt,
			"updated_at":  time.Now().UTC(),
		}).Error
}

// UpsertState 写入/更新 proposal_state 行（按 proposal_id 主键）。
func (r *Repo) UpsertState(ctx context.Context, st *model.ProposalState) error {
	return r.db.WithContext(ctx).Clauses().Save(st).Error
}

// LoadState 读 proposal_state 行。
func (r *Repo) LoadState(ctx context.Context, proposalID string) (*model.ProposalState, error) {
	var st model.ProposalState
	if err := r.db.WithContext(ctx).Where("proposal_id = ?", proposalID).First(&st).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &st, nil
}

// DeleteState 清理 proposal_state 行。
func (r *Repo) DeleteState(ctx context.Context, proposalID string) error {
	return r.db.WithContext(ctx).Where("proposal_id = ?", proposalID).Delete(&model.ProposalState{}).Error
}
