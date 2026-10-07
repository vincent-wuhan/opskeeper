//go:build integration

package store

import (
	"fmt"
	"os"
	"testing"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This file exists because the sibling SQLite test could not have caught
// the bug it guards against, and the reason it could not is the whole
// point.
//
// dedupeRaw's statement is accepted by SQLite and rejected by MySQL with
// "You can't specify target table for update in FROM clause" (1093). The
// migration therefore had a passing test and a deployment that could not
// boot — and it did not fail on the first boot either, because dedupeRaw
// returns early while the table does not exist. It was the second boot
// that died, which is exactly the case a CI that recreates its database
// every run never reaches.
//
// So the guard has to run on the dialect the deployment runs. It skips
// when OPSKEEPER_TEST_MYSQL_DSN is unset, and `make mysql-migration-check`
// is the target that sets it, because a test that only runs when somebody
// remembers is the same failure one level up.

func openMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("OPSKEEPER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("OPSKEEPER_TEST_MYSQL_DSN is not set; run make mysql-migration-check")
	}
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	return db
}

// TestTheDedupeMigrationRunsOnMySQLAtAll is the one that was missing: the
// statement has to parse at all. It is a small test and it is the one that
// would have caught the regression while the deployment was still healthy.
func TestTheDedupeMigrationRunsOnMySQLAtAll(t *testing.T) {
	db := openMySQL(t)
	fresh := "host_metrics_raw_dedupe_a"
	prepareRawTable(t, db, fresh, true)
	defer db.Migrator().DropTable(fresh)

	if err := db.Exec(insertRawRow(fresh, 1, 100)).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Exec(insertRawRow(fresh, 1, 100)).Error; err != nil {
		t.Fatalf("insert duplicate: %v", err)
	}
	if err := db.Exec(insertRawRow(fresh, 1, 200)).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := dedupeRawTable(t, db, fresh); err != nil {
		t.Fatalf("dedupe on mysql: %v", err)
	}
	var n int64
	if err := db.Table(fresh).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("collapsed to %d rows, want 2: the duplicate at (edge_id=1, ts=100) survived", n)
	}
}

// TestTheDedupeMigrationIsSafeToRunTwice is the shape the bug actually took:
// boot once to create the table, boot again to repair it. A migration that
// only works on a brand new database is a migration that has been tested on
// the one boot nobody performs twice.
func TestTheDedupeMigrationIsSafeToRunTwice(t *testing.T) {
	db := openMySQL(t)
	fresh := "host_metrics_raw_dedupe_b"
	prepareRawTable(t, db, fresh, true)
	defer db.Migrator().DropTable(fresh)

	for _, ts := range []int{100, 100, 200, 300} {
		if err := db.Exec(insertRawRow(fresh, 7, ts)).Error; err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := dedupeRawTable(t, db, fresh); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
	}
	var n int64
	if err := db.Table(fresh).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("after two passes %d rows remain, want 3", n)
	}
}

// TestTheDedupeMigrationKeepsTheLowestID is the property the SQLite test
// already asserts, repeated here because the SQL changed underneath it and
// a change in SQL is exactly when that assertion needs to be re-earned.
func TestTheDedupeMigrationKeepsTheLowestID(t *testing.T) {
	db := openMySQL(t)
	fresh := "host_metrics_raw_dedupe_c"
	prepareRawTable(t, db, fresh, true)
	defer db.Migrator().DropTable(fresh)

	if err := db.Exec(insertRawRow(fresh, 1, 100)).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	first := lastRawID(t, db, fresh)
	if err := db.Exec(insertRawRow(fresh, 1, 100)).Error; err != nil {
		t.Fatalf("insert duplicate: %v", err)
	}
	if err := dedupeRawTable(t, db, fresh); err != nil {
		t.Fatalf("dedupe: %v", err)
	}
	if got := lastRawID(t, db, fresh); got != first {
		t.Errorf("the surviving row is id %d, want the original %d: "+
			"created_at is what the retention job and the operator's timeline read", got, first)
	}
}

// The helpers below build the pre-dedup shape on a scratch table, because
// the deployment's own metrics are not a table a test may empty.

func prepareRawTable(t *testing.T, db *gorm.DB, table string, withLegacyIndex bool) {
	t.Helper()
	db.Migrator().DropTable(table)
	stmt := fmt.Sprintf(`CREATE TABLE %s (
		id BIGINT NOT NULL AUTO_INCREMENT,
		edge_id BIGINT NOT NULL,
		ts DATETIME(3) NOT NULL,
		cpu_pct DOUBLE NOT NULL,
		mem_pct DOUBLE NOT NULL,
		load1 DOUBLE NOT NULL,
		disk_used_ratio DOUBLE NOT NULL,
		net_rx_bytes BIGINT NOT NULL,
		net_tx_bytes BIGINT NOT NULL,
		proc_run_total INT NOT NULL,
		PRIMARY KEY (id)
	)`, table)
	if err := db.Exec(stmt).Error; err != nil {
		t.Fatalf("create %s: %v", table, err)
	}
	if withLegacyIndex {
		if err := db.Exec(fmt.Sprintf(
			"CREATE INDEX %s ON %s (edge_id, ts)", legacyRawIndex, table)).Error; err != nil {
			t.Fatalf("create the legacy index: %v", err)
		}
	}
}

// insertRawRow puts one row at (edgeID, ts). The ts is part of the dedup
// key, so a test that inserted a constant one would collapse every row of
// an edge into a single survivor and pass for the wrong reason.
func insertRawRow(table string, edgeID, ts int) string {
	stamp := time.Unix(int64(ts), 0).UTC().Format("2006-01-02 15:04:05.000")
	return fmt.Sprintf(
		"INSERT INTO %s (edge_id, ts, cpu_pct, mem_pct, load1, disk_used_ratio, "+
			"net_rx_bytes, net_tx_bytes, proc_run_total) VALUES (%d, '%s', "+
			"1, 1, 1, 0.1, 1, 1, 1)", table, edgeID, stamp)
}

func dedupeRawTable(t *testing.T, db *gorm.DB, table string) error {
	t.Helper()
	return dedupeTable(db, table)
}

func lastRawID(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var id int64
	row := db.Table(table).Select("MAX(id)").Row()
	if err := row.Scan(&id); err != nil {
		t.Fatalf("read the surviving id: %v", err)
	}
	return id
}
