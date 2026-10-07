// Package store is the GORM-backed persistence layer for HLD-010
// audit_logs. Migration is composed from cmd/opskeeper via dbx.RunMigrations.
package store

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	model "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
)

// Migrate registers the audit_logs table and the chain head.
//
// The head row is seeded rather than left to the first write. A lazily
// seeded head is created inside a transaction that may lose a
// compare-and-swap race, and the retry path then has to distinguish "I
// lost the race" from "somebody dropped the table" — a distinction that
// only costs a startup migration to avoid.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&model.Log{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.ChainHead{}); err != nil {
		return err
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&model.ChainHead{ID: model.ChainHeadID, Seq: 0, Hash: ""}).Error
}
