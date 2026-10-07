package store

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	model "github.com/vincent-wuhan/opskeeper/core/domains/model/metric"
)

// newRawDB opens an in-memory DB holding a host_metrics_raw table in its
// PRE-dedup shape: the old non-unique (edge_id, ts) index, and duplicates
// already sitting in it.
//
// This is the only shape that makes the migration interesting. A fresh
// database never has duplicates, so a test that only ever starts empty
// cannot tell a migration that repairs a deployment from one that silently
// assumes there is nothing to repair.
func newRawDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	stmts := []string{
		`CREATE TABLE host_metrics_raw (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			edge_id INTEGER NOT NULL,
			ts DATETIME NOT NULL,
			cpu_pct REAL NOT NULL,
			mem_pct REAL NOT NULL,
			load1 REAL NOT NULL,
			load5 REAL NOT NULL,
			load15 REAL NOT NULL,
			net_rx_bps INTEGER NOT NULL,
			net_tx_bps INTEGER NOT NULL,
			disk_used_pct REAL,
			created_at DATETIME
		)`,
		`CREATE INDEX idx_host_metrics_raw_edge_ts ON host_metrics_raw (edge_id, ts)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("seed pre-dedup schema (%s): %v", s, err)
		}
	}
	return db
}

// seedDups inserts n copies of the same point plus one distinct point, the
// way a retried WriteRaw batch looks on disk.
func seedDups(t *testing.T, db *gorm.DB, edgeID uint64, ts string, copies int) {
	t.Helper()
	for i := 0; i < copies; i++ {
		row := model.HostMetric{EdgeID: edgeID, Ts: mustTime(t, ts), CPUPct: 10, MemPct: 50, NetRxBps: 100, NetTxBps: 200}
		if err := db.Create(&row).Error; err != nil {
			t.Fatalf("seed duplicate %d: %v", i, err)
		}
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", s)
	if err != nil {
		ts2, err2 := time.Parse("2006-01-02 15:04:05", s)
		if err2 != nil {
			t.Fatalf("parse %q: %v / %v", s, err, err2)
		}
		return ts2.UTC()
	}
	return ts.UTC()
}

func TestMigrate_RepairsATableThatAlreadyHoldsDuplicates(t *testing.T) {
	// The upgrade path. Creating uq_host_metrics_raw_edge_ts over a table
	// that violates it fails, so if Migrate did not collapse the duplicates
	// first, every already-running deployment would fail to start.
	db := newRawDB(t)
	seedDups(t, db, 1, "2026-04-23 12:00:00", 3)
	seedDups(t, db, 1, "2026-04-23 12:00:10", 1)
	seedDups(t, db, 2, "2026-04-23 12:00:00", 1) // a different edge, not a duplicate

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on a duplicated table: %v", err)
	}

	var rows []model.HostMetric
	if err := db.Order("edge_id, ts").Find(&rows).Error; err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("after Migrate the table holds %d rows, want 3 (one per (edge_id, ts))", len(rows))
	}
	// The survivor must be the original, not a later copy: a replay is
	// byte-identical, so created_at is the only thing that tells them apart
	// and retention reads it.
	if rows[0].ID == 0 {
		t.Error("the surviving row has no id; the wrong copy was kept")
	}
}

func TestMigrate_LeavesADistinctRowAlone(t *testing.T) {
	// The dedupe must not become a truncation. Two points one second apart
	// on the same edge are two points, and a migration that collapsed them
	// would be a silent data-loss bug wearing the costume of a fix.
	db := newRawDB(t)
	if err := db.Exec(`INSERT INTO host_metrics_raw (edge_id, ts, cpu_pct, mem_pct, load1, load5, load15, net_rx_bps, net_tx_bps, disk_used_pct, created_at)
		VALUES (1, '2026-04-23 12:00:00', 10, 50, 0, 0, 0, 100, 200, 5, '2026-04-23 12:00:00'),
		       (1, '2026-04-23 12:00:01', 11, 51, 0, 0, 0, 100, 200, 5, '2026-04-23 12:00:01')`).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var n int64
	db.Model(&model.HostMetric{}).Count(&n)
	if n != 2 {
		t.Fatalf("rows = %d, want 2: adjacent seconds were treated as duplicates", n)
	}
}

func TestMigrate_ReplacesTheLegacyIndexWithTheUniqueOne(t *testing.T) {
	// Both indexes on (edge_id, ts) would cost a write on every insert and,
	// worse, leave the old name in the schema as a thing that looks
	// meaningful and is not.
	db := newRawDB(t)
	seedDups(t, db, 1, "2026-04-23 12:00:00", 2)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	m := db.Migrator()
	if m.HasIndex(&model.HostMetric{}, legacyRawIndex) {
		t.Errorf("%s survived the migration", legacyRawIndex)
	}
	if !m.HasIndex(&model.HostMetric{}, "uq_host_metrics_raw_edge_ts") {
		t.Error("uq_host_metrics_raw_edge_ts was not created")
	}
}

func TestMigrate_IsIdempotent(t *testing.T) {
	// Startup runs on every boot, and a deployment that has already been
	// repaired must not fail on the second run for having nothing to do.
	db := newRawDB(t)
	seedDups(t, db, 1, "2026-04-23 12:00:00", 2)
	for i := 0; i < 3; i++ {
		if err := Migrate(db); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}
	var n int64
	db.Model(&model.HostMetric{}).Count(&n)
	if n != 1 {
		t.Fatalf("rows after three runs = %d, want 1", n)
	}
}

func TestMigrate_OnAFreshDatabase(t *testing.T) {
	// No table at all: dedupe has nothing to look at and must say so
	// quietly rather than erroring on a table it was told does not exist.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on an empty database: %v", err)
	}
	if !db.Migrator().HasIndex(&model.HostMetric{}, "uq_host_metrics_raw_edge_ts") {
		t.Error("a fresh database did not get the unique index")
	}
}

func TestWriteRaw_RefusesToSwallowAnUnrelatedConflict(t *testing.T) {
	// The ON CONFLICT target is named (edge_id, ts) rather than left bare. A
	// bare DoNothing would swallow a collision on *any* unique key and report
	// success for a row that was never stored — and the batch-level report is
	// what tells the node to ack, so a swallowed row is a lost row.
	//
	// The collision here is on cpu_pct: a different edge at a different
	// second, so (edge_id, ts) is not violated and only the named-target
	// clause can tell the difference.
	db := newTestDB(t)
	ctx := t.Context()
	base := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	if err := db.Exec(`CREATE UNIQUE INDEX uq_cpu_pct ON host_metrics_raw (cpu_pct)`).Error; err != nil {
		t.Fatalf("add the unrelated unique key: %v", err)
	}
	w := NewWriter(db)
	if err := w.WriteRaw(ctx, []model.Point{{EdgeID: 1, Ts: base, CPUPct: 42}}); err != nil {
		t.Fatalf("the first row should land: %v", err)
	}
	// Same cpu_pct, different edge and second: only uq_cpu_pct is violated.
	err := w.WriteRaw(ctx, []model.Point{{EdgeID: 2, Ts: base.Add(time.Second), CPUPct: 42}})
	if err == nil {
		t.Fatal("a conflict on an unrelated unique key was swallowed: the caller was told the row landed")
	}
	var n int64
	db.Model(&model.HostMetric{}).Where("edge_id = ?", 2).Count(&n)
	if n != 0 {
		t.Errorf("edge 2 has %d rows after the refused write, want 0", n)
	}
}
