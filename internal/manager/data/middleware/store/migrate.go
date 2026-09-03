// Package sqlite is the GORM-backed persistence layer for the
// Middleware Adapter feature (路径 A 阶段 1 任务 1.3)。
//
// Three tables: middleware_resources / middleware_resource_conn_specs /
// middleware_resource_health. Naming follows existing convention
// (manager/data/setting/store, manager/data/alert/store, ...): the
// package is "store" but the AutoMigrate call is dialect-agnostic.
package store

import (
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/middleware"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/dbx"
)

// Migrate registers the middleware_resources,
// middleware_resource_conn_specs, and middleware_resource_health tables
// with GORM's AutoMigrate. Composed from cmd/opskeeper via dbx.RunMigrations
// like the other BC migrations.
func Migrate(db *gorm.DB) error {
	if dbx.NeedsDeleteMarkerMigration(db, model.MiddlewareResource{}.TableName()) {
		if err := dbx.DropIndexes(db, &model.MiddlewareResource{}, "idx_tenant_type_name"); err != nil {
			return err
		}
	}
	if err := db.AutoMigrate(
		&model.MiddlewareResource{},
		&model.MiddlewareConnSpec{},
		&model.MiddlewareResourceHealth{},
	); err != nil {
		return err
	}
	if err := dbx.BackfillDeleteMarker(db, model.MiddlewareResource{}.TableName()); err != nil {
		return err
	}
	return dbx.BackfillDeleteMarker(db, model.MiddlewareConnSpec{}.TableName())
}
