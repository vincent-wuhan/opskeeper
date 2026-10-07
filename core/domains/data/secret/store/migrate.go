package store

import (
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/core/domains/model/secret"
)

// Migrate AutoMigrates the secrets table. Registered in cmd/opskeeper/main.go
// alongside the other data-package migrations.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&model.Secret{})
}
