// Package store — recovery_state_store_db.go
//
// DB-backed RecoveryStateStore：复用 loop_state.retry_count 列（0001
// migration 本就为此设计）。同一 struct 通过 Go 结构化类型同时满足：
//   - loop.RecoveryStateStore（internal/manager/biz/loop/recovery.go:365）
//   - aiops/tools.RecoveryStateStore（verify_recovery_basetool.go:208）
//
// 两个接口签名完全同形（Get/Increment/Reset），不需要 proxy。data 层不
// import 这两个 biz 接口（会成环）；compile-time check 在 main.go。
//
// 设计要点：
//   - 只写 retry_count + updated_at，不碰 current_phase / last_event_id
//     （避免与 orchestrator.State 写竞争）
//   - Increment 用 GORM clause.OnConflict 跨方言原子 upsert（复用
//     chatruntime-kb-implementation §B7 模式，已验证 MySQL/SQLite 兼容）
//   - Get 未记录返回 (0, nil)（接口 spec："未记录返回 0"）
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// RecoveryStateStoreDB 是 RecoveryStateStore 的 DB-backed 实现。
// 复用 loop_state 表的 retry_count 列；不新增表 / migration。
type RecoveryStateStoreDB struct {
	db *gorm.DB
}

// NewRecoveryStateStoreDB 构造。db 不得为 nil。
func NewRecoveryStateStoreDB(db *gorm.DB) *RecoveryStateStoreDB {
	if db == nil {
		panic("loop store: NewRecoveryStateStoreDB: db is nil")
	}
	return &RecoveryStateStoreDB{db: db}
}

// Get 返回 incident_id 的 retry_count；未记录返回 (0, nil)。
func (s *RecoveryStateStoreDB) Get(ctx context.Context, incidentID string) (int, error) {
	if incidentID == "" {
		return 0, fmt.Errorf("loop recovery state: Get: incidentID required")
	}

	var state loopmodel.State
	err := s.db.WithContext(ctx).
		Select("retry_count").
		Where("incident_id = ?", incidentID).
		First(&state).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("loop recovery state get: %w", err)
	}
	return state.RetryCount, nil
}

// Increment 原子地把 retry_count +1 并返回新值。行不存在时插入 (1)。
// 使用 GORM clause.OnConflict 保证跨方言（MySQL / SQLite / Postgres）。
func (s *RecoveryStateStoreDB) Increment(ctx context.Context, incidentID string) (int, error) {
	if incidentID == "" {
		return 0, fmt.Errorf("loop recovery state: Increment: incidentID required")
	}

	now := time.Now().UTC()
	newState := loopmodel.State{
		IncidentID: incidentID,
		RetryCount: 1,
		UpdatedAt:  now,
	}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "incident_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"retry_count": gorm.Expr("retry_count + 1"),
			"updated_at":  now,
		}),
	}).Create(&newState).Error
	if err != nil {
		return 0, fmt.Errorf("loop recovery state increment: %w", err)
	}

	// 读回最新值（upsert 后 count 已是最新）
	return s.Get(ctx, incidentID)
}

// Reset 把 retry_count 清零。行不存在时插入 (0)（幂等）。
func (s *RecoveryStateStoreDB) Reset(ctx context.Context, incidentID string) error {
	if incidentID == "" {
		return fmt.Errorf("loop recovery state: Reset: incidentID required")
	}

	now := time.Now().UTC()
	newState := loopmodel.State{
		IncidentID: incidentID,
		RetryCount: 0,
		UpdatedAt:  now,
	}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "incident_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"retry_count": 0,
			"updated_at":  now,
		}),
	}).Create(&newState).Error
	if err != nil {
		return fmt.Errorf("loop recovery state reset: %w", err)
	}
	return nil
}
