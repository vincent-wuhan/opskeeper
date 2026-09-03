// Package loop implements the closed-loop orchestrator (zero-manual-ops-loop · D1/D3).
//
// This file holds the inter-phase contract types — the structured
// payloads that flow between consecutive phases of the seven-phase
// state machine. Each contract is:
//
//  1. JSON-serializable (with explicit schema_version for forward- and
//     backward-compatibility, per the v1 / v1.1 / v2 evolution rule in
//     the orchestrator design §4.3).
//  2. Validated: ContractValidator (lives next to this file in the
//     follow-up PR) enforces required-field rules before the orchestrator
//     advances to the next phase. Phase failure on invalid schema is
//     recorded as event_type="phase_failed" with status="invalid_schema".
//  3. Double-write friendly: the dual-write window for RootCauseJSON
//     keeps a legacy summary_text field so v1 consumers do not break
//     while v2 consumers switch over.
//
// Cross-phase contract map:
//
//	investigated → critiqued   : RootCauseJSON
//	critiqued    → approved    : CritiqueScore
//	approved     → recovered   : ApprovalDecision
//	recovered    → postmortem  : VerifiedDelta
//	postmortem   (terminal)    : PostmortemDoc
package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// Contract schema versions. Bump according to the evolution rule
// (orchestrator design §4.3):
//   - non-breaking field addition/optional → minor bump (v1 → v1.1)
//   - breaking change (type/required/enum narrowing) → major bump (v1 → v2)
//   - v1 → v2 must run a dual-write period >= 30 days before v1 is dropped
const (
	ContractSchemaV1   = "v1"
	ContractSchemaV1_1 = "v1.1"
)

// RootCauseJSON is the structured output of the investigated phase.
// It is the primary handoff between investigator work and the
// critiqued phase; downstream approval / execution / postmortem all
// derive from this contract.
//
// Schema version is "v1" for the canonical form, or "v1.1" when the
// optionally-populated legacy_summary_text field is filled for the
// dual-write window (Day 1–30 of the rollout).
//
// Why a typed struct (not a map[string]any):
//   - Phase validation needs reflection-free required-field checks.
//   - Web SPA renders fields directly; struct tags keep the wire format
//     stable even when Go field names change.
//
// Required fields (validated by ContractValidator.ValidateRootCauseJSON):
//
//	schema_version, root_cause_object, confidence, evidence_chain (>= 1),
//	time_window, remediation_options (>= 1).
type RootCauseJSON struct {
	// SchemaVersion discriminates parser behaviour. Required; literal
	// "v1" or "v1.1".
	SchemaVersion string `json:"schema_version"`

	// RootCauseObject is the structured root-cause descriptor. We
	// keep it as a structured value (object with kind/summary/detail)
	// rather than a string so the renderer / approval UI can render
	// with type-specific iconography.
	//
	// Note: the design uses a nested object; we mirror that with a
	// pointer so an absent value (e.g. an early v1 feed) is rejected
	// by the validator rather than rendered as an empty object.
	RootCauseObject *RootCauseObject `json:"root_cause_object"`

	// Confidence is the investigator's self-reported confidence in
	// [0, 1]. Below 0.6 the critiqued-phase guard rejects the transition
	// to approved (configurable threshold, per orchestrator design §3.2).
	Confidence float64 `json:"confidence"`

	// EvidenceChain is the ordered list of evidence that supports
	// the root cause. The validator requires at least 1 entry and
	// caps at 50 (per design §4.1).
	EvidenceChain []EvidenceItem `json:"evidence_chain"`

	// TimeWindow is the time range the investigator searched.
	TimeWindow TimeWindow `json:"time_window"`

	// RemediationOptions are the candidate fix actions proposed by
	// the investigator. Validated to >= 1 and <= 10.
	RemediationOptions []RemediationOption `json:"remediation_options"`

	// LegacySummaryText is the opt-in fan-out field for the dual-write
	// window. v1.1 schema may populate it; v1 leaves it empty. Old
	// consumers (still reading summary_text) keep working unchanged.
	LegacySummaryText string `json:"legacy_summary_text,omitempty"`
}

// RootCauseObject is the inner typed root-cause descriptor.
type RootCauseObject struct {
	// Kind is a controlled enum — see the orchestrator design §4.1
	// YAML schema for the allowlist. The validator enforces it.
	Kind string `json:"kind"`

	// Summary is a 1-sentence operator-facing description.
	Summary string `json:"summary"`

	// Detail is type-specific structured payload (e.g. SQL id + duration
	// for pg.long_running_tx). Kept as a free-form map so type-specific
	// extensions do not require a schema bump.
	Detail map[string]any `json:"detail,omitempty"`
}

// EvidenceItem is one piece of evidence supporting the root cause.
type EvidenceItem struct {
	// Tool is the tool name that produced the evidence (e.g. "query_metrics",
	// "query_logs", "get_pg_stat").
	Tool string `json:"tool"`

	// Query is the optional query expression / PromQL / SQL string.
	Query string `json:"query,omitempty"`

	// Value holds the parsed observation (kept as interface{} because
	// evidence payloads vary widely — scalar metrics, lists of rows,
	// nested structs). Concrete consumers assert at the call site.
	Value any `json:"value,omitempty"`

	// Count is the row / sample count associated with the evidence,
	// when applicable (e.g. "48 sessions matched").
	Count int `json:"count,omitempty"`

	// Timestamp is the wall-clock time the observation was sampled.
	Timestamp time.Time `json:"timestamp"`
}

// TimeWindow is the inclusive start / exclusive end of the search
// interval for the investigation.
type TimeWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// RemediationOption is one candidate fix action. The approval phase
// uses Risk to decide whether to auto-approve (safe), require hitl
// pause (mutating), or require two-person sign-off (dangerous).
type RemediationOption struct {
	// Action is the tool method name (e.g. "pg.terminate_long_tx").
	Action string `json:"action"`

	// Target is the resource locator the action targets (e.g.
	// "postgres://prod-cluster-1" or "host:i-0abc123").
	Target string `json:"target"`

	// Risk is one of "safe" / "mutating" / "dangerous". Validated
	// against the design §4.1 enum.
	Risk string `json:"risk"`

	// AutoApprove is true when the action is pre-approved by policy
	// (e.g. session kill on a known-safe PG cluster). The orchestrator
	// honors this on the approved → recovered transition.
	AutoApprove bool `json:"auto_approve"`
}

// CritiqueScore is the structured output of the critiqued phase.
// Consumed by the approved phase to decide whether to enter the
// approval flow or escalate to HITL.
type CritiqueScore struct {
	// SchemaVersion is "v1".
	SchemaVersion string `json:"schema_version"`

	// Verdict is "pass" / "fail" / "needs_info". The approved-phase
	// guard rejects "fail" entirely and "needs_info" is treated as
	// a HITL pause.
	Verdict string `json:"verdict"`

	// Score is the overall critic score in [0, 1]; mirrors Verdict
	// but allows numeric thresholds (e.g. > 0.6 → proceed).
	Score float64 `json:"score"`

	// Reasons are the issue list (empty when Verdict == "pass").
	Reasons []string `json:"reasons"`

	// Model is the model id used to produce the score (for audit).
	Model string `json:"model"`

	// LatencyMs is the critic latency in milliseconds (for SLO tracking).
	LatencyMs int `json:"latency_ms"`
}

// VerifiedDelta is the structured output of the recovered phase.
// Compares the post-fix observation against the pre-incident baseline.
// Drives the recovered → postmortem transition (passed=true) and the
// recovered → approved rollback (passed=false; see orchestrator
// rollback guard).
type VerifiedDelta struct {
	// SchemaVersion is "v1".
	SchemaVersion string `json:"schema_version"`

	// Passed is true when *all* metrics in Deltas are within tolerance.
	Passed bool `json:"passed"`

	// FailedMetrics is the subset of metric names that exceeded the
	// tolerance. Empty when Passed=true. Web UI uses this to render
	// "did not recover" callouts.
	FailedMetrics []string `json:"failed_metrics,omitempty"`

	// Deltas is the per-metric relative change: |current - baseline| / baseline.
	// Keyed by metric name (e.g. "pg.connections.idle", "host.cpu.user").
	Deltas map[string]float64 `json:"deltas"`

	// SampleSize is the number of observation points used for the
	// post-fix window. Small samples (< 3) trigger a confidence
	// warning in the postmortem.
	SampleSize int `json:"sample_size"`

	// Tolerance is the configured pass threshold (default 0.15).
	// Captured here so the postmortem can render the exact rule used.
	Tolerance float64 `json:"tolerance"`

	// RetryCount is the recovered→approved rollback count at the
	// time of this verification. Mirrors LoopState.RetryCount.
	RetryCount int `json:"retry_count"`

	// WarningLevel is "pass" / "warn" / "fail". "warn" means within
	// tolerance but with low sample size or noisy data.
	WarningLevel string `json:"warning_level"`
}

// PostmortemDoc is the terminal-phase deliverable. Written by the
// postmortem phase after Verify + redact + git artifact commit.
type PostmortemDoc struct {
	// SchemaVersion is "v1".
	SchemaVersion string `json:"schema_version"`

	// IncidentID is the owning incident.
	IncidentID string `json:"incident_id"`

	// ConversationID is set when the loop was entered from a chat
	// promote (D13–D16). Empty for alert-entry incidents.
	ConversationID string `json:"conversation_id,omitempty"`

	// Markdown is the rendered postmortem body. Multiple sections
	// per design §D5 (timeline / root cause / impact / remediation /
	// prevention / source commits / references).
	Markdown string `json:"markdown"`

	// GeneratedAt is the wall-clock time the markdown was rendered.
	GeneratedAt time.Time `json:"generated_at"`

	// Sources is the list of source artifacts the markdown was
	// synthesized from. Used by the postmortem Web UI to render
	// "View source" links.
	//
	// Typical values: "RootCauseJSON", "CritiqueScore", "VerifiedDelta",
	//                "toolreplay", "ConversationEvidence".
	Sources []string `json:"sources"`
}

// ErrInvalidSchema is returned by Validate* functions when the contract
// is missing required fields or carries an unsupported schema version.
// The orchestrator wraps this with %w when writing phase_failed events
// so the audit log preserves the validation failure reason.
var ErrInvalidSchema = errors.New("loop: invalid contract schema")

// ValidateRootCauseJSON enforces the RootCauseJSON contract invariants.
// Called by the orchestrator before advancing from investigated to
// critiqued.
//
// Required fields (per design §4.1):
//   - schema_version ∈ {"v1", "v1.1"}
//   - root_cause_object.kind + .summary
//   - confidence ∈ [0, 1]
//   - evidence_chain >= 1 entry, <= 50 entries
//   - time_window.start < time_window.end
//   - remediation_options >= 1 entry, <= 10 entries
//   - each remediation_options[].risk ∈ {"safe","mutating","dangerous"}
func ValidateRootCauseJSON(c *RootCauseJSON) error {
	if c == nil {
		return fmt.Errorf("%w: nil RootCauseJSON", ErrInvalidSchema)
	}
	if c.SchemaVersion != ContractSchemaV1 && c.SchemaVersion != ContractSchemaV1_1 {
		return fmt.Errorf("%w: root_cause_json schema_version=%q (want v1 or v1.1)", ErrInvalidSchema, c.SchemaVersion)
	}
	if c.RootCauseObject == nil {
		return fmt.Errorf("%w: root_cause_object missing", ErrInvalidSchema)
	}
	if c.RootCauseObject.Kind == "" {
		return fmt.Errorf("%w: root_cause_object.kind missing", ErrInvalidSchema)
	}
	if c.RootCauseObject.Summary == "" {
		return fmt.Errorf("%w: root_cause_object.summary missing", ErrInvalidSchema)
	}
	if c.Confidence < 0 || c.Confidence > 1 {
		return fmt.Errorf("%w: confidence=%v out of [0,1]", ErrInvalidSchema, c.Confidence)
	}
	if len(c.EvidenceChain) < 1 {
		return fmt.Errorf("%w: evidence_chain empty (need >= 1)", ErrInvalidSchema)
	}
	if len(c.EvidenceChain) > 50 {
		return fmt.Errorf("%w: evidence_chain has %d entries (max 50)", ErrInvalidSchema, len(c.EvidenceChain))
	}
	if !c.TimeWindow.End.After(c.TimeWindow.Start) {
		return fmt.Errorf("%w: time_window end <= start", ErrInvalidSchema)
	}
	if len(c.RemediationOptions) < 1 {
		return fmt.Errorf("%w: remediation_options empty (need >= 1)", ErrInvalidSchema)
	}
	if len(c.RemediationOptions) > 10 {
		return fmt.Errorf("%w: remediation_options has %d entries (max 10)", ErrInvalidSchema, len(c.RemediationOptions))
	}
	for i, ro := range c.RemediationOptions {
		if ro.Risk != "safe" && ro.Risk != "mutating" && ro.Risk != "dangerous" {
			return fmt.Errorf("%w: remediation_options[%d].risk=%q", ErrInvalidSchema, i, ro.Risk)
		}
	}
	return nil
}

// ValidateCritiqueScore enforces the CritiqueScore contract invariants.
// Called before advancing from critiqued to approved.
func ValidateCritiqueScore(c *CritiqueScore) error {
	if c == nil {
		return fmt.Errorf("%w: nil CritiqueScore", ErrInvalidSchema)
	}
	if c.SchemaVersion != ContractSchemaV1 {
		return fmt.Errorf("%w: critique_score schema_version=%q (want v1)", ErrInvalidSchema, c.SchemaVersion)
	}
	if c.Verdict != "pass" && c.Verdict != "fail" && c.Verdict != "needs_info" {
		return fmt.Errorf("%w: critique_score verdict=%q", ErrInvalidSchema, c.Verdict)
	}
	if c.Score < 0 || c.Score > 1 {
		return fmt.Errorf("%w: critique_score score=%v out of [0,1]", ErrInvalidSchema, c.Score)
	}
	return nil
}

// ValidateVerifiedDelta enforces the VerifiedDelta contract invariants.
// Called before advancing from recovered to postmortem (and consumed by
// the rollback guard for the recovered → approved path).
func ValidateVerifiedDelta(c *VerifiedDelta) error {
	if c == nil {
		return fmt.Errorf("%w: nil VerifiedDelta", ErrInvalidSchema)
	}
	if c.SchemaVersion != ContractSchemaV1 {
		return fmt.Errorf("%w: verified_delta schema_version=%q (want v1)", ErrInvalidSchema, c.SchemaVersion)
	}
	if c.Tolerance <= 0 || c.Tolerance > 1 {
		return fmt.Errorf("%w: verified_delta tolerance=%v out of (0,1]", ErrInvalidSchema, c.Tolerance)
	}
	if c.SampleSize < 0 {
		return fmt.Errorf("%w: verified_delta sample_size=%d negative", ErrInvalidSchema, c.SampleSize)
	}
	switch c.WarningLevel {
	case "pass", "warn", "fail":
	default:
		return fmt.Errorf("%w: verified_delta warning_level=%q", ErrInvalidSchema, c.WarningLevel)
	}
	if !c.Passed && len(c.FailedMetrics) == 0 {
		return fmt.Errorf("%w: verified_delta passed=false but failed_metrics empty", ErrInvalidSchema)
	}
	return nil
}

// ValidatePostmortemDoc enforces the PostmortemDoc contract invariants.
// Called by the postmortem phase before sealing the artifact commit.
func ValidatePostmortemDoc(p *PostmortemDoc) error {
	if p == nil {
		return fmt.Errorf("%w: nil PostmortemDoc", ErrInvalidSchema)
	}
	if p.SchemaVersion != ContractSchemaV1 {
		return fmt.Errorf("%w: postmortem_doc schema_version=%q (want v1)", ErrInvalidSchema, p.SchemaVersion)
	}
	if p.IncidentID == "" {
		return fmt.Errorf("%w: postmortem_doc incident_id missing", ErrInvalidSchema)
	}
	if p.Markdown == "" {
		return fmt.Errorf("%w: postmortem_doc markdown empty", ErrInvalidSchema)
	}
	if len(p.Sources) == 0 {
		return fmt.Errorf("%w: postmortem_doc sources empty", ErrInvalidSchema)
	}
	return nil
}

// MarshalContract is the canonical write-time encoder used by the
// contract repository. It picks the storage backend based on the
// payload size and returns the encoded JSON bytes plus the backend
// selector. Payloads > loopmodel.InlinePayloadMaxBytes are marked
// for OSS offloading (the OSS upload itself is performed by the
// repo layer).
//
// Returns (payload, storageBackend, sizeBytes, error).
func MarshalContract(c any) (string, string, int, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", "", 0, fmt.Errorf("marshal contract: %w", err)
	}
	backend := loopmodel.StorageBackendDB
	if len(raw) > loopmodel.InlinePayloadMaxBytes {
		backend = loopmodel.StorageBackendOSS
	}
	return string(raw), backend, len(raw), nil
}
