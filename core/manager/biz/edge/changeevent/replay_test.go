package changeevent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/edge/changeevent"
	edgestore "github.com/vincent-wuhan/opskeeper/core/manager/data/edge/store"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
)

// newDB opens an in-memory SQLite with the real edge schema and returns a
// usecase wired to the real store. The dedup contract spans three layers —
// the (edge_id, seq) unique index, StoredSeqs, and the filter in the usecase
// — so a fake at any one of them would be testing the fake.
func newDB(t *testing.T) (*changeevent.Usecase, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := edgestore.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return changeevent.New(edgestore.NewChangeEventRepo(db), nil), db
}

func seq(n uint64) *uint64 { return &n }

func event(edgeID uint64, kind, subject string, s *uint64, at time.Time) edgemodel.ChangeEventRow {
	return edgemodel.ChangeEventRow{
		EdgeID:    edgeID,
		Source:    "journald",
		Kind:      kind,
		Subject:   subject,
		Action:    "restart",
		Timestamp: at,
		Severity:  "notice",
		Labels:    "{}",
		Seq:       s,
	}
}

func countRows(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&edgemodel.ChangeEventRow{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestAReplayedBatchIsStoredOnce(t *testing.T) {
	// The ground truth. A node's write-ahead log is at-least-once: an ack
	// lost in a reconnect means the same rows come round again, and the
	// center has to recognise them.
	uc, db := newDB(t)
	ctx := context.Background()
	at := time.Date(2026, 4, 23, 3, 12, 0, 0, time.UTC)
	batch := []edgemodel.ChangeEventRow{
		event(7, "service_restart", "nginx.service", seq(1), at),
		event(7, "ssh_login", "ops", seq(2), at),
	}

	n, err := uc.BatchInsert(ctx, batch)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	if n != 2 {
		t.Fatalf("first push reported %d accepted, want 2", n)
	}

	// The node never heard the ack, so it sends the identical batch again.
	n, err = uc.BatchInsert(ctx, batch)
	if err != nil {
		t.Fatalf("replay must not be an error, got: %v", err)
	}
	if n != 0 {
		t.Errorf("replay reported %d accepted, want 0: the node would be told it had fresh events", n)
	}
	if got := countRows(t, db); got != 2 {
		t.Fatalf("table holds %d rows after a replay, want 2", got)
	}
}

func TestAPartlyReplayedBatchKeepsWhatIsNew(t *testing.T) {
	// The shape a reconnect actually produces: the log drains from the
	// oldest row, so a batch is usually some rows the center has and some
	// it has never seen. Dropping the whole batch would lose the new ones;
	// keeping the whole batch would duplicate the old ones.
	uc, db := newDB(t)
	ctx := context.Background()
	at := time.Date(2026, 4, 23, 3, 12, 0, 0, time.UTC)

	if _, err := uc.BatchInsert(ctx, []edgemodel.ChangeEventRow{
		event(7, "service_restart", "nginx.service", seq(1), at),
		event(7, "ssh_login", "ops", seq(2), at),
	}); err != nil {
		t.Fatalf("first push: %v", err)
	}

	n, err := uc.BatchInsert(ctx, []edgemodel.ChangeEventRow{
		event(7, "service_restart", "nginx.service", seq(1), at), // replayed
		event(7, "ssh_login", "ops", seq(2), at),                 // replayed
		event(7, "package_install", "curl", seq(3), at),          // new
		event(7, "service_restart", "redis.service", seq(4), at), // new
	})
	if err != nil {
		t.Fatalf("mixed push: %v", err)
	}
	if n != 2 {
		t.Errorf("mixed push reported %d accepted, want 2 (only the new rows)", n)
	}
	if got := countRows(t, db); got != 4 {
		t.Fatalf("table holds %d rows, want 4", got)
	}
}

func TestEventsWithNoSequenceAreNeverDeduped(t *testing.T) {
	// The reason this table has a nullable column and not a 0. A change
	// event has no natural key — two genuine restarts of the same unit in
	// the same second can agree on every other field — so an event the
	// center cannot name is always stored, and two such events are two
	// real events even when they are byte-identical.
	uc, db := newDB(t)
	ctx := context.Background()
	at := time.Date(2026, 4, 23, 3, 12, 0, 0, time.UTC)
	twin := event(7, "service_restart", "nginx.service", nil, at)

	for i := 0; i < 2; i++ {
		if _, err := uc.BatchInsert(ctx, []edgemodel.ChangeEventRow{twin}); err != nil {
			t.Fatalf("push %d: %v", i+1, err)
		}
	}
	if got := countRows(t, db); got != 2 {
		t.Fatalf("table holds %d rows, want 2: a seq-less event was mistaken for a replay", got)
	}
}

func TestTheSameSequenceOnTwoNodesIsNotADuplicate(t *testing.T) {
	// The sequence is the node's, not the center's. Two nodes each start
	// their logs at 1, and their first events are not the same event.
	uc, db := newDB(t)
	ctx := context.Background()
	at := time.Date(2026, 4, 23, 3, 12, 0, 0, time.UTC)

	if _, err := uc.BatchInsert(ctx, []edgemodel.ChangeEventRow{
		event(7, "ssh_login", "ops", seq(1), at),
		event(8, "ssh_login", "ops", seq(1), at),
	}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := countRows(t, db); got != 2 {
		t.Fatalf("table holds %d rows, want 2: one node's row swallowed another's", got)
	}
	// And each node's own replay is still caught.
	n, err := uc.BatchInsert(ctx, []edgemodel.ChangeEventRow{event(8, "ssh_login", "ops", seq(1), at)})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n != 0 {
		t.Errorf("node 8's replay reported %d accepted, want 0", n)
	}
}

func TestABatchThatMixesEdgesIsStoredUnfiltered(t *testing.T) {
	// Asking StoredSeqs for one edge's sequences and then filtering a batch
	// that names two would drop real events. A batch that does this is
	// malformed, and the safe response to malformed is "store it and say so",
	// never "guess which half to throw away".
	uc, db := newDB(t)
	ctx := context.Background()
	at := time.Date(2026, 4, 23, 3, 12, 0, 0, time.UTC)
	if _, err := uc.BatchInsert(ctx, []edgemodel.ChangeEventRow{
		event(7, "ssh_login", "ops", seq(1), at),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	n, err := uc.BatchInsert(ctx, []edgemodel.ChangeEventRow{
		event(7, "ssh_login", "ops", seq(1), at),
		event(8, "ssh_login", "ops", seq(1), at),
	})
	if err != nil {
		t.Fatalf("mixed-edge push: %v", err)
	}
	// The unique index still refuses the true duplicate, so the count is 1
	// not 2 — the constraint is the backstop the filter is not.
	if n < 1 {
		t.Errorf("mixed-edge push dropped a new event: reported %d", n)
	}
	if got := countRows(t, db); got != 2 {
		t.Fatalf("table holds %d rows, want 2 (node 7's original and node 8's new one)", got)
	}
}

// brokenLookup is a repo whose StoredSeqs always fails, standing in for a
// database blip between the arrival of a replay and the check for it.
type brokenLookup struct {
	*edgestore.ChangeEventRepo
	err error
}

func (b brokenLookup) StoredSeqs(context.Context, uint64, []uint64) ([]uint64, error) {
	return nil, b.err
}

func TestAFailedLookupStoresUnfilteredAndTheIndexStillHolds(t *testing.T) {
	// Two layers, and the point of the second is that the first can fail.
	// If the unique index were the only defence, one slow query would
	// duplicate a node's entire outage history; if the filter were the only
	// defence, the same query failing would wedge the table.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := edgestore.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	boom := errors.New("connection reset")
	uc := changeevent.New(brokenLookup{edgestore.NewChangeEventRepo(db), boom}, nil)
	ctx := context.Background()
	at := time.Date(2026, 4, 23, 3, 12, 0, 0, time.UTC)
	batch := []edgemodel.ChangeEventRow{event(7, "ssh_login", "ops", seq(1), at)}

	if _, err := uc.BatchInsert(ctx, batch); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// The lookup fails, so the usecase stores unfiltered. The report is
	// deliberately not "1 new event" — it cannot know — but the database
	// must still hold one row.
	if _, err := uc.BatchInsert(ctx, batch); err != nil {
		t.Fatalf("push with a broken lookup must not fail the whole batch: %v", err)
	}
	if got := countRows(t, db); got != 1 {
		t.Fatalf("table holds %d rows, want 1: the index is not a backstop", got)
	}
}
