package runtimemgmt

import "time"

// TaskResult is the terminal record of one task. There is at most
// one row per (TaskID, AttemptNumber); the row for the LAST attempt
// wins for Dashboard. The SanitizerStatus field is REQUIRED for
// persistence: a terminal result must declare whether output was
// sanitized, skipped (with rationale), or unclassified-by-error.
//
// Spec scenarios "Task reaches terminal state" and "Credential-bearing
// output is persisted" both reference this row.
type TaskResult struct {
	// TaskID + AttemptNumber composite primary key. The current
	// attempt's row is the dashboard surface; older attempts'
	// results remain queryable for replay.
	TaskID        string `gorm:"primaryKey;size:64"`
	AttemptNumber int    `gorm:"primaryKey"`

	TenantID string `gorm:"index;size:64;not null"`

	// Status mirrors the eventual task state (succeeded / failed /
	// cancelled). Stored on the result so readers can answer
	// "what happened?" without joining the task row.
	Status TaskState `gorm:"size:16;not null;index"`

	// EndedAt is the wall-clock completion timestamp.
	EndedAt time.Time `gorm:"index;not null"`

	// FailureClass is empty on success; otherwise it carries the
	// same taxonomy AttemptRecord.FailureClass. Duplicated so a
	// terminal-failure inspector only needs to read this row.
	FailureClass FailureClass `gorm:"size:32;not null;default:''"`

	// ErrorMessage is the human-readable error string on terminal
	// failure. Empty on success. Sanitized before persistence
	// (SanitizerStatus below).
	ErrorMessage string `gorm:"type:text"`

	// OutputRef points to the durable output payload (OSS key,
	// workspace row, evidence id). Free-form; the runtime owns the
	// semantics. Empty when the task produced no output.
	OutputRef string `gorm:"size:255;not null;default:''"`

	// OutputSize is the byte-size of OutputRef. Zero is valid (no
	// output). Stored explicitly so auditors can estimate evidence
	// size without re-fetching the ref.
	OutputSize int64 `gorm:"not null;default:0"`

	// SanitizerStatus is the classifier verdict for the Result /
	// ErrorMessage / OutputRef. Required at write time.
	SanitizerStatus SanitizerStatus `gorm:"size:16;not null"`

	// SanitizerNotes is optional human-readable rationale for a
	// skip / partial verdict. Required to be non-empty when
	// SanitizerStatus = SanitizerStatusSkipped so auditors know
	// why the chain broke. Sanitized itself before persistence.
	SanitizerNotes string `gorm:"type:text"`

	// OutputDigest is the sha256 of the output payload (or empty
	// string when there is no output). Used by replay tools to
	// detect "same task, different bytes" without downloading.
	OutputDigest string `gorm:"size:64;not null;default:''"`

	// CompletedBy is the runtime that wrote the result. Mirrors
	// Claim.ClaimedBy at write time.
	CompletedBy string `gorm:"size:128;not null;default:''"`

	// PodIdempotencyFingerprint is the IdempotencyContract fingerprint
	// the attempt was working against. Copied at write time so a
	// post-mortem can verify the result was produced under the
	// correct side-effect contract even after the original contract
	// row has been GC'd.
	PodIdempotencyFingerprint string `gorm:"size:64;not null;default:''"`
}

// TableName pins the schema name. Migrations MUST keep this exact
// name; the orchestrator reads by (task_id, attempt_number).
func (TaskResult) TableName() string { return "runtimemgmt_task_results" }

// SanitizerStatus is the closed taxonomy for the result-output
// redaction gate. The control plane refuses to persist a TaskResult
// whose SanitizerStatus is empty or otherwise unrecognised; this is
// the security boundary that prevents raw credential material
// entering evidence / telemetry / UI.
type SanitizerStatus string

const (
	// SanitizerStatusApplied means the worker (or the sanitizer
	// middleware) ran the redaction pipeline before persistence.
	SanitizerStatusApplied SanitizerStatus = "applied"

	// SanitizerStatusSkipped means no sanitization was performed;
	// SanitizerNotes MUST explain why. Whitelisted scenarios
	// (read-only / non-credential output) document their notes.
	SanitizerStatusSkipped SanitizerStatus = "skipped"

	// SanitizerStatusFailed means the sanitizer pipeline errored;
	// the task is considered insecure and the orchestrator blocks
	// persistence until the worker re-runs sanitization or the
	// operator overrides via audit.
	SanitizerStatusFailed SanitizerStatus = "failed"
)

// SanitizerStatusValues is the closed enumeration.
var SanitizerStatusValues = []SanitizerStatus{
	SanitizerStatusApplied,
	SanitizerStatusSkipped,
	SanitizerStatusFailed,
}

// IsSanitizerStatus reports whether s is a recognized status.
func IsSanitizerStatus(s SanitizerStatus) bool {
	for _, v := range SanitizerStatusValues {
		if v == s {
			return true
		}
	}
	return false
}

// Validate confirms the TaskResult is internally consistent. The
// failure API runs this before persisting.
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
