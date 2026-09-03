package store

import (
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/imbridge"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/dbx"
)

// Migrate creates the IM bridge tables — im_apps for platform bot
// credentials, im_threads for the IM-conversation → opskeeper-session
// mapping. cmd/opskeeper wires this through dbx.RunMigrations at boot.
func Migrate(db *gorm.DB) error {
	if dbx.NeedsDeleteMarkerMigration(db, model.ImApp{}.TableName()) {
		if err := dbx.DropIndexes(db, &model.ImApp{}, "uk_provider_app_id"); err != nil {
			return err
		}
	}
	if err := db.AutoMigrate(&model.ImApp{}, &model.ImThread{}); err != nil {
		return err
	}
	return dbx.BackfillDeleteMarker(db, model.ImApp{}.TableName())
}
