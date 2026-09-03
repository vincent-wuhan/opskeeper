package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	chatdiagnose "github.com/vincent-wuhan/opskeeper/internal/manager/biz/chatdiagnose"
	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sqlite handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestConversationRepoDB_CreateAndGet(t *testing.T) {
	db := newTestDB(t)
	repo := NewConversationRepoDB(db)
	ctx := context.Background()

	c := &chatdiagnosemodel.Conversation{
		ID:        "C1",
		TenantID:  "T1",
		UserID:    "U1",
		Title:     "test conv",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := repo.CreateConversation(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetConversation(ctx, "C1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.ID != "C1" {
		t.Fatalf("expected C1, got %+v", got)
	}

	// Tenant isolation
	if _, err := repo.GetConversationForTenant(ctx, "C1", "T2"); err == nil {
		t.Fatalf("expected ErrConversationTenantMismatch for cross-tenant access")
	} else if err != chatdiagnose.ErrConversationTenantMismatch {
		t.Fatalf("expected ErrConversationTenantMismatch, got %v", err)
	}

	// Update title
	if err := repo.UpdateConversationTitle(ctx, "C1", "new title", time.Now().UTC()); err != nil {
		t.Fatalf("update title: %v", err)
	}
	got2, _ := repo.GetConversation(ctx, "C1")
	if got2.Title != "new title" {
		t.Fatalf("title not updated: %s", got2.Title)
	}
}

func TestConversationRepoDB_TurnAppendOnly(t *testing.T) {
	db := newTestDB(t)
	repo := NewConversationRepoDB(db)
	ctx := context.Background()

	c := &chatdiagnosemodel.Conversation{
		ID:        "C2",
		TenantID:  "T1",
		UserID:    "U1",
		CreatedAt: time.Now().UTC(),
	}
	if err := repo.CreateConversation(ctx, c); err != nil {
		t.Fatalf("create conv: %v", err)
	}

	t1 := &chatdiagnosemodel.Turn{
		ConversationID:     "C2",
		Role:               "user",
		Content:            "hello",
		LLMContextSnapshot: `{"sha":"abc"}`,
	}
	if err := repo.SaveTurn(ctx, t1); err != nil {
		t.Fatalf("save turn: %v", err)
	}
	if t1.ID == 0 {
		t.Fatalf("expected ID populated")
	}

	t2 := &chatdiagnosemodel.Turn{
		ConversationID:     "C2",
		Role:               "assistant",
		Content:            "world",
		LLMContextSnapshot: `{"sha":"def"}`,
	}
	if err := repo.SaveTurn(ctx, t2); err != nil {
		t.Fatalf("save turn2: %v", err)
	}

	turns, err := repo.GetTurns(ctx, "C2")
	if err != nil {
		t.Fatalf("get turns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(turns))
	}

	// Set linked_loop_event_id (append-only exception)
	if err := repo.SetTurnLinkedLoopEvent(ctx, t1.ID, 999); err != nil {
		t.Fatalf("set linked loop event: %v", err)
	}
	turns2, _ := repo.GetTurns(ctx, "C2")
	if turns2[0].LinkedLoopEventID == nil || *turns2[0].LinkedLoopEventID != 999 {
		t.Fatalf("linked_loop_event_id not set")
	}

	// ID monotonicity
	if turns[0].ID >= turns[1].ID {
		t.Fatalf("expected ID monotonicity, got %d >= %d", turns[0].ID, turns[1].ID)
	}
}
