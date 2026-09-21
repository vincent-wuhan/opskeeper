package demo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
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
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer matrix-token" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
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
	run := &demomodel.ScenarioRun{
		IncidentID: 100, TargetFingerprint: "0123456789abcdef",
		ExpiresAt: time.Date(2026, 9, 19, 13, 19, 34, 0, time.UTC),
	}
	decision := &PreviewDecisionSummary{
		ReplayProfileID: "sha256:workload-v1", CandidateA: "candidate-a", CandidateB: "candidate-b",
		RootCause:    "症状：orders/inventory/audit 查询返回 503 或明显变慢；确认根因：PostgreSQL connection pool exhausted（pg_pool_exhaustion）。",
		ImpactScope:  "影响 orders、inventory、audit 演示业务查询；修复范围限定在 target pool fixture，不直接变更共享 PostgreSQL。",
		BoundaryText: "Controlled fixed-workload reconstruction in disposable preview-pg; original active sessions are not copied.",
		CandidateADetails: &PreviewCandidateSummary{
			CandidateID: "candidate-a", Name: "bounded pool resize", Action: "resize_pool",
			ChangeSummary: "Increase preview pool capacity", Decision: "PASS",
			Consistent: true, BusinessProbePass: true, AverageLatencyMS: 1.254,
			P95LatencyMS: 10.065, TPS: 6139.071, ErrorCount: 0, WriteImpact: "preview_only",
		},
		CandidateBDetails: &PreviewCandidateSummary{
			CandidateID: "candidate-b", Name: "risky pool reset", Action: "reset_pool",
			ChangeSummary: "Clear preview sessions and reset pool", Decision: "REJECTED_BY_PREVIEW",
			RejectionReason: "business probe failed; api_key=must-not-leak", Consistent: true, BusinessProbePass: false,
			AverageLatencyMS: 1.243, P95LatencyMS: 9.512, TPS: 6236.157,
			ErrorCount: 0, WriteImpact: "preview_only",
		},
	}
	if err := publisher.PublishWorkflow(context.Background(), run, "awaiting_approval", decision); err != nil {
		t.Fatal(err)
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
		authorityClaims.Stage != "awaiting_approval" || len(authorityClaims.Nonce) < 32 {
		t.Fatalf("authority claims = %+v", authorityClaims)
	}
	if len(authorityClaims.DecisionBriefSHA256) != 64 {
		t.Fatalf("decision brief hash = %q", authorityClaims.DecisionBriefSHA256)
	}
	workflow := body["agentteams.workflow"].(map[string]any)
	if workflow["runId"] != "100" || workflow["authorityStage"] != "awaiting_approval" {
		t.Fatalf("workflow = %+v", workflow)
	}
	if workflow["title"] != "OpsKeeper 事故恢复 100" || workflow["status"] != "in_progress" {
		t.Fatalf("workflow display fields = %+v", workflow)
	}
	rawSteps := workflow["steps"].([]any)
	if len(rawSteps) != 4 {
		t.Fatalf("workflow steps = %+v", rawSteps)
	}
	steps := make([]map[string]any, 0, len(rawSteps))
	for _, rawStep := range rawSteps {
		steps = append(steps, rawStep.(map[string]any))
	}
	if steps[0]["status"] != "completed" || steps[1]["status"] != "in_progress" {
		t.Fatalf("workflow steps = %+v", steps)
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
	assertNoNonIntegerJSONNumbers(t, body)
	candidate := workflow["decision_brief"].(map[string]any)["candidate_a"].(map[string]any)
	if candidate["average_latency_ms"] != "1.254" || candidate["p95_latency_ms"] != "10.065" || candidate["tps"] != "6139.071" {
		t.Fatalf("candidate metrics must be Matrix JSON-safe strings: %+v", candidate)
	}
	utcTime := authority["time_utc"].(string)
	beijingTime := authority["time_bjt"].(string)
	if !strings.HasSuffix(utcTime, "Z") || !strings.HasSuffix(beijingTime, "+08:00") {
		t.Fatalf("authority timestamps must be labelled UTC and UTC+8: utc=%q bjt=%q", utcTime, beijingTime)
	}
	if message := body["body"].(string); !strings.Contains(message, "time_utc=") || !strings.Contains(message, "time_bjt=") {
		t.Fatalf("authority message must expose both timezones: %q", message)
	}
	message := body["body"].(string)
	for _, required := range []string{
		"确认根因：症状：orders/inventory/audit 查询返回 503 或明显变慢",
		"影响范围：影响 orders、inventory、audit 演示业务查询",
		"预演边界：Controlled fixed-workload reconstruction in disposable preview-pg",
		"候选 A：candidate-a（bounded pool resize / resize_pool）：PASS",
		"average_latency_ms=1.254；p95_latency_ms=10.065；tps=6139.071；error_count=0；write_impact=preview_only",
		"候选 B：candidate-b（risky pool reset / reset_pool）：REJECTED_BY_PREVIEW",
		"rejection_reason=business probe failed",
		"审批有效期（UTC）：2026-09-19T13:19:34Z",
		"审批有效期（北京时间）：2026-09-19T21:19:34+08:00",
		"证据档案：https://teams.example/archive?incident_id=100",
		"建议审批命令：@manager 已批准 incident_id=100 Candidate A",
	} {
		if !strings.Contains(message, required) {
			t.Fatalf("authority message missing %q: %q", required, message)
		}
	}
	lowerMessage := strings.ToLower(message)
	for _, forbidden := range []string{"password=", "api_key=", "secret=", "authorization="} {
		if strings.Contains(lowerMessage, forbidden) {
			t.Fatalf("authority message must not contain sensitive data %q", forbidden)
		}
	}
	briefJSON, err = json.Marshal(authority["decision_brief"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(briefJSON)), "must-not-leak") {
		t.Fatalf("structured decision brief must redact sensitive evidence: %s", briefJSON)
	}
}

func assertNoNonIntegerJSONNumbers(t *testing.T, value any) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for _, nested := range typed {
			assertNoNonIntegerJSONNumbers(t, nested)
		}
	case []any:
		for _, nested := range typed {
			assertNoNonIntegerJSONNumbers(t, nested)
		}
	case float64:
		if typed != float64(int64(typed)) {
			t.Fatalf("Matrix canonical JSON contains non-integer number: %v", typed)
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

func TestMatrixWorkflowPublisherRetriesTransientFailureWithStableTransaction(t *testing.T) {
	var requests []struct {
		path string
		body string
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		requests = append(requests, struct {
			path string
			body string
		}{path: request.URL.Path, body: string(payload)})
		if len(requests) == 1 {
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = writer.Write([]byte(`{"event_id":"$authority-retry"}`))
	}))
	defer server.Close()

	publisher, err := NewMatrixWorkflowPublisher(
		server.URL, "matrix-token", "!room:hs", "@manager:hs", "0123456789abcdef",
	)
	if err != nil {
		t.Fatal(err)
	}
	run := &demomodel.ScenarioRun{
		IncidentID: 102, IdempotencyKey: "final-demo-retry", TargetFingerprint: "0123456789abcdef",
	}
	if err := publisher.PublishWorkflow(context.Background(), run, "awaiting_approval", nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[0].path != requests[1].path || requests[0].body != requests[1].body {
		t.Fatalf("retry changed Matrix transaction: first=%+v second=%+v", requests[0], requests[1])
	}
	if !strings.Contains(requests[0].path, "/final-demo-retry-awaiting_approval-authority") {
		t.Fatalf("transaction path = %q", requests[0].path)
	}
}

func TestMatrixWorkflowPublisherIncludesSanitizedMatrixErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"errcode":"M_INVALID_JSON","error":"invalid content api_key=secret-token"}`))
	}))
	defer server.Close()

	publisher, err := NewMatrixWorkflowPublisher(
		server.URL, "matrix-token", "!room:hs", "@manager:hs", "0123456789abcdef",
	)
	if err != nil {
		t.Fatal(err)
	}
	run := &demomodel.ScenarioRun{
		IncidentID: 103, IdempotencyKey: "final-demo-error-body", TargetFingerprint: "0123456789abcdef",
	}
	err = publisher.PublishWorkflow(context.Background(), run, "awaiting_approval", nil)
	if err == nil {
		t.Fatal("expected Matrix error")
	}
	message := err.Error()
	if !strings.Contains(message, "400 Bad Request") || !strings.Contains(message, "M_INVALID_JSON") {
		t.Fatalf("error missing Matrix response body: %v", err)
	}
	if strings.Contains(message, "secret-token") {
		t.Fatalf("error leaked sensitive response data: %v", err)
	}
	if !strings.Contains(message, "[REDACTED]") {
		t.Fatalf("error did not redact sensitive response data: %v", err)
	}
}

func TestWorkflowArchiveURLRejectsEmbeddedCredentials(t *testing.T) {
	t.Setenv("OPSKEEPER_DEMO_ARCHIVE_URL_TEMPLATE", "https://user:pass@teams.example/archive?incident_id={incident_id}&api_key=value")
	if got := workflowArchiveURL("100"); got != "opskeeper://incidents/100/archive" {
		t.Fatalf("archive URL = %q", got)
	}
}

func TestWorkflowArchiveURLAllowsSafeStaticTeamsArchive(t *testing.T) {
	t.Setenv("OPSKEEPER_DEMO_ARCHIVE_URL_TEMPLATE", "https://teams.example/#plugin-route:opskeeper-teamharness/archive")
	const template = "https://teams.example/#plugin-route:opskeeper-teamharness/archive"
	if got := workflowArchiveURL("100"); got != template {
		t.Fatalf("archive URL = %q", got)
	}
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
	authority := body["opskeeper.authority"].(map[string]any)
	if _, exists := workflow["decision_brief"]; exists {
		t.Fatalf("non-approval workflow must omit decision brief: %+v", workflow)
	}
	if _, exists := authority["decision_brief"]; exists {
		t.Fatalf("non-approval authority must omit decision brief: %+v", authority)
	}
	if !strings.Contains(body["body"].(string), "OPSKEEPER_AUTHORITY_V1 ") {
		t.Fatalf("non-approval message must retain authority token: %q", body["body"])
	}
}

func TestMatrixWorkflowPublisherSendsChineseExpiredClosure(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = writer.Write([]byte(`{"event_id":"$authority-closed"}`))
	}))
	defer server.Close()

	publisher, err := NewMatrixWorkflowPublisher(
		server.URL, "matrix-token", "!room:hs", "@manager:hs", "0123456789abcdef",
	)
	if err != nil {
		t.Fatal(err)
	}
	run := &demomodel.ScenarioRun{
		IncidentID: 104, IdempotencyKey: "final-demo-expired", TargetFingerprint: "0123456789abcdef",
		ExpiresAt: time.Date(2026, 9, 20, 15, 46, 38, 0, time.UTC),
	}
	if err := publisher.PublishWorkflow(context.Background(), run, demomodel.ScenarioStatusClosed, nil); err != nil {
		t.Fatal(err)
	}
	message := body["body"].(string)
	for _, required := range []string{
		"stage=closed", "处理结果：审批已过期，未伪造人工审批，未执行修复。",
		"审批过期时间（UTC）：2026-09-20T15:46:38Z", "审批过期时间（北京时间）：2026-09-20T23:46:38+08:00",
	} {
		if !strings.Contains(message, required) {
			t.Fatalf("closure message missing %q: %q", required, message)
		}
	}
	workflow := body["agentteams.workflow"].(map[string]any)
	if workflow["status"] != "expired" || workflow["summary"] != "审批过期，安全关闭，未执行修复" {
		t.Fatalf("workflow closure display = %+v", workflow)
	}
	if _, exists := workflow["decision_brief"]; exists {
		t.Fatalf("closure event unexpectedly carries decision brief: %+v", workflow)
	}
}
