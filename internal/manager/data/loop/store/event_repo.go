// Package store — event_repo.go
//
// *sql.DB 适配的 loop.EventRepo / loop.ContractRepo 实现。
// 设计要点：
//   - 通过 gorm.DB（GORM 内部持有 *sql.DB 池）做 IO，biz 层无需
//     直接持有 *sql.DB。
//   - AppendEvent 在 (idempotency_key) UNIQUE 冲突时回读并把
//     已存在行的 ID 回填到入参 e.ID（与 InMemoryEventRepo 语义一致）
//     —— orchestrator 的 retry/replay 路径依赖该语义。
//   - ReadEvents 按 created_at ASC 排序返回，满足 orchestrator
//     recoverFromEvents 的有序输入假设。
//   - 所有 IO 函数首个参数为 context.Context（AGENTS.md 红线）。
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// EventRepoDB 是 loop.EventRepo 的 *sql.DB-backed 实现（GORM）。
//
// 构造：
//
//	repo := store.NewEventRepoDB(db)
//
// 之后可直接传入 loop.OrchestratorDeps.EventRepo。
type EventRepoDB struct {
	db *gorm.DB
}

// NewEventRepoDB 构造 EventRepoDB。db 不得为 nil。
func NewEventRepoDB(db *gorm.DB) *EventRepoDB {
	return &EventRepoDB{db: db}
}

// Compile-time interface satisfaction check。
var _ loop.EventRepo = (*EventRepoDB)(nil)

// AppendEvent 实现 loop.EventRepo。
//
// 行为：
//   - 直接 INSERT；idempotency_key 冲突（MySQL 1062 / Postgres 23505）
//     时回读已存在行并把 ID 回填到 e，返回 nil（与 InMemoryEventRepo
//     一致 —— 满足 orchestrator 的 replay 语义）。
//   - 其他错误原样返回。
func (r *EventRepoDB) AppendEvent(ctx context.Context, e *loopmodel.Event) error {
	if e == nil {
		return nil
	}
	cp := *e
	if cp.ID == 0 {
		cp.ID = 0 // ensure gorm auto-fills via AutoIncrement
	}
	if len(cp.IdempotencyKey) > loopmodel.IdempotencyKeyMaxLen {
		digest := sha256.Sum256([]byte(cp.IdempotencyKey))
		cp.IdempotencyKey = hex.EncodeToString(digest[:loopmodel.IdempotencyKeyMaxLen/2])
	}
	if strings.TrimSpace(cp.Payload) == "" {
		cp.Payload = "{}"
	}
	if err := r.db.WithContext(ctx).Create(&cp).Error; err != nil {
		if !isDuplicateKey(err) {
			return fmt.Errorf("loop event append: %w", err)
		}
		// 冲突：回读已存在行
		var existing loopmodel.Event
		if qerr := r.db.WithContext(ctx).
			Where("idempotency_key = ?", cp.IdempotencyKey).
			First(&existing).Error; qerr != nil {
			return fmt.Errorf("loop event append (dup, lookup): %w", qerr)
		}
		e.ID = existing.ID
		return nil
	}
	e.ID = cp.ID
	return nil
}

// ReadEvents 实现 loop.EventRepo。
//
// 按 created_at ASC 返回事件列表。tenantID + incidentID 复合过滤
// 满足多租户隔离 + 单事件流边界。
func (r *EventRepoDB) ReadEvents(ctx context.Context, tenantID, incidentID string) ([]loopmodel.Event, error) {
	if tenantID == "" {
		return nil, errors.New("loop event repo: tenantID required")
	}
	if incidentID == "" {
		return nil, errors.New("loop event repo: incidentID required")
	}
	var rows []loopmodel.Event
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND incident_id = ?", tenantID, incidentID).
		Order("created_at ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("loop event read: %w", err)
	}
	return rows, nil
}

// ContractRepoDB 是 loop.ContractRepo 的 *sql.DB-backed 实现。
//
// 读路径：返回 (incident_id, phase, type, schema_ver) 匹配的最近
// 一行（按 created_at DESC 选 1 条）。
//
// 写路径：固定走 INSERT。同一 (incident, phase, type, schema) 多次
// 写入产生多条历史行；读路径取最新，与 in-memory 适配器"most
// recent wins"语义一致。
type ContractRepoDB struct {
	db *gorm.DB
}

// NewContractRepoDB 构造 ContractRepoDB。db 不得为 nil。
func NewContractRepoDB(db *gorm.DB) *ContractRepoDB {
	return &ContractRepoDB{db: db}
}

var _ loop.ContractRepo = (*ContractRepoDB)(nil)

// WriteContract 实现 loop.ContractRepo。
func (r *ContractRepoDB) WriteContract(ctx context.Context, c *loopmodel.Contract) error {
	if c == nil {
		return nil
	}
	cp := *c
	if cp.TenantID == "" {
		return errors.New("loop contract write: tenantID required")
	}
	if cp.StorageBackend == "" {
		cp.StorageBackend = loopmodel.StorageBackendDB
	}
	if err := r.db.WithContext(ctx).Create(&cp).Error; err != nil {
		return fmt.Errorf("loop contract write: %w", err)
	}
	c.ID = cp.ID
	return nil
}

// ReadContract 实现 loop.ContractRepo。
func (r *ContractRepoDB) ReadContract(ctx context.Context, tenantID, incidentID string, phase loop.Phase, contractType string) (*loopmodel.Contract, error) {
	if tenantID == "" {
		return nil, errors.New("loop contract repo: tenantID required")
	}
	if incidentID == "" {
		return nil, errors.New("loop contract repo: incidentID required")
	}
	var row loopmodel.Contract
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND incident_id = ? AND phase = ? AND type = ?", tenantID, incidentID, string(phase), contractType).
		Order("created_at DESC").
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("loop contract read: %w", err)
	}
	return &row, nil
}

// ReadContractByID 按主键查 1 行；不存在返回 (nil, nil)。
// 供 investigated worker 的 CorrelatedGroupLoaderAdapter 通过
// loop.CorrelatedGroupReader narrow interface 调用（不直接暴露 model）。
//
// 多租户安全：必须传 tenantID；WHERE 子句加 tenant_id = ? AND tenant_id != ”，
// 阻止跨租户读 —— 之前 ReadContractByID 不带 tenant 过滤，是 ⑥ 号差距。
// 老的单参 ReadContractByIDByIDLegacy 仅供迁移期回滚；新代码必须用本方法。
func (r *ContractRepoDB) ReadContractByID(ctx context.Context, tenantID string, id int64) (*loopmodel.Contract, error) {
	if tenantID == "" {
		return nil, errors.New("loop contract read by id: tenantID required")
	}
	if id <= 0 {
		return nil, fmt.Errorf("loop contract read by id: id must be > 0")
	}
	var row loopmodel.Contract
	err := r.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ?", id, tenantID).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("loop contract read by id: %w", err)
	}
	return &row, nil
}

// isDuplicateKey 是 GORM 错误 → UNIQUE-violation 的轻量判定。
//
// 支持：
//   - MySQL: 错误消息含 "Error 1062"
//   - Postgres: gorm.ErrDuplicatedKey
//   - SQLite: gorm.ErrDuplicatedKey
//
// 故意不依赖具体 driver 私有类型，便于跨 driver 测试。
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	// gorm 在某些 driver（MySQL）下未把 driver 错误 wrap 成
	// ErrDuplicatedKey，回退字符串判定。
	msg := err.Error()
	return contains(msg, "Error 1062") || contains(msg, "UNIQUE constraint") || contains(msg, "duplicate key")
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	// 避免 strings.Contains 的 import（保持该文件精简）
	n, m := len(s), len(sub)
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}
