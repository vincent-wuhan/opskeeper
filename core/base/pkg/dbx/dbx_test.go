package dbx

import (
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	"gorm.io/gorm"
)

// The MySQL path is exercised manually via `docker compose up`; tests here
// stick to the SQLite in-memory backend because CI has no docker.

func TestOpen_SQLiteInMemory(t *testing.T) {
	cfg := config.DBConfig{Dialect: "sqlite", Path: ":memory:"}
	db, err := Open(cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if db == nil {
		t.Fatal("Open returned nil *gorm.DB")
	}
	// Sanity: a trivial SELECT should work.
	var one int
	if err := db.Raw("SELECT 1").Scan(&one).Error; err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if one != 1 {
		t.Errorf("SELECT 1 = %d, want 1", one)
	}
}

func TestOpen_DefaultsToSQLite(t *testing.T) {
	// Empty Dialect routes to the SQLite branch, so a config that forgot to
	// name a backend still opens a working local database rather than
	// erroring. ":memory:" keeps the test off the default ./data path.
	cfg := config.DBConfig{Path: ":memory:"}
	db, err := Open(cfg, nil)
	if err != nil {
		t.Fatalf("empty Dialect should default to sqlite, got error: %v", err)
	}
	// A successful Open already proves we neither hit "unsupported dialect"
	// nor the MySQL branch (which would fail its ping with no server). Prove
	// the handle is actually usable, since that is the whole point of the
	// default.
	var one int
	if err := db.Raw("SELECT 1").Scan(&one).Error; err != nil {
		t.Fatalf("default sqlite handle not usable: %v", err)
	}
	if one != 1 {
		t.Errorf("SELECT 1 = %d, want 1", one)
	}
}

func TestOpen_UnsupportedDialect(t *testing.T) {
	cfg := config.DBConfig{Dialect: "unsupported"}
	_, err := Open(cfg, nil)
	if err == nil {
		t.Fatal("expected error for unsupported dialect")
	}
	if !contains(err.Error(), "unsupported dialect") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "unsupported dialect")
	}
}

func TestOpen_PostgresRequiresDSN(t *testing.T) {
	cfg := config.DBConfig{Dialect: "postgres"}
	_, err := Open(cfg, nil)
	if err == nil {
		t.Fatal("expected error for empty postgres DSN")
	}
	if !contains(err.Error(), "empty postgres DSN") {
		t.Fatalf("error = %q, want empty postgres DSN", err.Error())
	}
}

// fakeModel is a tiny model used to exercise RunMigrations end-to-end
// against the SQLite :memory: backend.
type fakeModel struct {
	ID   uint64 `gorm:"primaryKey"`
	Name string
}

func (fakeModel) TableName() string { return "fake_models" }

func fakeMigrator(db *gorm.DB) error {
	return db.AutoMigrate(&fakeModel{})
}

func TestRunMigrations_AppliesAndLogs(t *testing.T) {
	db, err := Open(config.DBConfig{Dialect: "sqlite", Path: ":memory:"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := RunMigrations(db, nil, fakeMigrator); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// Confirm the fake_models table exists in sqlite_master.
	var name string
	if err := db.Raw(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`,
		"fake_models",
	).Scan(&name).Error; err != nil {
		t.Fatalf("lookup fake_models: %v", err)
	}
	if name != "fake_models" {
		t.Errorf("table fake_models missing (got %q)", name)
	}

	// Running again should also succeed (AutoMigrate is idempotent).
	if err := RunMigrations(db, nil, fakeMigrator); err != nil {
		t.Fatalf("RunMigrations (second run): %v", err)
	}
}

func TestRunMigrations_NilDBRejected(t *testing.T) {
	if err := RunMigrations(nil, nil, fakeMigrator); err == nil {
		t.Fatal("expected error for nil db")
	}
}

func TestRunMigrations_NilMigratorRejected(t *testing.T) {
	db, err := Open(config.DBConfig{Dialect: "sqlite", Path: ":memory:"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := RunMigrations(db, nil, nil); err == nil {
		t.Fatal("expected error for nil migrator")
	}
}

func TestBackfillDeleteMarkerUsesDialectQuoting(t *testing.T) {
	cases := []struct {
		dialect string
		table   string
		want    string
	}{
		{dialect: "mysql", table: "devices", want: "`devices`"},
		{dialect: "postgres", table: "devices", want: `"devices"`},
		{dialect: "sqlite", table: "devices", want: "`devices`"},
	}
	for _, c := range cases {
		got, err := quoteIdentifier(c.dialect, c.table)
		if err != nil {
			t.Fatalf("quoteIdentifier(%q): %v", c.dialect, err)
		}
		if got != c.want {
			t.Errorf("quoteIdentifier(%q) = %q, want %q", c.dialect, got, c.want)
		}
	}
}

func TestBackfillDeleteMarkerRejectsUnknownDialect(t *testing.T) {
	if _, err := quoteIdentifier("unsupported", "devices"); err == nil {
		t.Fatal("expected unsupported dialect error")
	}
}

func TestRedactDSN(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			in:   "opskeeper:opskeeper@tcp(127.0.0.1:3306)/opskeeper?parseTime=true",
			want: "opskeeper:***@tcp(127.0.0.1:3306)/opskeeper?parseTime=true",
		},
		{
			in:   "root:hunter2@tcp(mysql:3306)/db",
			want: "root:***@tcp(mysql:3306)/db",
		},
		{
			in:   "noauth@tcp(host:3306)/db",
			want: "noauth@tcp(host:3306)/db",
		},
		{
			in:   "/no/at/sign",
			want: "/no/at/sign",
		},
	}
	for _, c := range cases {
		if got := redactDSN(c.in); got != c.want {
			t.Errorf("redactDSN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// contains is a tiny helper to avoid importing "strings" just for one call.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	n := len(sub)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(s); i++ {
		if s[i:i+n] == sub {
			return i
		}
	}
	return -1
}

// The pool knobs were read by config.Load, carried in DBPoolConfig, given
// documented defaults, and then never applied. database/sql's own defaults
// ran instead: unlimited open connections, two idle, and connections that
// never expire. Every bounded context in the process shares this one
// handle, so that is not a tuning preference — it is the whole control
// plane's connection budget with no ceiling on it.
func TestOpenAppliesTheConfiguredPoolCeilings(t *testing.T) {
	gdb, err := Open(config.DBConfig{
		Dialect: "sqlite",
		Path:    ":memory:",
		Pool: config.DBPoolConfig{
			MaxOpen:         7,
			MaxIdle:         3,
			ConnMaxLifetime: time.Hour,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("gdb.DB: %v", err)
	}
	if got := sqlDB.Stats().MaxOpenConnections; got != 7 {
		t.Fatalf("MaxOpenConnections = %d, want 7 — the configured ceiling is not reaching database/sql", got)
	}
}

// A non-positive knob means "let database/sql decide", which is a real
// thing an operator can ask for on purpose. It must not be silently
// rewritten into a small number, and it must not be clamped to zero either
// (zero max-open is a different statement: refuse every connection).
func TestAPoolKnobLeftAtZeroKeepsTheDriverDefault(t *testing.T) {
	gdb, err := Open(config.DBConfig{Dialect: "sqlite", Path: ":memory:"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("gdb.DB: %v", err)
	}
	if got := sqlDB.Stats().MaxOpenConnections; got != 0 {
		t.Fatalf("MaxOpenConnections = %d, want 0 (database/sql's unlimited default)", got)
	}
}

// ConnMaxLifetime is the knob behind the documented HA advice about load
// balancers cycling connections out from under a rolling upgrade. It is
// also the one nothing observed before, because MaxOpenConnections — the
// only pool number Stats reports — said nothing about it.
func TestTunePoolRetiresConnectionsOnTheConfiguredLifetime(t *testing.T) {
	gdb, err := Open(config.DBConfig{Dialect: "sqlite", Path: ":memory:"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("gdb.DB: %v", err)
	}

	tunePool(sqlDB, config.DBPoolConfig{ConnMaxLifetime: time.Nanosecond}, "sqlite", nil)
	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// The connection opened above is now idle and already past its
	// lifetime. Letting the next acquire find it expired is what proves
	// the setting landed: Stats counts the close.
	time.Sleep(5 * time.Millisecond)
	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("second ping: %v", err)
	}
	if got := sqlDB.Stats().MaxLifetimeClosed; got < 1 {
		t.Fatalf("MaxLifetimeClosed = %d, want at least 1 — ConnMaxLifetime did not retire the idle connection", got)
	}
}
