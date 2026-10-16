package runtimemgmt

import "time"

// AdmissionDecision 是认领前槽位准入快照，在 queued -> admitted 时记录。
// 即使后续准入上限变化，恢复流程也能回答“该任务当时依据什么容量获准”，
// 无需重新执行策略。
//
// 决策由 §2.2 准入服务计算后传入持久化层；本文件只定义持久化形状。
// 规格场景“无可用容量”映射 Reason=no_capacity、Outcome=Rejected。
type AdmissionDecision struct {
	// TaskID 是所属持久化任务；每个任务一条记录。表命名隐含与 TenantID
	// 的组合主键语义，显式保存 TenantID 便于过滤。
	TaskID string `gorm:"primaryKey;size:64"`

	TenantID string `gorm:"index;size:64;not null"`

	// Outcome 只能是 AdmissionOutcomeAdmitted 或 AdmissionOutcomeRejected。
	Outcome AdmissionOutcome `gorm:"size:16;not null;index"`

	// Reason 是简短原因，准入成功为空；拒绝时必须是 no_capacity、
	// runtime_not_ready、quota_exceeded、safety_not_allowed 或 policy_denied。
	Reason string `gorm:"size:64;not null;default:''"`

	// ServerLimit 是准入时 Manager 全局并发上限，持久化用于审计与差异分析。
	ServerLimit int `gorm:"not null;default:0"`

	// RuntimeLimit 是准入时运行时上报的空闲槽位或容量上限，与 ServerLimit
	// 一起保存；二者较小值是实际闸门。
	RuntimeLimit int `gorm:"not null;default:0"`

	// RuntimeSelector 是准入时记录的期望运行时表达式。实际 RuntimeID
	// 在认领时写入，不在此处。v1 为自由文本，后续切片预计收敛为标签选择器。
	RuntimeSelector string `gorm:"size:128;not null;default:''"`

	// DecidedAt 是准入决策墙钟时间；建立索引便于积压清扫优先处理最旧记录。
	DecidedAt time.Time `gorm:"index;not null"`

	// DecidedBy 是决策者：v1 准入服务为 scheduler.v1，人工覆盖为
	// operator:<user_id>。操作员可突破容量限制，审计保留两种原因。
	DecidedBy string `gorm:"size:64;not null;default:scheduler.v1"`
}

// TableName 固定表名。迁移必须保持不变；编排器按 task_id 读取。
func (AdmissionDecision) TableName() string { return "runtimemgmt_admission_decisions" }

// AdmissionOutcome 枚举准入与拒绝状态。
type AdmissionOutcome string

const (
	AdmissionOutcomeAdmitted AdmissionOutcome = "admitted"
	AdmissionOutcomeRejected AdmissionOutcome = "rejected"
)

// AdmissionReason 常量。显式列出拒绝原因，调用方与 Dashboard 无需解析字符串。
const (
	AdmissionReasonOK               = ""
	AdmissionReasonNoCapacity       = "no_capacity"
	AdmissionReasonRuntimeNotReady  = "runtime_not_ready"
	AdmissionReasonQuotaExceeded    = "quota_exceeded"
	AdmissionReasonSafetyNotAllowed = "safety_not_allowed"
	AdmissionReasonPolicyDenied     = "policy_denied"
)

// Validate 校验决策内部一致性；准入服务在持久化前执行。
func (a AdmissionDecision) Validate() error {
	switch a.Outcome {
	case AdmissionOutcomeAdmitted:
		if a.Reason != AdmissionReasonOK {
			return ErrNoCapacity
		}
		return nil
	case AdmissionOutcomeRejected:
		switch a.Reason {
		case AdmissionReasonNoCapacity,
			AdmissionReasonRuntimeNotReady,
			AdmissionReasonQuotaExceeded,
			AdmissionReasonSafetyNotAllowed,
			AdmissionReasonPolicyDenied:
			return nil
		default:
			return ErrNoCapacity
		}
	default:
		return ErrNoCapacity
	}
}
