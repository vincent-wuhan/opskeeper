package repairpreview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Decision string

const (
	DecisionPass   Decision = "PASS"
	DecisionFail   Decision = "FAIL"
	DecisionReject Decision = "REJECTED_BY_PREVIEW"
)

type Run struct {
	ID                  string      `json:"id"`
	TenantID            string      `json:"tenant_id"`
	IncidentID          string      `json:"incident_id"`
	ScenarioID          string      `json:"scenario_id,omitempty"`
	IdempotencyKey      string      `json:"idempotency_key,omitempty"`
	TargetFingerprint   string      `json:"target_fingerprint,omitempty"`
	BindingFingerprint  string      `json:"binding_fingerprint,omitempty"`
	BranchPrefix        string      `json:"branch_prefix"`
	SeedFingerprint     string      `json:"seed_fingerprint"`
	WorkloadFingerprint string      `json:"workload_fingerprint"`
	WorkloadRevision    string      `json:"workload_revision"`
	ControlledLoad      bool        `json:"controlled_load"`
	IsolationBoundary   string      `json:"isolation_boundary"`
	Status              string      `json:"status"`
	StartedAt           time.Time   `json:"started_at"`
	FinishedAt          time.Time   `json:"finished_at"`
	ErrorSummary        string      `json:"error_summary"`
	ArtifactRef         string      `json:"artifact_ref"`
	Candidates          []Candidate `json:"candidates"`
	CreatedAt           time.Time   `json:"created_at"`
	UpdatedAt           time.Time   `json:"updated_at"`
}

type runBindingFingerprintMaterial struct {
	RunID             string `json:"run_id"`
	TenantID          string `json:"tenant_id"`
	IncidentID        string `json:"incident_id"`
	ScenarioID        string `json:"scenario_id"`
	IdempotencyKey    string `json:"idempotency_key"`
	TargetFingerprint string `json:"target_fingerprint"`
}

func (binding WorkloadBinding) Fingerprint() string {
	material, err := json.Marshal(runBindingFingerprintMaterial{
		RunID: binding.RunID, TenantID: binding.TenantID, IncidentID: binding.IncidentID,
		ScenarioID: binding.ScenarioID, IdempotencyKey: binding.IdempotencyKey,
		TargetFingerprint: binding.TargetFingerprint,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(material)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func RunBindingMatches(run Run, scenarioID, idempotencyKey, targetFingerprint string) bool {
	binding := WorkloadBinding{
		RunID: run.ID, TenantID: run.TenantID, IncidentID: run.IncidentID,
		ScenarioID: scenarioID, IdempotencyKey: idempotencyKey,
		TargetFingerprint: targetFingerprint,
	}
	return run.ScenarioID == scenarioID &&
		run.IdempotencyKey == idempotencyKey &&
		run.TargetFingerprint == targetFingerprint &&
		run.BindingFingerprint == binding.Fingerprint()
}

type Candidate struct {
	ID                string   `json:"id"`
	RunID             string   `json:"run_id"`
	TenantID          string   `json:"tenant_id"`
	IncidentID        string   `json:"incident_id"`
	CandidateID       string   `json:"candidate_id"`
	Name              string   `json:"name"`
	Kind              string   `json:"kind"`
	Action            string   `json:"action"`
	ChangeSummary     string   `json:"change_summary"`
	Branch            string   `json:"branch"`
	ResultChecksum    string   `json:"result_checksum"`
	Consistent        bool     `json:"consistent"`
	AverageLatencyMS  float64  `json:"average_latency_ms"`
	MedianLatencyMS   float64  `json:"median_latency_ms"`
	P95LatencyMS      float64  `json:"p95_latency_ms"`
	SampleCount       int      `json:"sample_count"`
	TPS               float64  `json:"tps"`
	ErrorCount        int      `json:"error_count"`
	WriteImpact       string   `json:"write_impact"`
	StorageDeltaBytes int64    `json:"storage_delta_bytes"`
	BusinessProbePass bool     `json:"business_probe_pass"`
	Decision          Decision `json:"decision"`
	RejectionReason   string   `json:"rejection_reason"`
}

const (
	BaselineCandidateID = "baseline"
	BaselineKind        = "baseline"
	BaselineAction      = "baseline"
)

func (candidate Candidate) IsBaseline() bool {
	return candidate.CandidateID == BaselineCandidateID || candidate.Kind == BaselineKind
}

func (run Run) Validate() error {
	if run.TenantID == "" {
		return errors.New("repair preview: tenant id is required")
	}
	if run.IncidentID == "" {
		return errors.New("repair preview: incident id is required")
	}
	if run.ID == "" {
		return errors.New("repair preview: id is required")
	}
	if run.BranchPrefix == "" {
		return errors.New("repair preview: branch prefix is required")
	}
	if run.SeedFingerprint == "" {
		return errors.New("repair preview: seed fingerprint is required")
	}
	if run.WorkloadFingerprint == "" || run.WorkloadRevision == "" {
		return errors.New("repair preview: workload fingerprint and revision are required")
	}
	if !run.ControlledLoad {
		return errors.New("repair preview: controlled load is required")
	}
	if run.IsolationBoundary == "" {
		return errors.New("repair preview: isolation boundary is required")
	}
	if run.Status == "" {
		return errors.New("repair preview: status is required")
	}
	if run.StartedAt.IsZero() {
		return errors.New("repair preview: started_at is required")
	}
	if !run.FinishedAt.IsZero() && run.FinishedAt.Before(run.StartedAt) {
		return errors.New("repair preview: finished_at precedes started_at")
	}
	for _, candidate := range run.Candidates {
		if candidate.TenantID != run.TenantID || candidate.IncidentID != run.IncidentID || candidate.RunID != run.ID {
			return errors.New("repair preview: candidate bindings must match run")
		}
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("candidate %s: %w", candidate.CandidateID, err)
		}
	}
	return nil
}

func (candidate Candidate) Validate() error {
	if candidate.ID == "" {
		return errors.New("id is required")
	}
	if candidate.RunID == "" || candidate.TenantID == "" || candidate.IncidentID == "" || candidate.CandidateID == "" {
		return errors.New("run, tenant, incident, and candidate ids are required")
	}
	if candidate.Name == "" || candidate.Kind == "" || candidate.Action == "" || candidate.ChangeSummary == "" || candidate.Branch == "" {
		return errors.New("name, kind, action, change summary, and branch are required")
	}
	if candidate.AverageLatencyMS < 0 || candidate.MedianLatencyMS < 0 || candidate.P95LatencyMS < 0 {
		return errors.New("latency metrics cannot be negative")
	}
	if candidate.SampleCount <= 0 {
		return errors.New("sample count must be positive")
	}
	if candidate.TPS < 0 || candidate.ErrorCount < 0 || candidate.StorageDeltaBytes < 0 {
		return errors.New("throughput, error, and storage metrics cannot be negative")
	}
	if candidate.ResultChecksum == "" {
		return errors.New("result checksum is required")
	}
	if candidate.WriteImpact == "" {
		return errors.New("write impact is required")
	}
	return nil
}

func Evaluate(candidate Candidate) (Decision, string) {
	if err := candidate.Validate(); err != nil {
		return DecisionFail, err.Error()
	}
	if candidate.IsBaseline() {
		return DecisionPass, ""
	}
	if !candidate.BusinessProbePass {
		return DecisionReject, "business probe failed"
	}
	if !candidate.Consistent {
		return DecisionReject, "checksum divergence from baseline"
	}
	if !allowedWriteImpact(candidate.WriteImpact) {
		return DecisionReject, "write impact exceeds preview boundary: " + candidate.WriteImpact
	}
	return DecisionPass, ""
}

func allowedWriteImpact(writeImpact string) bool {
	switch writeImpact {
	case "none", "preview_only", "bounded":
		return true
	default:
		return false
	}
}

var sensitiveErrorPattern = regexp.MustCompile(`(?i)(password|passwd|token|secret|api[_-]?key)\s*=\s*[^\s,;]+`)
var connectionSecretPattern = regexp.MustCompile(`(?i)((?:postgres(?:ql)?|mysql|redis|https?)://)[^:/\s]+:[^@\s]+@`)

func SanitizeErrorSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if len(summary) > 2000 {
		summary = summary[:2000]
	}
	summary = connectionSecretPattern.ReplaceAllString(summary, "$1[redacted]@")
	return sensitiveErrorPattern.ReplaceAllString(summary, "$1=[redacted]")
}
