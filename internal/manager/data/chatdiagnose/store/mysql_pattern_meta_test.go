package store

import (
	"context"
	"sync"
	"testing"
	"time"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// makePattern 构造一个测试用 IncidentPattern。
func makePattern(tenantID, fingerprint string) chatdiagnosemodel.IncidentPattern {
	return chatdiagnosemodel.IncidentPattern{
		TenantID:           tenantID,
		ResourceType:       "pg",
		Symptom:            "long_running_tx",
		RootCauseObject:    "long_running_tx",
		Signature:          "pg:long_running_tx:high",
		SourcePostmortemID: "postmortem-001",
		Fingerprint:        fingerprint,
		Severity:           "high",
		Confidence:         0.85,
	}
}

func TestCompositePatternRepo_IncHitCount_ResolvesTenantSafely(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	repo := NewCompositePatternRepo(meta, nil, nil)
	pattern := makePattern("T_A", "fp_composite_inc")
	ctx := context.Background()
	if err := repo.Save(ctx, &pattern); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := repo.IncHitCount(ctx, pattern.ID); err != nil {
		t.Fatalf("IncHitCount: %v", err)
	}
	got, err := meta.FindByFingerprint(ctx, "T_A", "fp_composite_inc")
	if err != nil {
		t.Fatalf("FindByFingerprint: %v", err)
	}
	if got.HitCount != 1 {
		t.Fatalf("expected HitCount=1, got %d", got.HitCount)
	}
	if err := repo.IncHitCount(ctx, 999999); err != nil {
		t.Fatalf("expected missing pattern to be a no-op, got %v", err)
	}
}

func TestMigrate_IncidentPatternCandidateIndex(t *testing.T) {
	db := newTestDB(t)
	if !db.Migrator().HasIndex(&chatdiagnosemodel.IncidentPattern{}, "idx_incident_pattern_tenant_updated_at_id") {
		t.Fatal("expected composite candidate scan index to exist")
	}
}

func TestMySQLPatternMeta_Save_Insert(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	ctx := context.Background()
	p := makePattern("T1", "abc123def4567890")
	if err := meta.Save(ctx, &p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if p.ID == 0 {
		t.Fatalf("expected non-zero ID after insert, got 0")
	}
}

func TestMySQLPatternMeta_Save_Upsert(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	ctx := context.Background()
	p := makePattern("T1", "fingerprint-001")
	if err := meta.Save(ctx, &p); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	originalID := p.ID
	originalCreatedAt := p.CreatedAt

	// 第二次 Save：同 (tenant, fingerprint)，应该是 UPDATE 路径
	p.Symptom = "different_symptom"
	p.Confidence = 0.95
	if err := meta.Save(ctx, &p); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if p.ID != originalID {
		t.Errorf("expected id to be preserved (%d), got %d", originalID, p.ID)
	}
	if !p.CreatedAt.Equal(originalCreatedAt) {
		t.Errorf("expected CreatedAt preserved (%v), got %v", originalCreatedAt, p.CreatedAt)
	}
	// symptom 应被更新
	got, err := meta.FindByFingerprint(ctx, "T1", "fingerprint-001")
	if err != nil {
		t.Fatalf("FindByFingerprint: %v", err)
	}
	if got == nil {
		t.Fatalf("expected pattern, got nil")
	}
	if got.Symptom != "different_symptom" {
		t.Errorf("expected updated symptom, got %s", got.Symptom)
	}
	if got.Confidence != 0.95 {
		t.Errorf("expected updated confidence 0.95, got %f", got.Confidence)
	}
}

func TestMySQLPatternMeta_Save_MultiTenantIsolation(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	ctx := context.Background()
	pA := makePattern("T_A", "shared_fp")
	if err := meta.Save(ctx, &pA); err != nil {
		t.Fatalf("Save T_A: %v", err)
	}
	pB := makePattern("T_B", "shared_fp")
	if err := meta.Save(ctx, &pB); err != nil {
		t.Fatalf("Save T_B: %v", err)
	}
	// 两个 ID 应不同
	if pA.ID == pB.ID {
		t.Errorf("expected different IDs across tenants, got %d == %d", pA.ID, pB.ID)
	}
	// T_A 只能看到自己
	gotA, _ := meta.FindByFingerprint(ctx, "T_A", "shared_fp")
	gotB, _ := meta.FindByFingerprint(ctx, "T_B", "shared_fp")
	if gotA == nil || gotB == nil {
		t.Fatalf("expected both tenants to have pattern")
	}
	if gotA.ID != pA.ID || gotB.ID != pB.ID {
		t.Errorf("tenant IDs mismatch")
	}
}

func TestMySQLPatternMeta_Save_EmptyFingerprint(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	ctx := context.Background()
	p := makePattern("T1", "") // 空 fingerprint
	if err := meta.Save(ctx, &p); err != nil {
		t.Fatalf("Save with empty fingerprint: %v", err)
	}
	if p.ID == 0 {
		t.Errorf("expected non-zero ID even with empty fingerprint")
	}
	// 空 fingerprint 转 NULL，FindByFingerprint 返回 nil
	got, err := meta.FindByFingerprint(ctx, "T1", "")
	if err != nil {
		t.Fatalf("FindByFingerprint empty: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty fingerprint lookup, got %+v", got)
	}
}

func TestMySQLPatternMeta_FindByIDs_Empty(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	got, err := meta.FindByIDs(context.Background(), "T1", nil)
	if err != nil {
		t.Fatalf("FindByIDs empty: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestMySQLPatternMeta_FindByIDs_TenantIsolation(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	ctx := context.Background()
	pA := makePattern("T_A", "fp_a")
	if err := meta.Save(ctx, &pA); err != nil {
		t.Fatalf("Save T_A: %v", err)
	}
	pB := makePattern("T_B", "fp_b")
	if err := meta.Save(ctx, &pB); err != nil {
		t.Fatalf("Save T_B: %v", err)
	}
	// T_B 查 [pA.ID]：应返回空
	got, err := meta.FindByIDs(ctx, "T_B", []int64{pA.ID})
	if err != nil {
		t.Fatalf("FindByIDs cross-tenant: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected cross-tenant FindByIDs to return empty, got %d", len(got))
	}
}

func TestMySQLPatternMeta_SearchCandidates_TenantIsolationAndLimit(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	ctx := context.Background()
	for _, tenantID := range []string{"T_A", "T_B"} {
		for index := 0; index < 3; index++ {
			pattern := makePattern(tenantID, tenantID+"-fp-"+string(rune('a'+index)))
			if err := meta.Save(ctx, &pattern); err != nil {
				t.Fatalf("Save %s %d: %v", tenantID, index, err)
			}
		}
	}

	got, err := meta.SearchCandidates(ctx, "T_A", []string{"pg"}, 2)
	if err != nil {
		t.Fatalf("SearchCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected limit 2, got %d", len(got))
	}
	for _, pattern := range got {
		if pattern.TenantID != "T_A" {
			t.Fatalf("cross-tenant candidate leaked: %+v", pattern)
		}
	}

	if _, err := meta.SearchCandidates(ctx, "", []string{"pg"}, 2); err == nil {
		t.Fatal("expected empty tenant_id to fail")
	}
}

func TestMySQLPatternMeta_SearchCandidates_FiltersMatchingRowsWithoutEmbedding(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	ctx := context.Background()

	relevant := makePattern("T_A", "fp_relevant_old")
	relevant.ResourceType = "redis"
	relevant.Symptom = "redis_connection_timeout"
	relevant.RootCauseObject = "connection_pool_exhausted"
	relevant.Signature = "Redis connection timeout"
	relevant.Embedding = "[1,2,3]"
	if err := meta.Save(ctx, &relevant); err != nil {
		t.Fatalf("Save relevant: %v", err)
	}
	if err := db.Model(&chatdiagnosemodel.IncidentPattern{}).
		Where("id = ?", relevant.ID).
		Update("updated_at", relevant.UpdatedAt.Add(-24*time.Hour)).Error; err != nil {
		t.Fatalf("age relevant pattern: %v", err)
	}

	newer := makePattern("T_A", "fp_irrelevant_new")
	newer.ResourceType = "pg"
	newer.Symptom = "replica_lag"
	newer.RootCauseObject = "replica_lag"
	newer.Signature = "PostgreSQL replica lag"
	newer.Embedding = "[4,5,6]"
	if err := meta.Save(ctx, &newer); err != nil {
		t.Fatalf("Save newer: %v", err)
	}
	crossTenant := makePattern("T_B", "fp_cross_tenant")
	crossTenant.Signature = "Redis connection timeout"
	if err := meta.Save(ctx, &crossTenant); err != nil {
		t.Fatalf("Save cross-tenant: %v", err)
	}

	got, err := meta.SearchCandidates(ctx, "T_A", []string{"redis", "connection"}, 2)
	if err != nil {
		t.Fatalf("SearchCandidates: %v", err)
	}
	if len(got) != 1 || got[0].ID != relevant.ID {
		t.Fatalf("expected old relevant pattern to be returned, got %+v", got)
	}
	if got[0].Embedding != "" {
		t.Fatalf("expected candidate query not to load embedding, got %q", got[0].Embedding)
	}
}

func TestMySQLPatternMeta_IncHitCount_Atomic(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	ctx := context.Background()
	p := makePattern("T1", "fp_inc")
	if err := meta.Save(ctx, &p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	const goroutines = 100
	errors := make(chan error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			errors <- meta.IncHitCount(ctx, "T1", p.ID)
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("IncHitCount: %v", err)
		}
	}
	got, err := meta.FindByFingerprint(ctx, "T1", "fp_inc")
	if err != nil {
		t.Fatalf("FindByFingerprint: %v", err)
	}
	if got.HitCount != goroutines {
		t.Errorf("expected HitCount=%d, got %d", goroutines, got.HitCount)
	}
	if got.LastHitAt == nil {
		t.Errorf("expected LastHitAt non-nil")
	}
}

func TestMySQLPatternMeta_IncHitCount_TenantIsolation(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	ctx := context.Background()
	pA := makePattern("T_A", "fp_a")
	if err := meta.Save(ctx, &pA); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// T_B 调 IncHitCount(T_A 的 id) → 不应影响
	if err := meta.IncHitCount(ctx, "T_B", pA.ID); err != nil {
		t.Fatalf("IncHitCount cross-tenant: %v", err)
	}
	got, _ := meta.FindByFingerprint(ctx, "T_A", "fp_a")
	if got.HitCount != 0 {
		t.Errorf("expected HitCount=0 (cross-tenant IncHitCount ignored), got %d", got.HitCount)
	}
}

func TestMySQLPatternMeta_IncHitCount_NotFound(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	// 不存在的 id → 静默成功
	if err := meta.IncHitCount(context.Background(), "T1", 999999); err != nil {
		t.Errorf("expected silent success for missing id, got %v", err)
	}
}

func TestMySQLPatternMeta_FindByFingerprint_NotFound(t *testing.T) {
	db := newTestDB(t)
	meta := NewMySQLPatternMeta(db)
	got, err := meta.FindByFingerprint(context.Background(), "T1", "nonexistent")
	if err != nil {
		t.Fatalf("FindByFingerprint missing: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}
