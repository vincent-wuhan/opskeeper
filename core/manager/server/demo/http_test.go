package demo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	bizdemo "github.com/vincent-wuhan/opskeeper/core/manager/biz/demo"
	servicedemo "github.com/vincent-wuhan/opskeeper/core/manager/service/demo"
)

type fakeService struct {
	starts     int
	gets       int
	business   map[string]int
	baseline   map[string]int
	advances   map[string]string
	approvals  map[uint64]string
	lastInput  servicedemo.StartScenarioInput
	lastTenant uint64
}

func (f *fakeService) AdvanceWorkflow(_ context.Context, _ uint64, _, key, stage string) (*servicedemo.ScenarioStatus, error) {
	if f.advances == nil {
		f.advances = map[string]string{}
	}
	f.advances[key] = stage
	return &servicedemo.ScenarioStatus{
		IncidentID: 1001, ScenarioID: bizdemo.ScenarioID, Status: stage,
		TargetFingerprint: "0123456789abcdef", UpdatedAt: "2026-09-18T00:00:00Z",
	}, nil
}

func (f *fakeService) Approve(_ context.Context, _ uint64, incidentID uint64, input servicedemo.ApproveScenarioInput) (*servicedemo.ScenarioStatus, error) {
	if f.approvals == nil {
		f.approvals = map[uint64]string{}
	}
	f.approvals[incidentID] = input.ApproverID
	return &servicedemo.ScenarioStatus{
		IncidentID: incidentID, ScenarioID: bizdemo.ScenarioID, Status: "recovered",
		TargetFingerprint: "0123456789abcdef", UpdatedAt: "2026-09-18T00:00:00Z",
	}, nil
}

func (f *fakeService) Start(_ context.Context, tenantID uint64, input servicedemo.StartScenarioInput) (*servicedemo.ScenarioStatus, error) {
	f.starts++
	f.lastTenant = tenantID
	f.lastInput = input
	return &servicedemo.ScenarioStatus{
		IncidentID: 1001, ScenarioID: bizdemo.ScenarioID, Status: "awaiting_alert",
		PoolManifestID: "manifest-1", TargetFingerprint: input.TargetFingerprint,
		AlertFingerprint: input.AlertFingerprint, UpdatedAt: "2026-09-18T00:00:00Z",
	}, nil
}

func (f *fakeService) Get(context.Context, uint64, string, string) (*servicedemo.ScenarioStatus, error) {
	f.gets++
	return &servicedemo.ScenarioStatus{
		IncidentID: 1001, ScenarioID: bizdemo.ScenarioID, Status: "awaiting_alert",
		PoolManifestID: "manifest-1", TargetFingerprint: "0123456789abcdef",
		AlertFingerprint: "fedcba9876543210", UpdatedAt: "2026-09-18T00:00:00Z",
	}, nil
}

func (f *fakeService) BusinessSnapshot(_ context.Context, _ uint64, _, _, section string) (json.RawMessage, error) {
	if f.business == nil {
		f.business = map[string]int{}
	}
	f.business[section]++
	return json.RawMessage(`{"section":"` + section + `","ok":true}`), nil
}

func (f *fakeService) BusinessSnapshotBaseline(_ context.Context, section string) (json.RawMessage, error) {
	if f.baseline == nil {
		f.baseline = map[string]int{}
	}
	f.baseline[section]++
	return json.RawMessage(`{"section":"` + section + `","baseline":true}`), nil
}

func newRouter(service Service) http.Handler {
	router := chi.NewRouter()
	NewHandler(service, "demo-secret").Register(router)
	return router
}

func authorized(request *http.Request) *http.Request {
	request.Header.Set("Authorization", "Bearer demo-secret")
	request.Header.Set("X-Opskeeper-Version", "v1")
	return request
}

func TestDemoHandlerRejectsBearer(t *testing.T) {
	router := newRouter(&fakeService{})
	request := httptest.NewRequest(http.MethodPost, "/v1/demo/scenarios/pg-pool-exhaustion/start", strings.NewReader(`{}`))
	_ = authorized(request)
	request.Header.Set("Authorization", "Bearer wrong")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), `"error_code":"unauthorized"`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDemoHandlerAdvancesAuthoritativeWorkflow(t *testing.T) {
	service := &fakeService{}
	router := newRouter(service)
	request := authorized(httptest.NewRequest(
		http.MethodPost,
		"/v1/demo/scenarios/final-demo-key/workflow/repair_dispatched",
		nil,
	))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %d headers = %v", recorder.Code, recorder.Header())
	}
	if service.advances["final-demo-key"] != "repair_dispatched" ||
		!strings.Contains(recorder.Body.String(), `"status":"repair_dispatched"`) {
		t.Fatalf("advances = %v body = %s", service.advances, recorder.Body.String())
	}
}

func TestDemoHandlerApprovesDeterministicScenario(t *testing.T) {
	service := &fakeService{}
	router := newRouter(service)
	request := authorized(httptest.NewRequest(
		http.MethodPost,
		"/v1/demo/incidents/1001/approve",
		strings.NewReader(`{"approver_id":"@admin:matrix-local.agentteams.io:18080"}`),
	))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	if service.approvals[1001] != "@admin:matrix-local.agentteams.io:18080" ||
		!strings.Contains(recorder.Body.String(), `"status":"recovered"`) {
		t.Fatalf("approvals = %v body = %s", service.approvals, recorder.Body.String())
	}
}

func TestDemoHandlerStartsAndReplaysIdempotently(t *testing.T) {
	service := &fakeService{}
	router := newRouter(service)
	body := `{"idempotency_key":"final-demo-key","scenario_id":"pg-pool-exhaustion","target":"pg:pool-fixture","target_fingerprint":"0123456789abcdef","alert_fingerprint":"fedcba9876543210","duration_seconds":90}`
	var previous servicedemo.ScenarioStatus
	for i := 0; i < 2; i++ {
		request := authorized(httptest.NewRequest(http.MethodPost, "/v1/demo/scenarios/pg-pool-exhaustion/start", strings.NewReader(body)))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusCreated || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response = %d headers = %v", recorder.Code, recorder.Header())
		}
		var response struct {
			Data servicedemo.ScenarioStatus `json:"data"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.Data.IncidentID != 1001 || response.Data.PoolManifestID != "manifest-1" {
			t.Fatalf("status = %+v", response.Data)
		}
		if i > 0 && response.Data.IncidentID != previous.IncidentID {
			t.Fatalf("incident changed: %+v != %+v", response.Data, previous)
		}
		previous = response.Data
	}
	if service.starts != 2 || service.lastTenant != 1 || service.lastInput.IdempotencyKey != "final-demo-key" {
		t.Fatalf("service calls = %+v", service)
	}
}

func TestDemoHandlerReturnsStatusReadback(t *testing.T) {
	service := &fakeService{}
	router := newRouter(service)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, authorized(httptest.NewRequest(http.MethodGet, "/v1/demo/scenarios/final-demo-key", nil)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"incident_id":1001`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	if service.gets != 1 {
		t.Fatalf("gets = %d", service.gets)
	}
}

func TestDemoHandlerProxiesThreeBusinessSections(t *testing.T) {
	service := &fakeService{}
	router := newRouter(service)
	for _, section := range []string{"orders", "inventory", "audit"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, authorized(httptest.NewRequest(http.MethodGet, "/v1/demo/scenarios/final-demo-key/business/"+section, nil)))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), section) || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s response = %d %s", section, recorder.Code, recorder.Body.String())
		}
	}
	if service.business["orders"] != 1 || service.business["inventory"] != 1 || service.business["audit"] != 1 {
		t.Fatalf("business calls = %+v", service.business)
	}
}

func TestDemoHandlerProxiesBaselineBusinessSections(t *testing.T) {
	service := &fakeService{}
	router := newRouter(service)
	for _, section := range []string{"orders", "inventory", "audit"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, authorized(httptest.NewRequest(http.MethodGet, "/v1/demo/business/"+section, nil)))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), section) ||
			recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s response = %d %s", section, recorder.Code, recorder.Body.String())
		}
	}
	if service.baseline["orders"] != 1 || service.baseline["inventory"] != 1 || service.baseline["audit"] != 1 {
		t.Fatalf("baseline calls = %+v", service.baseline)
	}
	if len(service.business) != 0 || service.starts != 0 || service.gets != 0 {
		t.Fatalf("unexpected scenario calls: %+v", service)
	}
}
