package runtimemgmt

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Task 是 §2.1 定义的中央持久化记录。状态、幂等、准入、认领、租约、
// 检查点、尝试、失败分类与结果拆分为独立表，保持追加语义；Task 记录
// 保存身份、当前状态以及关联它们的指针字段。
//
// 多租户约束：所有查询必须按 TenantID 过滤。
//
// 存储不变量由编排器与仓储层执行，不在行对象内强制：
//   - State 进入 succeeded、failed 或 cancelled 后，仅失败 API 可写对应
//     TaskResult。
//   - State 进入 TaskStateAdmitted 前必须已有 AdmissionDecision。
//   - State 进入 TaskStateClaimed 前必须已有 Claim。
//   - 写入 Claim 时必须设置 Lease.PrepareDeadline。
//   - Safety 为 TaskSafetyMutating 时，首条 Claim 前必须已有
//     IdempotencyContract；该约束由编排器认领流执行。
type Task struct {
	// TaskID 是持久化主键，使用 UUID v4（见 NewTaskID）。作用域为租户，
	// 所有读取都必须包含 TenantID。
	TaskID string `gorm:"primaryKey;size:64"`

	TenantID string `gorm:"index;size:64;not null"`

	// IncidentID 是可选的 OpsKeeper 事件 ID。基础设施探测、preview-pg
	// 构建等非事件工作流为空。
	IncidentID string `gorm:"index;size:64;default:''"`

	// Kind 是生产者提供的任务类型，例如 read_only.diagnostic、
	// repair.preview.execute、agentteams.task。它作为处理器路由键，
	// 控制面本身不感知具体类型。
	Kind string `gorm:"size:64;not null;index"`

	// Safety 描述副作用边界（见 task_safety.go），插入时必须属于
	// TaskSafetyValues。
	Safety TaskSafety `gorm:"size:16;not null;index"`

	// State 是当前持久化阶段；允许迁移见 task_state.go。
	State TaskState `gorm:"size:16;not null;index"`

	// PayloadDigest 是生产者载荷字节的 SHA-256 十六进制摘要。插入时记录，
	// 供准入与重放路径校验输入是否一致。
	PayloadDigest string `gorm:"size:64;not null;default:''"`

	// DesiredRuntimeSelector 是生产者提供的运行时表达式，例如
	// kind=chat_worker tier=ha；准入据此匹配，v1 保持自由文本。
	DesiredRuntimeSelector string `gorm:"size:128;not null;default:''"`

	// RuntimeID 是当前持有认领的运行时。首次认领前为空；恢复或替换时
	// 写入或清空。
	RuntimeID string `gorm:"size:128;not null;default:'';index"`

	// WorkspaceID 是持久化工作区身份（见 §3）。短库存读取等无需工作区
	// 的任务为空。
	WorkspaceID string `gorm:"size:64;not null;default:'';index"`

	// Priority 由生产者提供，0 为默认值；准入服务用它打破平局，
	// 不构成严格 SLA。与 State 建立索引，降低高优队列查询成本。
	Priority int `gorm:"not null;default:0;index"`

	// TraceID 是 OTel trace_id，可为空；可通过 (TraceID, CreatedAt)
	// 关联运行时事件时间线。
	TraceID string `gorm:"size:64;index;default:''"`

	// CreatedAt 是生产者插入记录的时间。
	CreatedAt time.Time `gorm:"not null;index"`

	// UpdatedAt 是最近一次状态变更的墙钟时间。检查点、尝试、结果等
	// 追加子表保留自身排序时间戳；该字段用于快速查询当前状态。
	UpdatedAt time.Time `gorm:"not null"`
}

// TableName 固定表名。迁移必须保持不变；编排器按 TaskID 读写状态。
func (Task) TableName() string { return "runtimemgmt_tasks" }

// ComputePayloadDigest 是生产者侧标准哈希函数，接受任意字节（通常是
// 序列化后的载荷 JSON），返回 Task 记录保存的 SHA-256 十六进制字符串。
// 独立函数便于测试明确断言摘要来源字节。
func ComputePayloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// NewTask 是生产者路径构造函数：填充必要标识、计算 PayloadDigest，
// 并用同一时刻填充 CreatedAt 与 UpdatedAt。初始状态为 TaskStateQueued，
// 由准入服务迁移到 Admitted。
func NewTask(
	tenantID, incidentID, kind, payload string, payloadBytes []byte,
	safety TaskSafety, selector, traceID string, priority int, now time.Time,
) (Task, error) {
	if tenantID == "" || kind == "" {
		return Task{}, ErrUnknownState
	}
	if !IsTaskSafety(safety) {
		return Task{}, ErrUnknownState
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return Task{
		TaskID:                 NewTaskID(),
		TenantID:               tenantID,
		IncidentID:             incidentID,
		Kind:                   kind,
		Safety:                 safety,
		State:                  TaskStateQueued,
		PayloadDigest:          ComputePayloadDigest(payloadBytes),
		DesiredRuntimeSelector: selector,
		TraceID:                traceID,
		Priority:               priority,
		CreatedAt:              now,
		UpdatedAt:              now,
		// payload 参数仅保留说明语义；持久化指纹以 digest 为准。
		// 如需恢复原始字节，生产者应通过旁路存储附加载荷。
	}, nil
}

// CanTransition 使用与 IsValidTransition 相同的邻接表校验当前状态到
// next 的边。每次状态写入都应调用，让陈旧或非法迁移返回
// ErrInvalidTransition，避免静默破坏任务。
func (t Task) CanTransition(next TaskState) error {
	if IsTerminal(t.State) {
		return ErrTerminalState
	}
	if !IsValidTransition(t.State, next) {
		return ErrInvalidTransition
	}
	return nil
}

// WithState 返回 State 为 next、UpdatedAt 为 now 的副本。now 为零值时
// 使用当前 UTC 时钟。原值不变；调用方必须持久化返回副本。
func (t Task) WithState(next TaskState, now time.Time) (Task, error) {
	if err := t.CanTransition(next); err != nil {
		return t, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	t.State = next
	t.UpdatedAt = now
	return t, nil
}
