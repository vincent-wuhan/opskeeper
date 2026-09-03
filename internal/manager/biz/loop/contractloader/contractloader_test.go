package contractloader

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	loopstore "github.com/vincent-wuhan/opskeeper/internal/manager/data/loop/store"
	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// newTestDB 建一张含完整 loop schema 的 sqlite 文件库。
// 复用 store.Migrate 确保测试和生产 schema 一致。
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
	if err := loopstore.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// writeRC helper：构造 + 持久化一条 RootCauseJSON contract。
func writeRC(t *testing.T, repo *loopstore.ContractRepoDB, tenant, incident string) {
	t.Helper()
	rc := &loop.RootCauseJSON{
		SchemaVersion: loop.ContractSchemaV1,
		RootCauseObject: &loop.RootCauseObject{
			Kind:    "long_running_tx",
			Summary: "PG session 1234 holding AccessExclusiveLock for 30s",
		},
	}
	payload, err := json.Marshal(rc)
	if err != nil {
		t.Fatalf("marshal rc: %v", err)
	}
	if err := repo.WriteContract(context.Background(), &loopmodel.Contract{
		IncidentID: incident,
		TenantID:   tenant,
		Phase:      string(loop.PhaseInvestigated),
		Type:       "root_cause_json",
		SchemaVer:  loop.ContractSchemaV1,
		Payload:    string(payload),
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write rc: %v", err)
	}
}

// 1. 空 incidentID → (nil, nil)（与 NoopUpstreamContractLoader 一致）
func TestAdapter_LoadPostmortemInputs_EmptyIncident(t *testing.T) {
	a := NewAdapter(&loopstore.ContractRepoDB{}, "T1", nil)
	out, err := a.LoadPostmortemInputs(context.Background(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != nil {
		t.Fatalf("want nil, got %+v", out)
	}
}

// 2. 全空 DB → 所有字段 nil，不报错
func TestAdapter_LoadPostmortemInputs_AllMissing(t *testing.T) {
	db := newTestDB(t)
	repo := loopstore.NewContractRepoDB(db)
	a := NewAdapter(repo, "T1", nil)

	out, err := a.LoadPostmortemInputs(context.Background(), "INC-001")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out == nil {
		t.Fatal("want non-nil PostmortemInputs")
	}
	if out.RootCause != nil || out.Critique != nil || out.Verified != nil {
		t.Fatalf("want all nil, got %+v", out)
	}
}

// 3. 三 contract 齐全 → 三字段都填好
func TestAdapter_LoadPostmortemInputs_AllPresent(t *testing.T) {
	db := newTestDB(t)
	repo := loopstore.NewContractRepoDB(db)
	ctx := context.Background()
	incident := "INC-002"

	writeRC(t, repo, "T1", incident)

	critique := &loop.CritiqueScore{
		SchemaVersion: loop.ContractSchemaV1,
		Verdict:       "pass",
		Score:         0.87,
	}
	cp, _ := json.Marshal(critique)
	if err := repo.WriteContract(ctx, &loopmodel.Contract{
		IncidentID: incident,
		TenantID:   "T1",
		Phase:      string(loop.PhaseCritiqued), Type: "critique_score",
		SchemaVer: loop.ContractSchemaV1, Payload: string(cp),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write critique: %v", err)
	}

	verified := &loop.VerifiedDelta{
		SchemaVersion: loop.ContractSchemaV1,
	}
	vp, _ := json.Marshal(verified)
	if err := repo.WriteContract(ctx, &loopmodel.Contract{
		IncidentID: incident,
		TenantID:   "T1",
		Phase:      string(loop.PhaseRecovered), Type: "verified_delta",
		SchemaVer: loop.ContractSchemaV1, Payload: string(vp),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write verified: %v", err)
	}

	a := NewAdapter(repo, "T1", nil)
	out, err := a.LoadPostmortemInputs(ctx, incident)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.RootCause == nil || out.RootCause.RootCauseObject == nil {
		t.Fatal("RootCause nil")
	}
	if out.RootCause.RootCauseObject.Kind != "long_running_tx" {
		t.Errorf("RootCause.Kind = %q, want long_running_tx", out.RootCause.RootCauseObject.Kind)
	}
	if out.Critique == nil || out.Critique.Score != 0.87 {
		t.Errorf("Critique mismatch: %+v", out.Critique)
	}
	if out.Verified == nil || out.Verified.SchemaVersion != loop.ContractSchemaV1 {
		t.Errorf("Verified mismatch: %+v", out.Verified)
	}
}

// 4. 部分 contract 缺失 → 对应字段 nil，其他正常
func TestAdapter_LoadPostmortemInputs_PartialMissing(t *testing.T) {
	db := newTestDB(t)
	repo := loopstore.NewContractRepoDB(db)
	writeRC(t, repo, "T1", "INC-003")
	// 故意只写 RootCause，不写 Critique / Verified

	a := NewAdapter(repo, "T1", nil)
	out, err := a.LoadPostmortemInputs(context.Background(), "INC-003")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.RootCause == nil {
		t.Fatal("want RootCause populated")
	}
	if out.Critique != nil {
		t.Errorf("Critique should be nil, got %+v", out.Critique)
	}
	if out.Verified != nil {
		t.Errorf("Verified should be nil, got %+v", out.Verified)
	}
}

// 5. Payload 损坏 → 反序列化错误被 warn-log + 返回 nil（不阻塞 PostmortemInputs）
func TestAdapter_LoadPostmortemInputs_CorruptPayload(t *testing.T) {
	db := newTestDB(t)
	repo := loopstore.NewContractRepoDB(db)
	ctx := context.Background()
	if err := repo.WriteContract(ctx, &loopmodel.Contract{
		TenantID: "T1",
		Phase:    string(loop.PhaseInvestigated), Type: "root_cause_json",
		SchemaVer: loop.ContractSchemaV1,
		Payload:   `{ this is not valid JSON `,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	a := NewAdapter(repo, "T1", nil)
	out, err := a.LoadPostmortemInputs(ctx, "INC-004")
	if err != nil {
		t.Fatalf("want non-fatal, got err: %v", err)
	}
	if out == nil {
		t.Fatal("want PostmortemInputs, got nil")
	}
	if out.RootCause != nil {
		t.Errorf("want RootCause nil after corrupt payload, got %+v", out.RootCause)
	}
}

// 6. 空 tenantID 直接报错（接口契约：repo 强制 tenant）
func TestAdapter_LoadPostmortemInputs_EmptyTenantID(t *testing.T) {
	db := newTestDB(t)
	repo := loopstore.NewContractRepoDB(db)
	writeRC(t, repo, "T1", "INC-005")
	a := NewAdapter(repo, "", nil)
	// ReadContract 内部会拒空 tenantID，adapter 把错误 warn + 吞掉
	//（KB 风格），仍返回 PostmortemInputs，但 RootCause 字段 nil。
	out, err := a.LoadPostmortemInputs(context.Background(), "INC-005")
	if err != nil {
		t.Fatalf("want non-fatal, got err: %v", err)
	}
	if out == nil {
		t.Fatal("want PostmortemInputs, got nil")
	}
	if out.RootCause != nil {
		t.Errorf("want RootCause nil on empty tenantID, got %+v", out.RootCause)
	}
}

// 7. compile-time interface satisfaction
func TestAdapter_InterfaceSatisfaction(t *testing.T) {
	var _ loop.UpstreamContractLoader = (*Adapter)(nil)
}
