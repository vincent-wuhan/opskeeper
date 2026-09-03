package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func sampleTemplate(tenant, signal string) *model.QueryTemplate {
	return &model.QueryTemplate{
		TenantID:    tenant,
		NLHash:      "hash-abc",
		Signal:      signal,
		Question:    "Redis 内存使用率",
		Expr:        "redis_memory_used_bytes",
		Risk:        model.QueryTemplateRiskLow,
		Explanation: "Redis used bytes",
		Hits:        1,
		LastUsedAt:  time.Now().UTC(),
		CreatedAt:   time.Now().UTC(),
	}
}

func TestQueryTemplateStore_Upsert_Insert(t *testing.T) {
	db := openTestDB(t)
	s := NewQueryTemplateStore(db)
	ctx := context.Background()

	tpl := sampleTemplate("tenant-1", model.QueryTemplateSignalPromQL)
	if err := s.Upsert(ctx, tpl); err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	got, err := s.Get(ctx, "tenant-1", model.QueryTemplateSignalPromQL, "hash-abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// First hit is below WarmTemplateHits — Get returns (nil, nil)
	// by design (cold-start protection).
	if got != nil {
		t.Errorf("expected nil (cold), got %+v", got)
	}
}

func TestQueryTemplateStore_Upsert_BumpHits(t *testing.T) {
	db := openTestDB(t)
	s := NewQueryTemplateStore(db)
	ctx := context.Background()

	// 3 successful executions → hits reaches 3, then warm.
	for i := 0; i < 3; i++ {
		tpl := sampleTemplate("tenant-1", model.QueryTemplateSignalPromQL)
		tpl.Hits = i + 1 // first insert hits=1, subsequent bumps
		if err := s.Upsert(ctx, tpl); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	got, err := s.Get(ctx, "tenant-1", model.QueryTemplateSignalPromQL, "hash-abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatalf("expected warm template, got nil")
	}
	if got.Hits != 3 {
		t.Errorf("Hits=%d, want 3", got.Hits)
	}
}

func TestQueryTemplateStore_Get_TenantIsolation(t *testing.T) {
	db := openTestDB(t)
	s := NewQueryTemplateStore(db)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.Upsert(ctx, sampleTemplate("tenant-A", model.QueryTemplateSignalPromQL)); err != nil {
			t.Fatalf("upsert A: %v", err)
		}
	}
	got, err := s.Get(ctx, "tenant-B", model.QueryTemplateSignalPromQL, "hash-abc")
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if got != nil {
		t.Errorf("tenant-B should not see tenant-A's template")
	}
}

func TestQueryTemplateStore_Get_StaleCold(t *testing.T) {
	db := openTestDB(t)
	s := NewQueryTemplateStore(db)
	ctx := context.Background()

	// Insert + bump to warm.
	for i := 0; i < 3; i++ {
		_ = s.Upsert(ctx, sampleTemplate("tenant-1", model.QueryTemplateSignalPromQL))
	}
	// Force LastUsedAt to 31d ago directly via DB.
	db.Model(&model.QueryTemplate{}).
		Where("tenant_id = ?", "tenant-1").
		Update("last_used_at", time.Now().Add(-31*24*time.Hour))
	got, err := s.Get(ctx, "tenant-1", model.QueryTemplateSignalPromQL, "hash-abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Errorf("stale template should return nil")
	}
}

func TestQueryTemplateStore_InvalidSignal(t *testing.T) {
	db := openTestDB(t)
	s := NewQueryTemplateStore(db)
	tpl := sampleTemplate("tenant-1", "bogus")
	err := s.Upsert(context.Background(), tpl)
	if err == nil {
		t.Error("expected error on invalid signal")
	}
}

func TestQueryTemplateStore_InvalidRisk(t *testing.T) {
	db := openTestDB(t)
	s := NewQueryTemplateStore(db)
	tpl := sampleTemplate("tenant-1", model.QueryTemplateSignalPromQL)
	tpl.Risk = "extreme"
	err := s.Upsert(context.Background(), tpl)
	if err == nil {
		t.Error("expected error on invalid risk")
	}
}

func TestQueryTemplateStore_Touch(t *testing.T) {
	db := openTestDB(t)
	s := NewQueryTemplateStore(db)
	ctx := context.Background()

	_ = s.Upsert(ctx, sampleTemplate("tenant-1", model.QueryTemplateSignalPromQL))
	var got model.QueryTemplate
	db.Where("tenant_id = ?", "tenant-1").First(&got)
	id := got.ID
	before := got.Hits
	beforeUsed := got.LastUsedAt

	time.Sleep(10 * time.Millisecond)
	if err := s.Touch(ctx, id); err != nil {
		t.Fatalf("touch: %v", err)
	}
	db.Where("id = ?", id).First(&got)
	if got.Hits != before+1 {
		t.Errorf("Hits=%d, want %d", got.Hits, before+1)
	}
	if !got.LastUsedAt.After(beforeUsed) {
		t.Errorf("LastUsedAt should advance")
	}
}

func TestQueryTemplateStore_ListForTenant(t *testing.T) {
	db := openTestDB(t)
	s := NewQueryTemplateStore(db)
	ctx := context.Background()

	for _, sig := range []string{model.QueryTemplateSignalPromQL, model.QueryTemplateSignalLogQL} {
		tpl := sampleTemplate("tenant-1", sig)
		tpl.NLHash = "hash-" + sig
		_ = s.Upsert(ctx, tpl)
	}
	out, err := s.ListForTenant(ctx, "tenant-1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("len=%d, want 2", len(out))
	}
}

func TestQueryTemplateStore_Delete(t *testing.T) {
	db := openTestDB(t)
	s := NewQueryTemplateStore(db)
	ctx := context.Background()

	_ = s.Upsert(ctx, sampleTemplate("tenant-1", model.QueryTemplateSignalPromQL))
	var got model.QueryTemplate
	db.Where("tenant_id = ?", "tenant-1").First(&got)
	if err := s.Delete(ctx, got.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var afterDelete model.QueryTemplate
	var err error
	err = db.Where("id = ?", got.ID).First(&afterDelete).Error
	if err == nil {
		t.Errorf("template not deleted: found id=%d", afterDelete.ID)
	}
}
