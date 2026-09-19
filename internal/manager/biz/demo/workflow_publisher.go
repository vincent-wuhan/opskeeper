package demo

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"net/url"

	demomodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/demo"
)

const workflowAuthorityTimeout = 5 * time.Second

var beijingTimezone = time.FixedZone("UTC+8", 8*60*60)

var workflowSensitiveEvidencePattern = regexp.MustCompile(
	`(?i)\b(?:password|api[_-]?key|secret|token|authorization|credential)s?\s*[=:]\s*[^\s;；]+`,
)

type workflowAuthorityClaims struct {
	ManagerID           string    `json:"manager_id"`
	RoomID              string    `json:"room_id"`
	IncidentID          string    `json:"incident_id"`
	Stage               string    `json:"stage"`
	Nonce               string    `json:"nonce"`
	ReplayProfileID     string    `json:"replay_profile_id,omitempty"`
	CandidateA          string    `json:"candidate_a,omitempty"`
	TargetFingerprint   string    `json:"target_fingerprint"`
	DecisionBriefSHA256 string    `json:"decision_brief_sha256,omitempty"`
	IssuedAt            time.Time `json:"issued_at"`
	ExpiresAt           time.Time `json:"expires_at"`
}

type workflowDecisionBrief struct {
	RootCause          string                   `json:"root_cause"`
	ImpactScope        string                   `json:"impact_scope"`
	Boundary           string                   `json:"boundary"`
	ApprovalExpiresUTC string                   `json:"approval_expires_utc"`
	ApprovalExpiresBJT string                   `json:"approval_expires_bjt"`
	ArchiveURL         string                   `json:"archive_url"`
	ApprovalCommand    string                   `json:"approval_command"`
	CandidateA         *PreviewCandidateSummary `json:"candidate_a,omitempty"`
	CandidateB         *PreviewCandidateSummary `json:"candidate_b,omitempty"`
}

type MatrixWorkflowPublisher struct {
	baseURL   string
	token     string
	roomID    string
	managerID string
	secret    []byte
	client    *http.Client
}

type workflowStageDefinition struct {
	id     string
	title  string
	status string
}

var workflowAuthorityStages = []workflowStageDefinition{
	{id: "preview", title: "修复预演", status: "completed"},
	{id: "approval", title: "人工审批", status: "in_progress"},
	{id: "repair", title: "执行修复", status: "in_progress"},
	{id: "verify", title: "独立验证", status: "in_progress"},
	{id: "recovered", title: "恢复完成", status: "completed"},
}

func NewMatrixWorkflowPublisher(baseURL, token, roomID, managerID, secret string) (*MatrixWorkflowPublisher, error) {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(token) == "" ||
		!strings.HasPrefix(strings.TrimSpace(roomID), "!") ||
		!strings.HasPrefix(strings.TrimSpace(managerID), "@") || len(secret) < 16 {
		return nil, errors.New("workflow authority publisher configuration is incomplete")
	}
	return &MatrixWorkflowPublisher{
		baseURL: strings.TrimRight(baseURL, "/"), token: token,
		roomID: strings.TrimSpace(roomID), managerID: strings.TrimSpace(managerID),
		secret: []byte(secret), client: &http.Client{Timeout: workflowAuthorityTimeout},
	}, nil
}

func WorkflowPublisherFromEnv() (WorkflowPublisher, error) {
	return NewMatrixWorkflowPublisher(
		os.Getenv("AGENTTEAMS_MATRIX_URL"),
		os.Getenv("AGENTTEAMS_MANAGER_MATRIX_TOKEN"),
		os.Getenv("OPSKEEPER_DEMO_MATRIX_ROOM"),
		os.Getenv("OPSKEEPER_WORKFLOW_AUTHORITY_MANAGER_ID"),
		os.Getenv("OPSKEEPER_WORKFLOW_AUTHORITY_SECRET"),
	)
}

func buildWorkflowDecisionBrief(
	run *demomodel.ScenarioRun, decision *PreviewDecisionSummary, incidentID string,
) workflowDecisionBrief {
	brief := workflowDecisionBrief{
		ArchiveURL:      workflowArchiveURL(incidentID),
		ApprovalCommand: "@manager 已批准 incident_id=" + incidentID + " Candidate A",
	}
	if run != nil && !run.ExpiresAt.IsZero() {
		brief.ApprovalExpiresUTC = run.ExpiresAt.UTC().Format(time.RFC3339Nano)
		brief.ApprovalExpiresBJT = run.ExpiresAt.In(beijingTimezone).Format(time.RFC3339Nano)
	}
	if decision == nil {
		return brief
	}
	brief.RootCause = workflowSafeEvidenceText(decision.RootCause)
	brief.ImpactScope = workflowSafeEvidenceText(decision.ImpactScope)
	brief.Boundary = workflowSafeEvidenceText(decision.BoundaryText)
	brief.CandidateA = workflowSafeCandidateSummary(decision.CandidateADetails)
	brief.CandidateB = workflowSafeCandidateSummary(decision.CandidateBDetails)
	return brief
}

func workflowArchiveURL(incidentID string) string {
	template := strings.TrimSpace(os.Getenv("OPSKEEPER_DEMO_ARCHIVE_URL_TEMPLATE"))
	if template != "" {
		parsed, err := url.Parse(template)
		if err == nil {
			switch parsed.Scheme {
			case "http", "https", "opskeeper":
				if parsed.User == nil && strings.Contains(template, "{incident_id}") &&
					!workflowArchiveURLHasSensitiveQuery(parsed) {
					return strings.ReplaceAll(template, "{incident_id}", incidentID)
				}
			}
		}
	}
	return "opskeeper://incidents/" + incidentID + "/archive"
}

func workflowArchiveURLHasSensitiveQuery(parsed *url.URL) bool {
	for key := range parsed.Query() {
		normalized := strings.ToLower(key)
		if strings.Contains(normalized, "token") || strings.Contains(normalized, "key") ||
			strings.Contains(normalized, "secret") || strings.Contains(normalized, "password") ||
			strings.Contains(normalized, "credential") {
			return true
		}
	}
	return false
}

func workflowSafeEvidenceText(value string) string {
	return workflowDisplayText(workflowSensitiveEvidencePattern.ReplaceAllString(value, "[REDACTED]"))
}

func workflowSafeCandidateSummary(candidate *PreviewCandidateSummary) *PreviewCandidateSummary {
	if candidate == nil {
		return nil
	}
	safe := *candidate
	safe.CandidateID = workflowSafeEvidenceText(safe.CandidateID)
	safe.Name = workflowSafeEvidenceText(safe.Name)
	safe.Action = workflowSafeEvidenceText(safe.Action)
	safe.ChangeSummary = workflowSafeEvidenceText(safe.ChangeSummary)
	safe.Decision = workflowSafeEvidenceText(safe.Decision)
	safe.RejectionReason = workflowSafeEvidenceText(safe.RejectionReason)
	safe.WriteImpact = workflowSafeEvidenceText(safe.WriteImpact)
	return &safe
}

func (publisher *MatrixWorkflowPublisher) PublishWorkflow(
	ctx context.Context, run *demomodel.ScenarioRun, stage string, decision *PreviewDecisionSummary,
) error {
	if publisher == nil || run == nil {
		return errors.New("workflow authority publisher is not configured")
	}
	switch stage {
	case demomodel.ScenarioStatusPreviewReady, demomodel.ScenarioStatusAwaitingApproval,
		demomodel.ScenarioStatusRepairDispatched, demomodel.ScenarioStatusVerifying,
		demomodel.ScenarioStatusRecovered:
	default:
		return errors.New("unknown workflow authority stage")
	}
	now := time.Now().UTC()
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return err
	}
	var brief workflowDecisionBrief
	briefHash := ""
	if stage == demomodel.ScenarioStatusAwaitingApproval {
		brief = buildWorkflowDecisionBrief(run, decision, strconv.FormatUint(run.IncidentID, 10))
		encodedBrief, err := json.Marshal(brief)
		if err != nil {
			return err
		}
		briefSum := sha256.Sum256(encodedBrief)
		briefHash = hex.EncodeToString(briefSum[:])
	}
	claims := workflowAuthorityClaims{
		ManagerID: publisher.managerID, RoomID: publisher.roomID,
		IncidentID: strconv.FormatUint(run.IncidentID, 10), Stage: stage,
		Nonce: hex.EncodeToString(nonceBytes), TargetFingerprint: run.TargetFingerprint,
		DecisionBriefSHA256: briefHash,
		IssuedAt:            now, ExpiresAt: now.Add(30 * time.Second),
	}
	if decision != nil {
		claims.ReplayProfileID = decision.ReplayProfileID
		claims.CandidateA = decision.CandidateA
	}
	encodedClaims, err := json.Marshal(claims)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, publisher.secret)
	_, _ = mac.Write(encodedClaims)
	token := base64.RawURLEncoding.EncodeToString(encodedClaims) + "." + hex.EncodeToString(mac.Sum(nil))
	incidentID := claims.IncidentID
	utcTime := now.Format("2006-01-02T15:04:05Z")
	beijingTime := now.In(beijingTimezone).Format("2006-01-02T15:04:05+08:00")
	decisionText := fmt.Sprintf(
		"【OpsKeeper 人工审批决策简报】\nincident_id=%s stage=%s time_utc=%s time_bjt=%s\n",
		incidentID, stage, utcTime, beijingTime,
	)
	if stage == demomodel.ScenarioStatusAwaitingApproval {
		decisionText += fmt.Sprintf(
			"确认根因：%s\n影响范围：%s\n预演边界：%s\n候选 A：%s\n候选 B：%s\n"+
				"审批有效期（UTC）：%s\n审批有效期（北京时间）：%s\n"+
				"证据档案：%s\n建议审批命令：%s\n",
			workflowDisplayText(brief.RootCause),
			workflowDisplayText(brief.ImpactScope),
			workflowDisplayText(brief.Boundary),
			formatWorkflowCandidate(brief.CandidateA),
			formatWorkflowCandidate(brief.CandidateB),
			brief.ApprovalExpiresUTC,
			brief.ApprovalExpiresBJT,
			workflowDisplayText(brief.ArchiveURL),
			workflowDisplayText(brief.ApprovalCommand),
		)
	}
	decisionText += "OPSKEEPER_AUTHORITY_V1 " + token
	workflowMetadata := map[string]any{
		"type": "opskeeper-workflow", "runId": incidentID, "authorityStage": stage,
		"title":  "OpsKeeper 事故恢复 " + incidentID,
		"status": workflowStatus(stage), "source": "opskeeper-manager",
		"summary": workflowSummary(stage), "steps": workflowSteps(stage),
	}
	if stage == demomodel.ScenarioStatusAwaitingApproval {
		workflowMetadata["decision_brief"] = brief
	}
	authorityMetadata := map[string]any{
		"version": 1, "manager_id": publisher.managerID, "incident_id": incidentID,
		"stage": stage, "token": token,
		"time_utc": utcTime, "time_bjt": beijingTime,
	}
	if stage == demomodel.ScenarioStatusAwaitingApproval {
		authorityMetadata["decision_brief"] = brief
		authorityMetadata["decision_brief_sha256"] = claims.DecisionBriefSHA256
	}
	content := map[string]any{
		"msgtype":             "m.notice",
		"body":                decisionText,
		"agentteams.workflow": workflowMetadata,
		"opskeeper.authority": authorityMetadata,
	}
	encodedContent, err := json.Marshal(content)
	if err != nil {
		return err
	}
	eventURL := fmt.Sprintf(
		"%s/_matrix/client/v3/rooms/%s/send/m.room.message/%d-authority",
		publisher.baseURL, url.PathEscape(publisher.roomID), now.UnixNano(),
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, eventURL, bytes.NewReader(encodedContent))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+publisher.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := publisher.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("matrix workflow authority send failed: %s", response.Status)
	}
	return nil
}

func workflowDisplayText(value string) string {
	return strings.Map(func(run rune) rune {
		if run == '\n' || run == '\r' || run == '\t' {
			return ' '
		}
		if run < 32 || run == 127 {
			return -1
		}
		return run
	}, value)
}

func formatWorkflowCandidate(candidate *PreviewCandidateSummary) string {
	if candidate == nil {
		return "暂无数据"
	}
	text := fmt.Sprintf(
		"%s（%s / %s）：%s；变更：%s；consistent=%t；business_probe_pass=%t；"+
			"average_latency_ms=%.3f；p95_latency_ms=%.3f；tps=%.3f；error_count=%d；write_impact=%s",
		candidate.CandidateID, candidate.Name, candidate.Action, candidate.Decision,
		candidate.ChangeSummary, candidate.Consistent, candidate.BusinessProbePass,
		candidate.AverageLatencyMS, candidate.P95LatencyMS, candidate.TPS,
		candidate.ErrorCount, candidate.WriteImpact,
	)
	if candidate.RejectionReason != "" {
		text += "；rejection_reason=" + candidate.RejectionReason
	}
	return workflowDisplayText(text)
}

func workflowStatus(stage string) string {
	if stage == demomodel.ScenarioStatusRecovered {
		return "success"
	}
	return "in_progress"
}

func workflowSummary(stage string) string {
	switch stage {
	case demomodel.ScenarioStatusPreviewReady:
		return "修复预演完成，等待人工审批"
	case demomodel.ScenarioStatusAwaitingApproval:
		return "等待人工审批修复方案"
	case demomodel.ScenarioStatusRepairDispatched:
		return "修复方案已批准并派发执行"
	case demomodel.ScenarioStatusVerifying:
		return "修复执行完成，独立验证中"
	case demomodel.ScenarioStatusRecovered:
		return "修复验证通过，业务已恢复"
	default:
		return stage
	}
}

func workflowSteps(stage string) []map[string]any {
	selectedIndex := -1
	for index, definition := range workflowAuthorityStages {
		if definition.id == workflowStepID(stage) {
			selectedIndex = index
			break
		}
	}
	steps := make([]map[string]any, 0, len(workflowAuthorityStages)-1)
	for index, definition := range workflowAuthorityStages {
		if definition.id == "recovered" {
			continue
		}
		status := "pending"
		switch {
		case index < selectedIndex:
			status = "completed"
		case index == selectedIndex:
			status = definition.status
		case stage == demomodel.ScenarioStatusRecovered:
			status = "completed"
		}
		steps = append(steps, map[string]any{
			"id": definition.id, "name": definition.title, "status": status,
		})
	}
	return steps
}

func workflowStepID(stage string) string {
	switch stage {
	case demomodel.ScenarioStatusPreviewReady:
		return "preview"
	case demomodel.ScenarioStatusAwaitingApproval:
		return "approval"
	case demomodel.ScenarioStatusRepairDispatched:
		return "repair"
	case demomodel.ScenarioStatusVerifying, demomodel.ScenarioStatusRecovered:
		return "verify"
	default:
		return ""
	}
}
