package store

import (
	"fmt"

	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/core/domains/model/metric"
)

// legacyRawIndex is the non-unique (edge_id, ts) index the raw table carried
// before (edge_id, ts) became the dedup key. It is dropped once the unique
// index is in place, so the table is not left indexing the same two columns
// twice — which costs a write and, worse, invites the next reader to assume
// the first index name still means something.
const legacyRawIndex = "idx_host_metrics_raw_edge_ts"

// Migrate registers all metric tables with gorm's AutoMigrate: raw samples,
// 5-minute and 1-hour aggregate tiers, and the dead-letter table. Composite
// primary keys on the aggregate tables are expressed via `primaryKey;priority:N`
// tags so both MySQL and SQLite receive the right (edge_id, ts) PK ordering.
//
// The raw table gets three steps instead of one, in this order, and the order
// is the whole point:
//
//  1. collapse duplicates that are already on disk. A unique index cannot be
//     created over a table that violates it, so on any deployment that ran
//     before this change the create would simply fail and the schema would
//     be left half-migrated — which is how a migration turns into an outage.
//  2. AutoMigrate, which adds uq_host_metrics_raw_edge_ts from the model tag.
//  3. drop the old non-unique index.
//
// Each step is a no-op when it has nothing to do, so this runs unchanged on a
// fresh database, on an already-migrated one, and on one being repaired.
func Migrate(db *gorm.DB) error {
	if err := dedupeRaw(db); err != nil {
		return err
	}
	if err := db.AutoMigrate(
		&model.HostMetric{},
		&model.HostMetric5m{},
		&model.HostMetric1h{},
		&model.DeadLetter{},
	); err != nil {
		return err
	}
	return dropLegacyRawIndex(db)
}

// dedupeRaw keeps the lowest-id row for each (edge_id, ts) and deletes the
// rest. Lowest id, not "the newest": every copy of a point is byte-identical
// because a replay re-issues the same payload, so which one survives is a
// question with no right answer — and keeping the original preserves
// created_at, which is what retention and the operator's timeline read.
func dedupeRaw(db *gorm.DB) error {
	m := db.Migrator()
	if !m.HasTable(&model.HostMetric{}) {
		return nil // nothing has ever been written; there is nothing to repair
	}
	return dedupeTable(db, "host_metrics_raw")
}

// dedupeTable is dedupeRaw with the table named, so the statement can be
// exercised against a scratch table on a real MySQL instead of against the
// deployment's own metrics. That test is the only one that can catch a
// dialect the SQLite suite accepts and MySQL does not, and it needs a table
// it is allowed to delete every row of.
func dedupeTable(db *gorm.DB, table string) error {
	// The subquery is wrapped in a derived table, and that is not style. The
	// flat form — `id NOT IN (SELECT MIN(id) FROM host_metrics_raw ...)` —
	// is a parse error in MySQL: "You can't specify target table for update
	// in FROM clause" (1093). It is accepted by SQLite and by Postgres,
	// which is why it survived: this migration has only ever been tested
	// against an in-memory SQLite, so the one dialect the deployment
	// actually runs was the one nobody ran.
	//
	// The failure was not immediate either. dedupeRaw returns early when
	// the table does not exist yet, so the first boot composes the schema
	// and never reaches this statement; it is the *second* boot that
	// collapses the rows and dies. A deployment that had only ever been
	// started once looked fine.
	//
	// The derived table is the portable spelling: MySQL materialises it
	// before the delete, Postgres accepts the alias, and SQLite is
	// unchanged from what it used to do.
	res := db.Exec(`DELETE FROM ` + table + `
		WHERE id NOT IN (
			SELECT id FROM (
				SELECT MIN(id) AS id FROM ` + table + ` GROUP BY edge_id, ts
			) AS survivors
		)`)
	if res.Error != nil {
		return fmt.Errorf("collapse duplicate host_metrics_raw rows: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		// Loud on purpose and without a logger: a number here means the
		// deployment was double-counting, and the operator should be able
		// to find that sentence in the startup log rather than infer it.
		fmt.Printf("metric store: collapsed %d duplicate host_metrics_raw row(s) at (edge_id, ts)\n", res.RowsAffected)
	}
	return nil
}

// dropLegacyRawIndex removes the superseded non-unique index, tolerating both
// "it is not there" and "this dialect has no such index type" rather than
// making an upgrade fail over a leftover name.
func dropLegacyRawIndex(db *gorm.DB) error {
	m := db.Migrator()
	if !m.HasIndex(&model.HostMetric{}, legacyRawIndex) {
		return nil
	}
	if err := m.DropIndex(&model.HostMetric{}, legacyRawIndex); err != nil {
		return fmt.Errorf("drop %s: %w", legacyRawIndex, err)
	}
	return nil
}
