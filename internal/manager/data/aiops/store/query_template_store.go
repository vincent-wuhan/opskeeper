package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// QueryTemplateStore is the GORM-backed persistence for chat_to_query
// translation caches. The chat_to_query tool consumes it through a
// narrow interface (chat_to_query.TemplateSink) so unit tests can stub
// it; this file is the production binding.
//
// All public methods scope queries by TenantID — the chat_to_query tool
// MUST pass the request's tenant_id (pulled from the tenant_bind
// decorator). Cross-tenant leakage here would let one org peek at the
// queries another org's users asked, which is a real PII-style leak
// (NL questions frequently contain customer IDs, host names, etc.).
type QueryTemplateStore struct {
	db *gorm.DB
}

// NewQueryTemplateStore wires the repo around an opened *gorm.DB.
func NewQueryTemplateStore(db *gorm.DB) *QueryTemplateStore {
	return &QueryTemplateStore{db: db}
}

// Get returns the cached template for (tenant, signal, nl_hash) ONLY if
// it's warm — Hits >= WarmTemplateHits AND LastUsedAt within
// WarmTemplateTTL. Cold rows return (nil, nil) so callers can fall
// through to the LLM translation path without a special "not found"
// branch.
//
// Hits is NOT bumped here; that's the caller's job after the cached
// query actually executes successfully. Bumping on Get would let a
// "preview then cancel" path artificially warm a bad query.
func (s *QueryTemplateStore) Get(ctx context.Context, tenantID, signal, nlHash string) (*model.QueryTemplate, error) {
	if tenantID == "" || signal == "" || nlHash == "" {
		return nil, errs.ErrInvalid
	}
	var t model.QueryTemplate
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND signal = ? AND nl_hash = ?", tenantID, signal, nlHash).
		First(&t).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if t.Hits < model.WarmTemplateHits {
		return nil, nil
	}
	if time.Since(t.LastUsedAt) > model.WarmTemplateTTL {
		return nil, nil
	}
	return &t, nil
}

// Upsert inserts a new template row, or bumps Hits + LastUsedAt on an
// existing (tenant, signal, nl_hash) row. Both branches run inside a
// single transaction so a crash mid-write doesn't leave a half-row.
//
// Called only on a SUCCESSFUL execution (validator passed + query
// client returned no error). The chat_to_query tool MUST NOT call
// Upsert on a failed execution — see design doc §4 (错误路径).
func (s *QueryTemplateStore) Upsert(ctx context.Context, tpl *model.QueryTemplate) error {
	if tpl == nil {
		return errs.ErrInvalid
	}
	if tpl.TenantID == "" || tpl.Signal == "" || tpl.NLHash == "" || tpl.Expr == "" {
		return errs.ErrInvalid
	}
	switch tpl.Signal {
	case model.QueryTemplateSignalPromQL, model.QueryTemplateSignalLogQL, model.QueryTemplateSignalTraceQL:
	default:
		return errs.ErrInvalid
	}
	switch tpl.Risk {
	case model.QueryTemplateRiskLow, model.QueryTemplateRiskMedium, model.QueryTemplateRiskHigh:
	default:
		return errs.ErrInvalid
	}
	now := time.Now().UTC()
	if tpl.LastUsedAt.IsZero() {
		tpl.LastUsedAt = now
	}
	if tpl.CreatedAt.IsZero() {
		tpl.CreatedAt = now
	}
	if tpl.Hits == 0 {
		tpl.Hits = 1
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Try to find an existing row first; UPDATE is cheaper than
		// INSERT-on-duplicate for the hot path (most Upserts bump).
		var existing model.QueryTemplate
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND signal = ? AND nl_hash = ?", tpl.TenantID, tpl.Signal, tpl.NLHash).
			First(&existing).Error
		if err == nil {
			existing.Hits++
			existing.LastUsedAt = now
			existing.Expr = tpl.Expr
			existing.Explanation = tpl.Explanation
			existing.Risk = tpl.Risk
			return tx.Save(&existing).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(tpl).Error
	})
}

// Touch increments Hits and stamps LastUsedAt without re-writing the
// expression. Used by the chat_to_query tool after a cached template
// executes successfully (so the next read still passes the warm gate).
func (s *QueryTemplateStore) Touch(ctx context.Context, id uint) error {
	if id == 0 {
		return errs.ErrInvalid
	}
	return s.db.WithContext(ctx).
		Model(&model.QueryTemplate{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"hits":         gorm.Expr("hits + 1"),
			"last_used_at": time.Now().UTC(),
		}).Error
}

// Delete removes a template by ID. Reserved for the
// "user marks template as bad" follow-up; not wired in this change.
func (s *QueryTemplateStore) Delete(ctx context.Context, id uint) error {
	if id == 0 {
		return errs.ErrInvalid
	}
	return s.db.WithContext(ctx).Delete(&model.QueryTemplate{}, id).Error
}

// ListForTenant returns the most recently used templates for a tenant,
// capped at `limit`. Used by the future admin/debug page (out of scope
// for this change) and by retention sweeps.
func (s *QueryTemplateStore) ListForTenant(ctx context.Context, tenantID string, limit int) ([]model.QueryTemplate, error) {
	if tenantID == "" {
		return nil, errs.ErrInvalid
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []model.QueryTemplate
	err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("last_used_at DESC").
		Limit(limit).
		Find(&out).Error
	return out, err
}
