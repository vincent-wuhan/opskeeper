// Package store 是 HITL 提案与 ResumeToken 持久化的 GORM 实现。
//
// 同时实现统一 Proposal 模型（合并旧 approvals + chat_mutating_proposals）与
// 数据迁移期的双写入口。
package store

import (
	"errors"
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

// Migrate AutoMigrate 新增的 proposal / proposal_state 表（additive）。
//
// 不触碰旧 approvals / chat_mutating_proposals 表 — 双写期（7 天）由
// MigrateLegacy 函数搬运数据，详见 migrate_data.go。
func Migrate(db *gorm.DB) error {
	if db == nil {
		return errors.New("hitl/store: nil db")
	}
	return db.AutoMigrate(
		&model.Proposal{},
		&model.ProposalState{},
	)
}
