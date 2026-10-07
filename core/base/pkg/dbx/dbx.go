// Package dbx is the shared infrastructure helper for the opskeeper database.
//
// Opskeeper defaults to SQLite (via github.com/glebarez/sqlite), so a fresh
// checkout boots with no external service. MySQL (gorm.io/driver/mysql) and
// PostgreSQL remain available as opt-in backends. The data model itself is
// dialect-agnostic GORM; callers should not depend on any dialect-specific SQL.
//
// SQLite pragmas enabled at open time (when Dialect == "sqlite"):
//
//	journal_mode = WAL        // concurrent readers + single writer
//	busy_timeout = 5000 ms    // block briefly instead of SQLITE_BUSY
//	foreign_keys = ON         // SQLite ships with FKs disabled by default
//
// MySQL/PostgreSQL connections verify reachability with Ping() at Open time so
// config mistakes surface as a fail-fast error instead of lazily at first
// query. SQLite needs no server, so it simply materialises the database file.
package dbx

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/glebarez/sqlite"
	gormmysql "gorm.io/driver/mysql"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
)

// Open opens the configured database backend. Dialect selects SQLite (default),
// MySQL, or PostgreSQL; an empty dialect is treated as SQLite for defensive
// defaults, landing on config.DefaultSQLitePath when no path is supplied.
//
// The returned *gorm.DB uses a Warn-level logger so the normal query stream
// stays out of the application log. Callers that want query logs should wrap
// with db.Session(&gorm.Session{Logger: ...}) at call sites.
func Open(cfg config.DBConfig, log *slog.Logger) (*gorm.DB, error) {
	switch cfg.Dialect {
	case "mysql":
		return openMySQL(cfg.DSN, cfg.Pool, log)
	case "", "sqlite":
		// A zero-value DBConfig carries neither dialect nor path. Rather
		// than fail on the empty string, fall back to the same default
		// config.Load() applies so both paths converge on one database.
		path := cfg.Path
		if path == "" {
			path = config.DefaultSQLitePath
		}
		return openSQLite(path, cfg.Pool, log)
	case "postgres", "postgresql", "pg":
		return openPostgres(cfg.DSN, cfg.Pool, log)
	default:
		return nil, fmt.Errorf("dbx: unsupported dialect %q", cfg.Dialect)
	}
}

// openMySQL opens a MySQL connection via gorm and verifies reachability
// with Ping(). The DSN password is never logged.
func openMySQL(dsn string, pool config.DBPoolConfig, log *slog.Logger) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("dbx: empty mysql DSN")
	}

	gdb, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("dbx: mysql open: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("dbx: mysql sql.DB handle: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("dbx: mysql ping failed: %w", err)
	}
	tunePool(sqlDB, pool, "mysql", log)

	if log != nil {
		log.Info("mysql opened", "endpoint", redactDSN(dsn))
	}
	return gdb, nil
}

// openSQLite opens a SQLite database at path with WAL + busy_timeout +
// foreign_keys pragmas. Parent directories are created (0o755) if needed.
//
// Path may be:
//   - a plain filesystem path ("./data/opskeeper.db", "/var/lib/opskeeper/db")
//   - ":memory:" for an in-memory DB (tests)
func openSQLite(path string, pool config.DBPoolConfig, log *slog.Logger) (*gorm.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("dbx: empty sqlite path")
	}

	if path != ":memory:" {
		dir := filepath.Dir(path)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("dbx: sqlite mkdir %q: %w", dir, err)
			}
		}
	}

	dsn := buildSQLiteDSN(path)

	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("dbx: sqlite open %q: %w", path, err)
	}

	// SQLite has no server-side connection budget to overrun, but the
	// handle is still shared by every bounded context in this process, so
	// the same ceiling applies for the same reason: one domain's leak must
	// not be able to exhaust the rest.
	if sqlDB, err := gdb.DB(); err == nil {
		tunePool(sqlDB, pool, "sqlite", log)
	} else if log != nil {
		log.Warn("sqlite pool could not be tuned", "err", err)
	}

	if log != nil {
		log.Info("sqlite opened", "path", path, "journal_mode", "WAL", "foreign_keys", "on")
	}
	return gdb, nil
}

// openPostgres opens a PostgreSQL connection via gorm and verifies
// reachability with Ping(). Keyword-style DSNs are never logged because their
// password cannot be redacted without a dialect-specific parser.
func openPostgres(dsn string, pool config.DBPoolConfig, log *slog.Logger) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("dbx: empty postgres DSN")
	}

	gdb, err := gorm.Open(gormpostgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("dbx: postgres open: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("dbx: postgres sql.DB handle: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("dbx: postgres ping failed: %w", err)
	}

	tunePool(sqlDB, pool, "postgres", log)

	if log != nil {
		endpoint := "postgres:***"
		if strings.Contains(dsn, "@") {
			endpoint = redactDSN(dsn)
		}
		log.Info("postgres opened", "endpoint", endpoint)
	}
	return gdb, nil
}

// tunePool applies the configured ceilings to a freshly opened handle.
//
// This function is the whole reason DBPoolConfig exists. Until it was
// written, every knob in that struct was read by config.Load, carried in
// the struct, documented with a default, and then dropped on the floor:
// database/sql's own defaults were what actually ran, which are
// MaxOpenConns unlimited, MaxIdleConns 2, and connections that never
// expire. That is not a tuning opinion, it is a blast radius. Every
// bounded context in this process shares one handle, so with no ceiling a
// single domain leaking connections — or simply holding them during a slow
// query — spends the server's whole connection budget and takes the other
// fifty-odd domains down with it. The manager cannot be split into pieces
// that scale independently while its halves still draw from one unbounded
// pool against one MySQL.
//
// A non-positive knob keeps the database/sql default rather than clamping,
// because "<=0 means unlimited" is a meaningful thing for an operator to
// ask for on purpose.
func tunePool(sqlDB *sql.DB, pool config.DBPoolConfig, dialect string, log *slog.Logger) {
	if pool.MaxOpen > 0 {
		sqlDB.SetMaxOpenConns(pool.MaxOpen)
	}
	if pool.MaxIdle > 0 {
		sqlDB.SetMaxIdleConns(pool.MaxIdle)
	}
	if pool.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(pool.ConnMaxLifetime)
	}
	if log == nil {
		return
	}
	if pool.MaxOpen <= 0 {
		// Worth saying out loud: this is the state that took the whole
		// control plane down once, and it is also the state an operator
		// gets by forgetting the variable.
		log.Warn("database pool has no ceiling; every domain shares one unlimited pool",
			"dialect", dialect, "hint", "set OPSKEEPER_DB_POOL_MAX_OPEN")
		return
	}
	if pool.MaxIdle > pool.MaxOpen {
		// database/sql silently reduces MaxIdle to MaxOpen, so a
		// config that says otherwise is a config that lies to whoever
		// reads the startup log.
		log.Warn("database pool idle exceeds open; database/sql will clamp it",
			"dialect", dialect, "max_open", pool.MaxOpen, "max_idle", pool.MaxIdle)
	}
	log.Info("database pool configured",
		"dialect", dialect,
		"max_open", pool.MaxOpen,
		"max_idle", pool.MaxIdle,
		"conn_max_lifetime", pool.ConnMaxLifetime)
}

// buildSQLiteDSN appends pragma query params expected by modernc/glebarez sqlite.
func buildSQLiteDSN(path string) string {
	if path == ":memory:" {
		return path
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(on)")
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q.Encode()
}

// redactDSN strips the password from a go-sql-driver/mysql DSN for logging.
//
// The go-sql-driver DSN format is:
//
//	[user[:password]@][net[(addr)]]/dbname[?params]
//
// We drop everything between the first ':' after user and the final '@',
// preserving user@host:port/db?params so operators can still see what
// they're connecting to without leaking credentials.
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	userinfo := dsn[:at]
	rest := dsn[at:]
	if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
		userinfo = userinfo[:colon] + ":***"
	}
	return userinfo + rest
}
