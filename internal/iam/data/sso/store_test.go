package sso

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	iamodel "github.com/vincent-wuhan/opskeeper/internal/iam/model"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&iamodel.OrgSSOConfig{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func sampleConfig(org, name string) *iamodel.OrgSSOConfig {
	return &iamodel.OrgSSOConfig{
		OrgID:         org,
		ProviderType:  iamodel.SSOProviderTypeOIDC,
		ProviderName:  name,
		DisplayName:   "Okta 生产",
		IssuerURL:     "https://company.okta.com",
		ClientID:      "client-123",
		ClientSecret:  "encrypted-secret",
		RedirectURL:   "https://opskeeper.example.com/auth/sso/okta-prod/callback",
		Scopes:        `["openid","profile","email","groups"]`,
		ClaimMappings: `{"groups":{"opskeeper-admins":"admin","opskeeper-editors":"editor"},"fallback_role":"viewer"}`,
		DefaultRole:   "viewer",
		Enabled:       true,
	}
}

func TestOrgSSOConfigStore_Create(t *testing.T) {
	db := openTestDB(t)
	s := NewOrgSSOConfigStore(db)
	ctx := context.Background()

	cfg := sampleConfig("org-1", "okta-prod")
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cfg.ID == 0 {
		t.Error("expected ID assigned")
	}
}

func TestOrgSSOConfigStore_Create_Duplicate(t *testing.T) {
	db := openTestDB(t)
	s := NewOrgSSOConfigStore(db)
	ctx := context.Background()

	cfg := sampleConfig("org-1", "okta-prod")
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("first create: %v", err)
	}
	dup := sampleConfig("org-1", "okta-prod")
	if err := s.Create(ctx, dup); err == nil {
		t.Error("expected duplicate error")
	}
}

func TestOrgSSOConfigStore_Create_InvalidProviderType(t *testing.T) {
	db := openTestDB(t)
	s := NewOrgSSOConfigStore(db)
	cfg := sampleConfig("org-1", "x")
	cfg.ProviderType = "magic-link"
	if err := s.Create(context.Background(), cfg); err == nil {
		t.Error("expected error on invalid provider type")
	}
}

func TestOrgSSOConfigStore_Get(t *testing.T) {
	db := openTestDB(t)
	s := NewOrgSSOConfigStore(db)
	ctx := context.Background()
	_ = s.Create(ctx, sampleConfig("org-1", "okta-prod"))

	got, err := s.Get(ctx, "org-1", "okta-prod", false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected config")
	}
	if got.IssuerURL != "https://company.okta.com" {
		t.Errorf("IssuerURL=%q", got.IssuerURL)
	}
}

func TestOrgSSOConfigStore_Get_Disabled(t *testing.T) {
	db := openTestDB(t)
	s := NewOrgSSOConfigStore(db)
	ctx := context.Background()
	cfg := sampleConfig("org-1", "okta-prod")
	cfg.Enabled = false
	_ = s.Create(ctx, cfg)

	got, err := s.Get(ctx, "org-1", "okta-prod", false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Error("expected nil when filter enabled-only and row disabled")
	}
	got, err = s.Get(ctx, "org-1", "okta-prod", true)
	if err != nil || got == nil {
		t.Errorf("expected enabledAny=true to return row, got=%v err=%v", got, err)
	}
}

func TestOrgSSOConfigStore_Get_TenantIsolation(t *testing.T) {
	db := openTestDB(t)
	s := NewOrgSSOConfigStore(db)
	ctx := context.Background()
	_ = s.Create(ctx, sampleConfig("org-A", "okta-prod"))

	got, err := s.Get(ctx, "org-B", "okta-prod", true)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Error("org-B should not see org-A's config")
	}
}

func TestOrgSSOConfigStore_ListForOrg(t *testing.T) {
	db := openTestDB(t)
	s := NewOrgSSOConfigStore(db)
	ctx := context.Background()

	_ = s.Create(ctx, sampleConfig("org-1", "okta-prod"))
	_ = s.Create(ctx, sampleConfig("org-1", "azure-ad"))
	_ = s.Create(ctx, sampleConfig("org-2", "okta-prod"))

	out, err := s.ListForOrg(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListForOrg: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("len=%d, want 2", len(out))
	}
}

func TestOrgSSOConfigStore_Update(t *testing.T) {
	db := openTestDB(t)
	s := NewOrgSSOConfigStore(db)
	ctx := context.Background()
	cfg := sampleConfig("org-1", "okta-prod")
	_ = s.Create(ctx, cfg)

	cfg.DisplayName = "Okta 主域"
	if err := s.Update(ctx, cfg); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Get(ctx, "org-1", "okta-prod", true)
	if got.DisplayName != "Okta 主域" {
		t.Errorf("DisplayName=%q", got.DisplayName)
	}
}

func TestOrgSSOConfigStore_Delete(t *testing.T) {
	db := openTestDB(t)
	s := NewOrgSSOConfigStore(db)
	ctx := context.Background()
	cfg := sampleConfig("org-1", "okta-prod")
	_ = s.Create(ctx, cfg)

	if err := s.Delete(ctx, cfg.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ := s.Get(ctx, "org-1", "okta-prod", true)
	if got != nil {
		t.Error("expected nil after delete")
	}
}
