package store

import (
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/approval"
)

// Migrate AutoMigrates the approvals table (additive — new table only).
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&model.Approval{})
}
