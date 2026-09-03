// Package store 是 closed-loop orchestrator 的数据层（loop_event_log /
// loop_contract / loop_state 三表）。GORM AutoMigrate 实现；
// 配套 narrow *sql.DB 适配器供 biz 层用（无 ORM 依赖路径）。
//
// 路径 A 约束：
//   - biz/loop 不得 import data/loop（本包不向 biz 暴露任何 model
//     类型）；通过 loop.EventRepo / loop.ContractRepo narrow
//     interface 走，DB 字段映射在包内完成。
//   - 单调追加字段，不删除/重命名现有列。
package store

import (
	"gorm.io/gorm"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// Migrate 注册 closed-loop 三表（loop_event_log / loop_contract /
// loop_state）的 GORM schema。additive：AutoMigrate 只加新列，
// 不删/不改旧列。
//
// 启动时由 cmd/opskeeper/main.go 注入到全局 migration 列表。
func Migrate(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	return db.AutoMigrate(
		&loopmodel.Event{},
		&loopmodel.Contract{},
		&loopmodel.State{},
	)
}
