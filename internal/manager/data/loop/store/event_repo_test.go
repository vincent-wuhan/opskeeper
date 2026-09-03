package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestEventRepoDB_AppendAndRead(t *testing.T) {
	db := newTestDB(t)
	repo := NewEventRepoDB(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Millisecond)
	e := &loopmodel.Event{
		IncidentID:     "inc-1",
		TenantID:       "T1",
		EventType:      loopmodel.EventTypePhaseEntered,
		Phase:          "detected",
		IdempotencyKey: "inc-1:detected:phase_entered:1",
		Payload:        `{"k":"v"}`,
		CreatedAt:      now,
	}
	if err := repo.AppendEvent(ctx, e); err != nil {
		t.Fatalf("append: %v", err)
	}
	if e.ID == 0 {
		t.Fatalf("expected ID populated after AppendEvent")
	}

	// Replay with same idempotency key → should coalesce, not error.
	cp := *e
	cp.ID = 0
	if err := repo.AppendEvent(ctx, &cp); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if cp.ID != e.ID {
		t.Fatalf("expected replay to coalesce id=%d, got %d", e.ID, cp.ID)
	}

	// ReadEvents returns ordered by created_at ASC.
	rows, err := repo.ReadEvents(ctx, "T1", "inc-1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].EventType != loopmodel.EventTypePhaseEntered {
		t.Fatalf("event_type mismatch: %s", rows[0].EventType)
	}

	// Tenant isolation: T2 must not see T1 rows.
	rows2, err := repo.ReadEvents(ctx, "T2", "inc-1")
	if err != nil {
		t.Fatalf("read T2: %v", err)
	}
	if len(rows2) != 0 {
		t.Fatalf("expected 0 rows for T2, got %d", len(rows2))
	}
}

func TestEventRepoDB_NormalizesEmptyPayloadAndLongKey(t *testing.T) {
	db := newTestDB(t)
	repo := NewEventRepoDB(db)
	event := &loopmodel.Event{
		IncidentID:     "incident-long-id-for-mysql-idempotency-key-limit",
		TenantID:       "T1",
		EventType:      loopmodel.EventTypePhaseEntered,
		Phase:          "detected",
		IdempotencyKey: "incident-long-id-for-mysql-idempotency-key-limit:detected:phase_entered:1",
		CreatedAt:      time.Now().UTC(),
	}

	if err := repo.AppendEvent(context.Background(), event); err != nil {
		t.Fatalf("append: %v", err)
	}
	rows, err := repo.ReadEvents(context.Background(), "T1", event.IncidentID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 || rows[0].Payload != "{}" {
		t.Fatalf("events = %+v, want normalized empty JSON payload", rows)
	}
	if len(rows[0].IdempotencyKey) != 64 {
		t.Fatalf("idempotency key length = %d, want 64", len(rows[0].IdempotencyKey))
	}
}

func TestEventRepoDB_ReplayWithLongKeyCoalesces(t *testing.T) {
	db := newTestDB(t)
	repo := NewEventRepoDB(db)
	ctx := context.Background()
	event := &loopmodel.Event{
		IncidentID:     "incident-long-id-for-mysql-idempotency-key-limit",
		TenantID:       "T1",
		EventType:      loopmodel.EventTypePhaseEntered,
		Phase:          "detected",
		IdempotencyKey: "incident-long-id-for-mysql-idempotency-key-limit:detected:phase_entered:1",
		CreatedAt:      time.Now().UTC(),
	}
	if err := repo.AppendEvent(ctx, event); err != nil {
		t.Fatalf("append: %v", err)
	}

	replay := *event
	replay.ID = 0
	if err := repo.AppendEvent(ctx, &replay); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.ID != event.ID {
		t.Fatalf("expected replay to coalesce id=%d, got %d", event.ID, replay.ID)
	}
}

func TestContractRepoDB_WriteAndRead(t *testing.T) {
	db := newTestDB(t)
	repo := NewContractRepoDB(db)
	ctx := context.Background()

	c := &loopmodel.Contract{
		IncidentID:     "inc-1",
		TenantID:       "T1",
		Phase:          "investigated",
		Type:           "RootCauseJSON",
		SchemaVer:      "v1",
		Payload:        `{"k":"v"}`,
		SizeBytes:      10,
		StorageBackend: loopmodel.StorageBackendDB,
		CreatedAt:      time.Now().UTC(),
	}
	if err := repo.WriteContract(ctx, c); err != nil {
		t.Fatalf("write: %v", err)
	}
	if c.ID == 0 {
		t.Fatalf("expected ID populated")
	}

	got, err := repo.ReadContract(ctx, "T1", "inc-1", "investigated", "RootCauseJSON")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil {
		t.Fatalf("expected contract row, got nil")
	}
	if got.Payload != c.Payload {
		t.Fatalf("payload mismatch")
	}

	// Negative: different contract type returns nil.
	miss, err := repo.ReadContract(ctx, "T1", "inc-1", "investigated", "CritiqueScore")
	if err != nil {
		t.Fatalf("read miss: %v", err)
	}
	if miss != nil {
		t.Fatalf("expected nil for missing type, got %+v", miss)
	}
}

func TestContractRepoDB_FiltersConflictingIncidentByTenant(t *testing.T) {
	db := newTestDB(t)
	repo := NewContractRepoDB(db)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, tenantID := range []string{"T1", "T2"} {
		contract := &loopmodel.Contract{
			IncidentID: "conflicting-inc", TenantID: tenantID,
			Phase: "investigated", Type: "ApprovalDecision", SchemaVer: "v1",
			Payload: `{"tenant":"` + tenantID + `"}`, CreatedAt: now,
		}
		if err := repo.WriteContract(ctx, contract); err != nil {
			t.Fatalf("write %s: %v", tenantID, err)
		}
	}
	for _, tenantID := range []string{"T1", "T2"} {
		got, err := repo.ReadContract(ctx, tenantID, "conflicting-inc", "investigated", "ApprovalDecision")
		if err != nil {
			t.Fatalf("read %s: %v", tenantID, err)
		}
		if got == nil || got.TenantID != tenantID {
			t.Fatalf("read %s = %+v, want same tenant contract", tenantID, got)
		}
	}
	missed, err := repo.ReadContract(ctx, "T3", "conflicting-inc", "investigated", "ApprovalDecision")
	if err != nil {
		t.Fatalf("read T3: %v", err)
	}
	if missed != nil {
		t.Fatalf("T3 read leaked contract %+v", missed)
	}
}

// ReadContractByID 多租户安全：⑥ 修复。
//   - 写 tenant=T1 + tenant=T2 两条 ApprovalDecision
//   - 用 tenant=T1 读 → 必须返回 T1 那条
//   - 用 tenant=T2 读同一 ID → 必须返回 nil（不返回 T1 的行）
//   - 用 tenant="" 读 → 错误
func TestContractRepoDB_ReadContractByID_TenantIsolation(t *testing.T) {
	db := newTestDB(t)
	repo := NewContractRepoDB(db)
	ctx := context.Background()
	now := time.Now().UTC()
	var t1ID, t2ID int64
	for _, c := range []*loopmodel.Contract{
		{IncidentID: "by-id-inc", TenantID: "T1", Phase: "approved", Type: "ApprovalDecision",
			SchemaVer: "v1", Payload: `{"who":"t1"}`, CreatedAt: now},
		{IncidentID: "by-id-inc", TenantID: "T2", Phase: "approved", Type: "ApprovalDecision",
			SchemaVer: "v1", Payload: `{"who":"t2"}`, CreatedAt: now},
	} {
		if err := repo.WriteContract(ctx, c); err != nil {
			t.Fatalf("write %s: %v", c.TenantID, err)
		}
		if c.TenantID == "T1" {
			t1ID = c.ID
		} else {
			t2ID = c.ID
		}
	}
	if t1ID == 0 || t2ID == 0 {
		t.Fatalf("expected both writes to set IDs, got T1=%d T2=%d", t1ID, t2ID)
	}

	// tenant=T1 读 T1 的 ID → 命中
	gotT1, err := repo.ReadContractByID(ctx, "T1", t1ID)
	if err != nil {
		t.Fatalf("T1 read own id: %v", err)
	}
	if gotT1 == nil || gotT1.TenantID != "T1" {
		t.Errorf("T1 read own id returned %+v, want T1 contract", gotT1)
	}

	// 关键反例：tenant=T1 尝试读 T2 的 ID → 阻断，返回 nil
	cross, err := repo.ReadContractByID(ctx, "T1", t2ID)
	if err != nil {
		t.Fatalf("cross-tenant read should be silent nil, got error: %v", err)
	}
	if cross != nil {
		t.Errorf("cross-tenant read leaked %+v (tenant_id=%q)", cross, cross.TenantID)
	}

	// tenant=T2 读 T1 的 ID → 也阻断
	cross2, err := repo.ReadContractByID(ctx, "T2", t1ID)
	if err != nil {
		t.Fatalf("cross-tenant read T2→T1: %v", err)
	}
	if cross2 != nil {
		t.Errorf("cross-tenant read T2→T1 leaked %+v", cross2)
	}

	// tenant="" 拒绝
	if _, err := repo.ReadContractByID(ctx, "", t1ID); err == nil {
		t.Errorf("empty tenantID should be rejected")
	}

	// id<=0 拒绝
	if _, err := repo.ReadContractByID(ctx, "T1", 0); err == nil {
		t.Errorf("id=0 should be rejected")
	}
}
