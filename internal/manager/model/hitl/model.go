// Package hitl defines the unified proposal persistence model for the
// human-in-the-loop pause / resume subsystem (路径 A P1-2 阶段 1).
//
// 设计要点（见 openspec/changes/hitl-pause-resume/design.md §2.1）：
//
//   - Proposal 合并旧 approvals 表 + chat_mutating_proposals 表，单源真相
//   - 状态机：pending → approved/rejected/expired；approved → executed/failed/
//     paused/rolled-back；paused → resumed → executing → executed
//   - proposal_state 表持久化 ResumeToken BLOB（关联 Proposal.PauseStateVersion）
//   - DualWriteAt 字段标记数据迁移期由旧表写入的新行，便于 7 天切换窗口回溯
package hitl

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Proposal 是统一的人类决策请求模型。
//
// 它覆盖三种来源：
//   - approval_legacy（来自旧 approvals 表）：保留 1 个 release 周期用于回退
//   - mutating_legacy（来自旧 chat_mutating_proposals 表）：同上
//   - 直接由 Agent / Worker / Flow node 创建的新提案
type Proposal struct {
	ID string `gorm:"primaryKey;type:char(36);column:id" json:"id"`

	// Kind 路由执行器，例如 shell_command / restart_service /
	// review_gate / mutating_tool_call。biz 层按 kind 注册 Executor。
	Kind string `gorm:"size:64;not null;index:idx_proposal_kind_state,priority:1" json:"kind"`

	// LegacyKind 标记数据迁移遗留：approval_legacy / mutating_legacy / 空（新行）
	LegacyKind string `gorm:"size:32;index" json:"legacy_kind,omitempty"`

	Title   string `gorm:"size:255;not null" json:"title"`
	Summary string `gorm:"type:text" json:"summary,omitempty"`

	// PayloadJSON 是执行器所需的不透明 action spec。
	PayloadJSON string `gorm:"type:text;not null" json:"payload"`

	Source         string  `gorm:"size:32;not null;default:agent" json:"source"`
	SessionID      string  `gorm:"size:64;index" json:"session_id,omitempty"`
	MessageID      string  `gorm:"size:64" json:"message_id,omitempty"`
	IdempotencyKey *string `gorm:"size:191;uniqueIndex:idx_proposal_idempotency_key" json:"idempotency_key,omitempty"`

	ProposedBy uint64  `gorm:"not null;default:0;index" json:"proposed_by"`
	ApprovedBy *uint64 `gorm:"" json:"approved_by,omitempty"`
	RejectedBy *uint64 `gorm:"" json:"rejected_by,omitempty"`
	PausedBy   *uint64 `gorm:"" json:"paused_by,omitempty"`
	ResumedBy  *uint64 `gorm:"" json:"resumed_by,omitempty"`

	Severity    string `gorm:"size:16;not null;default:mutating;index" json:"severity"`
	Sensitivity string `gorm:"size:16;not null;default:internal;index" json:"sensitivity"`

	State string `gorm:"size:24;not null;default:pending;index:idx_proposal_kind_state,priority:2" json:"state"`

	Reason     *string `gorm:"type:text" json:"reason,omitempty"`
	ResultJSON *string `gorm:"type:text" json:"result,omitempty"`

	PausedAt          *time.Time `gorm:"column:paused_at" json:"paused_at,omitempty"`
	ResumedAt         *time.Time `gorm:"column:resumed_at" json:"resumed_at,omitempty"`
	PauseStateVersion int64      `gorm:"column:pause_state_version;not null;default:0" json:"pause_state_version"`

	ExpiresAt *time.Time `gorm:"column:expires_at;index" json:"expires_at,omitempty"`

	DryRunDiffURL           *string    `gorm:"type:text" json:"dry_run_diff_url,omitempty"`
	IMThreadID              *string    `gorm:"size:128;index" json:"im_thread_id,omitempty"`
	MatrixEventID           string     `gorm:"size:128;index" json:"matrix_event_id,omitempty"`
	ExecutionLeaseExpiresAt *time.Time `gorm:"column:execution_lease_expires_at;index" json:"execution_lease_expires_at,omitempty"`

	DualWriteAt *time.Time `gorm:"column:dual_write_at;index" json:"dual_write_at,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
	ExecutedAt *time.Time `json:"executed_at,omitempty"`
	UpdatedAt  time.Time  `gorm:"autoUpdateTime" json:"updated_at"`
}

func (Proposal) TableName() string { return "proposal" }

// AgentTeamsPayload is the immutable wire payload used by the AgentTeams HITL
// Proposal API. Its canonical JSON SHA-256 is the payload_hash fingerprint.
type AgentTeamsPayload struct {
	Fingerprint string                      `json:"fingerprint"`
	RequestID   string                      `json:"request_id"`
	IncidentID  string                      `json:"incident_id"`
	Action      string                      `json:"action"`
	BlastRadius string                      `json:"blast_radius"`
	Resource    string                      `json:"resource"`
	RoomID      string                      `json:"room_id"`
	Parameters  RecoveryExecutionParameters `json:"parameters"`
	RequestedAt time.Time                   `json:"requested_at"`
	ExpiresAt   time.Time                   `json:"expires_at"`
}

type RecoveryExecutionParameters struct {
	Command           string `json:"command"`
	DeviceID          uint64 `json:"device_id,omitempty"`
	Service           string `json:"service,omitempty"`
	Reason            string `json:"reason"`
	IncidentID        string `json:"incident_id,omitempty"`
	FixtureManifestID string `json:"fixture_manifest_id,omitempty"`
	PoolManifestID    string `json:"pool_manifest_id,omitempty"`
}

const RecoveryActionRestartService = "restart_service"
const RecoveryActionKillProcess = "kill_process"
const RecoveryActionResizePool = "resize_pool"

func (p RecoveryExecutionParameters) CanonicalJSON() ([]byte, error) {
	return json.Marshal(p)
}

func (p *Proposal) BeforeCreate(*gorm.DB) error {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	return nil
}

// Severity 三档。
const (
	SeveritySafe      = "safe"
	SeverityMutating  = "mutating"
	SeverityDangerous = "dangerous"
)

// Sensitivity 档位（与 P1-3 data-guard 对齐）。
const (
	SensitivityPublic       = "public"
	SensitivityInternal     = "internal"
	SensitivityConfidential = "confidential"
	SensitivityRestricted   = "restricted"
	SensitivityTopSecret    = "top_secret"
)

// Source 标识 proposal 产生方。
const (
	SourceAgent     = "agent"
	SourceFlow      = "flow"
	SourceHarness   = "harness"
	SourceMigration = "migration"
)

// Proposal 状态机常量。
const (
	KindAgentTeams = "agentteams_hitl"

	StatePending    = "pending"
	StateApproved   = "approved"
	StateRejected   = "rejected"
	StateExpired    = "expired"
	StateExecuted   = "executed"
	StateFailed     = "failed"
	StateExecuting  = "executing"
	StatePaused     = "paused"
	StateResumed    = "resumed"
	StateRolledBack = "rolled_back"
)

// ValidTransitions 定义状态机的允许迁移。
//
// 设计为 map[from][]to，proposal_test.go 用此表断言全路径 + 不允许迁移。
var ValidTransitions = map[string][]string{
	StatePending:    {StateApproved, StateRejected, StateExpired, StatePaused},
	StateApproved:   {StateExecuting, StateExecuted, StateFailed, StatePaused, StateRolledBack},
	StatePaused:     {StateResumed, StateRejected, StateExpired},
	StateResumed:    {StateExecuted, StateFailed, StatePaused, StateRolledBack},
	StateExecuting:  {StateExecuted, StateFailed},
	StateRejected:   {},
	StateExpired:    {},
	StateExecuted:   {StateRolledBack},
	StateFailed:     {},
	StateRolledBack: {},
}

// IsTerminal 报告 state 是否为终态。
func IsTerminal(state string) bool {
	switch state {
	case StateExecuted, StateRejected, StateExpired, StateFailed, StateRolledBack:
		return true
	}
	return false
}

// IsValidTransition 判定 from → to 是否在 ValidTransitions 表中。
func IsValidTransition(from, to string) bool {
	dests, ok := ValidTransitions[from]
	if !ok {
		return false
	}
	for _, d := range dests {
		if d == to {
			return true
		}
	}
	return false
}

// ProposalState 是 ResumeToken 持久化表（独立 BLOB 列）。
//
// 设计要点：
//   - PK 是 proposal_id（与 Proposal 表 1:1 反范式），便于 O(1) lookup
//   - state_version 跟随 Proposal.PauseStateVersion 自增，每次 Rehydrate +1
//   - token_blob 是 ResumeToken.Serialize() 输出
//   - 行存在 ≡ Proposal.State ∈ {paused}，终态后清理
type ProposalState struct {
	ProposalID   string    `gorm:"primaryKey;type:char(36);column:proposal_id" json:"proposal_id"`
	StateVersion int64     `gorm:"column:state_version;not null;default:1" json:"state_version"`
	TokenBlob    []byte    `gorm:"not null;column:token_blob" json:"token_blob"`
	CreatedAt    time.Time `gorm:"column:created_at;autoCreateTime" json:"created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at;autoUpdateTime" json:"updated_at"`
}

func (ProposalState) TableName() string { return "proposal_state" }
