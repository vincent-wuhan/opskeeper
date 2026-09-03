package store

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

func newDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newLabel(rt, rid, sens string) *DataSensitivityLabel {
	now := time.Now().UTC()
	return &DataSensitivityLabel{
		ResourceType: rt,
		ResourceID:   rid,
		Sensitivity:  sens,
		LabelSource:  string(SourceManual),
		Confidence:   1.0,
		LabeledBy:    "tester",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestRepo_CreateGet(t *testing.T) {
	db := newDB(t)
	r := NewRepo(db)
	ctx := context.Background()

	if err := r.Create(ctx, newLabel("pg", "t", "Confidential")); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, "pg", "t")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sensitivity != "Confidential" {
		t.Errorf("sensitivity = %s", got.Sensitivity)
	}
}

func TestRepo_UpsertOverwritesOnPKCollision(t *testing.T) {
	db := newDB(t)
	r := NewRepo(db)
	ctx := context.Background()

	_ = r.Create(ctx, newLabel("pg", "t", "Public"))
	updated := newLabel("pg", "t", "Restricted")
	updated.LabeledBy = "audit-override"
	if err := r.Create(ctx, updated); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(ctx, "pg", "t")
	if got.Sensitivity != "Restricted" || got.LabeledBy != "audit-override" {
		t.Errorf("upsert did not overwrite: %+v", got)
	}
}

func TestRepo_GetNotFound(t *testing.T) {
	db := newDB(t)
	r := NewRepo(db)
	_, err := r.Get(context.Background(), "pg", "x")
	if err != errs.ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRepo_ListWithFilters(t *testing.T) {
	db := newDB(t)
	r := NewRepo(db)
	ctx := context.Background()

	_ = r.Create(ctx, newLabel("pg", "a", "Public"))
	_ = r.Create(ctx, newLabel("pg", "b", "Confidential"))
	_ = r.Create(ctx, newLabel("redis", "c", "Confidential"))

	items, total, err := r.List(ctx, "Confidential", "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Errorf("filter sensitivity failed: total=%d len=%d", total, len(items))
	}

	items, total, _ = r.List(ctx, "", string(SourceManual), 100, 0)
	if total != 3 || len(items) != 3 {
		t.Errorf("filter source failed: total=%d len=%d", total, len(items))
	}
}

func TestRepo_Delete(t *testing.T) {
	db := newDB(t)
	r := NewRepo(db)
	ctx := context.Background()

	_ = r.Create(ctx, newLabel("pg", "t", "Public"))
	if err := r.Delete(ctx, "pg", "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "pg", "t"); err != errs.ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
	if err := r.Delete(ctx, "pg", "t"); err != errs.ErrNotFound {
		t.Errorf("second delete should ErrNotFound, got %v", err)
	}
}

func TestRepo_ListByResourceTypePrefix(t *testing.T) {
	db := newDB(t)
	r := NewRepo(db)
	ctx := context.Background()

	_ = r.Create(ctx, newLabel("pg", "db_main_tbl_a", "Public"))
	_ = r.Create(ctx, newLabel("pg", "db_main_tbl_b", "Confidential"))
	_ = r.Create(ctx, newLabel("pg", "other_tbl_c", "Public"))

	items, err := r.ListByResourceTypePrefix(ctx, "pg", "db_main")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 with prefix db_main, got %d", len(items))
	}
}

func TestDataSensitivityLabel_ToSensitivity(t *testing.T) {
	confidential := newLabel("pg", "t", "Confidential")
	s, err := confidential.ToSensitivity()
	if err != nil {
		t.Fatal(err)
	}
	if s != "Confidential" { // plain string compare
		t.Errorf("ToSensitivity = %s", s)
	}

	bad := newLabel("pg", "x", "bogus")
	if _, err := bad.ToSensitivity(); err == nil {
		t.Error("ToSensitivity should error on invalid string")
	}
}
