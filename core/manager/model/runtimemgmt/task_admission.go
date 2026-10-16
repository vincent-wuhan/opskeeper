package runtimemgmt

import "time"

// AdmissionDecision is the slot-before-claim admission snapshot. It is
// recorded at queue->admitted time so that even after admission limits
// shift, recovery can answer "what capacity was this task allowed
// under?" without re-running the policy.
//
// The decision itself is computed by §2.2 (admission service) and
// passed to the persistence layer; this file is just the durable
// shape. Spec scenario "No capacity is available" maps to
// Reason="no_capacity", Outcome=AdmissionOutcomeRejected.
type AdmissionDecision struct {
	// TaskID is the owning durable task; one row per task.
	// Composite primary key with TenantID is implicit via the table
	// naming, but TenantID is stored for filter clarity.
	TaskID string `gorm:"primaryKey;size:64"`

	TenantID string `gorm:"index;size:64;not null"`

	// Outcome is one of AdmissionOutcomeAdmitted or
	// AdmissionOutcomeRejected.
	Outcome AdmissionOutcome `gorm:"size:16;not null;index"`

	// Reason is a short human-readable explanation; one of
	// AdmissionReasonNoCapacity, AdmissionReasonRuntimeNotReady,
	// AdmissionReasonQuotaExceeded,
	// AdmissionReasonSafetyNotAllowed, AdmissionReasonPolicyDenied
	// or "" when admitted.
	Reason string `gorm:"size:64;not null;default:''"`

	// ServerLimit is the global Manager-side concurrency cap that
	// was active at admission time. Persisted for audit / diff.
	ServerLimit int `gorm:"not null;default:0"`

	// RuntimeLimit is the runtime-reported free-slot / capacity cap
	// that was active at admission time. Persisted alongside
	// ServerLimit; the smaller of the two was the effective gate.
	RuntimeLimit int `gorm:"not null;default:0"`

	// RuntimeSelector is the desired-runtime expression recorded at
	// admission. The actual RuntimeID is filled on claim, not here.
	// Free-form for v1; expected to be a label-selector expression
	// in later slices.
	RuntimeSelector string `gorm:"size:128;not null;default:''"`

	// DecidedAt is the wall-clock admission timestamp. Indexed so
	// backlog sweeps can target the oldest unadmitted rows first.
	DecidedAt time.Time `gorm:"index;not null"`

	// DecidedBy is "scheduler.v1" for the v1 admission service, or
	// "operator:<user_id>" for an override. Operators can override
	// out of capacity; the audit keeps both reasons.
	DecidedBy string `gorm:"size:64;not null;default:scheduler.v1"`
}

// TableName pins the schema name. Migrations MUST keep this exact
// name; the orchestrator reads by task_id.
func (AdmissionDecision) TableName() string { return "runtimemgmt_admission_decisions" }

// AdmissionOutcome enumerates the admit/reject states.
type AdmissionOutcome string

const (
	AdmissionOutcomeAdmitted AdmissionOutcome = "admitted"
	AdmissionOutcomeRejected AdmissionOutcome = "rejected"
)

// AdmissionReason constants. Rejection reasons are listed here so
// callers and dashboards can switch on them without parsing strings.
const (
	AdmissionReasonOK               = ""
	AdmissionReasonNoCapacity       = "no_capacity"
	AdmissionReasonRuntimeNotReady  = "runtime_not_ready"
	AdmissionReasonQuotaExceeded    = "quota_exceeded"
	AdmissionReasonSafetyNotAllowed = "safety_not_allowed"
	AdmissionReasonPolicyDenied     = "policy_denied"
)

// Validate confirms the decision is internally consistent. The
// admission service runs this check before persisting.
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
