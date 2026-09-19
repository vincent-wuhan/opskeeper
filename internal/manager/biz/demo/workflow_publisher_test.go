package demo

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	demomodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/demo"
)

func TestMatrixWorkflowPublisherSignsAndSendsAuthorityEvent(t *testing.T) {
	t.Setenv("OPSKEEPER_DEMO_ARCHIVE_URL_TEMPLATE", "https://teams.example/archive?incident_id={incident_id}")
	var body map[string]any
	var rawBody []byte
	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		rawBody, _ = io.ReadAll(request.Body)
		requestPath = request.URL.Path
		if request.Header.Get("Authorization") != "Bearer matrix-token" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		if err := json.Unmarshal(rawBody, &body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = writer.Write([]byte(`{"event_id":"$authority"}`))
	}))
	defer server.Close()

	publisher, err := NewMatrixWorkflowPublisher(
		server.URL, "matrix-token", "!room:hs", "@manager:hs", "0123456789abcdef",
	)
	if err != nil {
		t.Fatal(err)
	}
	run := &demomodel.ScenarioRun{
		IncidentID: 100, IdempotencyKey: "final-demo-key", TargetFingerprint: "0123456789abcdef",
		ExpiresAt: time.Date(2026, 9, 19, 13, 19, 34, 0, time.UTC),
	}
	decision := &PreviewDecisionSummary{
		ReplayProfileID: "sha256:workload-v1", CandidateA: "candidate-a", CandidateB: "candidate-b",
		RootCause:    "PostgreSQL connection pool exhausted; Manager confirmed the pg_pool_exhaustion root cause.",
		ImpactScope:  "orders, inventory, and audit demo business queries can return 503 or high latency.",
		BoundaryText: "Controlled fixed-workload reconstruction in disposable preview-pg.",
		CandidateADetails: &PreviewCandidateSummary{
			CandidateID: "candidate-a", Name: "bounded pool resize", Action: "resize_pool",
			ChangeSummary: "Increase preview pool capacity", Decision: "PASS", Consistent: true,
			BusinessProbePass: true, AverageLatencyMS: 8.184, P95LatencyMS: 75.571,
			TPS: 672.063, ErrorCount: 0, WriteImpact: "preview_only",
		},
		CandidateBDetails: &PreviewCandidateSummary{
			CandidateID: "candidate-b", Name: "risky pool reset", Action: "reset_pool",
			ChangeSummary: "Clear preview sessions and reset pool", Decision: "REJECTED_BY_PREVIEW",
			RejectionReason: "business probe failed", Consistent: true, BusinessProbePass: false,
			AverageLatencyMS: 9.103, P95LatencyMS: 67.342, TPS: 589.683,
			ErrorCount: 0, WriteImpact: "preview_only",
		},
	}
	if err := publisher.PublishWorkflow(context.Background(), run, "awaiting_approval", decision); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(requestPath, "/final-demo-key-awaiting_approval-authority") {
		t.Fatalf("workflow authority transaction path must include an idempotency key: %s", requestPath)
	}
	authority := body["opskeeper.authority"].(map[string]any)
	token := authority["token"].(string)
	encodedClaims, signature, _ := strings.Cut(token, ".")
	claims, err := base64.RawURLEncoding.DecodeString(encodedClaims)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("0123456789abcdef"))
	_, _ = mac.Write(claims)
	if !hmac.Equal([]byte(signature), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		t.Fatal("invalid authority signature")
	}
	var authorityClaims struct {
		ManagerID           string `json:"manager_id"`
		RoomID              string `json:"room_id"`
		Stage               string `json:"stage"`
		Nonce               string `json:"nonce"`
		DecisionBriefSHA256 string `json:"decision_brief_sha256"`
	}
	if err := json.Unmarshal(claims, &authorityClaims); err != nil {
		t.Fatal(err)
	}
	if authorityClaims.ManagerID != "@manager:hs" || authorityClaims.RoomID != "!room:hs" ||
		authorityClaims.Stage != "awaiting_approval" || len(authorityClaims.Nonce) < 32 ||
		len(authorityClaims.DecisionBriefSHA256) != 64 {
		t.Fatalf("authority claims = %+v", authorityClaims)
	}
	workflow := body["agentteams.workflow"].(map[string]any)
	if workflow["runId"] != "100" || workflow["authorityStage"] != "awaiting_approval" {
		t.Fatalf("workflow = %+v", workflow)
	}
	briefJSON, err := json.Marshal(workflow["decision_brief"])
	if err != nil {
		t.Fatal(err)
	}
	var sentBrief workflowDecisionBrief
	if err := json.Unmarshal(briefJSON, &sentBrief); err != nil {
		t.Fatal(err)
	}
	briefJSON, err = json.Marshal(sentBrief)
	if err != nil {
		t.Fatal(err)
	}
	briefSum := sha256.Sum256(briefJSON)
	if authorityClaims.DecisionBriefSHA256 != hex.EncodeToString(briefSum[:]) {
		t.Fatalf("decision brief hash = %q want %q", authorityClaims.DecisionBriefSHA256, hex.EncodeToString(briefSum[:]))
	}
	if authority["decision_brief_sha256"] != authorityClaims.DecisionBriefSHA256 {
		t.Fatalf("authority decision brief hash = %v", authority["decision_brief_sha256"])
	}
	assertNoJSONFloats(t, rawBody)
	utcTime := authority["time_utc"].(string)
	beijingTime := authority["time_bjt"].(string)
	if !strings.HasSuffix(utcTime, "Z") || !strings.HasSuffix(beijingTime, "+08:00") {
		t.Fatalf("authority timestamps must be labelled UTC and UTC+8: utc=%q bjt=%q", utcTime, beijingTime)
	}
	message := body["body"].(string)
	if !strings.Contains(message, "time_utc=") || !strings.Contains(message, "time_bjt=") {
		t.Fatalf("authority message must expose both timezones: %q", message)
	}
	for _, required := range []string{
		"Root cause: PostgreSQL connection pool exhausted",
		"Candidate A: candidate-a (bounded pool resize / resize_pool): PASS",
		"Candidate B: candidate-b (risky pool reset / reset_pool): REJECTED_BY_PREVIEW",
		"rejection_reason=business probe failed",
		"Approval expires (UTC): 2026-09-19T13:19:34Z",
		"Approval expires (BJT): 2026-09-19T21:19:34+08:00",
		"Archive: https://teams.example/archive?incident_id=100",
		"Approval command: @manager 已批准 incident_id=100",
	} {
		if !strings.Contains(message, required) {
			t.Fatalf("authority message missing %q: %q", required, message)
		}
	}
	for _, forbidden := range []string{"password=", "api_key=", "secret="} {
		if strings.Contains(strings.ToLower(message), forbidden) {
			t.Fatalf("authority message must not contain sensitive data %q", forbidden)
		}
	}
}

func TestMatrixWorkflowPublisherRequiresCompleteConfiguration(t *testing.T) {
	if _, err := NewMatrixWorkflowPublisher("", "token", "!room", "@manager:hs", "0123456789abcdef"); err == nil {
		t.Fatal("expected missing URL failure")
	}
	if _, err := NewMatrixWorkflowPublisher("http://matrix", "token", "#room", "@manager:hs", "0123456789abcdef"); err == nil {
		t.Fatal("expected room failure")
	}
	if _, err := NewMatrixWorkflowPublisher("http://matrix", "token", "!room", "@manager:hs", "short"); err == nil {
		t.Fatal("expected secret failure")
	}
}

func assertNoJSONFloats(t *testing.T, data []byte) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	var visit func(value any)
	visit = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for _, child := range typed {
				visit(child)
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		case json.Number:
			number, err := typed.Float64()
			if err != nil || strings.ContainsAny(typed.String(), ".eE") ||
				number != math.Trunc(number) || math.Abs(number) > 9007199254740991 {
				t.Fatalf("matrix event contains a non-JS-integer number: %s", typed.String())
			}
		}
	}
	visit(value)
}

func TestMatrixWorkflowPublisherOmitsDecisionBriefOutsideApproval(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = writer.Write([]byte(`{"event_id":"$authority"}`))
	}))
	defer server.Close()

	publisher, err := NewMatrixWorkflowPublisher(
		server.URL, "matrix-token", "!room:hs", "@manager:hs", "0123456789abcdef",
	)
	if err != nil {
		t.Fatal(err)
	}
	run := &demomodel.ScenarioRun{IncidentID: 101, TargetFingerprint: "0123456789abcdef"}
	if err := publisher.PublishWorkflow(context.Background(), run, "recovered", nil); err != nil {
		t.Fatal(err)
	}
	workflow := body["agentteams.workflow"].(map[string]any)
	if _, exists := workflow["decision_brief"]; exists {
		t.Fatalf("non-approval workflow must omit decision brief: %+v", workflow)
	}
}

func TestMatrixWorkflowPublisherRetriesTransientFailure(t *testing.T) {
	requests := 0
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		paths = append(paths, request.URL.Path)
		if requests == 1 {
			http.Error(writer, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = writer.Write([]byte(`{"event_id":"$authority"}`))
	}))
	defer server.Close()

	publisher, err := NewMatrixWorkflowPublisher(
		server.URL, "matrix-token", "!room:hs", "@manager:hs", "0123456789abcdef",
	)
	if err != nil {
		t.Fatal(err)
	}
	run := &demomodel.ScenarioRun{
		IncidentID: 102, IdempotencyKey: "stable-key", TargetFingerprint: "0123456789abcdef",
	}
	decision := &PreviewDecisionSummary{CandidateA: "candidate-a", RootCause: "pool exhausted"}
	if err := publisher.PublishWorkflow(context.Background(), run, "awaiting_approval", decision); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || paths[0] != paths[1] {
		t.Fatalf("requests = %d paths = %v", requests, paths)
	}
}
