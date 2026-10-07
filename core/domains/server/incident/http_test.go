package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	incidentcontrol "github.com/vincent-wuhan/opskeeper/core/domains/control/incident"
	repairpreview "github.com/vincent-wuhan/opskeeper/core/domains/control/repairpreview"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
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

func TestMetricsDefaultTenantCanBeConfigured(t *testing.T) {
	t.Setenv("OPSKEEPER_DEFAULT_INCIDENT_TENANT_ID", "open-source-test")
	repository := &stubMetricsRepository{tenantID: "open-source-test"}
	router := routerWithHandler(NewHandler(repository))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/metrics", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{
		AgentTeams: &tenantctx.AgentTeamsIdentity{TenantID: "default", Role: "worker"},
	}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if repository.lastTenantID != "open-source-test" {
		t.Fatalf("tenant = %q, want open-source-test", repository.lastTenantID)
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

func TestArchiveReturnsCompleteEvidenceChain(t *testing.T) {
	events := completeArchiveEvents("opskeeper-demo", "INC-ARCHIVE-FULL")
	repository := &stubMetricsRepository{
		incidentEvents: map[string][]incidentcontrol.Event{
			"opskeeper-demo/INC-ARCHIVE-FULL": events,
		},
		tenantEvents: append(events, similarArchiveEvent("opskeeper-demo", "INC-ARCHIVE-SIMILAR")),
		runbooks: []incidentcontrol.Postmortem{{
			ID: "runbook-1", TenantID: "opskeeper-demo", IncidentID: "INC-ARCHIVE-FULL",
			Diagnosis:   incidentcontrol.PostmortemDiagnosis{RootCause: "connection pool saturation"},
			ConfirmedBy: "reviewer", ConfirmedAt: time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC),
		}},
	}
	router := routerWithHandler(NewHandler(repository))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/INC-ARCHIVE-FULL/archive?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Code int            `json:"code"`
		Data archiveSummary `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	archive := response.Data
	if !archive.EvidenceComplete || archive.EventCount != 7 || len(archive.MissingEventTypes) != 0 {
		t.Fatalf("archive completeness = %+v", archive)
	}
	if !archive.RecoveryObserved || !archive.Closed {
		t.Fatalf("archive recovery status = %+v", archive)
	}
	if archive.LocalizationSeconds != 60 || archive.RecoverySeconds != 90 {
		t.Fatalf("archive timing = %+v", archive)
	}
	if len(archive.SimilarIncidents) != 1 || archive.SimilarIncidents[0].IncidentID != "INC-ARCHIVE-SIMILAR" {
		t.Fatalf("similar incidents = %+v", archive.SimilarIncidents)
	}
	if len(archive.PostmortemRefs) != 1 || archive.PostmortemRefs[0].RootCause != "connection pool saturation" {
		t.Fatalf("postmortem refs = %+v", archive.PostmortemRefs)
	}
	if repository.lastIncidentTenantID != "opskeeper-demo" || repository.lastIncidentID != "INC-ARCHIVE-FULL" {
		t.Fatalf("incident lookup = %s/%s", repository.lastIncidentTenantID, repository.lastIncidentID)
	}
}

func TestArchiveIndexListsClosedEvidenceIncidents(t *testing.T) {
	events := completeArchiveEvents("opskeeper-demo", "INC-ARCHIVE-FULL")
	repository := &stubMetricsRepository{tenantEvents: events}
	router := routerWithHandler(NewHandler(repository))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/archive-index?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Items []archiveIndexItem `json:"items"`
		Total int                `json:"total"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 || len(response.Items) != 1 {
		t.Fatalf("archive index = %+v", response)
	}
	item := response.Items[0]
	if item.IncidentID != "INC-ARCHIVE-FULL" || item.EventCount != 7 || !item.EvidenceComplete || !item.Closed {
		t.Fatalf("archive index item = %+v", item)
	}
}

func TestArchiveReportsMissingEvidence(t *testing.T) {
	events := completeArchiveEvents("opskeeper-demo", "INC-ARCHIVE-MISSING")[:2]
	repository := &stubMetricsRepository{
		incidentEvents: map[string][]incidentcontrol.Event{"opskeeper-demo/INC-ARCHIVE-MISSING": events},
	}
	router := routerWithHandler(NewHandler(repository))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/INC-ARCHIVE-MISSING/archive?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data archiveSummary `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.EvidenceComplete || len(response.Data.MissingEventTypes) != 5 {
		t.Fatalf("archive = %+v", response.Data)
	}
}

func TestArchiveIncludesBoundedRepairPreviews(t *testing.T) {
	events := completeArchiveEvents("opskeeper-demo", "INC-ARCHIVE-FULL")
	run := previewRun("opskeeper-demo", "INC-ARCHIVE-FULL")
	repository := &stubMetricsRepository{
		incidentEvents: map[string][]incidentcontrol.Event{
			"opskeeper-demo/INC-ARCHIVE-FULL": events,
			"2/INC-ARCHIVE-FULL":              events,
		},
		previewRuns: []repairpreview.Run{run},
	}
	router := routerWithHandler(NewHandler(repository, &stubPreviewRepository{runs: []repairpreview.Run{run}}))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/INC-ARCHIVE-FULL/archive?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data archiveSummary `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data.RepairPreviews) != 1 || len(response.Data.RepairPreviews[0].Candidates) != 3 {
		t.Fatalf("repair previews = %+v", response.Data.RepairPreviews)
	}
	if response.Data.RepairPreviews[0].Candidates[0].CandidateID != "baseline" {
		t.Fatalf("first repair preview row is not baseline: %+v", response.Data.RepairPreviews[0].Candidates[0])
	}
	body := recorder.Body.String()
	for _, wireField := range []string{
		`"workload_fingerprint":"sha256:workload-v1"`, `"seed_fingerprint":"sha256:seed-v1"`,
		`"isolation_boundary":"preview-pg"`, `"average_latency_ms":18`, `"p95_latency_ms":29`,
		`"write_impact":"none"`, `"storage_delta_bytes":0`, `"business_probe_pass":true`,
	} {
		if !strings.Contains(body, wireField) {
			t.Fatalf("repair preview wire field %s missing: %s", wireField, body)
		}
	}
	if !response.Data.EvidenceComplete {
		t.Fatalf("legacy evidence completeness changed: %+v", response.Data)
	}
}

func TestRepairPreviewSummaryReturnsEmptyForLegacyIncident(t *testing.T) {
	events := completeArchiveEvents("opskeeper-demo", "INC-ARCHIVE-FULL")
	repository := &stubMetricsRepository{
		incidentEvents: map[string][]incidentcontrol.Event{"opskeeper-demo/INC-ARCHIVE-FULL": events},
		tenantEvents:   events,
	}
	router := routerWithHandler(NewHandler(repository, &stubPreviewRepository{}))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/INC-ARCHIVE-FULL/repair-preview-summary?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"run_id":""`) {
		t.Fatalf("empty summary missing empty run id: %s", body)
	}
}

func TestRepairPreviewSummaryProjectsPersistedBaselineAndWireFields(t *testing.T) {
	events := completeArchiveEvents("opskeeper-demo", "INC-ARCHIVE-FULL")
	run := previewRun("opskeeper-demo", "INC-ARCHIVE-FULL")
	repository := &stubMetricsRepository{
		incidentEvents: map[string][]incidentcontrol.Event{"opskeeper-demo/INC-ARCHIVE-FULL": events},
		tenantEvents:   events,
	}
	router := routerWithHandler(NewHandler(repository, &stubPreviewRepository{runs: []repairpreview.Run{run}}))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/INC-ARCHIVE-FULL/repair-preview-summary?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, wireField := range []string{
		`"baseline":{"id":"017f2b01-6000-4000-8000-000000000000"`,
		`"candidate_id":"baseline"`, `"average_latency_ms":18`, `"write_impact":"none"`,
		`"storage_delta_bytes":0`, `"passing":{"id":"017f2b01-6001-4000-8000-000000000001"`,
		`"workload_fingerprint":"sha256:workload-v1"`, `"seed_fingerprint":"sha256:seed-v1"`,
		`"isolation_boundary":"preview-pg"`,
	} {
		if !strings.Contains(body, wireField) {
			t.Fatalf("compact wire field %s missing: %s", wireField, body)
		}
	}
}

func TestRepairPreviewSummaryIsTenantIsolatedAndDoesNotLeakRepositoryErrors(t *testing.T) {
	events := completeArchiveEvents("opskeeper-demo", "INC-ARCHIVE-FULL")
	repository := &stubMetricsRepository{
		incidentEvents: map[string][]incidentcontrol.Event{
			"opskeeper-demo/INC-ARCHIVE-FULL": events,
			"2/INC-ARCHIVE-FULL":              events,
		},
	}
	previews := &stubPreviewRepository{}
	router := routerWithHandler(NewHandler(repository, previews))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/INC-ARCHIVE-FULL/repair-preview-summary?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 2, Role: "user"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || previews.lastTenantID != "2" {
		t.Fatalf("tenant isolation status=%d tenant=%q", recorder.Code, previews.lastTenantID)
	}

	previews.err = errors.New("SQLSTATE=42P01 secret")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); strings.Contains(body, "SQLSTATE") || strings.Contains(body, "secret") {
		t.Fatalf("repository detail leaked: %s", body)
	}
}

func TestArchiveIsTenantIsolated(t *testing.T) {
	events := completeArchiveEvents("opskeeper-demo", "INC-ARCHIVE-FULL")
	repository := &stubMetricsRepository{
		incidentEvents: map[string][]incidentcontrol.Event{"opskeeper-demo/INC-ARCHIVE-FULL": events},
	}
	router := routerWithHandler(NewHandler(repository))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/INC-ARCHIVE-FULL/archive?tenant_id=opskeeper-demo", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 2, Role: "user"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if repository.lastIncidentTenantID != "2" {
		t.Fatalf("tenant = %q", repository.lastIncidentTenantID)
	}
}

func TestArchiveEmptyIncidentReturns404(t *testing.T) {
	router := routerWithHandler(NewHandler(&stubMetricsRepository{}))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/INC-MISSING/archive", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 2, Role: "user"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestArchiveRepositoryErrorReturns500(t *testing.T) {
	router := routerWithHandler(NewHandler(&stubMetricsRepository{err: errors.New("database unavailable")}))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/INC-API-001/archive", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 2, Role: "user"}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); strings.Contains(body, "database unavailable") {
		t.Fatalf("error leaked repository detail: %s", body)
	}
}

func routerWithHandler(handler *Handler) http.Handler {
	router := chi.NewRouter()
	handler.Register(router)
	return router
}

type stubMetricsRepository struct {
	tenantID             string
	lastTenantID         string
	lastIncidentTenantID string
	lastIncidentID       string
	err                  error
	incidentEvents       map[string][]incidentcontrol.Event
	tenantEvents         []incidentcontrol.Event
	runbooks             []incidentcontrol.Postmortem
	previewRuns          []repairpreview.Run
}

type stubPreviewRepository struct {
	runs           []repairpreview.Run
	err            error
	lastTenantID   string
	lastIncidentID string
}

func (repository *stubPreviewRepository) Save(context.Context, repairpreview.Run) error { return nil }

func (repository *stubPreviewRepository) ListByIncident(_ context.Context, tenantID, incidentID string, _ int) ([]repairpreview.Run, error) {
	repository.lastTenantID = tenantID
	repository.lastIncidentID = incidentID
	return repository.runs, repository.err
}

func (repository *stubPreviewRepository) FindEligible(_ context.Context, _, _, _, _, _ string) (repairpreview.Candidate, error) {
	return repairpreview.Candidate{}, repairpreview.ErrNotFound
}

func previewRun(tenantID, incidentID string) repairpreview.Run {
	baseline := repairpreview.Candidate{
		ID: "017f2b01-6000-4000-8000-000000000000", RunID: "017f2b01-6000-4000-8000-000000000000",
		TenantID: tenantID, IncidentID: incidentID, CandidateID: "baseline", Name: "Baseline replay",
		Kind: "baseline", Action: "baseline", ChangeSummary: "Controlled fixed-workload baseline",
		Branch: "preview/baseline", ResultChecksum: "sha256:baseline", Consistent: true,
		AverageLatencyMS: 18, MedianLatencyMS: 17, P95LatencyMS: 29, SampleCount: 10,
		TPS: 120, WriteImpact: "none", StorageDeltaBytes: 0, BusinessProbePass: true,
		Decision: repairpreview.DecisionPass,
	}
	passing := repairpreview.Candidate{
		ID: "017f2b01-6001-4000-8000-000000000001", RunID: "017f2b01-6000-4000-8000-000000000000",
		TenantID: tenantID, IncidentID: incidentID, CandidateID: "candidate-a", Name: "bounded resize",
		Kind: "postgresql", Action: "resize_pool", ChangeSummary: "bounded capacity change",
		Branch: "preview/candidate-a", ResultChecksum: "sha256:baseline", Consistent: true,
		AverageLatencyMS: 12, MedianLatencyMS: 11, P95LatencyMS: 20, SampleCount: 10,
		TPS: 100, WriteImpact: "preview_only", StorageDeltaBytes: 1024, BusinessProbePass: true,
		Decision: repairpreview.DecisionPass,
	}
	rejected := passing
	rejected.ID = "017f2b01-6002-4000-8000-000000000002"
	rejected.CandidateID = "candidate-b"
	rejected.Action = "reset_pool"
	rejected.BusinessProbePass = false
	rejected.Decision = repairpreview.DecisionReject
	rejected.RejectionReason = "business probe failed"
	return repairpreview.Run{
		ID: passing.RunID, TenantID: tenantID, IncidentID: incidentID, BranchPrefix: "preview/incident",
		SeedFingerprint: "sha256:seed-v1", WorkloadFingerprint: "sha256:workload-v1", WorkloadRevision: "workload-v1",
		ControlledLoad: true, IsolationBoundary: "preview-pg", Status: "finished",
		StartedAt:  time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 9, 18, 10, 1, 0, 0, time.UTC), Candidates: []repairpreview.Candidate{baseline, passing, rejected},
	}
}

func (repository *stubMetricsRepository) ListTenant(_ context.Context, tenantID string) ([]incidentcontrol.Event, error) {
	repository.lastTenantID = tenantID
	if repository.err != nil {
		return nil, repository.err
	}
	if repository.tenantEvents != nil {
		return repository.tenantEvents, nil
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

func (repository *stubMetricsRepository) ListIncident(_ context.Context, tenantID, incidentID string) ([]incidentcontrol.Event, error) {
	repository.lastIncidentTenantID = tenantID
	repository.lastIncidentID = incidentID
	if repository.err != nil {
		return nil, repository.err
	}
	return repository.incidentEvents[tenantID+"/"+incidentID], nil
}

func (repository *stubMetricsRepository) ListRunbooks(_ context.Context, tenantID, databaseType, faultFingerprint string) ([]incidentcontrol.Postmortem, error) {
	postmortem := incidentcontrol.Postmortem{
		TenantID: tenantID, IncidentID: "INC-API-001", DatabaseType: databaseType,
		FaultFingerprint: faultFingerprint,
	}
	return []incidentcontrol.Postmortem{postmortem}, nil
}

func (repository *stubMetricsRepository) ListIncidentRunbooks(_ context.Context, tenantID, incidentID string) ([]incidentcontrol.Postmortem, error) {
	if repository.err != nil {
		return nil, repository.err
	}
	return repository.runbooks, nil
}

func completeArchiveEvents(tenantID, incidentID string) []incidentcontrol.Event {
	base := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	eventTypes := []struct {
		eventType string
		phase     string
		actorType string
		actor     string
		status    string
		offset    time.Duration
	}{
		{incidentcontrol.EventAlertReceived, "detection", "system", "prometheus", "firing", 0},
		{incidentcontrol.EventRootCause, "diagnosis", "agent", "investigator", "confirmed", time.Minute},
		{incidentcontrol.EventEvidenceRefreshed, "diagnosis", "agent", "investigator", "complete", 2 * time.Minute},
		{incidentcontrol.EventApproved, "approval", "human", "reviewer", "approved", 3 * time.Minute},
		{incidentcontrol.EventAction, "execution", "agent", "repairer", "executed", 4 * time.Minute},
		{incidentcontrol.EventRecovery, "verification", "system", "verifier", "observed", 5*time.Minute + 30*time.Second},
		{incidentcontrol.EventClosed, "closure", "agent", "reporter", "closed", 6 * time.Minute},
	}
	events := make([]incidentcontrol.Event, 0, len(eventTypes))
	for index, item := range eventTypes {
		event := incidentcontrol.Event{
			ID: fmt.Sprintf("event-%02d", index+1), TenantID: tenantID, IncidentID: incidentID,
			OccurredAt: base.Add(item.offset), Phase: item.phase, EventType: item.eventType,
			ActorType: item.actorType, Actor: item.actor, Status: item.status,
			EvidenceRef: "evidence/" + item.eventType + ".json", TraceID: "trace-archive",
		}
		if item.eventType == incidentcontrol.EventRecovery {
			event.RecoverySignal = true
		}
		events = append(events, event)
	}
	return events
}

func similarArchiveEvent(tenantID, incidentID string) incidentcontrol.Event {
	return incidentcontrol.Event{
		ID: "event-similar", TenantID: tenantID, IncidentID: incidentID,
		OccurredAt: time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC), Phase: "detection",
		EventType: incidentcontrol.EventAlertReceived, ActorType: "system", Actor: "prometheus", Status: "firing",
		EvidenceRef: "evidence/similar.json", TraceID: "trace-similar",
	}
}

func (repository *stubMetricsRepository) ListRecallLogs(_ context.Context, tenantID, incidentID string) ([]incidentcontrol.RecallLog, error) {
	return []incidentcontrol.RecallLog{{
		TenantID: tenantID, IncidentID: incidentID, CandidateRef: "runbook:INC-API-001",
		QueryText: "pool saturation", RRFScore: 0.03225806451612903, Selected: true,
	}}, nil
}
