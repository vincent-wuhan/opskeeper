package runtimemgmt

import "time"

// Checkpoint 是 Worker 记录的持久化恢复指针。一个任务可有多个检查点，
// 包括每次尝试至少一个以及尝试内进度检查点。当前 (task_id,
// attempt_number) 的最新记录是恢复目标；旧记录保留用于重放与审计。
//
// 规格场景“检查点后 Worker 重启”依赖该记录：重启后 Worker 读取当前认领
// 的最新 Checkpoint，从 PayloadRef / ResumeHint 续跑而不是重跑。变更型
// Worker 还必须比对 IdempotencyContract.Fingerprint，确认恢复目标与原副作用
// 面一致。
type Checkpoint struct {
	// ID 是自增行 ID。自然查询键是 (TaskID, AttemptNumber, Sequence)，
	// 代理主键让记录保持追加友好，也便于存储层在新尝试替代后按 DELETE
	// 回收。
	ID int64 `gorm:"primaryKey;autoIncrement"`

	// TaskID 是所属任务。
	TaskID string `gorm:"index;size:64;not null"`

	TenantID string `gorm:"index;size:64;not null"`

	// AttemptNumber 关联一次 AttemptRecord；从 1 开始，避免与初始排队
	// 阶段的第 0 次尝试混淆。
	AttemptNumber int `gorm:"not null;default:1"`

	// Sequence 是同一尝试内检查点序号：首个为 1，之后单调递增；
	// 由 (TaskID, AttemptNumber) 维护自增。
	Sequence int `gorm:"not null;default:1"`

	// Generation 记录写入时 Claim.Generation。恢复流程跳过落后于当前
	// 认领代际的检查点，避免旧运行时的记录被新认领者误用。
	Generation int64 `gorm:"not null;default:0"`

	// Phase 是 Worker 可选阶段标签，例如 collecting_evidence、
	// writing_result；仅用于诊断，不属于恢复契约。
	Phase string `gorm:"size:64;not null;default:''"`

	// ResumeHint 是 Worker 提供的不透明游标，例如 evidence_id=42、
	// page_token=...。编排器原样透传，控制面不解释其内容。
	ResumeHint string `gorm:"type:text"`

	// PayloadRef 指向支撑恢复的持久化载荷，例如工作区产物、OSS key 或
	// 数据库行 ID。引用语义由运行时持有，控制面只持久化字符串。
	PayloadRef string `gorm:"size:255;not null;default:''"`

	// RecordedAt 是 Worker 写入记录的墙钟时间。
	RecordedAt time.Time `gorm:"index;not null"`

	// RecordedBy 冗余写入时 Claim.ClaimedBy，事后查询无需关联 Claim
	// 即可判断检查点来源运行时。
	RecordedBy string `gorm:"size:128;not null;default:''"`
}

// TableName 固定表名。迁移必须保持不变；编排器按
// (task_id, attempt_number, sequence) 读取。
func (Checkpoint) TableName() string { return "runtimemgmt_checkpoints" }

// ResumeFor 返回按 Sequence 降序得到的最新检查点；nil 表示尚无恢复指针。
//
// 注意：调用方传入该任务与尝试的 Checkpoint 切片，本函数只选择最佳记录，
// 不扫描存储层；实际 SQL 由仓储实现负责。
func ResumeFor(checkpoints []Checkpoint) *Checkpoint {
	var best *Checkpoint
	for i := range checkpoints {
		c := &checkpoints[i]
		if c == nil {
			continue
		}
		if best == nil || c.Sequence > best.Sequence {
			best = c
		}
	}
	return best
}

// IsForCurrentClaim 判断检查点代际是否匹配当前 Claim.Generation。
// 陈旧认领恢复用它跳过前认领者写入的检查点。
func (c Checkpoint) IsForCurrentClaim(claimGeneration int64) bool {
	return c.Generation == claimGeneration && c.Generation != 0
}
