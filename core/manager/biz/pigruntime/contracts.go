package pigruntime

import (
	"context"
	"errors"
	"time"
)

// AgentRuntimeAdapter 是 Manager 调用任意 Agent 运行时的稳定端口。
// 实现方只暴露生命周期、健康、取消与终态语义，不得通过该端口绕过
// Manager 的准入、能力、审批、凭据与审计控制。
type AgentRuntimeAdapter interface {
	// Descriptor 返回固定不变的运行时身份与能力摘要。
	Descriptor() RuntimeDescriptor

	// Launch 启动一次隔离执行，并通过事件流返回注册、心跳、进度、
	// 取消、失败与完成观察。实现方不得在 Launch 内同步等待终态。
	Launch(ctx context.Context, request LaunchRequest) (<-chan LifecycleEvent, error)

	// Health 返回当前运行时健康观察。信息缺失时必须返回 Degraded 或
	// Offline，不得用 Healthy 掩盖未知状态。
	Health(ctx context.Context) (HealthReport, error)

	// Cancel 请求取消一次已启动执行，并返回确认语义。若进程仍在退出，
	// 返回 State=TerminalPending；实现方必须继续发送终态事件。
	Cancel(ctx context.Context, request CancellationRequest) (CancellationResult, error)
}

// RuntimeDescriptor 是注册后不可变的运行时身份。
type RuntimeDescriptor struct {
	RuntimeID        string
	Kind             RuntimeKind
	Version          string
	ReleaseDigest    string
	CapabilityDigest string
	MaxConcurrency   int
	SandboxProfile   SandboxProfile
}

// RuntimeKind 标识运行时实现族。
type RuntimeKind string

const (
	RuntimeKindChat      RuntimeKind = "chat"
	RuntimeKindAgentTeam RuntimeKind = "agent_team"
	RuntimeKindPiG       RuntimeKind = "pig"
)

// RuntimeSemantics 统一 ChatRuntime 与 AgentTeams 的调用语义。字段是
// 两种运行时共同可表达的治理事实；实现方不得把业务语义私塞到扩展字段。
type RuntimeSemantics struct {
	TenantID       string
	IncidentID     string
	Role           string
	ConversationID string
	TeamID         string
	TurnID         string
	TaskID         string
	ReadOnly       bool
}

// FromChatRuntimeSemantics 把 ChatRuntime 的会话语义映射到通用端口。
// 函数只提取治理事实，不调用、不修改既有 ChatRuntime 实现。
func FromChatRuntimeSemantics(tenantID, incidentID, role, conversationID, taskID string, readOnly bool) RuntimeSemantics {
	return RuntimeSemantics{
		TenantID:       tenantID,
		IncidentID:     incidentID,
		Role:           role,
		ConversationID: conversationID,
		TaskID:         taskID,
		ReadOnly:       readOnly,
	}
}

// FromAgentTeamsSemantics 把 AgentTeams 的小队回合语义映射到通用端口。
// TeamID 与 TurnID 保留来源身份，避免把团队治理折叠成普通会话。
func FromAgentTeamsSemantics(tenantID, incidentID, role, teamID, turnID, taskID string, readOnly bool) RuntimeSemantics {
	return RuntimeSemantics{
		TenantID:   tenantID,
		IncidentID: incidentID,
		Role:       role,
		TeamID:     teamID,
		TurnID:     turnID,
		TaskID:     taskID,
		ReadOnly:   readOnly,
	}
}

// Validate 要求租户、角色与任务存在，并至少保留 Chat 会话或 AgentTeams
// 小队回合之一的来源语义。
func (s RuntimeSemantics) Validate() error {
	if s.TenantID == "" || s.Role == "" || s.TaskID == "" {
		return ErrInvalidContract
	}
	if s.ConversationID == "" && (s.TeamID == "" || s.TurnID == "") {
		return ErrInvalidContract
	}
	return nil
}

// LaunchRequest 是一次运行时执行的完整授权输入。
type LaunchRequest struct {
	Semantics       RuntimeSemantics
	Execution       ExecutionSnapshot
	Deadline        time.Time
	MaxEvents       int
	AllowedTools    []string
	HeartbeatWindow time.Duration
}

// LifecycleEvent 是运行时向 Manager 上报的 append-only 观察。
type LifecycleEvent struct {
	Sequence     int64
	RuntimeID    string
	TenantID     string
	TaskID       string
	Type         LifecycleEventType
	OccurredAt   time.Time
	Health       *HealthReport
	Capabilities []CapabilityObservation
	Terminal     *TerminalResult
}

// LifecycleEventType 枚举运行时必须归一化的事件类型。
type LifecycleEventType string

const (
	LifecycleEventRegistered LifecycleEventType = "registered"
	LifecycleEventHeartbeat  LifecycleEventType = "heartbeat"
	LifecycleEventProgress   LifecycleEventType = "progress"
	LifecycleEventCancelled  LifecycleEventType = "cancelled"
	LifecycleEventFailed     LifecycleEventType = "failed"
	LifecycleEventCompleted  LifecycleEventType = "completed"
)

// HealthStatus 是健康观察的闭集。
type HealthStatus string

const (
	HealthHealthy  HealthStatus = "healthy"
	HealthDegraded HealthStatus = "degraded"
	HealthOffline  HealthStatus = "offline"
)

// HealthReport 描述运行时进程、事件流与最近心跳。
type HealthReport struct {
	Status           HealthStatus
	LastHeartbeatAt  time.Time
	ActiveExecutions int
	HeartbeatOverdue bool
	Reason           string
}

// CapabilityObservation 是注册或运行期观察到的能力证据；它不是授权。
type CapabilityObservation struct {
	RuntimeToolID  string
	StableToolID   string
	Name           string
	Class          ToolClass
	AllowedInScope bool
	DriftReason    string
}

// ToolClass 描述工具副作用级别。
type ToolClass string

const (
	ToolClassReadOnly    ToolClass = "read_only"
	ToolClassWrite       ToolClass = "write"
	ToolClassDestructive ToolClass = "destructive"
)

// CancellationRequest 表示操作员或恢复流程发起的取消。
type CancellationRequest struct {
	RuntimeID string
	TaskID    string
	Reason    string
	Force     bool
}

// CancellationState 是取消确认状态。
type CancellationState string

const (
	CancellationAccepted        CancellationState = "accepted"
	CancellationTerminalPending CancellationState = "terminal_pending"
	CancellationAlreadyTerminal CancellationState = "already_terminal"
)

// CancellationResult 是取消请求的结果。
type CancellationResult struct {
	State             CancellationState
	TerminatedProcess bool
	Reason            string
}

// TerminalResult 是一次执行的终态与证据引用。
type TerminalResult struct {
	State            TerminalState
	FailureReason    string
	OutputRef        string
	OutputDigest     string
	Sanitized        bool
	SkippedSanitizer bool
	CompletedAt      time.Time
}

// TerminalState 是执行终态闭集。
type TerminalState string

const (
	TerminalSucceeded TerminalState = "succeeded"
	TerminalFailed    TerminalState = "failed"
	TerminalCancelled TerminalState = "cancelled"
)

var (
	// ErrInvalidContract 表示契约对象缺失必要治理事实。
	ErrInvalidContract = errors.New("pigruntime: invalid adapter contract")

	// ErrTerminalRequired 表示终态事件缺少 TerminalResult。
	ErrTerminalRequired = errors.New("pigruntime: terminal result is required")
)

// Validate 校验描述符必须携带完整身份、非零并发上限与固定沙箱约束。
func (d RuntimeDescriptor) Validate() error {
	if d.RuntimeID == "" || d.Version == "" || !d.Kind.Valid() ||
		!IsDigest(d.ReleaseDigest) || !IsDigest(d.CapabilityDigest) ||
		d.MaxConcurrency <= 0 {
		return ErrInvalidContract
	}
	return d.SandboxProfile.Validate()
}

// Validate 校验事件序号、租户、任务与类型；终态事件必须携带结果。
func (e LifecycleEvent) Validate() error {
	if e.Sequence <= 0 || e.RuntimeID == "" || e.TenantID == "" ||
		e.TaskID == "" || !e.Type.Valid() || e.OccurredAt.IsZero() {
		return ErrInvalidContract
	}
	if e.Type.Terminal() {
		if e.Terminal == nil {
			return ErrTerminalRequired
		}
		return e.Terminal.Validate()
	}
	return nil
}

// Validate 要求健康状态属于闭集，Degraded/Offline 必须说明原因。
func (h HealthReport) Validate() error {
	if !h.Status.Valid() {
		return ErrInvalidContract
	}
	if h.Status != HealthHealthy && h.Reason == "" {
		return ErrInvalidContract
	}
	if h.ActiveExecutions < 0 {
		return ErrInvalidContract
	}
	return nil
}

// Validate 要求终态、时间与脱敏决策完整。
func (r TerminalResult) Validate() error {
	if !r.State.Valid() || r.CompletedAt.IsZero() || (!r.Sanitized && !r.SkippedSanitizer) {
		return ErrInvalidContract
	}
	if r.State == TerminalFailed && r.FailureReason == "" {
		return ErrInvalidContract
	}
	if r.OutputRef != "" && !IsDigest(r.OutputDigest) {
		return ErrInvalidContract
	}
	return nil
}

// Valid 判断运行时类型是否在闭集内。
func (k RuntimeKind) Valid() bool {
	return k == RuntimeKindChat || k == RuntimeKindAgentTeam || k == RuntimeKindPiG
}

// Valid 判断生命周期事件类型是否在闭集内。
func (t LifecycleEventType) Valid() bool {
	switch t {
	case LifecycleEventRegistered, LifecycleEventHeartbeat, LifecycleEventProgress,
		LifecycleEventCancelled, LifecycleEventFailed, LifecycleEventCompleted:
		return true
	default:
		return false
	}
}

// Terminal 判断事件是否为终态。
func (t LifecycleEventType) Terminal() bool {
	return t == LifecycleEventCancelled || t == LifecycleEventFailed || t == LifecycleEventCompleted
}

// Valid 判断健康状态是否在闭集内。
func (s HealthStatus) Valid() bool {
	return s == HealthHealthy || s == HealthDegraded || s == HealthOffline
}

// Valid 判断工具类别是否在闭集内。
func (c ToolClass) Valid() bool {
	return c == ToolClassReadOnly || c == ToolClassWrite || c == ToolClassDestructive
}

// Valid 判断取消确认状态是否在闭集内。
func (s CancellationState) Valid() bool {
	return s == CancellationAccepted || s == CancellationTerminalPending ||
		s == CancellationAlreadyTerminal
}

// Valid 判断终态是否在闭集内。
func (s TerminalState) Valid() bool {
	return s == TerminalSucceeded || s == TerminalFailed || s == TerminalCancelled
}
