package incident

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	incidentcontrol "github.com/vincent-wuhan/opskeeper/internal/control/incident"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

func TestMetricsReturnsJudgeReport(t *testing.T) {
	repository := &stubMetricsRepository{tenantID: "opskeeper-demo"}
	router := routerWithHandler(NewHandler(repository))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/metrics?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Code int                    `json:"code"`
		Data incidentcontrol.Report `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != 0 || response.Data.IncidentCount != 1 {
		t.Fatalf("response = %+v", response)
	}
	if repository.lastTenantID != "opskeeper-demo" {
		t.Fatalf("tenant = %q", repository.lastTenantID)
	}
}

func TestMetricsUserCannotOverrideTenant(t *testing.T) {
	repository := &stubMetricsRepository{tenantID: "2"}
	router := routerWithHandler(NewHandler(repository))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/metrics?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 2, Role: "user"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if repository.lastTenantID != "2" {
		t.Fatalf("tenant = %q", repository.lastTenantID)
	}
}

func TestMetricsRepositoryErrorReturns500(t *testing.T) {
	router := routerWithHandler(NewHandler(&stubMetricsRepository{err: errors.New("database unavailable")}))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/metrics", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 2, Role: "user"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRunbooksAndRecallLogsAreExposed(t *testing.T) {
	repository := &stubMetricsRepository{tenantID: "opskeeper-demo"}
	router := routerWithHandler(NewHandler(repository))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/runbooks?tenant_id=opskeeper-demo&database_type=PostgreSQL", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("runbook status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/incidents/INC-API-001/recall-logs?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("recall status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func routerWithHandler(handler *Handler) http.Handler {
	router := chi.NewRouter()
	handler.Register(router)
	return router
}

type stubMetricsRepository struct {
	tenantID     string
	lastTenantID string
	err          error
}

func (repository *stubMetricsRepository) ListTenant(_ context.Context, tenantID string) ([]incidentcontrol.Event, error) {
	repository.lastTenantID = tenantID
	if repository.err != nil {
		return nil, repository.err
	}
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	return []incidentcontrol.Event{
		{
			ID: "017f2b01-4001-4000-8000-000000000001", TenantID: tenantID, IncidentID: "INC-API-001",
			OccurredAt: base, Phase: "detection", EventType: incidentcontrol.EventAlertReceived,
			ActorType: "system", Actor: "prometheus", Status: "firing",
			EvidenceRef: "evidence/alert.json", TraceID: "trace-api",
		},
		{
			ID: "017f2b01-4002-4000-8000-000000000002", TenantID: tenantID, IncidentID: "INC-API-001",
			OccurredAt: base.Add(time.Minute), Phase: "diagnosis", EventType: incidentcontrol.EventRootCause,
			ActorType: "agent", Actor: "diagnostics", Status: "confirmed",
			EvidenceRef: "evidence/diagnosis.json", TraceID: "trace-api",
		},
	}, nil
}

func (repository *stubMetricsRepository) ListRunbooks(_ context.Context, tenantID, databaseType, faultFingerprint string) ([]incidentcontrol.Postmortem, error) {
	postmortem := incidentcontrol.Postmortem{
		TenantID: tenantID, IncidentID: "INC-API-001", DatabaseType: databaseType,
		FaultFingerprint: faultFingerprint,
	}
	return []incidentcontrol.Postmortem{postmortem}, nil
}

func (repository *stubMetricsRepository) ListRecallLogs(_ context.Context, tenantID, incidentID string) ([]incidentcontrol.RecallLog, error) {
	return []incidentcontrol.RecallLog{{
		TenantID: tenantID, IncidentID: incidentID, CandidateRef: "runbook:INC-API-001",
		QueryText: "pool saturation", RRFScore: 0.03225806451612903, Selected: true,
	}}, nil
}
