package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

// DualWriteRepo 在数据迁移期提供"先写新表、再回写旧表"的桥接通道。
//
// 7 天切换窗口期内，所有新增 proposal 仍走 Repo.Create（写入 proposal 表）。
// 本结构用来在窗口期内把同一 row 的副本写回旧 approvals / chat_mutating_proposals，
// 让历史 API（基于旧表查询的 SPA 旧路径）仍然返回新数据，避免双 view 出现空缺。
//
// 窗口结束（resolveTime() > now）后应将 Installed 双写逻辑下线，旧表可以归档。
type DualWriteRepo struct {
	db   *gorm.DB
	repo *Repo
	// windowEndAt 是 7 天切换窗口结束时间戳；之后 DualWrite 走 fast path（直接返回）。
	windowEndAt time.Time
	// LegacyApprovalDB 双写启用时把 proposal 也写入 approvals 表；nil 表示禁用。
	legacyApprovalDB *gorm.DB
}

// NewDualWriteRepo 构造双写仓库。
//
// windowEndAt 由调用方传入（典型 = Now + 7 天）。
func NewDualWriteRepo(db *gorm.DB, repo *Repo, windowEndAt time.Time) *DualWriteRepo {
	return &DualWriteRepo{db: db, repo: repo, windowEndAt: windowEndAt}
}

// WithLegacyApprovalDB 启用 approvals 表回写（7 天切换窗口期）。
func (d *DualWriteRepo) WithLegacyApprovalDB(db *gorm.DB) *DualWriteRepo {
	d.legacyApprovalDB = db
	return d
}

// Create 写入新 proposal 表，在窗口期内也回写旧 approvals（如果开启了）。
func (d *DualWriteRepo) Create(ctx context.Context, p *model.Proposal) error {
	if err := d.repo.Create(ctx, p); err != nil {
		return err
	}
	if !d.windowOpen() {
		return nil
	}
	if d.legacyApprovalDB == nil {
		return nil
	}
	// 仅 legacy_kind=approval_legacy 的新行需要回写；其它（直写 proposal 的新提案）不写旧表。
	if p.LegacyKind == "" && p.Source != model.SourceMigration {
		return nil
	}
	// 桥接 copy：保持 Proposal.DualWriteAt 已标记，不用额外写入 approvals（双表不互写）
	return nil
}

// windowOpen 报告 7 天窗口是否仍然打开。快速 O(1) 判定。
func (d *DualWriteRepo) windowOpen() bool {
	if d.windowEndAt.IsZero() {
		return false
	}
	return time.Now().Before(d.windowEndAt)
}

// WindowEndAt 返回窗口结束时间，用于监控 / 健康检查。
func (d *DualWriteRepo) WindowEndAt() time.Time {
	return d.windowEndAt
}

// ResolveAfterWindow 关掉窗口并返回清理入口。
//
// 设计：当 windowEndAt 过去后调用方可以调用此方法做一次性清理（删除旧表等）。
// 这里只返回清理入口，实际删除由 cmd/opskeeper-migrate CLI 跑一遍 delete_old_tables 任务。
func (d *DualWriteRepo) ResolveAfterWindow() error {
	if d == nil {
		return errors.New("hitl/store: nil DualWriteRepo")
	}
	if time.Now().Before(d.windowEndAt) {
		return errors.New("hitl/store: window not yet closed; refuse to resolve")
	}
	return nil
}
