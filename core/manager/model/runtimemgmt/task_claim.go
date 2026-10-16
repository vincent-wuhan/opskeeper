package runtimemgmt

import "time"

// Claim 是运行时认领的持久化记录。每个任务最多一条活动认领；被替换
// 认领的历史由后续切片的 RecoveryLog 表保存。Claim.Generation 在编排器
// 每次移交给新认领者时递增；运行时在检查点、进度与结果调用中校验，
//
// 规格场景“检查点后 Worker 重启”依赖 Generation 与 ClaimedBy 语义：
// 从检查点恢复的 Worker 必须观察到与检查点一致的配对，否则得到
// ErrClaimLost，而不是重复副作用。
type Claim struct {
	// TaskID 是所属持久化任务；每个任务一条记录。
	TaskID string `gorm:"primaryKey;size:64"`

	TenantID string `gorm:"index;size:64;not null"`

	// Generation 在新认领替换旧认领时递增，例如租约过期、人工接管、
	// 节点重启后备份提升。运行时必须在检查点、进度、结果调用中回传
	// 认领时观察到的值；不一致则由编排器拒绝。
	Generation int64 `gorm:"not null;default:0"`

	// ClaimedBy 是持有当前代际的运行时身份，例如 runtime:edge:abc、
	// runtime:worker:harness:42；建立索引便于 Dashboard 按运行时下钻。
	ClaimedBy string `gorm:"size:128;not null;index"`

	// ClaimedAt 是运行时接受认领的墙钟时间，用于区分第 N 代开始时间与
	// claimed_at + prepare_lease 得出的准备租约期限。
	ClaimedAt time.Time `gorm:"index;not null"`

	// LeasedUntil 是滚动存活租约期限。运行时必须在此之前续约或写检查点；
	// 否则编排器可安全使认领过期并恢复。
	LeasedUntil time.Time `gorm:"index;not null"`

	// OwnerLeaseID 是最近一次租约延长令牌。编排器每次推进 LeasedUntil
	// 都写新令牌，运行时在下一检查点回传；首次续约前为空。
	OwnerLeaseID string `gorm:"size:64;not null;default:''"`
}

// TableName 固定表名。迁移必须保持不变；编排器认领与租约状态机按
// task_id 读取。
func (Claim) TableName() string { return "runtimemgmt_claims" }

// IsStale 判断 now 时租约是否超过期限。陈旧租约可由编排器清扫器安全
// 推进并恢复。
func (c Claim) IsStale(now time.Time) bool {
	if c.LeasedUntil.IsZero() {
		return true
	}
	return !now.Before(c.LeasedUntil)
}

// BelongsToRuntime 判断到达的检查点、进度或结果调用回传的 candidate
// 是否匹配当前认领。运行时身份必须精确相等；重新注册或恢复应推进新
// 代际，而不是复用当前认领。
func (c Claim) BelongsToRuntime(candidate string) bool {
	if c.ClaimedBy == "" || candidate == "" {
		return false
	}
	return c.ClaimedBy == candidate
}

// AdvanceTo 是恢复时辅助函数，返回 Generation 加一、ClaimedBy 为
// nextRuntime、ClaimedAt 为 now、LeasedUntil 由调用方重置、OwnerLeaseID
// 清空的新 Claim。旧记录进入后续切片新增的恢复日志；这里只计算替换记录。
//
// 编排器必须在同一事务内写恢复日志与 Claim。
func (c Claim) AdvanceTo(nextRuntime string, now time.Time, newLeasedUntil time.Time) Claim {
	return Claim{
		TaskID:       c.TaskID,
		TenantID:     c.TenantID,
		Generation:   c.Generation + 1,
		ClaimedBy:    nextRuntime,
		ClaimedAt:    now.UTC(),
		LeasedUntil:  newLeasedUntil.UTC(),
		OwnerLeaseID: "",
	}
}
