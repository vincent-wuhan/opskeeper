// Package approval is the persistence entity for the human propose-confirm
// inbox (HLD-017). It is a GENERAL approval primitive — one row per
// dangerous action awaiting a human decision, regardless of producer
// (agent cloud-shell command, restart_service, a flow approval node …).
//
// Strictly additive: a brand-new `approvals` table + package. It does NOT
// touch the existing chat_mutating_proposals table or any live path; an
// action only flows through here when a producer explicitly Proposes.
package approval

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Approval is one pending/decided dangerous action.
type Approval struct {
	ID string `gorm:"primaryKey;type:char(36);column:id" json:"id"`

	// Kind routes execution on approve (biz registers an executor per kind),
	// e.g. "shell_command", "restart_service". Also the UI category.
	Kind string `gorm:"size:64;not null;index" json:"kind"`

	// Title is a one-line human label ("terraform apply (tencent-prod)").
	Title string `gorm:"size:255;not null" json:"title"`
	// Summary is a short preview the inbox shows without opening details
	// (e.g. a terraform plan diff summary).
	Summary string `gorm:"type:text" json:"summary"`
	// PayloadJSON is the opaque action spec the executor needs to run it
	// (command, runner, credential ref, workdir …). Producer-defined.
	PayloadJSON string `gorm:"type:text;not null" json:"payload"`

	// Source records where the proposal came from for the UI: "agent"
	// (chat) / "flow" (an approval node). SessionID optionally links back.
	Source    string `gorm:"size:32;not null;default:agent" json:"source"`
	SessionID string `gorm:"size:64;index" json:"session_id,omitempty"`

	// Status lifecycle. Constants below.
	Status string `gorm:"size:16;not null;default:pending;index" json:"status"`

	// ProposedBy is the user who triggered the proposal (chat owner / flow
	// author). ApprovedBy is the human who decided (NULL until decided).
	ProposedBy uint64  `gorm:"not null;default:0" json:"proposed_by"`
	ApprovedBy *uint64 `gorm:"" json:"approved_by,omitempty"`

	// SignersJSON is the list of people who have signed so far, as a JSON
	// array of {"user_id","role","at"}.
	//
	// It exists because the two columns above are single-valued: ADR-019's
	// dual sign has nowhere to put a second signature, which is why
	// TestDualSignCannotBeEnforcedBecauseNowhereStoresTwoSigners was written
	// and why the boot log said the policy was UNENFORCED. A row that has
	// been signed once and is still pending carries its first signer here and
	// its ApprovedBy NULL, so "approved by" and "signed by" never disagree.
	SignersJSON *string `gorm:"column:signers_json;type:text" json:"signers,omitempty"`

	// RiskClass and BlastRadius are the producer's own statement of how far
	// this action reaches: "read" / "write" / "destructive", and
	// "pod" / "namespace" / "cluster" / "tenant_wide".
	//
	// They were already being produced — the agent tool call carries both,
	// ADR-019 keys on both — and they were being written into PayloadJSON,
	// where no policy can read them. That is the other half of why dual sign
	// was unenforced: not only could a row not hold two signers, it could not
	// say what it was asking to be signed for.
	RiskClass   string `gorm:"size:32;not null;default:'';index" json:"risk_class,omitempty"`
	BlastRadius string `gorm:"size:32;not null;default:''" json:"blast_radius,omitempty"`

	// Target is the resource the action reaches — the bare id the tool call
	// named (`web-1`), not a `type:id` pair.
	//
	// It is a column because the escalation that raises a row's risk class
	// looks the id up in the sensitivity labels, and the labels are keyed by
	// (type, id). Without this column the only place the target existed was
	// inside the opaque PayloadJSON, which is the same mistake decision 362
	// already corrected once for risk_class.
	Target string `gorm:"size:128;not null;default:'';index" json:"target,omitempty"`

	// Reason is the approver's note / reject rationale. ResultJSON holds
	// the execution outcome after an approve runs the action.
	Reason     *string `gorm:"type:text" json:"reason,omitempty"`
	ResultJSON *string `gorm:"type:text" json:"result,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
	ExecutedAt *time.Time `json:"executed_at,omitempty"`
}

// TableName pins the schema name.
func (Approval) TableName() string { return "approvals" }

// BeforeCreate fills a UUID when unset.
func (a *Approval) BeforeCreate(*gorm.DB) error {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	return nil
}

// Status constants.
const (
	StatusPending  = "pending"  // awaiting human decision
	StatusApproved = "approved" // approved, executor not run / no executor
	StatusRejected = "rejected" // human said no
	StatusExecuted = "executed" // approved + executor ran successfully
	StatusFailed   = "failed"   // approved + executor errored
)

// Source constants.
const (
	SourceAgent = "agent"
	SourceFlow  = "flow"
)
