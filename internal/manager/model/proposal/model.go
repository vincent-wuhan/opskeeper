// Package model 实现 Proposal 数据模型。
//
// 路径 A P1-2 阶段 1 任务 1.1 — 统一 Proposal 表 GORM 模型。
//
// 状态机扩展（与现有 approval + mutating_proposal 合并）：
//
//	pending → approved → executed
//	                  → paused → resumed → executing → executed
//	       → rejected
//	       → expired (24h default)
//
// 关键字段：
//   - state: 8 状态机
//   - severity: safe/mutating/dangerous（来自 ROADMAP D.4.3）
//   - sensitivity: 来自 P1-3 data-guard 的 5 级（联动）
//   - resume_token: ResumeToken BLOB（持久化）
//   - audit_hash_chain_*: hash-chained audit 字段
package model

import "time"

// ProposalState 状态机（8 状态）。
type ProposalState string

const (
	StatePending    ProposalState = "pending"
	StateApproved   ProposalState = "approved"
	StateRejected   ProposalState = "rejected"
	StateExpired    ProposalState = "expired"
	StateExecuted   ProposalState = "executed"
	StateRolledBack ProposalState = "rolled_back"
	StatePaused     ProposalState = "paused"
	StateResumed    ProposalState = "resumed"
)

// IsValid 校验状态是否合法。
func (s ProposalState) IsValid() bool {
	switch s {
	case StatePending, StateApproved, StateRejected, StateExpired,
		StateExecuted, StateRolledBack, StatePaused, StateResumed:
		return true
	}
	return false
}

// ProposalKind Proposal 类型（兼容旧 approval / mutating_proposal）。
type ProposalKind string

const (
	KindApprovalLegacy ProposalKind = "approval_legacy"
	KindMutatingLegacy ProposalKind = "mutating_legacy"
	KindHITLPause      ProposalKind = "hitl_pause"
	KindStandard       ProposalKind = "standard"
)

// Severity 风险等级（ROADMAP D.4.3）。
type Severity string

const (
	SeveritySafe      Severity = "safe"
	SeverityMutating  Severity = "mutating"
	SeverityDangerous Severity = "dangerous"
)

// Proposal 统一 Proposal 模型。
//
// 路径 A P1-2 阶段 1 任务 1.1 — 统一 approval + mutating_proposal。
type Proposal struct {
	ID                 uint64     `gorm:"primaryKey;autoIncrement" json:"id"`
	PublicID           string     `gorm:"size:64;not null;uniqueIndex" json:"public_id"`
	Kind               string     `gorm:"size:32;not null;index" json:"kind"`
	Title              string     `gorm:"size:256;not null" json:"title"`
	Summary            string     `gorm:"type:text" json:"summary"`
	PayloadJSON        string     `gorm:"type:text;not null" json:"payload_json"`
	SourceAgent        string     `gorm:"size:64" json:"source_agent,omitempty"`
	SessionID          string     `gorm:"size:64;index" json:"session_id,omitempty"`
	ProposedBy         uint64     `gorm:"not null;index" json:"proposed_by"`
	Severity           string     `gorm:"size:16;not null;default:'mutating';index" json:"severity"`
	Sensitivity        string     `gorm:"size:16;index" json:"sensitivity,omitempty"` // 来自 P1-3
	State              string     `gorm:"size:16;not null;default:'pending';index" json:"state"`
	PausedAt           *time.Time `json:"paused_at,omitempty"`
	ResumeToken        []byte     `json:"-"`
	DryRunDiffURL      string     `gorm:"size:512" json:"dry_run_diff_url,omitempty"`
	ExpiresAt          time.Time  `gorm:"not null;index" json:"expires_at"`
	DelegatedTo        string     `gorm:"size:64" json:"delegated_to,omitempty"`
	IMThreadID         string     `gorm:"size:128" json:"im_thread_id,omitempty"`
	AuditHashChainPrev string     `gorm:"size:64" json:"audit_hash_chain_prev,omitempty"`
	AuditHashChainSelf string     `gorm:"size:64;index" json:"audit_hash_chain_self"`
	ExecutedAt         *time.Time `json:"executed_at,omitempty"`
	ResultJSON         string     `gorm:"type:text" json:"result_json,omitempty"`
	CreatedAt          time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt          time.Time  `gorm:"not null" json:"updated_at"`
}

// TableName GORM 表名。
func (Proposal) TableName() string {
	return "proposal"
}

// ProposalStateAudit 状态机审计（hash-chained）。
type ProposalStateAudit struct {
	ID         uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	ProposalID uint64    `gorm:"not null;index" json:"proposal_id"`
	FromState  string    `gorm:"size:16" json:"from_state,omitempty"`
	ToState    string    `gorm:"size:16;not null" json:"to_state"`
	Actor      string    `gorm:"size:128;not null" json:"actor"` // user/im_callback/system
	Notes      string    `gorm:"type:text" json:"notes,omitempty"`
	Timestamp  time.Time `gorm:"not null" json:"timestamp"`
	PrevHash   string    `gorm:"size:64" json:"prev_hash"`
	SelfHash   string    `gorm:"size:64;not null;index" json:"self_hash"`
	CreatedAt  time.Time `gorm:"not null" json:"created_at"`
}

// TableName GORM 表名。
func (ProposalStateAudit) TableName() string {
	return "proposal_state_audit"
}

// ProposalStateStore 状态持久化（ResumeToken BLOB）。
type ProposalStateStore struct {
	ProposalID   uint64    `gorm:"primaryKey" json:"proposal_id"`
	ResumeToken  []byte    `json:"resume_token"`
	CallStack    string    `gorm:"type:text" json:"call_stack"`
	LLMContext   string    `gorm:"type:text" json:"llm_context"`
	DBRefs       string    `gorm:"type:text" json:"db_refs"` // JSON array of row refs
	DBRowVersion int64     `gorm:"not null;default:0" json:"db_row_version"`
	UpdatedAt    time.Time `gorm:"not null" json:"updated_at"`
}

// TableName GORM 表名。
func (ProposalStateStore) TableName() string {
	return "proposal_state"
}
