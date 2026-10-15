package dbx

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	"gorm.io/gorm"
)

// DialectFactory opens one database backend from the process config.
//
// It is the extension point a package registers through: a deployment whose
// control-plane database is one gorm has no driver for is a registration
// away, not a new case in Open.
type DialectFactory func(cfg config.DBConfig, log *slog.Logger) (*gorm.DB, error)

var (
	dialectMu sync.RWMutex
	// dialectFactories is keyed by canonical name.
	dialectFactories = map[string]DialectFactory{
		"mysql": func(cfg config.DBConfig, log *slog.Logger) (*gorm.DB, error) {
			return openMySQL(cfg.DSN, cfg.Pool, log)
		},
		"sqlite": openSQLiteFactory,
		"postgresql": func(cfg config.DBConfig, log *slog.Logger) (*gorm.DB, error) {
			return openPostgres(cfg.DSN, cfg.Pool, log)
		},
	}
	// dialectAliases maps an alternate spelling to its canonical name. An
	// empty dialect resolves to sqlite for defensive defaults, the same way
	// config.Load() applies them.
	dialectAliases = map[string]string{
		"":         "sqlite",
		"pg":       "postgresql",
		"postgres": "postgresql",
	}
)

// openSQLiteFactory carries the empty-path default that used to live inline
// in Open, so the fallback to config.DefaultSQLitePath is a property of the
// sqlite factory rather than of the dispatch.
func openSQLiteFactory(cfg config.DBConfig, log *slog.Logger) (*gorm.DB, error) {
	path := cfg.Path
	if path == "" {
		path = config.DefaultSQLitePath
	}
	return openSQLite(path, cfg.Pool, log)
}

// RegisterDialect registers a database backend by canonical name.
//
// It refuses a duplicate rather than replacing, and an empty name, for the
// same reason the microkernel registry does: a silent replace is how one
// dialect takes over another's name.
func RegisterDialect(name string, f DialectFactory) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return fmt.Errorf("dbx: dialect name is empty")
	}
	if f == nil {
		return fmt.Errorf("dbx: dialect %q has no implementation", name)
	}
	dialectMu.Lock()
	defer dialectMu.Unlock()
	if _, ok := dialectFactories[name]; ok {
		return fmt.Errorf("dbx: dialect %q is already registered", name)
	}
	dialectFactories[name] = f
	return nil
}

// resolveDialect maps a configured spelling to its canonical factory. It is
// the whole of the dispatch Open does.
func resolveDialect(cfg config.DBConfig) (DialectFactory, bool) {
	dialectMu.RLock()
	defer dialectMu.RUnlock()
	name := strings.ToLower(strings.TrimSpace(cfg.Dialect))
	if canonical, ok := dialectAliases[name]; ok {
		name = canonical
	}
	f, ok := dialectFactories[name]
	return f, ok
}

// RegisteredDialects returns the canonical dialect names a factory is
// registered for, sorted. It is what a diagnostics page lists, derived from
// the registry so it cannot drift from what Open will actually open.
func RegisteredDialects() []string {
	dialectMu.RLock()
	defer dialectMu.RUnlock()
	out := make([]string, 0, len(dialectFactories))
	for name := range dialectFactories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
