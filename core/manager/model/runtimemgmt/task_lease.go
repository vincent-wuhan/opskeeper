package runtimemgmt

import "time"

// Lease 保存单个认领的准备租约与存活租约期限。它独立于 Claim：
// 准备租约短期且有界，存活租约随检查点滚动；恢复流程也需要只延长其中一个
// 而不重写另一个。
//
// 字段语义来自 design.md「Durable task model」：
//   - 准备期限是 preparing 必须进入 running 或失败的时间，在认领时设置，
//     之后不重置。
//   - 存活期限由每次检查点、进度或租约续期调用向前滚动。
//
// 任一期限为零表示尚未开始、认领未分配；编排器必须视为不可操作，
// 而不是已过期。
type Lease struct {
	// TaskID 是所属持久化任务。以 (TaskID, Generation) 作为主键，
	// 恢复后的认领可自然保留历史租约。
	TaskID string `gorm:"primaryKey;size:64"`

	// Generation 对应 Claim.Generation，与 TaskID 组成自然主键。
	Generation int64 `gorm:"primaryKey"`

	TenantID string `gorm:"index;size:64;not null"`

	// PrepareDeadline 是 preparing 必须进入 running 或失败的时间；
	// 认领时设置且不重置。
	PrepareDeadline time.Time `gorm:"index"`

	// LivenessDeadline 是 running 必须写检查点否则可视为丢失的时间，
	// 由续约或检查点向前滚动。
	LivenessDeadline time.Time `gorm:"index"`

	// ExtendedAt 是最近一次成功续期或检查点的墙钟时间；仅用于诊断，
	// 不参与期限计算。
	ExtendedAt time.Time

	// Extensions 统计编排器滚动存活租约的次数，由任务类型重试预算约束
	// （见 AttemptRecord.MaxAttempts）。
	Extensions int `gorm:"not null;default:0"`
}

// TableName 固定表名。迁移必须保持不变；租约清扫器按
// (task_id, generation) 读取。
func (Lease) TableName() string { return "runtimemgmt_leases" }

// HasPrepare 判断准备期限是否已设置。租约在认领时即存在，但只有运行时
// 开始准备输入后该期限才有意义。
func (l Lease) HasPrepare() bool {
	return !l.PrepareDeadline.IsZero()
}

// HasLiveness 判断存活期限是否已设置。running 任务始终有值；
// queued、admitted 与终态任务返回 false。
func (l Lease) HasLiveness() bool {
	return !l.LivenessDeadline.IsZero()
}

// PrepareExpired 判断 now 时准备期限是否已过。零值返回 false；
// 调用方应先使用 HasPrepare 区分未开始与未过期。
func (l Lease) PrepareExpired(now time.Time) bool {
	if l.PrepareDeadline.IsZero() {
		return false
	}
	return !now.Before(l.PrepareDeadline)
}

// LivenessExpired 判断 now 时存活期限是否已过；零值语义与 PrepareExpired 相同。
func (l Lease) LivenessExpired(now time.Time) bool {
	if l.LivenessDeadline.IsZero() {
		return false
	}
	return !now.Before(l.LivenessDeadline)
}

// NewPrepareDeadline 是认领时准备期限的标准辅助函数，返回 claimTime + ttl。
// ttl <= 0 返回零值，允许只读任务跳过准备窗口。
func NewPrepareDeadline(claimTime time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 || claimTime.IsZero() {
		return time.Time{}
	}
	return claimTime.Add(ttl).UTC()
}

// NewLivenessDeadline 是首次进入 running 时的标准辅助函数，
// 返回 startTime + ttl；ttl <= 0 返回零值。
func NewLivenessDeadline(startTime time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 || startTime.IsZero() {
		return time.Time{}
	}
	return startTime.Add(ttl).UTC()
}

// ExtendLiveness 从 now 起按 ttl 滚动期限，递增 Extensions 并更新
// ExtendedAt。函数返回副本；调用方必须显式持久化结果，接收者不变，
// 同一值仍可进入审计日志。
//
// 已过期租约不在此续期；编排器必须向上返回 ErrLeaseExpired，
// 由恢复流程推进认领，而不是静默复活陈旧记录。
func (l Lease) ExtendLiveness(now time.Time, ttl time.Duration) (Lease, error) {
	if l.LivenessDeadline.IsZero() {
		return l, ErrLeaseExpired
	}
	if l.LivenessExpired(now) {
		return l, ErrLeaseExpired
	}
	if ttl <= 0 {
		return l, ErrLeaseExpired
	}
	l.LivenessDeadline = now.Add(ttl).UTC()
	l.ExtendedAt = now.UTC()
	l.Extensions++
	return l, nil
}
