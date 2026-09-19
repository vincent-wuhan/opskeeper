package setting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	bizsetting "github.com/vincent-wuhan/opskeeper/internal/manager/biz/setting"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

type llmRevealTestService struct {
	getCalls int
}

func (s *llmRevealTestService) Get(_ context.Context, _, _ string) (string, bool, error) {
	s.getCalls++
	return "secret-value", true, nil
}

func (s *llmRevealTestService) Set(_ context.Context, _, _, _ string, _ bool) error { return nil }

func (s *llmRevealTestService) List(_ context.Context, _ string) ([]bizsetting.SettingDTO, error) {
	return nil, nil
}

func (s *llmRevealTestService) Delete(_ context.Context, _, _ string) error { return nil }

func TestRevealLLMAPIKeyIsForbidden(t *testing.T) {
	svc := &llmRevealTestService{}
	router := chi.NewRouter()
	NewHandler(svc).Register(router)

	request := httptest.NewRequest(http.MethodGet, "/v1/system-settings/llm/openai_api_key/reveal", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{
		UserID: 1, Role: "admin", IsSuperuser: true,
	}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", body.Code)
	}
	if svc.getCalls != 0 {
		t.Fatalf("Get calls = %d, want 0", svc.getCalls)
	}
}
