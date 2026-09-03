// Package store 是 conversational-diagnosis（chatdiagnose）的数据层。
// 包含两张表：
//   - diagnostic_conversation：会话事实源（DB 单一源 + rehydration）
//   - diagnostic_turn：append-only turn 流（seq 单调 + LLM context snapshot）
//   - incident_pattern：KB-first 检索表（feature kb_first 开启后用）
//
// 路径 A 约束：
//   - biz/chatdiagnose 不得直接 import 本包；通过
//     chatdiagnose.ConversationRepo narrow interface 交互。
//   - 单调追加字段，不删除/重命名现有列（AGENTS.md §"数据存储"）。
package store

import (
	"gorm.io/gorm"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// Migrate 注册 chatdiagnose 三表（diagnostic_conversation /
// diagnostic_turn / incident_pattern）的 GORM schema。
func Migrate(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	return db.AutoMigrate(
		&chatdiagnosemodel.Conversation{},
		&chatdiagnosemodel.Turn{},
		&chatdiagnosemodel.IncidentPattern{},
	)
}
