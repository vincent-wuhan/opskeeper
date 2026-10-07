package ports

import (
	"context"
	"encoding/json"
	"time"
)

// AuditAction names what happened. The set is closed so the ledger schema
// does not drift as features ship.
type AuditAction string

const (
	ActionToolCall        AuditAction = "tool_call"
	ActionToolBlocked     AuditAction = "tool_blocked"
	ActionToolFailed      AuditAction = "tool_failed"
	ActionApprovalRequest AuditAction = "approval_requested"
	ActionApprovalGrant   AuditAction = "approval_granted"
	ActionApprovalDeny    AuditAction = "approval_denied"
	ActionAgentTurn       AuditAction = "agent_turn"
	ActionModelCall       AuditAction = "model_call"
	ActionPluginInstall   AuditAction = "plugin_installed"
	ActionPluginRemove    AuditAction = "plugin_removed"
	ActionPluginLoad      AuditAction = "plugin_loaded"
	ActionProposalCreate  AuditAction = "proposal_created"
	ActionRecoveryApply   AuditAction = "recovery_applied"
)

// AuditEntry is one record in the append-only ledger.
//
// The ledger is an HMAC chain: each entry carries the digest of its
// predecessor, so removing or editing any entry breaks verification from
// that point forward. Only the host writes it.
type AuditEntry struct {
	At      time.Time   `json:"at"`
	Actor   string      `json:"actor"`
	Action  AuditAction `json:"action"`
	Target  string      `json:"target"`
	Outcome string      `json:"outcome"`
	Class   string      `json:"class,omitempty"`
	// Detail carries action-specific structured context. It must never
	// contain credentials or raw prompt content.
	Detail json.RawMessage `json:"detail,omitempty"`
	// PrevHash and Hash chain the entry. They are set by the sink.
	PrevHash string `json:"prev_hash,omitempty"`
	Hash     string `json:"hash,omitempty"`
}

// AuditSink is the host's write path to the ledger.
//
// This port is deliberately not reachable from a plugin. A plugin declares
// that it emits audit-worthy activity; the host derives the entry from its
// own gate events, so a plugin can neither forge nor suppress a record.
type AuditSink interface {
	// Record appends one entry. It must be safe for concurrent use and
	// must not block on network I/O: an unavailable ledger degrades to an
	// in-memory buffer plus a loud signal, never to a silent drop.
	Record(ctx context.Context, entry AuditEntry) error
	// Verify walks the chain and reports the first entry whose hash does
	// not match, or nil when the whole chain is intact.
	Verify(ctx context.Context) error
}

// AuditEmitter is the narrow read-side a plugin or adapter is given. It
// carries no method that can write, so holding one cannot forge a record.
type AuditEmitter interface {
	// Emit reports that something noteworthy happened. The host decides
	// whether it becomes a ledger entry.
	Emit(ctx context.Context, action AuditAction, target, outcome string, detail any)
}
