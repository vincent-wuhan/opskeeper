package model

import "time"

// OrgSSOConfig is the per-org SSO provider configuration. A single
// org can have multiple rows (different IdPs / tenants) — the
// login page lists them and the user picks. ProviderName is the
// unique key within an org; the (OrgID, ProviderName) pair is the
// natural lookup key.
//
// The IdP protocol details are opaque to the rest of the system;
// SSOService dispatches by ProviderType to the matching IDPAdapter.
// ClientSecret is stored KMS-encrypted (see credinject); the
// Store.Get method transparently decrypts on read.
type OrgSSOConfig struct {
	ID            uint      `gorm:"primaryKey;column:id"`
	OrgID         string    `gorm:"size:64;not null;index;column:org_id"`
	ProviderType  string    `gorm:"size:16;not null;column:provider_type"` // oidc | saml
	ProviderName  string    `gorm:"size:64;not null;column:provider_name"` // "okta-prod"
	DisplayName   string    `gorm:"size:128;column:display_name"`          // user-visible
	IssuerURL     string    `gorm:"size:512;not null;column:issuer_url"`
	ClientID      string    `gorm:"size:256;not null;column:client_id"`
	ClientSecret  string    `gorm:"size:1024;not null;column:client_secret"` // KMS-encrypted
	RedirectURL   string    `gorm:"size:512;column:redirect_url"`
	Scopes        string    `gorm:"size:512;column:scopes"`          // JSON array
	ClaimMappings string    `gorm:"type:text;column:claim_mappings"` // JSON: {groups: {...}, fallback_role}
	DefaultRole   string    `gorm:"size:32;column:default_role"`     // "viewer"
	Enabled       bool      `gorm:"not null;column:enabled"`
	CreatedAt     time.Time `gorm:"not null;column:created_at"`
	UpdatedAt     time.Time `gorm:"not null;column:updated_at"`
}

// TableName pins the table name to a stable string so AutoMigrate
// doesn't depend on GORM's pluralization rules.
func (OrgSSOConfig) TableName() string {
	return "org_sso_configs"
}

// ProviderType constants — kept on the model so the store layer
// doesn't have to import the sso package just to validate.
const (
	SSOProviderTypeOIDC = "oidc"
	SSOProviderTypeSAML = "saml"
)

// AuthMethod constants — used in users.auth_provider and
// auth_sessions.auth_method to distinguish how a row was created.
const (
	AuthMethodLocal = "local"
	AuthMethodOIDC  = "oidc"
	AuthMethodSAML  = "saml"
)

// DefaultRoleFallback returns the role to grant when no IdP group
// matches and no explicit fallback_role is configured. The chain
// is: configured fallback → "viewer" (the smallest grant we
// offer). Super-admin is NEVER a default — operators must promote
// explicitly through the casbin group table.
func DefaultRoleFallback(configured string) string {
	if configured != "" {
		return configured
	}
	return "viewer"
}
