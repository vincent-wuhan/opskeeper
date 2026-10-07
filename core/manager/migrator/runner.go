package migrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
)

// Direction 迁移方向。
type Direction string

const (
	// Up 应用未执行的迁移。
	Up Direction = "up"
	// Down 回滚已应用的迁移。
	Down Direction = "down"
)

// RunOptions 控制单次迁移执行。
type RunOptions struct {
	// Direction Up / Down。
	Direction Direction
	// Steps 限制执行步数；0 = 全部（Up）/ 全部回滚（Down）。
	Steps int
	// DryRun true 时仅记录 + 报告，不实际写入。
	DryRun bool
	// Logger 可选 slog logger；nil 用 slog.Default()。
	Logger *slog.Logger
	// LockKey advisory lock 键（避免并发迁移）；0 = 不加锁。
	LockKey int64
}

// RunResult 单次执行的统计。
type RunResult struct {
	Applied   []string          // 已应用（或已回滚）的 version 列表
	Skipped   []string          // 跳过的 version（已存在/不存在）
	Failed    []string          // 失败的 version
	Records   []MigrationRecord // 写入 history 的记录（dry-run 时也是预览）
	StartedAt time.Time
	EndedAt   time.Time
}

// Runner 迁移执行引擎。
type Runner struct {
	db       *gorm.DB
	registry *Registry
}

// NewRunner 创建执行器。
func NewRunner(db *gorm.DB, registry *Registry) *Runner {
	return &Runner{db: db, registry: registry}
}

// EnsureHistoryTable 确保 schema_migrations 表存在。
// v1 简化：用 AutoMigrate 创建（idempotent）。
// 后续可改为固定 SQL 避免依赖 GORM。
func (r *Runner) EnsureHistoryTable(ctx context.Context) error {
	return r.db.WithContext(ctx).AutoMigrate(&MigrationRecord{})
}

// Status 返回当前迁移状态：applied / pending 分类。
type Status struct {
	Total   int
	Applied []MigrationRecord
	Pending []Migration
}

// GetStatus 读取历史并对比注册表，返回应用与待应用分类。
func (r *Runner) GetStatus(ctx context.Context) (*Status, error) {
	if err := r.EnsureHistoryTable(ctx); err != nil {
		return nil, fmt.Errorf("创建 history 表失败: %w", err)
	}

	var applied []MigrationRecord
	if err := r.db.WithContext(ctx).
		Where("status = ?", "applied").
		Order("version ASC").
		Find(&applied).Error; err != nil {
		return nil, fmt.Errorf("读 history 失败: %w", err)
	}

	appliedSet := make(map[string]bool, len(applied))
	for _, a := range applied {
		appliedSet[a.Version] = true
	}

	var pending []Migration
	for _, m := range r.registry.All() {
		if !appliedSet[m.Version()] {
			pending = append(pending, m)
		}
	}
	return &Status{
		Total:   r.registry.Count(),
		Applied: applied,
		Pending: pending,
	}, nil
}

// Run 执行迁移。
func (r *Runner) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	if opts.Direction != Up && opts.Direction != Down {
		return nil, fmt.Errorf("无效 direction: %q", opts.Direction)
	}

	if err := r.EnsureHistoryTable(ctx); err != nil {
		return nil, err
	}

	result := &RunResult{
		StartedAt: time.Now().UTC(),
	}

	if opts.LockKey > 0 {
		if err := r.acquireLock(ctx, opts.LockKey); err != nil {
			return nil, fmt.Errorf("获取 advisory lock 失败: %w", err)
		}
		defer r.releaseLock(ctx, opts.LockKey)
	}

	if opts.Direction == Up {
		return result, r.runUp(ctx, opts, log, result)
	}
	return result, r.runDown(ctx, opts, log, result)
}

func (r *Runner) runUp(
	ctx context.Context,
	opts RunOptions,
	log *slog.Logger,
	result *RunResult,
) error {
	status, err := r.GetStatus(ctx)
	if err != nil {
		return err
	}

	pending := status.Pending
	if opts.Steps > 0 && len(pending) > opts.Steps {
		pending = pending[:opts.Steps]
	}

	for _, m := range pending {
		log.Info("migrator: applying",
			"version", m.Version(),
			"description", m.Description(),
			"dry_run", opts.DryRun)
		start := time.Now()
		err := r.executeInTx(ctx, func(tx *gorm.DB) error {
			if opts.DryRun {
				// 干跑：仅模拟，不写 history（DB 状态完全不变）
				return nil
			}
			if err := m.Up(ctx, tx); err != nil {
				return err
			}
			return tx.Create(&MigrationRecord{
				Version:     m.Version(),
				Description: m.Description(),
				AppliedAt:   time.Now().UTC(),
				DurationMs:  time.Since(start).Milliseconds(),
				Status:      "applied",
			}).Error
		})
		duration := time.Since(start)
		if err != nil {
			log.Error("migrator: failed",
				"version", m.Version(),
				"err", err,
				"duration", duration.String())
			result.Failed = append(result.Failed, m.Version())
			result.Records = append(result.Records, MigrationRecord{
				Version:     m.Version(),
				Description: m.Description(),
				AppliedAt:   time.Now().UTC(),
				DurationMs:  duration.Milliseconds(),
				Status:      "failed",
			})
			return fmt.Errorf("migration %s 失败: %w", m.Version(), err)
		}
		log.Info("migrator: applied",
			"version", m.Version(),
			"duration", duration.String())
		result.Applied = append(result.Applied, m.Version())
		result.Records = append(result.Records, MigrationRecord{
			Version:     m.Version(),
			Description: m.Description(),
			AppliedAt:   time.Now().UTC(),
			DurationMs:  duration.Milliseconds(),
			Status:      "applied",
		})
	}

	result.EndedAt = time.Now().UTC()
	return nil
}

func (r *Runner) runDown(
	ctx context.Context,
	opts RunOptions,
	log *slog.Logger,
	result *RunResult,
) error {
	status, err := r.GetStatus(ctx)
	if err != nil {
		return err
	}

	// 回滚：按 version 倒序
	applied := status.Applied
	for i := len(applied) - 1; i >= 0; i-- {
		if opts.Steps > 0 && len(result.Applied) >= opts.Steps {
			break
		}
		rec := applied[i]
		m, ok := r.registry.Get(rec.Version)
		if !ok {
			log.Warn("migrator: history record has no matching migration",
				"version", rec.Version)
			result.Skipped = append(result.Skipped, rec.Version)
			continue
		}

		log.Info("migrator: rolling back",
			"version", m.Version(),
			"description", m.Description(),
			"dry_run", opts.DryRun)
		start := time.Now()
		err := r.executeInTx(ctx, func(tx *gorm.DB) error {
			if opts.DryRun {
				return nil
			}
			if err := m.Down(ctx, tx); err != nil {
				if errors.Is(err, ErrIrreversible) {
					return fmt.Errorf("migration %s 不可回滚: %w", m.Version(), err)
				}
				return err
			}
			// 标记 history 为 rolled_back（保留审计）
			return tx.Model(&MigrationRecord{}).
				Where("version = ?", rec.Version).
				Updates(map[string]any{
					"status":      "rolled_back",
					"applied_at":  time.Now().UTC(),
					"duration_ms": time.Since(start).Milliseconds(),
				}).Error
		})
		duration := time.Since(start)
		if err != nil {
			log.Error("migrator: rollback failed",
				"version", m.Version(),
				"err", err,
				"duration", duration.String())
			result.Failed = append(result.Failed, m.Version())
			return fmt.Errorf("rollback %s 失败: %w", m.Version(), err)
		}
		log.Info("migrator: rolled back",
			"version", m.Version(),
			"duration", duration.String())
		result.Applied = append(result.Applied, m.Version())
	}

	result.EndedAt = time.Now().UTC()
	return nil
}

// executeInTx 在事务中执行回调；失败回滚。
func (r *Runner) executeInTx(ctx context.Context, fn func(*gorm.DB) error) error {
	return r.db.WithContext(ctx).Transaction(fn)
}

// acquireLock 获取 MySQL GET_LOCK 或 PG advisory lock。
//
// v1 实现：MySQL GET_LOCK(key, timeout)。
// v2 计划：支持 PG advisory lock + Redis SETNX 分布式锁。
func (r *Runner) acquireLock(ctx context.Context, key int64) error {
	var result int
	if err := r.db.WithContext(ctx).Raw("SELECT GET_LOCK(?, 10)", key).Scan(&result).Error; err != nil {
		return err
	}
	if result != 1 {
		return fmt.Errorf("GET_LOCK(%d) 返回 %d（期望 1）", key, result)
	}
	return nil
}

func (r *Runner) releaseLock(ctx context.Context, key int64) error {
	var result int
	if err := r.db.WithContext(ctx).Raw("SELECT RELEASE_LOCK(?)", key).Scan(&result).Error; err != nil {
		return err
	}
	if result != 1 {
		// 不是 fatal，记 log 即可
		slog.Default().Warn("migrator: RELEASE_LOCK 失败", "key", key, "result", result)
	}
	return nil
}
