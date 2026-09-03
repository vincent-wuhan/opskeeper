package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// newProposalRepo opens an in-memory SQLite DB and applies this
// package's Migrate so chat_mutating_proposals exists.
func newProposalRepo(t *testing.T) *MutatingProposalRepo {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open sqlite :memory:: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return NewMutatingProposalRepo(db)
}

func TestMutatingProposalRepo_InsertDefaults(t *testing.T) {
	repo := newProposalRepo(t)
	ctx := context.Background()

	p := &model.MutatingProposal{
		SessionID:      "sess-1",
		ToolName:       "host_restart_service",
		ArgsJSON:       `{"device_id":1,"service":"nginx"}`,
		ToolClass:      "write",
		ReviewerAgent:  "reviewer",
		ReviewerTaskID: "agent-deadbeef",
		OperatorUserID: 42,
	}
	if err := repo.Insert(ctx, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if p.ID == "" {
		t.Fatalf("expected auto-generated ID")
	}
	if p.Decision != model.DecisionPending {
		t.Errorf("Decision default = %q, want %q", p.Decision, model.DecisionPending)
	}
	if p.CreatedAt.IsZero() {
		t.Errorf("CreatedAt should be auto-stamped")
	}
}

func TestMutatingProposalRepo_DecisionUpdate(t *testing.T) {
	repo := newProposalRepo(t)
	ctx := context.Background()

	p := &model.MutatingProposal{
		SessionID:      "sess-1",
		ToolName:       "host_restart_service",
		ArgsJSON:       `{}`,
		ToolClass:      "write",
		ReviewerAgent:  "reviewer",
		ReviewerTaskID: "agent-1",
	}
	if err := repo.Insert(ctx, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	reason := "no SOP found"
	if err := repo.UpdateDecision(ctx, p.ID, model.DecisionReject, &reason); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}

	got, err := repo.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Decision != model.DecisionReject {
		t.Errorf("Decision = %q, want reject", got.Decision)
	}
	if got.DecisionReason == nil || *got.DecisionReason != reason {
		t.Errorf("DecisionReason = %v, want %q", got.DecisionReason, reason)
	}
	if got.DecidedAt == nil {
		t.Errorf("DecidedAt should be stamped after update")
	}
}

func TestMutatingProposalRepo_DecisionRejectsInvalidValue(t *testing.T) {
	repo := newProposalRepo(t)
	ctx := context.Background()
	if err := repo.UpdateDecision(ctx, "x", "maybe", nil); !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("invalid decision should return ErrInvalid, got %v", err)
	}
}

func TestMutatingProposalRepo_MarkExecuted(t *testing.T) {
	repo := newProposalRepo(t)
	ctx := context.Background()

	p := &model.MutatingProposal{
		SessionID:      "sess-1",
		ToolName:       "host_restart_service",
		ArgsJSON:       `{}`,
		ToolClass:      "write",
		ReviewerAgent:  "reviewer",
		ReviewerTaskID: "agent-1",
	}
	if err := repo.Insert(ctx, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	when := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	if err := repo.MarkExecuted(ctx, p.ID, when); err != nil {
		t.Fatalf("MarkExecuted: %v", err)
	}
	got, err := repo.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExecutedAt == nil || !got.ExecutedAt.Equal(when) {
		t.Errorf("ExecutedAt = %v, want %v", got.ExecutedAt, when)
	}
}

func TestMutatingProposalRepo_GetMissing(t *testing.T) {
	repo := newProposalRepo(t)
	if _, err := repo.Get(context.Background(), "nonexistent"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing proposal should return ErrNotFound, got %v", err)
	}
}

func TestMutatingProposalRepo_UpdateDecisionToExpired(t *testing.T) {
	repo := newProposalRepo(t)
	ctx := context.Background()

	p := &model.MutatingProposal{
		SessionID: "sess-1", ToolName: "host_restart_service",
		ArgsJSON: "{}", ToolClass: "write",
		ReviewerAgent: "reviewer", ReviewerTaskID: "t1",
		TTLSeconds: 60,
	}
	if err := repo.Insert(ctx, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := repo.UpdateDecisionToExpired(ctx, p.ID, "auto-decline: exceeded TTL"); err != nil {
		t.Fatalf("UpdateDecisionToExpired: %v", err)
	}

	got, err := repo.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Decision != model.DecisionExpired {
		t.Errorf("Decision = %q, want %q", got.Decision, model.DecisionExpired)
	}
	if got.ExpiredAt == nil {
		t.Errorf("ExpiredAt should be stamped")
	}
}

func TestMutatingProposalRepo_UpdateDecisionToExpired_RejectsAlreadyDecided(t *testing.T) {
	repo := newProposalRepo(t)
	ctx := context.Background()

	p := &model.MutatingProposal{
		SessionID: "sess-1", ToolName: "t", ArgsJSON: "{}", ToolClass: "write",
		ReviewerAgent: "reviewer", ReviewerTaskID: "t1",
	}
	if err := repo.Insert(ctx, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	reason := "no SOP"
	if err := repo.UpdateDecision(ctx, p.ID, model.DecisionReject, &reason); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}

	err := repo.UpdateDecisionToExpired(ctx, p.ID, "should fail")
	if err == nil {
		t.Fatal("expected error for illegal transition reject→expired")
	}
}

func TestMutatingProposalRepo_MarkRolledBack(t *testing.T) {
	repo := newProposalRepo(t)
	ctx := context.Background()

	p := &model.MutatingProposal{
		SessionID: "sess-1", ToolName: "host_restart_service",
		ArgsJSON: "{}", ToolClass: "write",
		ReviewerAgent: "reviewer", ReviewerTaskID: "t1",
	}
	if err := repo.Insert(ctx, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := repo.UpdateDecision(ctx, p.ID, model.DecisionApprove, nil); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}

	if err := repo.MarkRolledBack(ctx, p.ID, "rollback-id-1"); err != nil {
		t.Fatalf("MarkRolledBack: %v", err)
	}

	got, _ := repo.Get(ctx, p.ID)
	if got.Decision != model.DecisionRolledBack {
		t.Errorf("Decision = %q, want %q", got.Decision, model.DecisionRolledBack)
	}
	if got.RolledBackAt == nil {
		t.Errorf("RolledBackAt should be stamped")
	}
	if got.RollbackOf == nil || *got.RollbackOf != "rollback-id-1" {
		t.Errorf("RollbackOf = %v, want rollback-id-1", got.RollbackOf)
	}
}

func TestMutatingProposalRepo_ListPendingBefore(t *testing.T) {
	repo := newProposalRepo(t)
	ctx := context.Background()

	// Insert one old pending proposal.
	old := &model.MutatingProposal{
		SessionID: "sess-1", ToolName: "t1", ArgsJSON: "{}", ToolClass: "write",
		ReviewerAgent: "reviewer", ReviewerTaskID: "t1",
		TTLSeconds: 60,
	}
	if err := repo.Insert(ctx, old); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Backdate CreatedAt so the effective deadline is in the past.
	if err := repo.db.WithContext(ctx).Model(&model.MutatingProposal{}).
		Where("id = ?", old.ID).Update("created_at", time.Now().Add(-2*time.Hour)).Error; err != nil {
		t.Fatalf("backdate: %v", err)
	}

	out, err := repo.ListPendingBefore(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("ListPendingBefore: %v", err)
	}
	if len(out) != 1 || out[0].ID != old.ID {
		t.Errorf("expected 1 old pending, got %d", len(out))
	}
}
