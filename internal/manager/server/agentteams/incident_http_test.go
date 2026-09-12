package agentteams

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	alertbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/alert"
	alertmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
	mcpauth "github.com/vincent-wuhan/opskeeper/internal/manager/server/mcp/middleware"
)

type linkedAlertResolver struct {
	incidents []*alertmodel.Incident
	resolved  []string
	seenLimit int
}

const testTenantID = "open-source-test"

func (resolver *linkedAlertResolver) ListIncidents(
	_ context.Context,
	filter alertbiz.IncidentFilter,
) ([]*alertmodel.Incident, error) {
	resolver.seenLimit = filter.Limit
	return resolver.incidents, nil
}

func (resolver *linkedAlertResolver) SystemResolveIncident(
	_ context.Context,
	dedupeKey,
	_ string,
	_ time.Time,
) (bool, error) {
	resolver.resolved = append(resolver.resolved, dedupeKey)
	return true, nil
}

func TestRecordIncidentEventClosesLinkedAlert(t *testing.T) {
	t.Setenv("OPSKEEPER_DEFAULT_INCIDENT_TENANT_ID", testTenantID)
	recorder := &memIncidentRecorder{}
	resolver := &linkedAlertResolver{
		incidents: []*alertmodel.Incident{{
			DedupeKey:  "pg-pool-exhaustion",
			Status:     alertmodel.IncidentStatusOpen,
			LabelsJSON: `{"incident_id":"incident-live-pool-smoke","target":"pg:pool-fixture"}`,
		}},
	}
	handler := NewHandler(newMemBackend(), nil, "")
	handler.SetIncidentRecorder(recorder)
	handler.SetAlertIncidentResolver(resolver)
	router := newRouter(handler)

	stages := []struct {
		role string
		body string
		at   time.Time
	}{
		{role: "alerter", body: `{"evidence_ref":"evidence/alert.json"}`},
		{role: "investigator", body: `{"evidence_ref":"evidence/root-cause.json"}`},
		{role: "reviewer", body: `{"evidence_ref":"evidence/proposal.json"}`},
		{role: "repairer", body: `{"evidence_ref":"evidence/recovery.json","action_fingerprint":"proposal:resize_pool:pg:pool-fixture:manifest"}`},
		{role: "verifier", body: `{"evidence_ref":"evidence/verified-delta.json","recovery_signal":true}`},
		{role: "reporter", body: `{"evidence_ref":"evidence/postmortem.json"}`},
	}
	base := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	for index, stage := range stages {
		occurredAt := base.Add(time.Duration(index) * time.Minute)
		requestBody := fmt.Sprintf(
			`{"incident_id":"incident-live-pool-smoke","occurred_at":%q,%s}`,
			occurredAt.UTC().Format(time.RFC3339Nano),
			stage.body[1:len(stage.body)-1],
		)
		request := httptest.NewRequest(http.MethodPost, "/v1/incidents/events", bytes.NewReader([]byte(requestBody)))
		identity := mcpauth.ResolvedIdentity{
			ConsumerName: "worker-" + stage.role,
			Role:         stage.role,
			TenantID:     "default",
		}
		context := mcpauth.WithTraceContext(
			mcpauth.WithIdentity(request.Context(), identity),
			mcpauth.TraceContext{TraceID: "0123456789abcdef0123456789abcdef"},
		)
		*request = *request.WithContext(context)
		recorderResponse := httptest.NewRecorder()
		router.ServeHTTP(recorderResponse, request)
		if recorderResponse.Code != http.StatusCreated {
			t.Fatalf("stage %s: status = %d body=%s", stage.role, recorderResponse.Code, recorderResponse.Body.String())
		}
	}

	events := recorder.events["incident-live-pool-smoke"]
	if len(events) != len(stages) {
		t.Fatalf("event count = %d, want %d", len(events), len(stages))
	}
	for _, event := range events {
		if event.TenantID != testTenantID {
			t.Fatalf("tenant = %q, want %s", event.TenantID, testTenantID)
		}
	}
	if len(resolver.resolved) != 1 || resolver.resolved[0] != "pg-pool-exhaustion" {
		t.Fatalf("resolved alerts = %v", resolver.resolved)
	}
	if resolver.seenLimit < 1 {
		t.Fatalf("resolver limit = %d, want a bounded open-alert query", resolver.seenLimit)
	}
}

func TestRecordIncidentEventRejectsFutureOccurredAt(t *testing.T) {
	handler := NewHandler(newMemBackend(), nil, "")
	handler.SetIncidentRecorder(&memIncidentRecorder{})
	router := newRouter(handler)
	body := fmt.Sprintf(
		`{"incident_id":"incident-future","occurred_at":%q,"evidence_ref":"evidence/alert.json"}`,
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	)
	request := httptest.NewRequest(http.MethodPost, "/v1/incidents/events", bytes.NewReader([]byte(body)))
	context := mcpauth.WithTraceContext(
		mcpauth.WithIdentity(request.Context(), mcpauth.ResolvedIdentity{Role: "alerter", TenantID: testTenantID}),
		mcpauth.TraceContext{TraceID: "0123456789abcdef0123456789abcdef"},
	)
	*request = *request.WithContext(context)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestRecordIncidentEventRejectsInvalidRecoverySignal(t *testing.T) {
	handler := NewHandler(newMemBackend(), nil, "")
	handler.SetIncidentRecorder(&memIncidentRecorder{})
	router := newRouter(handler)

	tests := []struct {
		role string
		body string
	}{
		{role: "investigator", body: `{"incident_id":"inc-1","evidence_ref":"evidence/root-cause.json","recovery_signal":true}`},
		{role: "verifier", body: `{"incident_id":"inc-1","evidence_ref":"evidence/verified-delta.json"}`},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodPost, "/v1/incidents/events", bytes.NewReader([]byte(test.body)))
		identity := mcpauth.ResolvedIdentity{Role: test.role, TenantID: testTenantID}
		context := mcpauth.WithTraceContext(
			mcpauth.WithIdentity(request.Context(), identity),
			mcpauth.TraceContext{TraceID: "0123456789abcdef0123456789abcdef"},
		)
		*request = *request.WithContext(context)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body=%s", test.role, response.Code, response.Body.String())
		}
	}
}

func TestIncidentLabelsContain(t *testing.T) {
	if !incidentLabelsContain(`{"incident_id":"inc-1"}`, "incident_id", "inc-1") {
		t.Fatal("expected label match")
	}
	if incidentLabelsContain(`{"incident_id":"inc-1"}`, "incident_id", "inc-2") {
		t.Fatal("expected label mismatch")
	}
	if incidentLabelsContain(`invalid`, "incident_id", "inc-1") {
		t.Fatal("expected invalid JSON to fail closed")
	}
}
