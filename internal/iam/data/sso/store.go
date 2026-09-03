package sso

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	iamodel "github.com/vincent-wuhan/opskeeper/internal/iam/model"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// OrgSSOConfigStore persists per-org SSO provider configs. All reads
// scope by OrgID — there's no global "list all providers" path,
// because the manager UI shouldn't be able to see another tenant's
// SSO configs.
type OrgSSOConfigStore struct {
	db *gorm.DB
}

// NewOrgSSOConfigStore wires the store around an opened *gorm.DB.
func NewOrgSSOConfigStore(db *gorm.DB) *OrgSSOConfigStore {
	return &OrgSSOConfigStore{db: db}
}

// Get returns the SSO config for (orgID, providerName) or (nil, nil)
// when not found. Enabled-only filter is OPTIONAL — callers that want
// the raw row (admin UI) pass enabledAny=true.
func (s *OrgSSOConfigStore) Get(ctx context.Context, orgID, providerName string, enabledAny bool) (*iamodel.OrgSSOConfig, error) {
	if orgID == "" || providerName == "" {
		return nil, errs.ErrInvalid
	}
	q := s.db.WithContext(ctx).
		Where("org_id = ? AND provider_name = ?", orgID, providerName)
	if !enabledAny {
		q = q.Where("enabled = ?", true)
	}
	var cfg iamodel.OrgSSOConfig
	if err := q.First(&cfg).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &cfg, nil
}

// ListForOrg returns all SSO configs for an org, enabled or not.
// Used by the admin UI to render the "SSO providers" page.
func (s *OrgSSOConfigStore) ListForOrg(ctx context.Context, orgID string) ([]iamodel.OrgSSOConfig, error) {
	if orgID == "" {
		return nil, errs.ErrInvalid
	}
	var out []iamodel.OrgSSOConfig
	err := s.db.WithContext(ctx).
		Where("org_id = ?", orgID).
		Order("created_at DESC").
		Find(&out).Error
	return out, err
}

// Create inserts a new SSO config. The (OrgID, ProviderName) pair is
// unique — duplicate creates return errs.ErrConflict so callers can
// handle it without importing pg/mysql.
func (s *OrgSSOConfigStore) Create(ctx context.Context, cfg *iamodel.OrgSSOConfig) error {
	if cfg == nil || cfg.OrgID == "" || cfg.ProviderName == "" {
		return errs.ErrInvalid
	}
	switch cfg.ProviderType {
	case iamodel.SSOProviderTypeOIDC, iamodel.SSOProviderTypeSAML:
	default:
		return errs.ErrInvalid
	}
	now := time.Now().UTC()
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = now
	}
	cfg.UpdatedAt = now

	existing, err := s.Get(ctx, cfg.OrgID, cfg.ProviderName, true)
	if err != nil {
		return err
	}
	if existing != nil {
		return errs.ErrConflict
	}
	return s.db.WithContext(ctx).Create(cfg).Error
}

// Update mutates an existing SSO config in place. ID must be set;
// UpdatedAt is stamped automatically.
func (s *OrgSSOConfigStore) Update(ctx context.Context, cfg *iamodel.OrgSSOConfig) error {
	if cfg == nil || cfg.ID == 0 {
		return errs.ErrInvalid
	}
	cfg.UpdatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).
		Model(&iamodel.OrgSSOConfig{}).
		Where("id = ?", cfg.ID).
		Updates(cfg).Error
}

// Delete removes the SSO config by ID. The login page just stops
// showing it; existing user sessions aren't affected (the JWT/cookie
// still validates until expiry).
func (s *OrgSSOConfigStore) Delete(ctx context.Context, id uint) error {
	if id == 0 {
		return errs.ErrInvalid
	}
	return s.db.WithContext(ctx).Delete(&iamodel.OrgSSOConfig{}, id).Error
}
