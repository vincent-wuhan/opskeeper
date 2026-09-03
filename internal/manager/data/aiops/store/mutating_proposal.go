package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// MutatingProposalRepo is the GORM-backed persistence for
// reviewer audit rows. The decorator package consumes this through a
// narrow interface (decorators.MutatingProposalSink) so tests can
// inject an in-memory fake without standing up a SQLite DB; this file
// is the production binding.
//
// Concurrency: each method runs in its own DB context. Insert at
// intercept time + UpdateDecision at reviewer-return time form the
// canonical write pair; both are independent transactions because the
// reviewer round-trip can outlive an HTTP request.
type MutatingProposalRepo struct {
	db *gorm.DB
}

// NewMutatingProposalRepo constructs the repo around an opened *gorm.DB.
func NewMutatingProposalRepo(db *gorm.DB) *MutatingProposalRepo {
	return &MutatingProposalRepo{db: db}
}

// Insert writes a fresh proposal row in DecisionPending state. ID is
// auto-filled by BeforeCreate when zero.
func (r *MutatingProposalRepo) Insert(ctx context.Context, p *model.MutatingProposal) error {
	if p == nil {
		return errs.ErrInvalid
	}
	if p.Decision == "" {
		p.Decision = model.DecisionPending
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	return r.db.WithContext(ctx).Create(p).Error
}

// UpdateDecision flips the row from pending to approve / reject and
// stamps DecidedAt. ExecutedAt is set lazily by ExecutionStamp once
// the tool actually dispatches (or never, on reject).
func (r *MutatingProposalRepo) UpdateDecision(ctx context.Context, id, decision string, reason *string) error {
	if id == "" {
		return errs.ErrInvalid
	}
	switch decision {
	case model.DecisionApprove, model.DecisionReject:
	default:
		return errs.ErrInvalid
	}
	now := time.Now().UTC()
	res := r.db.WithContext(ctx).Model(&model.MutatingProposal{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"decision":        decision,
			"decision_reason": reason,
			"decided_at":      now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// MarkExecuted stamps ExecutedAt for the given proposal — fired after
// the wrapped tool's InvokableRun returns (success or failure). Best-
// effort: a missing row should not fail the tool execution.
func (r *MutatingProposalRepo) MarkExecuted(ctx context.Context, id string, t time.Time) error {
	if id == "" {
		return errs.ErrInvalid
	}
	return r.db.WithContext(ctx).Model(&model.MutatingProposal{}).
		Where("id = ?", id).
		Update("executed_at", t.UTC()).Error
}

// Get returns the proposal by id; (nil, errs.ErrNotFound) when missing.
func (r *MutatingProposalRepo) Get(ctx context.Context, id string) (*model.MutatingProposal, error) {
	var p model.MutatingProposal
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

// UpdateDecisionToExpired transitions a pending proposal to
// DecisionExpired and stamps ExpiredAt. The expirer calls this when
// a proposal's TTL has elapsed without a reviewer decision.
//
// Validates the transition via MutatingProposal.ValidateTransition;
// returns errs.ErrInvalid for illegal transitions.
func (r *MutatingProposalRepo) UpdateDecisionToExpired(ctx context.Context, id, reason string) error {
	if id == "" {
		return errs.ErrInvalid
	}
	now := time.Now().UTC()

	// Load + validate transition. We do this in two steps (read then
	// write) to avoid relying on gorm's CHECK constraint for the
	// business rule — the app layer is the source of truth.
	var p model.MutatingProposal
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errs.ErrNotFound
		}
		return err
	}
	if err := p.ValidateTransition(model.DecisionExpired); err != nil {
		return fmt.Errorf("store: %w: %v", errs.ErrInvalid, err)
	}

	res := r.db.WithContext(ctx).Model(&model.MutatingProposal{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"decision":        model.DecisionExpired,
			"decided_at":      now,
			"decision_reason": reason,
			"expired_at":      now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// MarkRolledBack transitions an approved proposal to
// DecisionRolledBack and stamps RolledBackAt. Records rollbackOfID
// in the RollbackOf column so audit queries can link the rollback
// to the original mutating action.
func (r *MutatingProposalRepo) MarkRolledBack(ctx context.Context, id, rollbackOfID string) error {
	if id == "" {
		return errs.ErrInvalid
	}
	now := time.Now().UTC()

	var p model.MutatingProposal
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errs.ErrNotFound
		}
		return err
	}
	if err := p.ValidateTransition(model.DecisionRolledBack); err != nil {
		return fmt.Errorf("store: %w: %v", errs.ErrInvalid, err)
	}

	res := r.db.WithContext(ctx).Model(&model.MutatingProposal{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"decision":       model.DecisionRolledBack,
			"rolled_back_at": now,
			"rollback_of":    rollbackOfID,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// ListPendingBefore returns pending proposals whose effective deadline
// (created_at + ttl_seconds) is before cutoff. The expirer uses this
// to find candidates for auto-decline. Limit caps the result for
// batch safety; pass 0 for "no limit" (caller's responsibility).
func (r *MutatingProposalRepo) ListPendingBefore(ctx context.Context, cutoff time.Time, limit int) ([]model.MutatingProposal, error) {
	q := r.db.WithContext(ctx).Where("decision = ?", model.DecisionPending)
	if limit > 0 {
		q = q.Limit(limit)
	}
	var allPending []model.MutatingProposal
	if err := q.Find(&allPending).Error; err != nil {
		return nil, err
	}
	out := make([]model.MutatingProposal, 0, len(allPending))
	for i := range allPending {
		deadline := allPending[i].CreatedAt.Add(time.Duration(allPending[i].EffectiveTTL()) * time.Second)
		if deadline.Before(cutoff) {
			out = append(out, allPending[i])
		}
	}
	return out, nil
}

// ListExpiredForAudit returns proposals that were auto-declined
// after the given timestamp. Used by the audit export endpoint.
func (r *MutatingProposalRepo) ListExpiredForAudit(ctx context.Context, since time.Time, limit int) ([]model.MutatingProposal, error) {
	var out []model.MutatingProposal
	q := r.db.WithContext(ctx).Where("decision = ?", model.DecisionExpired)
	if !since.IsZero() {
		q = q.Where("expired_at >= ?", since.UTC())
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Order("expired_at DESC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}
