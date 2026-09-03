package store

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newAuditRepo(t *testing.T) *ProposalAuditRepo {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ProposalAuditEntry{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewProposalAuditRepo(db)
}

func makeAuditEntry(proposalID, action, payload, prevHash string) *ProposalAuditEntry {
	return &ProposalAuditEntry{
		ID:         uuid.NewString(),
		ProposalID: proposalID,
		Action:     action,
		Payload:    payload,
		PrevHash:   prevHash,
		Hash:       ComputeHash(prevHash, proposalID, action, payload),
	}
}

func TestComputeHash_Deterministic(t *testing.T) {
	h1 := ComputeHash("prev", "p1", "decide", `{"x":1}`)
	h2 := ComputeHash("prev", "p1", "decide", `{"x":1}`)
	if h1 != h2 {
		t.Errorf("same inputs should yield same hash")
	}
	if len(h1) != 64 {
		t.Errorf("hash should be 64 hex chars, got %d", len(h1))
	}
}

func TestComputeHash_InputsChangeOutput(t *testing.T) {
	base := ComputeHash("", "p1", "decide", "{}")
	if ComputeHash("x", "p1", "decide", "{}") == base {
		t.Error("prevHash should affect result")
	}
	if ComputeHash("", "p2", "decide", "{}") == base {
		t.Error("proposalID should affect result")
	}
	if ComputeHash("", "p1", "approve", "{}") == base {
		t.Error("action should affect result")
	}
}

func TestCanonicalizeJSON_SortedKeys(t *testing.T) {
	c1 := canonicalizeJSON(`{"b":2,"a":1}`)
	c2 := canonicalizeJSON(`{"a":1,"b":2}`)
	if c1 != c2 {
		t.Errorf("expected sorted canonical form, got %s vs %s", c1, c2)
	}
}

func TestAudit_AppendAndTail(t *testing.T) {
	repo := newAuditRepo(t)
	ctx := context.Background()

	tail, err := repo.GetChainTail(ctx)
	if err != nil || tail != "" {
		t.Errorf("empty chain: tail=%q err=%v", tail, err)
	}

	e1 := makeAuditEntry("p1", "decide", `{"x":1}`, "")
	if err := repo.AppendEntry(ctx, e1); err != nil {
		t.Fatalf("append e1: %v", err)
	}
	tail, _ = repo.GetChainTail(ctx)
	if tail != e1.Hash {
		t.Errorf("tail after e1 = %q, want %q", tail, e1.Hash)
	}

	e2 := makeAuditEntry("p2", "expire", `{"y":2}`, e1.Hash)
	if err := repo.AppendEntry(ctx, e2); err != nil {
		t.Fatalf("append e2: %v", err)
	}
	tail, _ = repo.GetChainTail(ctx)
	if tail != e2.Hash {
		t.Errorf("tail after e2 = %q, want %q", tail, e2.Hash)
	}
}

func TestAudit_AppendRejectsBadPrevHash(t *testing.T) {
	repo := newAuditRepo(t)
	ctx := context.Background()

	e1 := makeAuditEntry("p1", "decide", `{}`, "")
	if err := repo.AppendEntry(ctx, e1); err != nil {
		t.Fatalf("append e1: %v", err)
	}

	e2 := makeAuditEntry("p2", "decide", `{}`, "deadbeef")
	err := repo.AppendEntry(ctx, e2)
	if err == nil {
		t.Fatal("expected error for bad prev_hash")
	}
	if !strings.Contains(err.Error(), "chain mismatch") {
		t.Errorf("error should mention chain mismatch, got: %v", err)
	}
}

func TestAudit_VerifyIntactChain(t *testing.T) {
	repo := newAuditRepo(t)
	ctx := context.Background()

	var prev string
	for i := 0; i < 5; i++ {
		pid := "p" + string(rune('1'+i))
		payload := `{"n":` + string(rune('0'+i)) + `}`
		e := makeAuditEntry(pid, "decide", payload, prev)
		if err := repo.AppendEntry(ctx, e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		prev = e.Hash
	}

	idx, err := repo.VerifyChain(ctx)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if idx != -1 {
		t.Errorf("expected intact chain (-1), got broken at %d", idx)
	}
}

func TestAudit_VerifyDetectsTampering(t *testing.T) {
	repo := newAuditRepo(t)
	ctx := context.Background()

	var prev string
	for i := 0; i < 3; i++ {
		pid := "p" + string(rune('1'+i))
		payload := `{"n":` + string(rune('0'+i)) + `}`
		e := makeAuditEntry(pid, "decide", payload, prev)
		if err := repo.AppendEntry(ctx, e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		prev = e.Hash
	}

	// Tamper with the middle entry's payload.
	if err := repo.db.WithContext(ctx).Model(&ProposalAuditEntry{}).
		Where("proposal_id = ?", "p2").
		Update("payload", `{"n":999}`).Error; err != nil {
		t.Fatalf("tamper: %v", err)
	}

	idx, err := repo.VerifyChain(ctx)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if idx != 1 {
		t.Errorf("expected tampering detected at index 1, got %d", idx)
	}
}

func TestAudit_ListByProposal(t *testing.T) {
	repo := newAuditRepo(t)
	ctx := context.Background()

	var prev string
	for i := 0; i < 4; i++ {
		e := makeAuditEntry("p1", "decide", `{"n":1}`, prev)
		if err := repo.AppendEntry(ctx, e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		prev = e.Hash
	}
	other := makeAuditEntry("p2", "decide", `{}`, prev)
	if err := repo.AppendEntry(ctx, other); err != nil {
		t.Fatalf("append other: %v", err)
	}

	entries, err := repo.ListByProposal(ctx, "p1")
	if err != nil {
		t.Fatalf("ListByProposal: %v", err)
	}
	if len(entries) != 4 {
		t.Errorf("expected 4 entries for p1, got %d", len(entries))
	}
}
