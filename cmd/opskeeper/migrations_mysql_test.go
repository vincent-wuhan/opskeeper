//go:build integration

package main

import (
	"os"
	"testing"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/dbx"
)

// The manager's migrations are replayed in full on every boot, and there is
// no version ledger in front of them. The single guarantee a deployment has
// is therefore that every migrator is idempotent — and that guarantee was
// never tested, because the whole suite recreates a database per run and so
// only ever performs boot #1.
//
// Two migrators were not idempotent, and both failed the same way: a step
// that is a no-op on an empty database and a hard error on the second one.
// A deployment that had been started exactly once looked perfectly healthy.
//
// So this file replays the whole list, on the same database, more than
// once, on the dialect the deployment actually runs. That is the test both
// of them needed, and it is a list-level assertion rather than a
// per-migration one: a per-migration test only exists for the migrations
// somebody already thought of, and this is about the next one.

func openMigrationDB(t *testing.T) *gorm.DB {
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

func dropEverything(t *testing.T, db *gorm.DB) {
	t.Helper()
	rows, err := db.Raw("SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()").Rows()
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := db.Exec("SET FOREIGN_KEY_CHECKS = 0").Error; err != nil {
		t.Fatalf("relax foreign keys: %v", err)
	}
	defer func() { _ = db.Exec("SET FOREIGN_KEY_CHECKS = 1").Error }()
	for _, name := range tables {
		if err := db.Exec("DROP TABLE IF EXISTS `" + name + "`").Error; err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
	}
}

// TestTheManagerSchemaReplaysOnEveryBoot is the invariant. Three passes
// rather than two because the second is the one that catches the class of
// bug above, and the third is the one that catches a step whose repair pass
// is itself not idempotent.
func TestTheManagerSchemaReplaysOnEveryBoot(t *testing.T) {
	db := openMigrationDB(t)
	dropEverything(t, db)

	migrators := managerMigrators()
	if len(migrators) == 0 {
		t.Fatal("the manager has no migrations; this test would pass for the wrong reason")
	}
	for pass := 1; pass <= 3; pass++ {
		if err := runMigrationsForTest(t, db, migrators); err != nil {
			t.Fatalf("boot #%d could not migrate: %v\n"+
				"This build replays every migrator on every boot, so a step that is not "+
				"idempotent means the deployment cannot be restarted.", pass, err)
		}
	}
}

// TestAMigratorThatNamesAnUnsupportedDialectFailsLoudly is the other half of
// "the test above is not vacuous": the list is not allowed to be empty, and
// a migrator that silently does nothing would make three green passes mean
// nothing. A database with the tables present is the proof that the passes
// did work.
func TestThePassesActuallyBuiltASchema(t *testing.T) {
	db := openMigrationDB(t)
	dropEverything(t, db)
	if err := runMigrationsForTest(t, db, managerMigrators()); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	for _, table := range []string{
		"audit_logs", "edges", "host_metrics_raw", "repair_preview_runs",
		"repair_preview_run_bindings", "users",
	} {
		if !db.Migrator().HasTable(table) {
			t.Errorf("table %s does not exist after a migration pass; the passes above would be vacuous", table)
		}
	}
	// And the index that migration #24 used to try to create twice.
	if !db.Migrator().HasIndex("repair_preview_runs", "idx_repair_preview_runs_incident") {
		t.Error("the repair preview index is missing: moving its DDL out of the schema list " +
			"must not have dropped it")
	}
}

// TestEveryMigratorIsCalledOnEveryBoot guards the trivial failure of this
// test: a migrator added to main() and forgotten here, tested by nothing.
func TestEveryMigratorIsCalledOnEveryBoot(t *testing.T) {
	// 24 data packages, in startup order. A new one has to be added to
	// this count in the same change, which is the point: the list is now a
	// thing with a test on it rather than a literal in a function body.
	const want = 24
	if got := len(managerMigrators()); got != want {
		t.Errorf("managerMigrators has %d entries, this test expects %d; "+
			"a new data package is either not migrated or not covered", got, want)
	}
}

func runMigrationsForTest(t *testing.T, db *gorm.DB, migrators []dbx.Migrator) error {
	var firstErr error
	for i, m := range migrators {
		if m == nil {
			t.Fatalf("migrator #%d is nil", i+1)
		}
		if err := m(db); err != nil {
			firstErr = err
			break
		}
	}
	return firstErr
}
