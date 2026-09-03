// migrate_git_artifact.go — git-artifact + runtime_symbol_links migration
package store

import (
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/middleware"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/dbx"
)

// MigrateGitArtifact registers the git_artifacts and
// runtime_symbol_links tables. Called separately from Migrate()
// so the git-artifact feature is opt-in during staged rollout.
func MigrateGitArtifact(db *gorm.DB) error {
	if dbx.NeedsDeleteMarkerMigration(db, model.GitArtifact{}.TableName()) {
		if err := dbx.DropIndexes(db, &model.GitArtifact{}, "idx_tenant_public"); err != nil {
			return err
		}
	}
	if err := db.AutoMigrate(
		&model.GitArtifact{},
		&model.RuntimeSymbolLink{},
	); err != nil {
		return err
	}
	if err := dbx.BackfillDeleteMarker(db, model.GitArtifact{}.TableName()); err != nil {
		return err
	}
	return dbx.BackfillDeleteMarker(db, model.RuntimeSymbolLink{}.TableName())
}
