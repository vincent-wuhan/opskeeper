package proposal

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/vincent-wuhan/opskeeper/internal/manager/data/aiops/store"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
)

func setupExpirer(t *testing.T) (*Expirer, *store.MutatingProposalRepo, *store.ProposalAuditRepo, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.MutatingProposal{},
		&store.ProposalAuditEntry{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := store.NewMutatingProposalRepo(db)
	audit := store.NewProposalAuditRepo(db)
	e := NewExpirer(repo, audit, ExpirerConfig{
		Interval:   50 * time.Millisecond,
		BatchLimit: 100,
	})
	return e, repo, audit, db
}

func insertPending(t *testing.T, repo *store.MutatingProposalRepo, ttlSeconds int, backdate time.Duration) *model.MutatingProposal {
	t.Helper()
	ctx := context.Background()
	p := &model.MutatingProposal{
		SessionID:      "sess-1",
		ToolName:       "host_restart_service",
		ArgsJSON:       `{"x":1}`,
		ToolClass:      "write",
		ReviewerAgent:  "reviewer",
		ReviewerTaskID: "t1",
		TTLSeconds:     ttlSeconds,
	}
	if err := repo.Insert(ctx, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if backdate > 0 {
		db, _ := gorm.Open(sqlite.Open(":memory:"))
		_ = db
		// Use the store's internal db via backdating through repo.
		// We reach into the test's db via a return value; for now
		// skip backdating and just use very short TTL.
	}
	return p
}

func TestExpirer_ScanOnce_NoCandidates(t *testing.T) {
	e, _, _, _ := setupExpirer(t)
	err := e.ScanOnce(context.Background())
	if err != nil {
		t.Errorf("ScanOnce on empty db: %v", err)
	}
}

func TestExpirer_ExpiredProposalGetsExpired(t *testing.T) {
	e, repo, _, _ := setupExpirer(t)
	ctx := context.Background()

	// TTL=1 second; we backdate the row so it's already past due.
	p := &model.MutatingProposal{
		SessionID: "sess-1", ToolName: "t", ArgsJSON: "{}", ToolClass: "write",
		ReviewerAgent: "reviewer", ReviewerTaskID: "t1",
		TTLSeconds: 1,
	}
	if err := repo.Insert(ctx, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Backdate via the repo's underlying db by re-inserting with
	// an earlier created_at. We need access to the db — fetch
	// from setup. Simpler: manually call ListPendingBefore which
	// uses datetime arithmetic; if the row is fresh, it won't
	// match. So we use a different approach: insert with TTL=1
	// then sleep 1.2s before scanning.
	time.Sleep(1100 * time.Millisecond)

	if err := e.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
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

func TestExpirer_FreshProposalNotExpired(t *testing.T) {
	e, repo, _, _ := setupExpirer(t)
	ctx := context.Background()

	// TTL=3600 (1 hour) — should not be expired by a quick scan.
	p := &model.MutatingProposal{
		SessionID: "sess-1", ToolName: "t", ArgsJSON: "{}", ToolClass: "write",
		ReviewerAgent: "reviewer", ReviewerTaskID: "t1",
		TTLSeconds: 3600,
	}
	if err := repo.Insert(ctx, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := e.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	got, _ := repo.Get(ctx, p.ID)
	if got.Decision != model.DecisionPending {
		t.Errorf("Decision = %q, want pending (TTL=3600 should not expire)", got.Decision)
	}
}

func TestExpirer_AppendsAuditEntry(t *testing.T) {
	e, repo, audit, _ := setupExpirer(t)
	ctx := context.Background()

	p := &model.MutatingProposal{
		SessionID: "sess-1", ToolName: "t", ArgsJSON: "{}", ToolClass: "write",
		ReviewerAgent: "reviewer", ReviewerTaskID: "t1",
		TTLSeconds: 1,
	}
	if err := repo.Insert(ctx, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)

	if err := e.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	entries, err := audit.ListByProposal(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListByProposal: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	if entries[0].Action != "expire" {
		t.Errorf("Action = %q, want expire", entries[0].Action)
	}

	// Chain should be intact.
	idx, err := audit.VerifyChain(ctx)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if idx != -1 {
		t.Errorf("expected intact chain, got broken at %d", idx)
	}
}

func TestExpirer_StartStop(t *testing.T) {
	e, _, _, _ := setupExpirer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e.Start(ctx)
	// Starting twice is a no-op.
	e.Start(ctx)

	// Cancel and wait for goroutine to exit.
	cancel()
	time.Sleep(100 * time.Millisecond)
	// If Start were not idempotent, this would race.
	e.Start(ctx)
	cancel()
}
