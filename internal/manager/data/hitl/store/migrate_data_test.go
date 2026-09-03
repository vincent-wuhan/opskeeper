package store

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	aiopsmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	approvalmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/approval"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

// fullTestDB 建一张含 approvals + chat_mutating_proposals + proposal 全套表的测试 DB。
//
// 用于验证 MigrateLegacy 把旧表行搬运到新 proposal 表 + 行数 / 完整性。
func fullTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// 三张表的 AutoMigrate（顺序无所谓，AutoMigrate 内部会排序）。
	if err := db.AutoMigrate(
		&approvalmodel.Approval{},
		&aiopsmodel.MutatingProposal{},
		&model.Proposal{},
		&model.ProposalState{},
	); err != nil {
		t.Fatalf("auto-migrate: %v", err)
	}
	return db
}

func TestMigrateLegacy_ApprovalRowCount(t *testing.T) {
	db := fullTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	rows := []*approvalmodel.Approval{
		{ID: "appr-1", Kind: "shell_command", Title: "rm", Summary: "rm -rf", PayloadJSON: `{"cmd":"rm"}`, Source: "agent", Status: approvalmodel.StatusPending, ProposedBy: 1, CreatedAt: now},
		{ID: "appr-2", Kind: "restart_service", Title: "restart", Summary: "nginx", PayloadJSON: `{"svc":"nginx"}`, Status: approvalmodel.StatusApproved, ProposedBy: 2, CreatedAt: now},
	}
	for _, a := range rows {
		if err := db.Create(a).Error; err != nil {
			t.Fatalf("seed approval: %v", err)
		}
	}

	repo := NewRepo(db)
	res, err := repo.MigrateLegacy(ctx)
	if err != nil {
		t.Fatalf("MigrateLegacy err: %v", err)
	}
	if res.ApprovalMigrated != 2 {
		t.Errorf("ApprovalMigrated = %d, want 2", res.ApprovalMigrated)
	}
	if res.MutatingMigrated != 0 {
		t.Errorf("MutatingMigrated = %d, want 0", res.MutatingMigrated)
	}

	// 校验行数
	mig, err := repo.CountByLegacyKind(ctx, "approval_legacy")
	if err != nil || mig != 2 {
		t.Errorf("CountByLegacyKind(approval_legacy) = %d, want 2", mig)
	}

	// 校验 1 行抽样
	loaded, err := repo.Get(ctx, "legacy-approval-appr-1")
	if err != nil {
		t.Fatalf("Get migrated row: %v", err)
	}
	if loaded.Kind != "shell_command" {
		t.Errorf("kind = %s", loaded.Kind)
	}
	if loaded.State != model.StatePending {
		t.Errorf("state = %s, want pending", loaded.State)
	}
	if loaded.LegacyKind != "approval_legacy" {
		t.Errorf("legacy_kind = %s", loaded.LegacyKind)
	}
	if loaded.DualWriteAt == nil {
		t.Error("dual_write_at should be stamped on migrated rows")
	}
}

func TestMigrateLegacy_MutatingRowCount(t *testing.T) {
	db := fullTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	rows := []*aiopsmodel.MutatingProposal{
		{ID: "mut-1", SessionID: "s1", ToolName: "host_write", ArgsJSON: `{"path":"/tmp/x"}`, ToolClass: "write", ReviewerAgent: "reviewer", ReviewerTaskID: "task-1", Decision: aiopsmodel.DecisionApprove, OperatorUserID: 5, CreatedAt: now},
		{ID: "mut-2", SessionID: "s2", ToolName: "drop_table", ArgsJSON: `{"tbl":"orders"}`, ToolClass: "destructive", ReviewerAgent: "reviewer", ReviewerTaskID: "task-2", Decision: aiopsmodel.DecisionReject, OperatorUserID: 6, CreatedAt: now, DecidedAt: &now},
		{ID: "mut-3", SessionID: "s3", ToolName: "host_read", ArgsJSON: `{"path":"/tmp/y"}`, ToolClass: "read", ReviewerAgent: "reviewer", ReviewerTaskID: "task-3", Decision: aiopsmodel.DecisionPending, OperatorUserID: 7, CreatedAt: now},
	}
	for _, p := range rows {
		if err := db.Create(p).Error; err != nil {
			t.Fatalf("seed mutating: %v", err)
		}
	}

	repo := NewRepo(db)
	res, err := repo.MigrateLegacy(ctx)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.MutatingMigrated != 3 {
		t.Errorf("MutatingMigrated = %d, want 3", res.MutatingMigrated)
	}

	// severity 升级正确：destructive → dangerous；write → mutating；read → safe
	dangerous, _ := repo.CountByState(ctx, model.StateRejected) // mut-2 状态 rejected
	if dangerous != 1 {
		t.Errorf("rejected count = %d, want 1", dangerous)
	}
	d1, _ := repo.Get(ctx, "legacy-mutating-mut-1")
	if d1.Severity != model.SeverityMutating {
		t.Errorf("mut-1 severity = %s, want mutating", d1.Severity)
	}
	d2, _ := repo.Get(ctx, "legacy-mutating-mut-2")
	if d2.Severity != model.SeverityDangerous {
		t.Errorf("mut-2 severity = %s, want dangerous (destructive)", d2.Severity)
	}
	d3, _ := repo.Get(ctx, "legacy-mutating-mut-3")
	if d3.Severity != model.SeveritySafe {
		t.Errorf("mut-3 severity = %s, want safe (read)", d3.Severity)
	}
}

func TestMigrateLegacy_Idempotent(t *testing.T) {
	db := fullTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_ = db.Create(&approvalmodel.Approval{ID: "appr-x", Kind: "k", PayloadJSON: "{}", Status: approvalmodel.StatusPending, CreatedAt: now}).Error

	repo := NewRepo(db)
	r1, err := repo.MigrateLegacy(ctx)
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if r1.ApprovalMigrated != 1 {
		t.Errorf("first ApprovalMigrated = %d, want 1", r1.ApprovalMigrated)
	}
	r2, err := repo.MigrateLegacy(ctx)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if r2.ApprovalMigrated != 0 {
		t.Errorf("second ApprovalMigrated = %d, want 0", r2.ApprovalMigrated)
	}
	if r2.ApprovalSkipped != 1 {
		t.Errorf("second ApprovalSkipped = %d, want 1", r2.ApprovalSkipped)
	}
}

func TestMigrateLegacy_ApprovalRowIntegrity(t *testing.T) {
	db := fullTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	by := uint64(42)
	decidedAt := now.Add(time.Minute)
	_ = db.Create(&approvalmodel.Approval{
		ID: "appr-i", Kind: "shell_command", Title: "t", Summary: "s", PayloadJSON: `{"x":1}`,
		Source: "flow", SessionID: "sid", Status: approvalmodel.StatusRejected,
		ProposedBy: 7, ApprovedBy: &by, Reason: strPtr("dangerous"),
		CreatedAt: now, DecidedAt: &decidedAt,
	}).Error

	repo := NewRepo(db)
	if _, err := repo.MigrateLegacy(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, "legacy-approval-appr-i")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "flow" {
		t.Errorf("source = %s, want flow", got.Source)
	}
	if got.State != model.StateRejected {
		t.Errorf("state = %s, want rejected", got.State)
	}
	if got.Reason == nil || *got.Reason != "dangerous" {
		t.Errorf("reason = %v, want 'dangerous'", got.Reason)
	}
	if got.ApprovedBy == nil || *got.ApprovedBy != 42 {
		t.Errorf("approved_by = %v, want 42", got.ApprovedBy)
	}
}
