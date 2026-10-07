// Package store is the GORM-backed persistence layer for monitor_panels.
// Mirrors the conventions of core/manager/data/setting/store —
// dialect-agnostic AutoMigrate composed from cmd/opskeeper via
// dbx.RunMigrations.
package store

import (
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/core/domains/model/monitor"
)

// Migrate registers the monitor_panels table.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&model.Panel{})
}
