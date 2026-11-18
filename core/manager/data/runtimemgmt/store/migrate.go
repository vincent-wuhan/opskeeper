// Package store 保存运行时管理域的持久化层。首切片只有一张表：
// runtimemgmt_runtimes（§1 Runtime Inventory and Readback）。
package store

import (
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// Migrate 注册 runtime 注册表模型。纯增量：只新建 runtimemgmt_runtimes，
// 不触碰既有表族；AutoMigrate additive 不删列，回滚即 drop 该表。
//
// 全新表无历史索引需清理（同 ChangeEventRow 先例），跳过
// NeedsDeleteMarkerMigration；模型不带软删标记列，BackfillDeleteMarker
// 在 HasColumn 检查处自然短路，无需调用。
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&model.RuntimeRecord{})
}
