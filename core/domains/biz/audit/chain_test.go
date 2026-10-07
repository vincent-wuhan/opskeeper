package audit

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	store "github.com/vincent-wuhan/opskeeper/core/domains/data/audit/store"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
)

const testKey = "audit-hmac-test-key-0123456789"

func newChainDB(t *testing.T) *gorm.DB {
	t.Helper()
	// A file-backed SQLite rather than :memory: — the concurrency test
	// opens a transaction on a pooled connection, and an in-memory
	// database is per-connection, so a second connection would see an
	// empty database and the test would pass for the wrong reason.
	path := filepath.Join(t.TempDir(), "audit.db")
	// busy_timeout mirrors what dbx.Open sets for the sqlite dialect.
	// Without it a concurrent write transaction fails instantly with
	// SQLITE_BUSY instead of waiting for the writer in front of it, and
	// the test would be measuring SQLite's locking policy rather than
	// the chain's compare-and-swap.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newChainedUC(t *testing.T, key string) (*Usecase, *gorm.DB) {
	t.Helper()
	db := newChainDB(t)
	cs := store.NewChainStore(db)
	return New(store.New(db), nil, WithChain(key, cs)), db
}

func emit(t *testing.T, uc *Usecase, action string) {
	t.Helper()
	uc.Emit(context.Background(), Event{
		UserEmail:    "op@example.com",
		Role:         "admin",
		IP:           "10.0.0.7",
		Action:       action,
		ResourceType: "device",
		ResourceID:   "dev-" + action,
		ResourceName: "web-01",
		Status:       "success",
		RequestID:    "req-" + action,
	})
}

// emitAt appends a row with a caller-chosen OccurredAt.
//
// It exists because OccurredAt is inside the digest: a fixture that
// backdates a row with a direct UPDATE has changed a hashed field, and
// the chain is right to report that as tampering. Clock skew has to be
// built into the row at write time to produce a legitimate row with an
// unusual timestamp.
func emitAt(t *testing.T, uc *Usecase, at time.Time, action string) {
	t.Helper()
	row := &model.Log{
		OccurredAt: at, UserEmail: "op@example.com", Role: "admin", IP: "10.0.0.7",
		Action: action, ResourceType: "device", ResourceID: "dev-" + action,
		ResourceName: "web-01", Status: "success", RequestID: "req-" + action,
	}
	stamper := uc.chain
	if err := uc.chainStore.AppendChained(context.Background(), row, func(head store.Head) (store.Seal, error) {
		return stamper.sealRow(row, head)
	}); err != nil {
		t.Fatalf("append at %s: %v", at, err)
	}
}

func chainedRows(t *testing.T, db *gorm.DB) []model.Log {
	t.Helper()
	var rows []model.Log
	if err := db.Order("seq ASC").Where("seq > 0").Find(&rows).Error; err != nil {
		t.Fatalf("read rows: %v", err)
	}
	return rows
}

// TestChain_AppendedRowsVerify is the floor: rows written through the
// normal path must verify, or the feature is pure cost.
func TestChain_AppendedRowsVerify(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		emit(t, uc, fmt.Sprintf("device_update_%d", i))
	}
	if err := uc.VerifyChain(ctx); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	rows := chainedRows(t, db)
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	if rows[0].PrevHash != GenesisHash {
		t.Errorf("first row PrevHash = %q, want genesis %q", rows[0].PrevHash, GenesisHash)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Seq != uint64(i+1) {
			t.Errorf("rows[%d].Seq = %d, want %d", i, rows[i].Seq, i+1)
		}
		if rows[i].PrevHash != rows[i-1].Hash {
			t.Errorf("rows[%d].PrevHash does not match rows[%d].Hash", i, i-1)
		}
	}
}

// TestChain_DetectsEditedRow is the property the whole feature exists
// for. A row whose payload was altered in the database must not verify,
// even though every link still points at its neighbour.
func TestChain_DetectsEditedRow(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		emit(t, uc, fmt.Sprintf("device_update_%d", i))
	}
	// The quietest possible tamper: change a field nobody recomputes.
	if err := db.Model(&model.Log{}).Where("seq = ?", 3).
		Update("resource_name", "web-01-pwned").Error; err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := uc.VerifyChain(ctx)
	var broken *ErrChainBroken
	if !errors.As(err, &broken) {
		t.Fatalf("VerifyChain after edit = %v, want *ErrChainBroken", err)
	}
	if broken.Seq != 3 {
		t.Errorf("broken at seq %d, want 3", broken.Seq)
	}
}

// TestChain_DetectsDeletedRow covers the deletion half. Removing a row
// leaves both neighbours internally consistent, so only the link check
// can catch it.
func TestChain_DetectsDeletedRow(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		emit(t, uc, fmt.Sprintf("device_update_%d", i))
	}
	if err := db.Where("seq = ?", 2).Delete(&model.Log{}).Error; err != nil {
		t.Fatalf("delete: %v", err)
	}
	err := uc.VerifyChain(ctx)
	var broken *ErrChainBroken
	if !errors.As(err, &broken) {
		t.Fatalf("VerifyChain after delete = %v, want *ErrChainBroken", err)
	}
	// The row after the hole is where the link fails, which is the
	// correct place to point an operator: the hole is at seq 2, and
	// seq 3 is the first row that cannot be believed.
	if broken.Seq != 3 {
		t.Errorf("broken at seq %d, want 3", broken.Seq)
	}
}

// TestChain_DetectsAppendedRowWithoutKey models an attacker with write
// access to the table but not to the key: they can add a row that looks
// like a legitimate entry, and it must not verify.
func TestChain_DetectsAppendedRowWithoutKey(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	ctx := context.Background()
	emit(t, uc, "device_update")
	rows := chainedRows(t, db)
	last := rows[len(rows)-1]
	forged := model.Log{
		OccurredAt: time.Now().UTC(), UserEmail: "root@example.com", Role: "admin",
		IP: "10.0.0.9", Action: "device_delete", ResourceType: "device",
		ResourceID: "dev-1", ResourceName: "web-01", Status: "success",
		RequestID: "req-forged", Seq: last.Seq + 1, PrevHash: last.Hash,
		Hash: strings.Repeat("0", 64),
	}
	if err := db.Create(&forged).Error; err != nil {
		t.Fatalf("insert forged row: %v", err)
	}
	var broken *ErrChainBroken
	if !errors.As(uc.VerifyChain(ctx), &broken) {
		t.Fatal("forged row verified; the chain accepted a row it could not have produced")
	}
}

// TestChain_DisabledWithoutKey pins the honest-failure contract: rows
// are still recorded, and verification says "no chain" rather than
// "chain intact".
func TestChain_DisabledWithoutKey(t *testing.T) {
	uc, db := newChainedUC(t, "")
	ctx := context.Background()
	emit(t, uc, "device_update")

	if err := uc.VerifyChain(ctx); !errors.Is(err, ErrChainDisabled) {
		t.Fatalf("VerifyChain = %v, want ErrChainDisabled", err)
	}
	state, err := uc.ChainState(ctx)
	if err != nil {
		t.Fatalf("ChainState: %v", err)
	}
	if state.Enabled {
		t.Error("ChainState.Enabled = true with no key configured")
	}
	// The row must still exist: losing audit rows because nobody set a
	// key would be a worse outcome than unchained rows.
	if n := len(chainedRows(t, db)); n != 0 {
		t.Errorf("unchained rows got a seq: %d", n)
	}
	var total int64
	if err := db.Model(&model.Log{}).Count(&total).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Errorf("audit rows = %d, want 1 — an unkeyed deployment must still record", total)
	}
}

// TestChain_ConcurrentAppendsAllVerify exercises the compare-and-swap
// head. Under contention the losers must re-read and retry rather than
// stamp two rows onto the same predecessor, which would fork the chain.
func TestChain_ConcurrentAppendsAllVerify(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	ctx := context.Background()

	const writers, each = 8, 10
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				uc.Emit(ctx, Event{
					UserEmail: "op@example.com", Role: "admin", IP: "10.0.0.7",
					Action: "device_update", ResourceType: "device",
					ResourceID: fmt.Sprintf("dev-%d-%d", w, i), Status: "success",
					RequestID: fmt.Sprintf("req-%d-%d", w, i),
				})
			}
		}(w)
	}
	wg.Wait()

	if err := uc.VerifyChain(ctx); err != nil {
		t.Fatalf("VerifyChain after concurrent appends: %v", err)
	}
	rows := chainedRows(t, db)
	if len(rows) != writers*each {
		t.Fatalf("rows = %d, want %d", len(rows), writers*each)
	}
	for i, row := range rows {
		if row.Seq != uint64(i+1) {
			t.Fatalf("rows[%d].Seq = %d, want %d — the chain has a gap or a repeat", i, row.Seq, i+1)
		}
	}
}

// TestChain_RetentionTruncatesPrefixOnly is the retention correctness
// test, and it is the one that justifies the prefix rule.
//
// The fixture has expired rows in two different positions: a run at the
// front (seq 1-3) and a single ancient row wedged behind live ones
// (seq 6). An age-based delete — the obvious implementation, and the one
// the unchained path still uses — removes seq 6 on its own and leaves a
// hole, after which verification reports a break that no operator can
// distinguish from real tampering. A clock step is enough to produce
// this shape in production, so the fixture builds it deliberately.
//
// The correct behaviour is to cut only the front run and keep seq 6,
// even though seq 6 is the oldest row in the table.
func TestChain_RetentionTruncatesPrefixOnly(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	ctx := context.Background()
	cs := store.NewChainStore(db)
	now := time.Now().UTC()

	emitAt(t, uc, now.Add(-10*24*time.Hour), "device_update_1")
	emitAt(t, uc, now.Add(-9*24*time.Hour), "device_update_2")
	emitAt(t, uc, now.Add(-365*24*time.Hour), "device_update_3")
	emitAt(t, uc, now.Add(-24*time.Hour), "device_update_4")
	emitAt(t, uc, now.Add(-time.Hour), "device_update_5")
	emitAt(t, uc, now.Add(-800*24*time.Hour), "device_update_6")

	cut, anchor, err := cs.TruncateExpiredPrefix(ctx, now.Add(-2*24*time.Hour))
	if err != nil {
		t.Fatalf("TruncateExpiredPrefix: %v", err)
	}
	if cut != 3 {
		t.Fatalf("removed %d rows, want 3 (the expired prefix only)", cut)
	}
	if anchor != 4 {
		t.Errorf("new anchor = %d, want 4", anchor)
	}
	survivors := chainedRows(t, db)
	if len(survivors) != 3 {
		t.Fatalf("surviving rows = %d, want 3", len(survivors))
	}
	if survivors[2].Seq != 6 {
		t.Errorf("seq 6 (expired but behind live rows) was removed; the prefix rule is not holding")
	}
	if err := uc.VerifyChain(ctx); err != nil {
		t.Fatalf("VerifyChain after prefix truncation: %v", err)
	}
}

// TestChain_AgeBasedDeleteWouldHoleTheChain is the control for the test
// above: it produces exactly the shape a naive age-based retention
// creates, and asserts that verification does catch it. Without it a
// reader could believe the prefix rule is cosmetic.
func TestChain_AgeBasedDeleteWouldHoleTheChain(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	ctx := context.Background()
	now := time.Now().UTC()
	emitAt(t, uc, now, "device_update_1")
	emitAt(t, uc, now.Add(-365*24*time.Hour), "device_update_2")
	emitAt(t, uc, now, "device_update_3")

	// Exactly what the unchained retention path does: delete by age.
	res := db.Where("occurred_at < ?", now.Add(-48*time.Hour)).Delete(&model.Log{})
	if res.Error != nil {
		t.Fatalf("age delete: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("age delete removed %d rows, want 1", res.RowsAffected)
	}
	if err := uc.VerifyChain(ctx); err == nil {
		t.Fatal("an age-based delete left the chain verifying; the prefix rule is not protecting anything")
	}
}

// TestChain_DifferentKeyFailsVerification documents what a key rotation
// does. Rotating the key invalidates every digest written under the old
// one, so rotation is a retention-sized event: the old window has to be
// dropped or verified against the retired key.
func TestChain_DifferentKeyFailsVerification(t *testing.T) {
	uc, _ := newChainedUC(t, testKey)
	ctx := context.Background()
	emit(t, uc, "device_update")

	rotated := New(uc.repo, nil, WithChain("a-completely-different-key", uc.chainStore))
	var broken *ErrChainBroken
	if !errors.As(rotated.VerifyChain(ctx), &broken) {
		t.Fatal("verification passed under a different key; rotation would silently bless old rows")
	}
}

// TestChain_EmptyLedgerVerifies guards the trivial case: a fresh install
// with no rows has nothing wrong with it.
func TestChain_EmptyLedgerVerifies(t *testing.T) {
	uc, _ := newChainedUC(t, testKey)
	if err := uc.VerifyChain(context.Background()); err != nil {
		t.Fatalf("VerifyChain on empty ledger: %v", err)
	}
	state, err := uc.ChainState(context.Background())
	if err != nil {
		t.Fatalf("ChainState: %v", err)
	}
	if !state.Enabled || state.HeadSeq != 0 || state.AnchorSeq != 0 {
		t.Errorf("ChainState = %+v, want enabled with zero seqs", state)
	}
}

// TestChain_ChainStateReportsAnchor makes the weak spot visible instead
// of hiding it: after retention, the ledger is verified only from the
// anchor forward, and an operator has to be able to ask where that is.
func TestChain_ChainStateReportsAnchor(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	ctx := context.Background()
	now := time.Now().UTC()
	emitAt(t, uc, now.Add(-240*time.Hour), "device_update_1")
	emitAt(t, uc, now, "device_update_2")
	emitAt(t, uc, now, "device_update_3")
	if _, _, err := store.NewChainStore(db).TruncateExpiredPrefix(ctx, now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	state, err := uc.ChainState(ctx)
	if err != nil {
		t.Fatalf("ChainState: %v", err)
	}
	if state.HeadSeq != 3 || state.AnchorSeq != 2 {
		t.Errorf("ChainState = %+v, want head 3 anchor 2", state)
	}
}
