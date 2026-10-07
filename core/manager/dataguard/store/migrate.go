package store

import (
	"errors"

	"gorm.io/gorm"
)

// Migrate AutoMigrate data_sensitivity_label 表（additive — 新表）。
//
// 不触碰其他业务表；Phase 1 主键为 (resource_type, resource_id)。
func Migrate(db *gorm.DB) error {
	if db == nil {
		return errors.New("dataguard/store: nil db")
	}
	return db.AutoMigrate(&DataSensitivityLabel{})
}
