package agentteams

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/vincent-wuhan/opskeeper/internal/agentteams"
	incidentcontrol "github.com/vincent-wuhan/opskeeper/internal/control/incident"
	knowledgebiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/knowledge"
	knowledgemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/knowledge"
	mcpauth "github.com/vincent-wuhan/opskeeper/internal/manager/server/mcp/middleware"
)

type memBackend struct {
	data map[string][]byte
}

type memKnowledgeWriter struct {
	input knowledgebiz.CreateManualDocInput
}

type memIncidentRecorder struct {
	events map[string][]incidentcontrol.Event
}

func (m *memIncidentRecorder) Append(_ context.Context, event incidentcontrol.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if m.events == nil {
		m.events = map[string][]incidentcontrol.Event{}
	}
	for _, existing := range m.events[event.IncidentID] {
		if existing.ID == event.ID {
			return incidentcontrol.ErrDuplicateEvent
		}
	}
	m.events[event.IncidentID] = append(m.events[event.IncidentID], event)
	return nil
}

func (m *memIncidentRecorder) ListIncident(_ context.Context, _, incidentID string) ([]incidentcontrol.Event, error) {
	return m.events[incidentID], nil
}

func (m *memKnowledgeWriter) CreateManualDoc(_ context.Context, in knowledgebiz.CreateManualDocInput) (*knowledgemodel.Doc, error) {
	m.input = in
	return &knowledgemodel.Doc{ID: 1, Title: in.Title, Content: in.Content, Tags: in.Tags}, nil
}

func newMemBackend() *memBackend { return &memBackend{data: map[string][]byte{}} }

func (m *memBackend) Get(_ context.Context, taskID string) ([]byte, error) {
	d, ok := m.data[taskID]
	if !ok {
		return nil, agentteams.ErrStateNotFound
	}
	return d, nil
}

func (m *memBackend) Put(_ context.Context, taskID string, body []byte) error {
	m.data[taskID] = body
	return nil
}

func newRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	h.Register(r)
	return r
}

func TestPutGetState(t *testing.T) {
	backend := newMemBackend()
	h := NewHandler(backend, nil, "")
	r := newRouter(h)

	// put
	body := []byte(`{"phase":"rca","status":"in_progress","version":1}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/state/incident-123", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	// get
	req = httptest.NewRequest(http.MethodGet, "/v1/state/incident-123", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["task_id"] != "incident-123" {
		t.Fatalf("expected task_id overwritten, got %v", out["task_id"])
	}
	if out["phase"] != "rca" {
		t.Fatalf("phase lost: %v", out["phase"])
	}
}

func TestHitlDecideRequiresIdentity(t *testing.T) {
	backend := newMemBackend()
	backend.data["incident-123"] = []byte(`{"phase":"repair","version":1}`)
	h := NewHandler(backend, nil, "")
	r := newRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/v1/hitl/decide",
		bytes.NewReader([]byte(`{"task_id":"incident-123","decision":"approve","signers":["admin1","admin2"]}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without identity, got %d", w.Code)
	}
}

func TestGetSkillReadsFromDisk(t *testing.T) {
	tmp := t.TempDir()
	// agent/opskeeper-alerter/SKILL.md
	skillDir := filepath.Join(tmp, "skills")
	if err := os.MkdirAll(filepath.Join(skillDir, "agent", "opskeeper-alerter"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "agent", "opskeeper-alerter", "SKILL.md"),
		[]byte("---\nname: opskeeper-alerter\n---\n# body"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(nil, nil, skillDir)
	r := newRouter(h)
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/opskeeper-alerter", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("opskeeper-alerter")) {
		t.Fatalf("expected skill name in body, got: %s", w.Body.String())
	}

	// path traversal block
	req = httptest.NewRequest(http.MethodGet, "/v1/skills/..%2F..%2Fetc%2Fpasswd", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("path traversal should be blocked, got 200")
	}
}

func TestCreateKnowledgeDocRequiresAllowedRole(t *testing.T) {
	h := NewHandler(nil, nil, "")
	h.SetKnowledgeWriter(&memKnowledgeWriter{})
	r := newRouter(h)
	req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/docs",
		bytes.NewReader([]byte(`{"title":"incident","content":"root cause"}`)))
	req = req.WithContext(mcpauth.WithIdentity(req.Context(), mcpauth.ResolvedIdentity{
		TenantID: "1", ConsumerName: "opskeeper-investigator", Role: "investigator",
	}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for investigator, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestCreateKnowledgeDocForReporter(t *testing.T) {
	writer := &memKnowledgeWriter{}
	h := NewHandler(nil, nil, "")
	h.SetKnowledgeWriter(writer)
	r := newRouter(h)
	body := `{"title":"PG pool saturation","content":"restart pgbouncer","tags":["postmortem"],"source":"postmortem-worker:inc-1","fingerprint":"pg-pool-2026"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/docs", bytes.NewReader([]byte(body)))
	req = req.WithContext(mcpauth.WithIdentity(req.Context(), mcpauth.ResolvedIdentity{
		TenantID: "1", ConsumerName: "opskeeper-reporter", Role: "reporter",
	}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", w.Code, w.Body.String())
	}
	if writer.input.TenantID != "1" || writer.input.Title != "PG pool saturation" {
		t.Fatalf("unexpected input: %+v", writer.input)
	}
	wantTags := []string{"postmortem", "source:postmortem-worker:inc-1", "fingerprint:pg-pool-2026"}
	if fmt.Sprint(writer.input.Tags) != fmt.Sprint(wantTags) {
		t.Fatalf("tags mismatch: got %v want %v", writer.input.Tags, wantTags)
	}
}

func TestRecordIncidentEventRecordsSixRoleBoundStages(t *testing.T) {
	recorder := &memIncidentRecorder{}
	handler := NewHandler(nil, nil, "")
	handler.SetIncidentRecorder(recorder)
	router := newRouter(handler)
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	stages := []struct {
		role    string
		event   string
		body    string
		seconds int
	}{
		{role: "alerter", event: incidentcontrol.EventAlertReceived, seconds: 0, body: `{"evidence_ref":"alert:1"}`},
		{role: "investigator", event: incidentcontrol.EventRootCause, seconds: 30, body: `{"evidence_ref":"diagnosis:1"}`},
		{role: "reviewer", event: incidentcontrol.EventApproved, seconds: 60, body: `{"evidence_ref":"hitl:1"}`},
		{role: "repairer", event: incidentcontrol.EventAction, seconds: 90, body: `{"evidence_ref":"recovery:1","action_fingerprint":"kill:fixture"}`},
		{role: "verifier", event: incidentcontrol.EventRecovery, seconds: 120, body: `{"evidence_ref":"observe:1","recovery_signal":true}`},
		{role: "reporter", event: incidentcontrol.EventClosed, seconds: 150, body: `{"evidence_ref":"postmortem:1"}`},
	}

	for _, stage := range stages {
		requestBody := `{"incident_id":"OPSKEEPER-113-CONTINUOUS","occurred_at":"` + base.Add(time.Duration(stage.seconds)*time.Second).Format(time.RFC3339Nano) + `","trace_id":"must-be-ignored",` + stage.body[1:]
		request := httptest.NewRequest(http.MethodPost, "/v1/incidents/events", bytes.NewReader([]byte(requestBody)))
		identity := mcpauth.ResolvedIdentity{
			TenantID: "tenant-a", ConsumerName: "opskeeper-" + stage.role, Role: stage.role,
		}
		context := mcpauth.WithTraceContext(
			mcpauth.WithIdentity(request.Context(), identity),
			mcpauth.TraceContext{TraceID: "11111111111111111111111111111111"},
		)
		request = request.WithContext(context)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("stage %s: expected 201, got %d body=%s", stage.role, recorder.Code, recorder.Body.String())
		}
		var event incidentcontrol.Event
		if err := json.Unmarshal(recorder.Body.Bytes(), &event); err != nil {
			t.Fatalf("stage %s: unmarshal response: %v", stage.role, err)
		}
		if event.EventType != stage.event || event.TenantID != "tenant-a" || event.Actor != "opskeeper-"+stage.role || event.TraceID != "11111111111111111111111111111111" {
			t.Fatalf("stage %s: server-derived event mismatch: %+v", stage.role, event)
		}
	}
	if len(recorder.events["OPSKEEPER-113-CONTINUOUS"]) != len(stages) {
		t.Fatalf("stored %d events, want %d", len(recorder.events["OPSKEEPER-113-CONTINUOUS"]), len(stages))
	}
}

func TestRecordIncidentEventRejectsMissingTraceAndOutOfOrderStage(t *testing.T) {
	handler := NewHandler(nil, nil, "")
	handler.SetIncidentRecorder(&memIncidentRecorder{})
	router := newRouter(handler)

	request := httptest.NewRequest(http.MethodPost, "/v1/incidents/events", bytes.NewReader([]byte(`{"incident_id":"missing-trace","evidence_ref":"alert:1"}`)))
	request = request.WithContext(mcpauth.WithIdentity(request.Context(), mcpauth.ResolvedIdentity{
		TenantID: "tenant-a", ConsumerName: "opskeeper-alerter", Role: "alerter",
	}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without trace, got %d body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/incidents/events", bytes.NewReader([]byte(`{"incident_id":"skip-alert","evidence_ref":"diagnosis:1"}`)))
	context := mcpauth.WithTraceContext(
		mcpauth.WithIdentity(request.Context(), mcpauth.ResolvedIdentity{
			TenantID: "tenant-a", ConsumerName: "opskeeper-investigator", Role: "investigator",
		}),
		mcpauth.TraceContext{TraceID: "22222222222222222222222222222222"},
	)
	request = request.WithContext(context)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409 for out-of-order stage, got %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRecordIncidentEventEnforcesRecoverySignalBoundary(t *testing.T) {
	recorder := &memIncidentRecorder{}
	handler := NewHandler(nil, nil, "")
	handler.SetIncidentRecorder(recorder)
	router := newRouter(handler)
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	post := func(role, body string, seconds int) *httptest.ResponseRecorder {
		requestBody := `{"incident_id":"OPSKEEPER-113-RECOVERY-BOUNDARY","occurred_at":"` + base.Add(time.Duration(seconds)*time.Second).Format(time.RFC3339Nano) + `","trace_id":"must-be-ignored",` + body[1:]
		request := httptest.NewRequest(http.MethodPost, "/v1/incidents/events", bytes.NewReader([]byte(requestBody)))
		context := mcpauth.WithTraceContext(
			mcpauth.WithIdentity(request.Context(), mcpauth.ResolvedIdentity{
				TenantID: "tenant-a", ConsumerName: "opskeeper-" + role, Role: role,
			}),
			mcpauth.TraceContext{TraceID: "33333333333333333333333333333333"},
		)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request.WithContext(context))
		return response
	}

	prerequisites := []struct {
		role, body string
	}{
		{role: "alerter", body: `{"evidence_ref":"alert:1"}`},
		{role: "investigator", body: `{"evidence_ref":"diagnosis:1"}`},
		{role: "reviewer", body: `{"evidence_ref":"hitl:1"}`},
		{role: "repairer", body: `{"evidence_ref":"recovery:1","action_fingerprint":"kill:fixture"}`},
	}
	for index, stage := range prerequisites {
		if response := post(stage.role, stage.body, index); response.Code != http.StatusCreated {
			t.Fatalf("stage %s: expected 201, got %d body=%s", stage.role, response.Code, response.Body.String())
		}
	}

	invalid := post("verifier", `{"evidence_ref":"observe:false","recovery_signal":false}`, len(prerequisites))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("expected verifier false to return 400, got %d body=%s", invalid.Code, invalid.Body.String())
	}
	invalid = post("alerter", `{"evidence_ref":"alert:true","recovery_signal":true}`, len(prerequisites)+1)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("expected alerter true to return 400, got %d body=%s", invalid.Code, invalid.Body.String())
	}
	require.Len(t, recorder.events["OPSKEEPER-113-RECOVERY-BOUNDARY"], len(prerequisites))

	valid := post("verifier", `{"evidence_ref":"observe:true","recovery_signal":true}`, len(prerequisites)+2)
	if valid.Code != http.StatusCreated {
		t.Fatalf("expected verifier true to return 201, got %d body=%s", valid.Code, valid.Body.String())
	}
	require.Len(t, recorder.events["OPSKEEPER-113-RECOVERY-BOUNDARY"], len(prerequisites)+1)
	require.True(t, recorder.events["OPSKEEPER-113-RECOVERY-BOUNDARY"][len(prerequisites)].RecoverySignal)
}

// 强制 import 使用（避免 unused 编译错误）
var _ = mcpauth.FromContext
var _ = io.Discard
