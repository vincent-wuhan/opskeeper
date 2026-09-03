// server/chatdiagnose/http_test.go — minimal handler tests for the
// three chatdiagnose endpoints. Uses an in-memory fake service.
package chatdiagnose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	chatdiagnosebiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/chatdiagnose"
	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
)

// fakeService implements ChatDiagnoseService for tests.
type fakeService struct {
	diagnoseResp *chatdiagnosebiz.ChatDiagnoseResponse
	diagnoseErr  error

	promoteResp *chatdiagnosebiz.OrchestratorRunResult
	promoteErr  error

	reportErr  error
	lastReport struct {
		ConversationID string
		TenantID       string
		Markdown       string
	}
}

func (f *fakeService) Diagnose(_ context.Context, _ chatdiagnosebiz.ChatDiagnoseRequest) (*chatdiagnosebiz.ChatDiagnoseResponse, error) {
	return f.diagnoseResp, f.diagnoseErr
}
func (f *fakeService) PromoteToLoop(_ context.Context, _ string, _ int64, _ string) (*chatdiagnosebiz.OrchestratorRunResult, error) {
	return f.promoteResp, f.promoteErr
}
func (f *fakeService) PushReportToConversation(_ context.Context, convID, tenant, md string) error {
	f.lastReport.ConversationID = convID
	f.lastReport.TenantID = tenant
	f.lastReport.Markdown = md
	return f.reportErr
}

func newRouter(svc ChatDiagnoseService) http.Handler {
	r := chi.NewRouter()
	NewHandler(svc).Register(r)
	return r
}

func TestDiagnose_HappyPath(t *testing.T) {
	svc := &fakeService{
		diagnoseResp: &chatdiagnosebiz.ChatDiagnoseResponse{
			ConversationID: "conv-1",
			TurnID:         42,
			Reply:          "ok",
		},
	}
	body, _ := json.Marshal(ChatDiagnoseRequestBody{
		UserMessage:    "@sre-agent help",
		MentionedAgent: "sre-agent",
		TenantID:       "t-1",
		UserID:         "u-1",
	})
	r := newRouter(svc)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/diagnose", bytes.NewReader(body))
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp apiResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Code != 0 {
		t.Errorf("code = %d, want 0", resp.Code)
	}
}

func TestDiagnose_MissingTenant(t *testing.T) {
	svc := &fakeService{}
	body, _ := json.Marshal(ChatDiagnoseRequestBody{
		UserMessage: "@sre-agent help",
	})
	r := newRouter(svc)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/diagnose", bytes.NewReader(body))
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestDiagnose_FeatureDisabled(t *testing.T) {
	svc := &fakeService{diagnoseErr: chatdiagnosebiz.ErrFeatureDisabled}
	body, _ := json.Marshal(ChatDiagnoseRequestBody{
		UserMessage: "@sre-agent help",
		TenantID:    "t-1",
	})
	r := newRouter(svc)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/diagnose", bytes.NewReader(body))
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestPromote_HappyPath(t *testing.T) {
	svc := &fakeService{
		promoteResp: &chatdiagnosebiz.OrchestratorRunResult{
			IncidentID:       "inc-1",
			FirstLoopEventID: 7,
		},
	}
	body, _ := json.Marshal(PromoteRequestBody{TurnID: 1, TenantID: "t-1"})
	r := newRouter(svc)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/conversations/conv-1/promote", bytes.NewReader(body))
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func TestPromote_TenantMismatch(t *testing.T) {
	svc := &fakeService{promoteErr: chatdiagnosebiz.ErrConversationTenantMismatch}
	body, _ := json.Marshal(PromoteRequestBody{TurnID: 1, TenantID: "t-2"})
	r := newRouter(svc)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/conversations/conv-1/promote", bytes.NewReader(body))
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestPushReport_HappyPath(t *testing.T) {
	svc := &fakeService{}
	body, _ := json.Marshal(ReportRequestBody{TenantID: "t-1", ReportMarkdown: "# done"})
	r := newRouter(svc)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/conversations/conv-1/reports", bytes.NewReader(body))
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if svc.lastReport.ConversationID != "conv-1" || svc.lastReport.TenantID != "t-1" {
		t.Errorf("pusher got conv=%q tenant=%q", svc.lastReport.ConversationID, svc.lastReport.TenantID)
	}
}

func TestPushReport_InternalError(t *testing.T) {
	svc := &fakeService{reportErr: errors.New("boom")}
	body, _ := json.Marshal(ReportRequestBody{TenantID: "t-1", ReportMarkdown: "# done"})
	r := newRouter(svc)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/conversations/conv-1/reports", bytes.NewReader(body))
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// keep loop import used (go vet) — referenced indirectly via biz types
var _ = loop.PhasePostmortem
