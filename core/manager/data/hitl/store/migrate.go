// Package store 是 HITL 提案与 ResumeToken 持久化的 GORM 实现。
//
// 这里只读写统一的 Proposal 模型。旧 approvals / chat_mutating_proposals 的
// 一次性迁移与 7 天双写窗口曾在本包内实现（migrate_data.go / dualwrite.go），
// 零生产调用方、窗口早已过期，连同它们对 aiops 域模型的一处跨域 import 一起
// 下线，见 docs/opskeeper2-architecture.md 决策 116。旧表数据在需要时用一次性
// 脚本回填，不要在这里重建迁移器。
package store

import (
	"errors"
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/hitl"
)

// Migrate AutoMigrate 新增的 proposal / proposal_state 表（additive）。
//
// 不触碰旧 approvals / chat_mutating_proposals 表：它们已不在写路径上，
// AutoMigrate 只做加法，永不删列（删列是迁移，不是本函数该做的事）。
func Migrate(db *gorm.DB) error {
	if db == nil {
		return errors.New("hitl/store: nil db")
	}
	return db.AutoMigrate(
		&model.Proposal{},
		&model.ProposalState{},
	)
}
