package runtimemgmt

import "time"

// TaskResult 是任务终态记录。每个 (TaskID, AttemptNumber) 最多一条；
// Dashboard 展示最后一次尝试。持久化必须填写 SanitizerStatus，
// 明确输出已脱敏、因理由跳过，还是因错误无法分类。
//
// 规格场景“任务进入终态”和“持久化含凭据输出”都依赖该记录。
type TaskResult struct {
	// TaskID 与 AttemptNumber 组成组合主键。当前尝试结果用于展示，
	// 历史尝试结果仍可查询用于重放。
	TaskID        string `gorm:"primaryKey;size:64"`
	AttemptNumber int    `gorm:"primaryKey"`

	TenantID string `gorm:"index;size:64;not null"`

	// Status 复制最终任务状态 succeeded、failed 或 cancelled；
	// 读取方无需关联任务记录即可知道结果。
	Status TaskState `gorm:"size:16;not null;index"`

	// EndedAt 是完成时的墙钟时间。
	EndedAt time.Time `gorm:"index;not null"`

	// FailureClass 成功时为空，否则与 AttemptRecord.FailureClass 相同；
	// 冗余保存后，终态失败检查器只需读取本记录。
	FailureClass FailureClass `gorm:"size:32;not null;default:''"`

	// ErrorMessage 是终态失败的可读错误信息，成功时为空；
	// 持久化前必须按 SanitizerStatus 脱敏。
	ErrorMessage string `gorm:"type:text"`

	// OutputRef 指向持久化输出载荷，例如 OSS key、工作区记录或证据 ID。
	// v1 为自由文本，语义由运行时持有；无输出时为空。
	OutputRef string `gorm:"size:255;not null;default:''"`

	// OutputSize 是输出载荷字节数；零表示无输出。显式保存，
	// 审计者无需重新拉取引用即可估算证据规模。
	OutputSize int64 `gorm:"not null;default:0"`

	// SanitizerStatus 是结果、错误信息与输出引用的脱敏裁决，写入时必填。
	SanitizerStatus SanitizerStatus `gorm:"size:16;not null"`

	// SanitizerNotes 是跳过或部分脱敏的可选理由；状态为 skipped 时必填，
	// 让审计者知道链路为何中断。字段自身持久化前也需脱敏。
	SanitizerNotes string `gorm:"type:text"`

	// OutputDigest 是输出载荷 SHA-256，无输出时为空；重放工具无需下载
	// 即可发现同一任务出现不同字节。
	OutputDigest string `gorm:"size:64;not null;default:''"`

	// CompletedBy 是写入结果的运行时，等于写入时 Claim.ClaimedBy。
	CompletedBy string `gorm:"size:128;not null;default:''"`

	// PodIdempotencyFingerprint 是本次尝试使用的幂等契约指纹。写入时复制，
	// 即使原契约记录之后被回收，事后分析仍可验证结果产生于正确副作用契约。
	PodIdempotencyFingerprint string `gorm:"size:64;not null;default:''"`
}

// TableName 固定表名。迁移必须保持不变；编排器按
// (task_id, attempt_number) 读取。
func (TaskResult) TableName() string { return "runtimemgmt_task_results" }

// SanitizerStatus 是结果输出脱敏闸门的闭集分类。控制面拒绝持久化
// 状态为空或未识别的 TaskResult，防止原始凭据进入证据、遥测或 UI。
type SanitizerStatus string

const (
	// SanitizerStatusApplied 表示 Worker 或脱敏中间件在持久化前已执行
	// 脱敏流水线。
	SanitizerStatusApplied SanitizerStatus = "applied"

	// SanitizerStatusSkipped 表示未执行脱敏；SanitizerNotes 必须说明理由。
	// 只读或不含凭据等白名单场景需记录说明。
	SanitizerStatusSkipped SanitizerStatus = "skipped"

	// SanitizerStatusFailed 表示脱敏流水线错误；任务视为不安全，
	// Worker 重跑脱敏或操作员经审计覆盖前，编排器阻止持久化。
	SanitizerStatusFailed SanitizerStatus = "failed"
)

// SanitizerStatusValues 是闭集枚举。
var SanitizerStatusValues = []SanitizerStatus{
	SanitizerStatusApplied,
	SanitizerStatusSkipped,
	SanitizerStatusFailed,
}

// IsSanitizerStatus 判断 s 是否为已识别状态。
func IsSanitizerStatus(s SanitizerStatus) bool {
	for _, v := range SanitizerStatusValues {
		if v == s {
			return true
		}
	}
	return false
}

// Validate 校验 TaskResult 内部一致性；失败 API 在持久化前执行。
func (r TaskResult) Validate() error {
	if r.TaskID == "" {
		return ErrResultNotReady
	}
	if !IsTerminal(r.Status) {
		return ErrResultNotReady
	}
	if !IsSanitizerStatus(r.SanitizerStatus) {
		return ErrSanitizerMissing
	}
	if r.SanitizerStatus == SanitizerStatusSkipped && r.SanitizerNotes == "" {
		return ErrSanitizerMissing
	}
	if r.Status == TaskStateFailed && !IsFailureClass(r.FailureClass) {
		return ErrFailureClassInvalid
	}
	if r.Status == TaskStateFailed && r.FailureClass == "" {
		return ErrFailureClassInvalid
	}
	return nil
}
