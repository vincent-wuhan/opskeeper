package authz

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
)

func newTestEnforcer(t *testing.T) (*Enforcer, SensitivityTierRepo) {
	t.Helper()
	dir, err := os.MkdirTemp("", "authz-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dbPath := filepath.Join(dir, "test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// 同时 migrate Authz tier 表 + casbin_rule（New 会自行 AutoMigrate gorm-adapter）
	if err := MigrateSensitivityTier(db); err != nil {
		t.Fatal(err)
	}
	a, err := New(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SeedRolePolicies(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.HydrateMemberships(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	return a, NewSensitivityTierRepo(db)
}

func TestAllowWithSensitivity_PublicAllowed(t *testing.T) {
	a, tierRepo := newTestEnforcer(t)
	ctx := context.Background()

	// 用户 alice（uid=42）在 org 7 是 member；member 没任何 resource reader policy 时 Allow 通过
	_ = a.SyncMembership(ctx, 42, 7, "member")

	ok, err := a.AllowWithSensitivity(ctx, 42, 7, "edge:abc", "read", dataguard.Public, tierRepo)
	if err != nil || !ok {
		t.Errorf("Public should allow: ok=%v err=%v", ok, err)
	}
}

func TestAllowWithSensitivity_RBACDenied(t *testing.T) {
	a, tierRepo := newTestEnforcer(t)
	ctx := context.Background()
	// 不给用户任何 role
	ok, err := a.AllowWithSensitivity(ctx, 42, 7, "edge:abc", "read", dataguard.Public, tierRepo)
	if err != nil || ok {
		t.Errorf("no role should deny: ok=%v err=%v", ok, err)
	}
}

func TestAllowWithSensitivity_TierGate(t *testing.T) {
	a, tierRepo := newTestEnforcer(t)
	ctx := context.Background()

	// alice 是 member of org 7
	_ = a.SyncMembership(ctx, 42, 7, "member")
	// member role 有 read on edge:* — Allow 通过
	// 但 Confidential 资源需要 confidential_reader tier
	ok, _ := a.AllowWithSensitivity(ctx, 42, 7, "edge:abc", "read", dataguard.Confidential, tierRepo)
	if ok {
		t.Error("no tier should NOT allow Confidential")
	}
	// Grant confidential-reader → 通过
	if err := a.GrantSensitivityTier(ctx, tierRepo, 42, 7, TierConfidential, 1); err != nil {
		t.Fatal(err)
	}
	ok, _ = a.AllowWithSensitivity(ctx, 42, 7, "edge:abc", "read", dataguard.Confidential, tierRepo)
	if !ok {
		t.Error("confidential-reader should allow Confidential")
	}
	// Restricted 仍需 restricted_reader
	ok, _ = a.AllowWithSensitivity(ctx, 42, 7, "edge:abc", "read", dataguard.Restricted, tierRepo)
	if ok {
		t.Error("confidential-reader should NOT allow Restricted")
	}
	// Grant topsecret-reader → Restricted 通过
	if err := a.GrantSensitivityTier(ctx, tierRepo, 42, 7, TierTopSecret, 1); err != nil {
		t.Fatal(err)
	}
	ok, _ = a.AllowWithSensitivity(ctx, 42, 7, "edge:abc", "read", dataguard.Restricted, tierRepo)
	if !ok {
		t.Error("topsecret-reader should allow Restricted")
	}
}

func TestAllowWithSensitivity_NilTierRepo(t *testing.T) {
	a, _ := newTestEnforcer(t)
	ctx := context.Background()
	_ = a.SyncMembership(ctx, 42, 7, "member")

	// nil tierRepo 自动 fallback：保持向后兼容（task 2.5）；Restricted 也通过，
	// 由 RBAC role / Phase 3 SensitivityGate 拦截
	ok, err := a.AllowWithSensitivity(ctx, 42, 7, "edge:abc", "read", dataguard.Restricted, nil)
	if err != nil || !ok {
		t.Errorf("nil tierRepo should pass through (backward compat): ok=%v err=%v", ok, err)
	}
	_ = RestrictedConst // silence unused

}

// RestrictedConst 是测试用的 dataguard.Restricted 别名。
const RestrictedConst = dataguard.Restricted
