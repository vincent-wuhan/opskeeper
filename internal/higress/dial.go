package higress

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// gormOnConflictName makes Upsert conflict on the primary key (name).
var gormOnConflictName = clause.OnConflict{
	Columns:   []clause.Column{{Name: "name"}},
	UpdateAll: true,
}

// sqliteDialector returns the gorm dialector for the given file path.
// Pure-Go (glebarez/sqlite) — no CGo, runs anywhere.
func sqliteDialector(path string) gorm.Dialector {
	return sqlite.Open(path)
}

// discardWriter satisfies gorm's logger.Writer interface (Printf + Write).
type discardWriter struct{}

func (discardWriter) Printf(format string, args ...any) {} // discard
func (discardWriter) Write(p []byte) (int, error)       { return len(p), nil }

func newDiscardWriter() discardWriter { return discardWriter{} }
