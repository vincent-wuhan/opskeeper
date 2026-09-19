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
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"net/url"

	demomodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/demo"
)

const (
	workflowAuthorityTimeout = 5 * time.Second
	workflowAuthorityRetries = 3
)

var beijingTimezone = time.FixedZone("UTC+8", 8*60*60)

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
		ApprovalCommand: "@manager 已批准 incident_id=" + incidentID,
	}
	if run != nil && !run.ExpiresAt.IsZero() {
		brief.ApprovalExpiresUTC = run.ExpiresAt.UTC().Format(time.RFC3339Nano)
		brief.ApprovalExpiresBJT = run.ExpiresAt.In(beijingTimezone).Format(time.RFC3339Nano)
	}
	if decision == nil {
		return brief
	}
	brief.RootCause = decision.RootCause
	brief.ImpactScope = decision.ImpactScope
	brief.Boundary = decision.BoundaryText
	brief.CandidateA = decision.CandidateADetails
	brief.CandidateB = decision.CandidateBDetails
	return brief
}

func workflowArchiveURL(incidentID string) string {
	template := strings.TrimSpace(os.Getenv("OPSKEEPER_DEMO_ARCHIVE_URL_TEMPLATE"))
	if template != "" {
		parsed, err := url.Parse(template)
		if err == nil {
			switch parsed.Scheme {
			case "http", "https", "opskeeper":
				return strings.ReplaceAll(template, "{incident_id}", incidentID)
			}
		}
	}
	return "opskeeper://incidents/" + incidentID + "/archive"
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
		"[OpsKeeper Authority] incident=%s stage=%s time_utc=%s time_bjt=%s\n",
		incidentID, stage, utcTime, beijingTime,
	)
	if stage == demomodel.ScenarioStatusAwaitingApproval {
		decisionText += fmt.Sprintf(
			"Root cause: %s\nImpact scope: %s\nPreview boundary: %s\n"+
				"Candidate A: %s\nCandidate B: %s\n"+
				"Approval expires (UTC): %s\nApproval expires (BJT): %s\n"+
				"Archive: %s\nApproval command: %s\n",
			brief.RootCause, brief.ImpactScope, brief.Boundary,
			formatWorkflowCandidate(brief.CandidateA), formatWorkflowCandidate(brief.CandidateB),
			brief.ApprovalExpiresUTC, brief.ApprovalExpiresBJT, brief.ArchiveURL, brief.ApprovalCommand,
		)
	}
	decisionText += "OPSKEEPER_AUTHORITY_V1 " + token
	workflowMetadata := map[string]any{
		"type": "opskeeper-workflow", "runId": incidentID, "authorityStage": stage,
		"status": "manager_authority", "source": "opskeeper-manager",
	}
	if stage == demomodel.ScenarioStatusAwaitingApproval {
		workflowMetadata["decision_brief"] = brief
	}
	authorityMetadata := map[string]any{
		"version": 1, "manager_id": publisher.managerID, "incident_id": incidentID,
		"stage": stage, "token": token, "time_utc": utcTime, "time_bjt": beijingTime,
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
		"%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s-%s-authority",
		publisher.baseURL, url.PathEscape(publisher.roomID), url.PathEscape(run.IdempotencyKey), stage,
	)
	var lastErr error
	for attempt := 0; attempt < workflowAuthorityRetries; attempt++ {
		if attempt > 0 {
			if err := sleepWithContext(ctx, time.Duration(1<<attempt)*100*time.Millisecond); err != nil {
				return err
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, eventURL, bytes.NewReader(encodedContent))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+publisher.token)
		request.Header.Set("Content-Type", "application/json")
		response, err := publisher.client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
			err = fmt.Errorf("matrix workflow authority send failed: %s", response.Status)
			if response.StatusCode != http.StatusTooManyRequests && response.StatusCode < 500 {
				return err
			}
		}
		lastErr = err
	}
	return lastErr
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func formatWorkflowCandidate(candidate *PreviewCandidateSummary) string {
	if candidate == nil {
		return "not available"
	}
	text := fmt.Sprintf(
		"%s (%s / %s): %s; consistent=%t; business_probe=%t; avg_ms=%.3f; p95_ms=%.3f; tps=%.3f; errors=%d; write_impact=%s",
		candidate.CandidateID, candidate.Name, candidate.Action, candidate.Decision,
		candidate.Consistent, candidate.BusinessProbePass, candidate.AverageLatencyMS,
		candidate.P95LatencyMS, candidate.TPS, candidate.ErrorCount, candidate.WriteImpact,
	)
	if candidate.RejectionReason != "" {
		text += "; rejection_reason=" + candidate.RejectionReason
	}
	return text
}
