package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	realStateSchemaVersion      = "v1"
	realHITLSchemaVersion       = "opskeeper.io/agentteams-hitl-evidence/v1"
	realMCPSchemaVersion        = "opskeeper.io/mcp-role-call-evidence/v1"
	realFixtureSchemaVersion    = "opskeeper.io/fixture-recovery-evidence/v1"
	realPostmortemSchemaVersion = "opskeeper.io/postmortem-evidence/v1"
	realAgentTeamsExecutionMode = "real_agentteams"
)

var (
	traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	uuidPattern    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// RealAgentTeamsEvidencePaths names every evidence artifact required by the
// strict evaluator. No path is optional and no missing artifact may fall back
// to the synthetic dry-run timeline.
type RealAgentTeamsEvidencePaths struct {
	State         string
	HITL          string
	MCP           string
	FixtureBefore string
	FixtureAfter  string
	Postmortem    string
}

type realStateEvidence struct {
	SchemaVersion string                           `json:"schema_version"`
	Tasks         map[string]realStateTaskEvidence `json:"tasks"`
}

type realStateTaskEvidence struct {
	IncidentID         string                       `json:"incident_id"`
	ProjectID          string                       `json:"project_id"`
	MatrixRoomID       string                       `json:"matrix_room_id"`
	ProjectRoomID      string                       `json:"project_room_id,omitempty"`
	Type               string                       `json:"type"`
	Status             string                       `json:"status"`
	AssignedTo         string                       `json:"assigned_to"`
	CreatedAt          time.Time                    `json:"created_at"`
	UpdatedAt          time.Time                    `json:"updated_at"`
	Phases             map[string]realStatePhase    `json:"phases"`
	HITLDecisions      []realStateHITLDecision      `json:"hitl_decisions"`
	AuditRefs          []realStateAuditRef          `json:"audit_refs"`
	EscalationDelivery *realStateEscalationDelivery `json:"escalation_delivery,omitempty"`
}

type realStatePhase struct {
	AssignedTo        string     `json:"assigned_to"`
	Status            string     `json:"status"`
	ApprovalRequestID string     `json:"approval_request_id,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	CompletedAt       *time.Time `json:"completed_at"`
}

type realStateHITLDecision struct {
	Admin             string    `json:"admin"`
	Decision          string    `json:"decision"`
	ApprovalRequestID string    `json:"approval_request_id,omitempty"`
	DecidedAt         time.Time `json:"decided_at"`
	Reason            string    `json:"reason,omitempty"`
}

type realStateEscalationDelivery struct {
	IncidentID        string    `json:"incident_id"`
	MatrixRoomID      string    `json:"matrix_room_id"`
	ApprovalRequestID string    `json:"approval_request_id"`
	Admin             string    `json:"admin"`
	Reason            string    `json:"reason"`
	DecidedAt         time.Time `json:"decided_at"`
	Status            string    `json:"status"`
	Attempts          int       `json:"attempts"`
	LastError         string    `json:"last_error,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type realStateAuditRef struct {
	AuditLogID string `json:"audit_log_id"`
	TraceID    string `json:"trace_id"`
	EventID    string `json:"event_id,omitempty"`
}

type realHITLEvidence struct {
	SchemaVersion string                  `json:"schema_version"`
	ExecutionMode string                  `json:"execution_mode"`
	DryRun        bool                    `json:"dry_run"`
	IncidentID    string                  `json:"incident_id"`
	TraceID       string                  `json:"trace_id"`
	RoomID        string                  `json:"room_id"`
	ProposalID    string                  `json:"proposal_id"`
	PayloadHash   string                  `json:"payload_hash"`
	Action        string                  `json:"action"`
	Resource      string                  `json:"resource"`
	Parameters    realHITLParameters      `json:"parameters"`
	MatrixEvent   realMatrixApprovalEvent `json:"matrix_event"`
}

type realHITLParameters struct {
	Command           string `json:"command"`
	IncidentID        string `json:"incident_id"`
	FixtureManifestID string `json:"fixture_manifest_id"`
	Reason            string `json:"reason"`
}

type realMatrixApprovalEvent struct {
	EventID    string    `json:"event_id"`
	Sender     string    `json:"sender"`
	Decision   string    `json:"decision"`
	Source     string    `json:"source"`
	ApprovedBy string    `json:"approved_by"`
	DecidedAt  time.Time `json:"decided_at"`
}

type realMCPEvidence struct {
	SchemaVersion string                `json:"schema_version"`
	ExecutionMode string                `json:"execution_mode"`
	DryRun        bool                  `json:"dry_run"`
	IncidentID    string                `json:"incident_id"`
	TraceID       string                `json:"trace_id"`
	Calls         []realMCPCallEvidence `json:"calls"`
}

type realMCPCallEvidence struct {
	Tool        string `json:"tool"`
	Role        string `json:"role"`
	Worker      string `json:"worker"`
	Status      string `json:"status"`
	AuditLogID  string `json:"audit_log_id"`
	ProposalID  string `json:"proposal_id,omitempty"`
	PayloadHash string `json:"payload_hash,omitempty"`
}

type realFixtureEvidence struct {
	SchemaVersion     string              `json:"schema_version"`
	ExecutionMode     string              `json:"execution_mode"`
	DryRun            bool                `json:"dry_run"`
	IncidentID        string              `json:"incident_id"`
	TraceID           string              `json:"trace_id"`
	Stage             string              `json:"stage"`
	FixtureManifestID string              `json:"fixture_manifest_id"`
	Status            string              `json:"status"`
	ProcessCount      int                 `json:"process_count"`
	CapturedAt        time.Time           `json:"captured_at"`
	Metrics           []realFixtureMetric `json:"metrics"`
}

type realFixtureMetric struct {
	Name       string    `json:"name"`
	Values     []float64 `json:"values"`
	SampleSize int       `json:"sample_size"`
}

type realPostmortemEvidence struct {
	SchemaVersion string    `json:"schema_version"`
	ExecutionMode string    `json:"execution_mode"`
	DryRun        bool      `json:"dry_run"`
	IncidentID    string    `json:"incident_id"`
	TraceID       string    `json:"trace_id"`
	GeneratedBy   string    `json:"generated_by"`
	CompletedAt   time.Time `json:"completed_at"`
	RootCause     string    `json:"root_cause"`
	Impact        string    `json:"impact"`
	Resolution    string    `json:"resolution"`
	Verification  string    `json:"verification"`
}

type validatedRealAgentTeamsEvidence struct {
	Task       realStateTaskEvidence
	FileHashes map[string]string
}

// ValidateRealAgentTeamsEvidence validates mutually independent evidence
// files and the cross-file invariants that make one real AgentTeams incident.
// It returns all failures so an operator can repair an evidence bundle in one
// pass; the CLI still fails on the first non-empty error list.
func ValidateRealAgentTeamsEvidence(ctx context.Context, incidentID string, traceID string, paths RealAgentTeamsEvidencePaths) (*validatedRealAgentTeamsEvidence, error) {
	if incidentID == "" {
		return nil, errors.New("real-agentteams harness: --incident-id is required")
	}
	if !traceIDPattern.MatchString(traceID) {
		return nil, errors.New("real-agentteams harness: --trace-id must be a 32-character lowercase hex W3C trace ID")
	}

	validationErrors := make([]string, 0)
	requirePath := func(name, path string) bool {
		if strings.TrimSpace(path) == "" {
			validationErrors = append(validationErrors, fmt.Sprintf("%s evidence path is required", name))
			return false
		}
		return true
	}
	pathsPresent := map[string]bool{
		"state":          requirePath("state", paths.State),
		"hitl":           requirePath("HITL", paths.HITL),
		"mcp":            requirePath("MCP", paths.MCP),
		"fixture-before": requirePath("fixture-before", paths.FixtureBefore),
		"fixture-after":  requirePath("fixture-after", paths.FixtureAfter),
		"postmortem":     requirePath("postmortem", paths.Postmortem),
	}
	allPathsPresent := true
	for _, present := range pathsPresent {
		allPathsPresent = allPathsPresent && present
	}
	if !allPathsPresent {
		return nil, errors.New("real-agentteams harness: " + strings.Join(validationErrors, "; "))
	}

	var state realStateEvidence
	var hitl realHITLEvidence
	var mcp realMCPEvidence
	var fixtureBefore realFixtureEvidence
	var fixtureAfter realFixtureEvidence
	var postmortem realPostmortemEvidence

	load := func(label, path string, dst any) {
		if err := readStrictJSON(ctx, path, dst); err != nil {
			validationErrors = append(validationErrors, fmt.Sprintf("%s: %v", label, err))
		}
	}
	load("state", paths.State, &state)
	load("HITL", paths.HITL, &hitl)
	load("MCP", paths.MCP, &mcp)
	load("fixture-before", paths.FixtureBefore, &fixtureBefore)
	load("fixture-after", paths.FixtureAfter, &fixtureAfter)
	load("postmortem", paths.Postmortem, &postmortem)
	if len(validationErrors) > 0 {
		return nil, errors.New("real-agentteams harness: " + strings.Join(validationErrors, "; "))
	}

	if err := validateStateEvidence(&state, incidentID, traceID); err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	if err := validateHITLEvidence(&hitl, incidentID, traceID); err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	if err := validateMCPEvidence(&mcp, incidentID, traceID, hitl.ProposalID, hitl.PayloadHash); err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	if err := validateFixtureEvidence(&fixtureBefore, &fixtureAfter, incidentID, traceID, hitl.Parameters.FixtureManifestID); err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	if err := validatePostmortemEvidence(&postmortem, incidentID, traceID); err != nil {
		validationErrors = append(validationErrors, err.Error())
	}

	task, err := matchingStateTask(state, incidentID)
	if err != nil {
		validationErrors = append(validationErrors, err.Error())
	} else {
		if err := validateHITLStateBinding(*task, hitl); err != nil {
			validationErrors = append(validationErrors, err.Error())
		}
		if err := validateAuditRefs(*task, mcp); err != nil {
			validationErrors = append(validationErrors, err.Error())
		}
	}

	hashes := make(map[string]string, 6)
	for label, path := range map[string]string{
		"state":          paths.State,
		"hitl":           paths.HITL,
		"mcp":            paths.MCP,
		"fixture-before": paths.FixtureBefore,
		"fixture-after":  paths.FixtureAfter,
		"postmortem":     paths.Postmortem,
	} {
		digest, err := hashFile(ctx, path)
		if err != nil {
			validationErrors = append(validationErrors, fmt.Sprintf("%s hash: %v", label, err))
			continue
		}
		hashes[label] = digest
	}
	if len(validationErrors) > 0 {
		return nil, errors.New("real-agentteams harness validation failed: " + strings.Join(validationErrors, "; "))
	}
	return &validatedRealAgentTeamsEvidence{Task: *task, FileHashes: hashes}, nil
}

func readStrictJSON(ctx context.Context, path string, dst any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("expected exactly one JSON document")
	}
	return ctx.Err()
}

func matchingStateTask(state realStateEvidence, incidentID string) (*realStateTaskEvidence, error) {
	matches := make([]realStateTaskEvidence, 0)
	taskKeys := make([]string, 0, len(state.Tasks))
	for key := range state.Tasks {
		taskKeys = append(taskKeys, key)
	}
	sort.Strings(taskKeys)
	for _, key := range taskKeys {
		task := state.Tasks[key]
		if task.IncidentID == incidentID {
			matches = append(matches, task)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("state: expected exactly one task for incident %q, got %d", incidentID, len(matches))
	}
	return &matches[0], nil
}

func validateStateEvidence(state *realStateEvidence, incidentID, traceID string) error {
	errs := make([]string, 0)
	if state.SchemaVersion != realStateSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version must be %q", realStateSchemaVersion))
	}
	task, err := matchingStateTask(*state, incidentID)
	if err != nil {
		return err
	}
	if task.Status != "completed" {
		errs = append(errs, "task status must be completed")
	}
	if task.MatrixRoomID == "" {
		errs = append(errs, "matrix_room_id is required")
	}
	expectedPhases := map[string]string{
		"alert_dedup":      "alerter",
		"rca":              "investigator",
		"critic_audit":     "critic",
		"review":           "reviewer",
		"hitl_approval":    "@",
		"repair_execution": "repairer",
		"verification":     "verifier",
		"postmortem":       "reporter",
	}
	phaseKeys := make([]string, 0, len(task.Phases))
	for phase := range task.Phases {
		phaseKeys = append(phaseKeys, phase)
	}
	sort.Strings(phaseKeys)
	for _, phase := range phaseKeys {
		if _, ok := expectedPhases[phase]; !ok {
			errs = append(errs, fmt.Sprintf("state phase %q is not allowed", phase))
		}
	}
	for phase, assignment := range expectedPhases {
		record, ok := task.Phases[phase]
		if !ok {
			errs = append(errs, fmt.Sprintf("state phase %s is missing", phase))
			continue
		}
		if record.Status != "completed" {
			errs = append(errs, fmt.Sprintf("state phase %s status must be completed", phase))
		}
		if phase == "hitl_approval" {
			if !strings.HasPrefix(record.AssignedTo, "@") {
				errs = append(errs, "state hitl_approval must be assigned to a Matrix user")
			}
		} else if record.AssignedTo != assignment {
			errs = append(errs, fmt.Sprintf("state phase %s must be assigned to %s", phase, assignment))
		}
		if record.StartedAt.IsZero() || record.CompletedAt == nil || record.CompletedAt.Before(record.StartedAt) {
			errs = append(errs, fmt.Sprintf("state phase %s has invalid timestamps", phase))
		}
	}
	if len(task.AuditRefs) == 0 {
		errs = append(errs, "state audit_refs must not be empty")
	}
	for index, ref := range task.AuditRefs {
		if ref.AuditLogID == "" {
			errs = append(errs, fmt.Sprintf("state audit_refs[%d].audit_log_id is required", index))
		}
		if ref.TraceID != traceID {
			errs = append(errs, fmt.Sprintf("state audit_refs[%d].trace_id must equal input trace_id", index))
		}
	}
	if len(errs) > 0 {
		return errors.New("state: " + strings.Join(errs, "; "))
	}
	return nil
}

func validateHITLEvidence(hitl *realHITLEvidence, incidentID, traceID string) error {
	errs := make([]string, 0)
	if hitl.SchemaVersion != realHITLSchemaVersion {
		errs = append(errs, "schema_version is invalid")
	}
	if hitl.ExecutionMode != realAgentTeamsExecutionMode || hitl.DryRun {
		errs = append(errs, "execution must be real_agentteams and dry_run=false")
	}
	if hitl.IncidentID != incidentID || hitl.TraceID != traceID {
		errs = append(errs, "incident_id/trace_id must equal harness input")
	}
	if !uuidPattern.MatchString(hitl.ProposalID) {
		errs = append(errs, "proposal_id must be an exact Proposal UUID")
	}
	if len(hitl.PayloadHash) != sha256.Size*2 || !isLowerHex(hitl.PayloadHash) {
		errs = append(errs, "payload_hash must be a lowercase SHA-256 hex digest")
	}
	if hitl.Action != "kill_process" || hitl.Parameters.Command != hitl.Action {
		errs = append(errs, "host/cpu-spike requires action=parameters.command=kill_process")
	}
	if hitl.Resource != "host:fixture" || hitl.Parameters.IncidentID != incidentID ||
		hitl.Parameters.FixtureManifestID == "" || hitl.Parameters.Reason == "" {
		errs = append(errs, "resource, incident_id, fixture_manifest_id and reason are required and must match")
	}
	event := hitl.MatrixEvent
	if event.EventID == "" || event.Sender == "" || !strings.HasPrefix(event.Sender, "@") {
		errs = append(errs, "matrix event id and Matrix sender are required")
	}
	if event.Decision != "approve" || event.Source != "human" || event.ApprovedBy != event.Sender {
		errs = append(errs, "matrix event must be a human approve from the same sender")
	}
	if hitl.RoomID == "" || event.DecidedAt.IsZero() {
		errs = append(errs, "room_id and decided_at are required")
	}
	if len(errs) > 0 {
		return errors.New("hitl: " + strings.Join(errs, "; "))
	}
	return nil
}

func validateMCPEvidence(mcp *realMCPEvidence, incidentID, traceID, proposalID, payloadHash string) error {
	errs := make([]string, 0)
	if mcp.SchemaVersion != realMCPSchemaVersion {
		errs = append(errs, "schema_version is invalid")
	}
	if mcp.ExecutionMode != realAgentTeamsExecutionMode || mcp.DryRun {
		errs = append(errs, "execution must be real_agentteams and dry_run=false")
	}
	if mcp.IncidentID != incidentID || mcp.TraceID != traceID {
		errs = append(errs, "incident_id/trace_id must equal harness input")
	}
	allowedToolsByRole := map[string][]string{
		"alerter":      {"loop.correlate"},
		"investigator": {"loop.correlate", "loop.investigate", "query_knowledge"},
		"critic":       {"query_knowledge"},
		"reviewer":     {"query_knowledge"},
		"repairer":     {"recovery.execute", "query_knowledge"},
		"verifier":     {"recovery.verify", "query_knowledge"},
	}
	hasExecute := false
	hasVerify := false
	for index, call := range mcp.Calls {
		if call.Status != "success" {
			errs = append(errs, fmt.Sprintf("calls[%d] must be successful for strict evidence", index))
		}
		if call.Worker != "opskeeper-"+call.Role {
			errs = append(errs, fmt.Sprintf("calls[%d] worker %q is not bound to role %q", index, call.Worker, call.Role))
		}
		allowed, ok := allowedToolsByRole[call.Role]
		if !ok || !slices.Contains(allowed, call.Tool) {
			errs = append(errs, fmt.Sprintf("calls[%d] cross-role tool call %s/%s is not allowed", index, call.Role, call.Tool))
		}
		if call.AuditLogID == "" {
			errs = append(errs, fmt.Sprintf("calls[%d].audit_log_id is required", index))
		}
		switch call.Tool {
		case "recovery.execute":
			hasExecute = true
			if call.Role != "repairer" || call.ProposalID != proposalID || call.PayloadHash != payloadHash {
				errs = append(errs, fmt.Sprintf("calls[%d] must be repairer recovery.execute bound to the HITL proposal and payload hash", index))
			}
		case "recovery.verify":
			hasVerify = true
			if call.Role != "verifier" {
				errs = append(errs, fmt.Sprintf("calls[%d] must be verifier recovery.verify", index))
			}
		}
	}
	if !hasExecute {
		errs = append(errs, "successful repairer recovery.execute call is missing")
	}
	if !hasVerify {
		errs = append(errs, "successful verifier recovery.verify call is missing")
	}
	if len(errs) > 0 {
		return errors.New("mcp: " + strings.Join(errs, "; "))
	}
	return nil
}

func validateFixtureEvidence(before, after *realFixtureEvidence, incidentID, traceID, fixtureManifestID string) error {
	errs := make([]string, 0)
	validateOne := func(label string, evidence *realFixtureEvidence, stage string) (int, float64) {
		localErrs := make([]string, 0)
		if evidence.SchemaVersion != realFixtureSchemaVersion {
			localErrs = append(localErrs, "schema_version is invalid")
		}
		if evidence.ExecutionMode != realAgentTeamsExecutionMode || evidence.DryRun {
			localErrs = append(localErrs, "execution must be real_agentteams and dry_run=false")
		}
		if evidence.IncidentID != incidentID || evidence.TraceID != traceID {
			localErrs = append(localErrs, "incident_id/trace_id must equal harness input")
		}
		if evidence.Stage != stage {
			localErrs = append(localErrs, fmt.Sprintf("stage must be %q", stage))
		}
		if evidence.FixtureManifestID == "" || (fixtureManifestID != "" && evidence.FixtureManifestID != fixtureManifestID) {
			localErrs = append(localErrs, "fixture_manifest_id must match the HITL payload")
		}
		if evidence.CapturedAt.IsZero() {
			localErrs = append(localErrs, "captured_at is required")
		}
		if evidence.ProcessCount < 2 || evidence.ProcessCount > 4 {
			localErrs = append(localErrs, "process_count must be between 2 and 4")
		}
		expectedStatus := "running"
		if stage == "after" {
			expectedStatus = "terminated"
		}
		if evidence.Status != expectedStatus {
			localErrs = append(localErrs, fmt.Sprintf("status must be %q", expectedStatus))
		}
		cpu, metricErr := averageMetric(evidence.Metrics, "cpu_usage_percent")
		if metricErr != nil {
			localErrs = append(localErrs, metricErr.Error())
		}
		if len(localErrs) > 0 {
			errs = append(errs, label+": "+strings.Join(localErrs, "; "))
		}
		return evidence.ProcessCount, cpu
	}
	beforeCount, beforeCPU := validateOne("fixture-before", before, "before")
	_, afterCPU := validateOne("fixture-after", after, "after")
	if beforeCount < 2 || beforeCount > 4 {
		errs = append(errs, "fixture-before must contain 2-4 case-owned processes")
	}
	if beforeCount > 0 && after.ProcessCount > 0 && beforeCount != after.ProcessCount {
		errs = append(errs, "fixture process_count must remain stable before and after repair")
	}
	if beforeCPU >= 0 && afterCPU >= 0 && afterCPU >= beforeCPU*0.5 {
		errs = append(errs, "fixture cpu_usage_percent must fall by at least 50% after repair")
	}
	if !after.CapturedAt.After(before.CapturedAt) {
		errs = append(errs, "fixture-after captured_at must be after fixture-before captured_at")
	}
	if len(errs) > 0 {
		return errors.New("fixture: " + strings.Join(errs, "; "))
	}
	return nil
}

func averageMetric(metrics []realFixtureMetric, name string) (float64, error) {
	for _, metric := range metrics {
		if metric.Name != name {
			continue
		}
		if metric.SampleSize < 3 || len(metric.Values) != metric.SampleSize {
			return -1, fmt.Errorf("metric %s requires sample_size>=3 matching values length", name)
		}
		total := 0.0
		for _, value := range metric.Values {
			if value < 0 {
				return -1, fmt.Errorf("metric %s values must be non-negative", name)
			}
			total += value
		}
		return total / float64(len(metric.Values)), nil
	}
	return -1, fmt.Errorf("metric %s is required", name)
}

func validatePostmortemEvidence(postmortem *realPostmortemEvidence, incidentID, traceID string) error {
	errs := make([]string, 0)
	if postmortem.SchemaVersion != realPostmortemSchemaVersion {
		errs = append(errs, "schema_version is invalid")
	}
	if postmortem.ExecutionMode != realAgentTeamsExecutionMode || postmortem.DryRun {
		errs = append(errs, "execution must be real_agentteams and dry_run=false")
	}
	if postmortem.IncidentID != incidentID || postmortem.TraceID != traceID {
		errs = append(errs, "incident_id/trace_id must equal harness input")
	}
	if postmortem.GeneratedBy != "opskeeper-reporter" {
		errs = append(errs, "generated_by must be opskeeper-reporter")
	}
	for field, value := range map[string]string{
		"root_cause":   postmortem.RootCause,
		"impact":       postmortem.Impact,
		"resolution":   postmortem.Resolution,
		"verification": postmortem.Verification,
	} {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, field+" is required")
		}
	}
	if postmortem.CompletedAt.IsZero() {
		errs = append(errs, "completed_at is required")
	}
	if len(errs) > 0 {
		return errors.New("postmortem: " + strings.Join(errs, "; "))
	}
	return nil
}

func validateHITLStateBinding(task realStateTaskEvidence, hitl realHITLEvidence) error {
	errs := make([]string, 0)
	if task.MatrixRoomID != hitl.RoomID {
		errs = append(errs, "state matrix_room_id must match HITL room_id")
	}
	phase, ok := task.Phases["hitl_approval"]
	if !ok || phase.ApprovalRequestID != hitl.ProposalID {
		errs = append(errs, "state hitl approval_request_id must match proposal_id")
	}
	found := false
	for _, decision := range task.HITLDecisions {
		if decision.ApprovalRequestID == hitl.ProposalID &&
			decision.Decision == "approved" &&
			decision.Admin == hitl.MatrixEvent.Sender {
			found = true
		}
	}
	if !found {
		errs = append(errs, "state hitl_decisions must contain the human Matrix approval")
	}
	if len(errs) > 0 {
		return errors.New("state/hitl binding: " + strings.Join(errs, "; "))
	}
	return nil
}

func validateAuditRefs(task realStateTaskEvidence, mcp realMCPEvidence) error {
	stateAuditIDs := make(map[string]bool, len(task.AuditRefs))
	for _, ref := range task.AuditRefs {
		stateAuditIDs[ref.AuditLogID] = true
	}
	for index, call := range mcp.Calls {
		if !stateAuditIDs[call.AuditLogID] {
			return fmt.Errorf("state audit_refs does not contain MCP calls[%d] audit_log_id", index)
		}
	}
	return nil
}

func hashFile(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func applyRealAgentTeamsEvidence(result *LoopResult, evidence *validatedRealAgentTeamsEvidence, traceID string) {
	phaseOrder := []struct {
		stateName string
		loopName  string
	}{
		{stateName: "alert_dedup", loopName: "detected"},
		{stateName: "rca", loopName: "investigated"},
		{stateName: "critic_audit", loopName: "critiqued"},
		{stateName: "review", loopName: "reviewed"},
		{stateName: "hitl_approval", loopName: "approved"},
		{stateName: "repair_execution", loopName: "recovered"},
		{stateName: "verification", loopName: "verified"},
		{stateName: "postmortem", loopName: "postmortem"},
	}
	result.IncidentID = evidence.Task.IncidentID
	result.ExecutionMode = ExecutionModeRealAgentTeams
	result.FinalPhase = "postmortem"
	result.Flags = []string{"real_agentteams"}
	result.Metadata = map[string]any{
		"trace_id":        traceID,
		"evidence_sha256": evidence.FileHashes,
	}
	result.Phases = make([]LoopPhaseRecord, 0, len(phaseOrder))
	result.Durations = make(map[string]int64, len(phaseOrder))
	for _, phase := range phaseOrder {
		statePhase := evidence.Task.Phases[phase.stateName]
		startedAt := statePhase.StartedAt
		completedAt := startedAt
		if statePhase.CompletedAt != nil {
			completedAt = *statePhase.CompletedAt
		}
		durationMs := completedAt.Sub(startedAt).Milliseconds()
		result.Phases = append(result.Phases, LoopPhaseRecord{
			Phase:      phase.loopName,
			Status:     "success",
			StartedAt:  startedAt,
			DurationMs: durationMs,
		})
		result.Durations[phase.loopName] = durationMs
		if result.StartedAt.IsZero() || startedAt.Before(result.StartedAt) {
			result.StartedAt = startedAt
		}
	}
	result.FinishedAt = time.Now().UTC()
	for _, phase := range result.Phases {
		finishedAt := phase.StartedAt.Add(time.Duration(phase.DurationMs) * time.Millisecond)
		if finishedAt.After(result.FinishedAt) {
			result.FinishedAt = finishedAt
		}
	}
	result.StartedAt = evidence.Task.Phases["alert_dedup"].StartedAt
	result.ContractSummary = []ContractSummary{
		{Phase: "investigated", Type: "RealRootCauseEvidence", SchemaVer: "v1"},
		{Phase: "critiqued", Type: "RealCriticEvidence", SchemaVer: "v1"},
		{Phase: "approved", Type: "HumanMatrixApprovalEvidence", SchemaVer: "v1"},
		{Phase: "recovered", Type: "MCPRecoveryAuditEvidence", SchemaVer: "v1"},
		{Phase: "verified", Type: "MCPVerificationAuditEvidence", SchemaVer: "v1"},
		{Phase: "postmortem", Type: "PostmortemEvidence", SchemaVer: "v1"},
	}
}

func writeRealAgentTeamsResult(opts LoopOptions, result *LoopResult) error {
	outDir := opts.OutDir
	if outDir == "" {
		outDir = "harness/result/loop"
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("run-loop real-agentteams: mkdir %s: %w", outDir, err)
	}
	outPath := filepath.Join(outDir, fileSafeCaseID(result.CaseID)+".json")
	if err := writeJSONFile(outPath, result); err != nil {
		return fmt.Errorf("run-loop real-agentteams: write %s: %w", outPath, err)
	}
	return nil
}
