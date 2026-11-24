package runtimemgmt

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ToolCallRecord 是 PiG 治理工具网关的一次调用记录
// （pig-runtime-adapter 3.3）。每次网关裁决——允许或拒绝——都落一行；
// denied 行是失败闭合矩阵（3.4）的持久化断言证据。
//
// 为什么不复用 chat_tool_calls（design.md §四）：该行挂 chat 会话消息
// （message_id NOT NULL、LLMCallID 配对重放），PiG 调用挂在
// (runtime, task, incident) 上；且该表缺 release/capability digest 与
// role/incident 治理列。本表与 runtimemgmt_task_results 同族——PiG 的
// 终态写在那里，调用证据写在这里。
//
// 参数与结果只落哈希：与 MCP audit sink 的 arguments_sha256 同一取舍——
// 审计要的是「可比对」（重放检测、漂移比对），不是「可重放」。chat 侧
// arguments_json 落原文有 tool 消息配对的消费者，PiG 调用没有。
type ToolCallRecord struct {
	// ID 是 correlation token：RecordStart 返回它，RecordEnd 按它回填。
	ID string `gorm:"primaryKey;type:char(36);column:id"`

	// RuntimeID / TaskID / TenantID 是调用归属三元组。
	RuntimeID string `gorm:"index;size:128;not null;column:runtime_id"`
	TaskID    string `gorm:"index;size:64;not null;column:task_id"`
	TenantID  string `gorm:"index;size:64;not null;column:tenant_id"`

	// IncidentID 是任务钉死的事件作用域；跨事件拒绝的 denied 行靠它
	// 回答「这次调用想碰哪个事件」。
	IncidentID string `gorm:"size:64;not null;default:'';column:incident_id"`

	// StableToolID 是稳定 OpsKeeper 身份。不是 PiG 侧 RuntimeToolID
	// （不出翻译层），也不是 chat wire name。
	StableToolID string `gorm:"index;size:128;not null;column:stable_tool_id"`

	// ToolClass 是 domain 拼写的类别闭集 read/write/destructive。
	ToolClass string `gorm:"size:16;not null;column:tool_class"`

	// CallerRole / CallerWorker 是调用者身份；PiG 调用者是
	// (tenant, role, worker) 三元组，不是 user。
	CallerRole   string `gorm:"size:64;not null;default:'';column:caller_role"`
	CallerWorker string `gorm:"size:128;not null;default:'';column:caller_worker"`

	// ReleaseDigest / CapabilityDigest 是裁决时的快照证据：即使发布
	// 之后被禁用或替换，这一行仍能回答「裁决时运行的是什么」。
	ReleaseDigest    string `gorm:"size:64;not null;default:'';column:release_digest"`
	CapabilityDigest string `gorm:"size:64;not null;default:'';column:capability_digest"`

	// ParametersSHA256 是参数哈希，不落原文。
	ParametersSHA256 string `gorm:"size:64;not null;column:parameters_sha256"`

	// TraceID 是 trace 关联；span 未起时为空串。
	TraceID string `gorm:"size:64;not null;default:'';column:trace_id"`

	// StartedAt / EndedAt 是起止时戳；EndedAt 为 nil 表示该行尚未收尾。
	StartedAt time.Time  `gorm:"not null;column:started_at"`
	EndedAt   *time.Time `gorm:"column:ended_at"`

	// Status 是调用状态闭集：dispatched（已分发未收尾）/ blocked（闸上
	// 被拒，永不分发）/ success / error / timeout。
	Status string `gorm:"size:16;not null;default:dispatched;column:status"`

	// Decision 是 Admit 裁决：allowed / blocked。与 Status 分开——
	// 「被拒」是裁决事实，「未分发」是状态事实，两列各自回答审计问题。
	Decision string `gorm:"size:16;not null;default:'';column:decision"`

	// DenyReason 是拒绝理由码（网关闭集）；decision=blocked 时必填。
	DenyReason string `gorm:"size:32;not null;default:'';column:deny_reason"`

	// ResultSHA256 是结果哈希，不落原文。
	ResultSHA256 string `gorm:"size:64;not null;default:'';column:result_sha256"`

	// ErrorMessage 是错误摘要（截断到 512）；成功为空。TEXT 列无 DB
	// 默认值（MySQL 1101 陷阱，遵循 migrate.go 先例），代码侧初始化。
	ErrorMessage string `gorm:"type:text;column:error_message"`

	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名。
func (ToolCallRecord) TableName() string { return "runtimemgmt_pig_tool_calls" }

// 调用状态与裁决闭集。
const (
	// ToolCallStatusDispatched 已分发未收尾。
	ToolCallStatusDispatched = "dispatched"
	// ToolCallStatusBlocked 在闸上被拒，永不分发。
	ToolCallStatusBlocked = "blocked"
	// ToolCallStatusSuccess 分发后成功。
	ToolCallStatusSuccess = "success"
	// ToolCallStatusError 分发后失败。
	ToolCallStatusError = "error"
	// ToolCallStatusTimeout 分发后超时。
	ToolCallStatusTimeout = "timeout"

	// ToolCallDecisionAllowed 裁决放行。
	ToolCallDecisionAllowed = "allowed"
	// ToolCallDecisionBlocked 裁决拒绝。
	ToolCallDecisionBlocked = "blocked"
	// ToolCallDecisionProposalRequired 裁决要求提案：write/destructive
	// 且 ReadOnly=false 时不直接分发，落 proposal 待人工批准（3.5）。
	ToolCallDecisionProposalRequired = "proposal_required"
)

// ErrToolCallInvalid 表示记录缺必要治理事实，拒绝落库（fail closed）。
var ErrToolCallInvalid = errors.New("runtimemgmt: invalid pig tool call record")

// Validate 校验落库前内部一致性。
func (r ToolCallRecord) Validate() error {
	if r.ID == "" || r.RuntimeID == "" || r.TaskID == "" || r.TenantID == "" ||
		r.StableToolID == "" || r.ToolClass == "" ||
		len(r.ParametersSHA256) != 64 || r.StartedAt.IsZero() {
		return ErrToolCallInvalid
	}
	switch r.Decision {
	case ToolCallDecisionAllowed, ToolCallDecisionProposalRequired:
		if r.DenyReason != "" {
			return ErrToolCallInvalid
		}
	case ToolCallDecisionBlocked:
		if r.DenyReason == "" {
			return ErrToolCallInvalid
		}
	default:
		return ErrToolCallInvalid
	}
	switch r.Status {
	case ToolCallStatusDispatched, ToolCallStatusBlocked, ToolCallStatusSuccess,
		ToolCallStatusError, ToolCallStatusTimeout:
	default:
		return ErrToolCallInvalid
	}
	if r.Status == ToolCallStatusBlocked && r.Decision != ToolCallDecisionBlocked {
		return ErrToolCallInvalid
	}
	if r.EndedAt != nil && r.EndedAt.Before(r.StartedAt) {
		return ErrToolCallInvalid
	}
	return nil
}

// BeforeCreate 自动补 UUID（与其他模型同一形态）。
func (r *ToolCallRecord) BeforeCreate(tx *gorm.DB) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	return nil
}
