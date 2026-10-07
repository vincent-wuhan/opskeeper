package dbx

import (
	"fmt"

	"gorm.io/gorm"
)

// DropIndexes drops named indexes when they already exist. It is intended for
// AutoMigrate-era index shape changes where GORM will not rewrite an existing
// index with the same name.
func DropIndexes(db *gorm.DB, model any, names ...string) error {
	if db == nil {
		return fmt.Errorf("dbx.DropIndexes: nil db")
	}
	if model == nil {
		return fmt.Errorf("dbx.DropIndexes: nil model")
	}
	for _, name := range names {
		if name == "" {
			return fmt.Errorf("dbx.DropIndexes: empty index name")
		}
		if db.Migrator().HasIndex(model, name) {
			if err := db.Migrator().DropIndex(model, name); err != nil {
				return fmt.Errorf("dbx.DropIndexes: drop %s: %w", name, err)
			}
		}
	}
	return nil
}

// NeedsDeleteMarkerMigration reports whether a legacy table still needs the
// one-time index rewrite that introduces delete_marker into unique keys.
func NeedsDeleteMarkerMigration(db *gorm.DB, table string) bool {
	if db == nil || !db.Migrator().HasTable(table) {
		return false
	}
	return !db.Migrator().HasColumn(table, "delete_marker")
}

// BackfillDeleteMarker moves legacy soft-deleted rows out of the active
// delete_marker=0 slot. New deletes are handled by gorm.io/plugin/soft_delete;
// this function only protects rows created before delete_marker existed.
func BackfillDeleteMarker(db *gorm.DB, table string) error {
	return BackfillDeleteMarkerWithValue(db, table, "id")
}

// BackfillDeleteMarkerWithValue is BackfillDeleteMarker with an explicit SQL
// value expression. Use "id" for numeric primary-key tables and "1" for tables
// where legacy uniqueness already guarantees at most one deleted row per key.
func BackfillDeleteMarkerWithValue(db *gorm.DB, table, valueExpr string) error {
	if db == nil {
		return fmt.Errorf("dbx.BackfillDeleteMarker: nil db")
	}
	if !db.Migrator().HasTable(table) {
		return nil
	}
	if !db.Migrator().HasColumn(table, "delete_marker") || !db.Migrator().HasColumn(table, "deleted_at") {
		return nil
	}
	dialect := db.Dialector.Name()
	quotedTable, err := quoteIdentifier(dialect, table)
	if err != nil {
		return err
	}
	expr, err := quoteValueExpr(dialect, valueExpr)
	if err != nil {
		return err
	}
	quotedDeleteMarker, err := quoteIdentifier(dialect, "delete_marker")
	if err != nil {
		return err
	}
	quotedDeletedAt, err := quoteIdentifier(dialect, "deleted_at")
	if err != nil {
		return err
	}
	sql := fmt.Sprintf(
		"UPDATE %s SET %s = %s WHERE %s IS NOT NULL AND %s = 0",
		quotedTable,
		quotedDeleteMarker,
		expr,
		quotedDeletedAt,
		quotedDeleteMarker,
	)
	if err := db.Exec(sql).Error; err != nil {
		return fmt.Errorf("dbx.BackfillDeleteMarker: %s: %w", table, err)
	}
	return nil
}

func quoteIdentifier(dialect, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("dbx: empty identifier")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return "", fmt.Errorf("dbx: unsafe identifier %q", name)
	}
	switch dialect {
	case "mysql":
		return "`" + name + "`", nil
	case "postgres", "postgresql":
		return `"` + name + `"`, nil
	case "sqlite":
		return "`" + name + "`", nil
	default:
		return "", fmt.Errorf("dbx: unsupported dialect %q", dialect)
	}
}

func quoteValueExpr(dialect, expr string) (string, error) {
	switch expr {
	case "1":
		return "1", nil
	case "id":
		return quoteIdentifier(dialect, "id")
	default:
		return "", fmt.Errorf("dbx: unsafe delete marker expression %q", expr)
	}
}
