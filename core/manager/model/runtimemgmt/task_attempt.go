package runtimemgmt

import "time"

// AttemptRecord 是任务的一次执行尝试。初始执行与每次重试各生成一条记录；
// 自动重试由 IdempotencyContract 与 FailureClass.IsRetryable 共同门控。
//
// 该记录同时是单次尝试的安全与分类快照，事后分析与失败 API 无需关联
// 每次重试调用即可还原重试决策树。
type AttemptRecord struct {
	// TaskID 与 AttemptNumber 组成组合主键，尝试序号从 1 开始。
	TaskID string `gorm:"primaryKey;size:64"`

	// AttemptNumber 从 1 开始；编排器只在投影状态中用 0 表示尚无尝试，
	// 不持久化该值。
	AttemptNumber int `gorm:"primaryKey"`

	TenantID string `gorm:"index;size:64;not null"`

	// MaxAttempts 是任务重试上限。首次尝试时读取并快照到后续记录，
	// 避免运行时配置变化回溯扩大预算。
	MaxAttempts int `gorm:"not null;default:1"`

	// ParentRetryID 可选关联同任务内上一次失败尝试；初始尝试为空，
	// 重试时填充，便于 Dashboard 展示重试树。
	ParentRetryID string `gorm:"size:64;index;default:''"`

	// StartedAt 是尝试开始（状态进入 preparing）的墙钟时间；
	// 建立索引用于 SLA 与延迟视图。
	StartedAt time.Time `gorm:"index;not null"`

	// EndedAt 是尝试进入终态（成功或已分类失败）的时间；与 StartedAt
	// 建立索引，重试树可展示耗时。
	EndedAt *time.Time `gorm:"index"`

	// Outcome 在执行中为空；终态时必须是 succeeded、failed、cancelled
	// 或 superseded。陈旧租约提升后，旧尝试标记为 superseded。
	Outcome string `gorm:"size:16;not null;default:''"`

	// FailureClass 成功时为空；终态失败时保存编排器分类后的失败分类值。
	FailureClass FailureClass `gorm:"size:32;not null;default:''"`

	// FailureReason 是与失败分类配套的可读错误信息。Worker 字符串必须
	// 先脱敏再持久化；闸门见 TaskResult.SanitizerStatus。
	FailureReason string `gorm:"type:text"`

	// RuntimeID 是持有该尝试的运行时，等于尝试开始时 Claim.ClaimedBy；
	// 冗余保存便于重放与事后查询，无需遍历 Claim 历史。
	RuntimeID string `gorm:"size:128;not null;index"`

	// ParentTaskID 可选关联后续任务，例如编排器将重试重新物化为新持久化
	// 记录。它与任务内 ParentRetryID 不同，使尝试树与后续任务树可并存。
	ParentTaskID string `gorm:"size:64;index;default:''"`
}

// TableName 固定表名。迁移必须保持不变；编排器按
// (task_id, attempt_number) 读取。
func (AttemptRecord) TableName() string { return "runtimemgmt_attempt_records" }

// 尝试结果使用类型化字符串，保持向后兼容，对应 Outcome 字段。
const (
	AttemptOutcomeSucceeded  = "succeeded"
	AttemptOutcomeFailed     = "failed"
	AttemptOutcomeCancelled  = "cancelled"
	AttemptOutcomeSuperseded = "superseded"
)

// IsTerminalOutcome 判断 outcome 是否是 AttemptRecord 四种终态之一。
func IsTerminalOutcome(outcome string) bool {
	switch outcome {
	case AttemptOutcomeSucceeded,
		AttemptOutcomeFailed,
		AttemptOutcomeCancelled,
		AttemptOutcomeSuperseded:
		return true
	default:
		return false
	}
}

// FailureClassOf 在尝试终态失败时返回失败分类；成功、被替换或取消返回空。
// 独立辅助函数避免 Dashboard 与重放代码重复判断 Outcome。
func (a AttemptRecord) FailureClassOf() FailureClass {
	if a.Outcome == AttemptOutcomeFailed {
		return a.FailureClass
	}
	return ""
}

// HasRetryBudget 判断是否还有至少一次尝试额度（AttemptNumber < MaxAttempts）。
// 编排器结合失败分类决定按检查点续跑、新建尝试或进入终态失败。
func (a AttemptRecord) HasRetryBudget() bool {
	if a.MaxAttempts <= 0 {
		return false
	}
	return a.AttemptNumber < a.MaxAttempts
}

// Elapsed 返回尝试执行耗时；尚未结束时返回零。
func (a AttemptRecord) Elapsed() time.Duration {
	if a.EndedAt == nil {
		return 0
	}
	return a.EndedAt.Sub(a.StartedAt)
}
