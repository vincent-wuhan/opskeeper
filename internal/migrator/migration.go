// Package migrator 提供 opskeeper 数据库 schema 的版本化迁移运行时。
//
// 设计目标（Task 3.4 migration-runtime 重构）：
//  1. 版本化迁移：每次迁移带 Version + Description，避免 AutoMigrate 的隐式行为
//  2. 幂等：相同版本多次执行不报错（基于 history 表）
//  3. 回滚：Down() 支持反向迁移
//  4. 锁：执行期间持 advisory lock，避免并发迁移冲突
//  5. 干跑：DryRun=true 时仅报告不实际写入
//
// 与 dbx.Migrator 的关系：
//   - dbx.Migrator 是 GORM AutoMigrate 的薄封装，用于早期开发期
//   - migrator.Migration 是生产级版本化迁移，逐步替换 dbx.AutoMigrate
//   - 兼容：现有 dbx.Migrator 仍可作为"v0 迁移"在新体系下运行一次
package migrator

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Migration 表示一次可执行的 schema 迁移。
//
// Version 必须单调递增（推荐时间戳或语义版本）。
// Description 一句话说明迁移目的。
// Up 执行迁移；Down 必须能完全反向（v1 暂时允许 Up-only）。
type Migration interface {
	// Version 唯一标识（建议时间戳 14 位 YYYYMMDDHHMMSS 或 semver）。
	Version() string
	// Description 一句话描述。
	Description() string
	// Up 执行迁移（应用变更）。
	Up(ctx context.Context, db *gorm.DB) error
	// Down 回滚迁移（撤销变更）。如果不可回滚（v1），返回 ErrIrreversible。
	Down(ctx context.Context, db *gorm.DB) error
}

// ErrIrreversible 标记不可回滚的迁移。
var ErrIrreversible = fmt.Errorf("migration is irreversible")

// MigrationRecord 持久化在 schema_migrations 表中的记录。
type MigrationRecord struct {
	Version     string    `gorm:"primaryKey;size:32" json:"version"`
	Description string    `gorm:"size:255" json:"description"`
	AppliedAt   time.Time `gorm:"index" json:"applied_at"`
	// DurationMs 迁移耗时（毫秒）。
	DurationMs int64 `json:"duration_ms"`
	// Status applied / rolled_back / failed。
	Status string `gorm:"size:16;index" json:"status"`
	// Checksum 用于校验历史记录未被篡改（v2 计划）。
	// v1 暂不启用。
}

// TableName 显式指定表名。
func (MigrationRecord) TableName() string {
	return "schema_migrations"
}

// Registry 全局迁移注册表。
// 注册顺序无所谓；执行时按 Version 排序。
type Registry struct {
	mu    sync.RWMutex
	items map[string]Migration
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{items: make(map[string]Migration)}
}

// Register 注册一个迁移。
// 同 Version 重复注册时，后者覆盖前者（便于开发期迭代）。
func (r *Registry) Register(m Migration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[m.Version()] = m
}

// MustRegister 同 Register，panic on duplicate（非 nil）。
func (r *Registry) MustRegister(m Migration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.items[m.Version()]; exists {
		panic(fmt.Sprintf("migrator: duplicate version %s", m.Version()))
	}
	r.items[m.Version()] = m
}

// All 返回全部已注册迁移（按 Version 排序）。
func (r *Registry) All() []Migration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Migration, 0, len(r.items))
	for _, m := range r.items {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Version() < out[j].Version()
	})
	return out
}

// Get 按 Version 查询迁移。
func (r *Registry) Get(version string) (Migration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.items[version]
	return m, ok
}

// Count 返回注册数。
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.items)
}
