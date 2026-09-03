// loop/http_test.go — unit tests for the closed-loop HTTP handler.
//
// Test matrix (mirrors the spec scenarios at
// openspec/changes/zero-manual-ops-loop/specs/closed-loop-orchestrator/spec.md):
//
//  1. Trigger happy path: 202 Accepted with FinalPhase.
//  2. Trigger with chat-promote FromPhase=approved.
//  3. Trigger requires admin (non-admin → 403).
//  4. Timeline happy path: 200 with events slice.
//  5. Timeline requires admin.
//  6. Recovery verify happy path: 200 with VerifiedDelta.
//  7. Recovery verify requires incident_id.
//  8. Phase whitelist rejects unknown phase strings.
//  9. NewHandler rejects nil deps.
package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	loopbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// ---------- mocks ----------

type stubOrchestrator struct {
	mu             sync.Mutex
	lastOpts       loopbiz.RunOptions
	result         *loopbiz.RunResult
	err            error
	runCalls       int
	state          *loopmodel.State
	stateErr       error
	stateCalls     int
	lastIncidentID string
}

func (s *stubOrchestrator) Run(_ context.Context, opts loopbiz.RunOptions) (*loopbiz.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runCalls++
	s.lastOpts = opts
	if s.err != nil {
		return nil, s.err
	}
	return &loopbiz.RunResult{
		IncidentID: opts.IncidentID,
		FinalPhase: opts.FromPhase,
		LoopEvents: []loopmodel.Event{{
			ID:        9999,
			CreatedAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
		}},
	}, nil
}

func (s *stubOrchestrator) State(_ context.Context, _, incidentID string) (*loopmodel.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateCalls++
	s.lastIncidentID = incidentID
	if s.stateErr != nil {
		return nil, s.stateErr
	}
	if s.state != nil {
		return s.state, nil
	}
	return nil, nil
}

type stubEventRepo struct {
	mu        sync.Mutex
	events    []loopmodel.Event
	err       error
	readCalls int
}

func (s *stubEventRepo) ReadEvents(_ context.Context, _, incidentID string) ([]loopmodel.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readCalls++
	if s.err != nil {
		return nil, s.err
	}
	out := make([]loopmodel.Event, len(s.events))
	copy(out, s.events)
	return out, nil
}

type stubVerifyCaller struct {
	mu       sync.Mutex
	lastArgs string
	raw      string
	err      error
	calls    int
}

func (s *stubVerifyCaller) InvokeVerifyRecovery(_ context.Context, argsJSON string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastArgs = argsJSON
	if s.err != nil {
		return "", s.err
	}
	return s.raw, nil
}

// ---------- helpers ----------

func newTestHandler(t *testing.T) (*Handler, *stubOrchestrator, *stubEventRepo, *stubVerifyCaller) {
	t.Helper()
	orch := &stubOrchestrator{}
	events := &stubEventRepo{}
	verify := &stubVerifyCaller{
		raw: `{"schema_version":"v1","passed":true,"deltas":{"cpu_usage":0.03},"sample_size":10,"tolerance":0.15,"retry_count":0,"warning_level":"pass"}`,
	}
	h, err := NewHandler(orch, events, verify)
	if err != nil {
		t.Fatalf("NewHandler err: %v", err)
	}
	return h, orch, events, verify
}

func adminCtx(r *http.Request) context.Context {
	return tenantctx.With(r.Context(), tenantctx.Tenant{
		UserID: 42, Email: "admin@example.com", Role: "admin", IsSuperuser: true,
	})
}

func userCtx(r *http.Request) context.Context {
	return tenantctx.With(r.Context(), tenantctx.Tenant{
		UserID: 7, Email: "user@example.com", Role: "user",
	})
}

func newRequest(method, target string, body []byte) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		buf.Write(body)
	}
	r := httptest.NewRequest(method, target, &buf)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

// ---------- tests ----------

func TestNewHandler_NilDepsRejected(t *testing.T) {
	t.Parallel()
	if _, err := NewHandler(nil, &stubEventRepo{}, &stubVerifyCaller{}); err == nil {
		t.Errorf("NewHandler(nil orchestrator) should error")
	}
	if _, err := NewHandler(&stubOrchestrator{}, nil, &stubVerifyCaller{}); err == nil {
		t.Errorf("NewHandler(nil eventRepo) should error")
	}
	if _, err := NewHandler(&stubOrchestrator{}, &stubEventRepo{}, nil); err == nil {
		t.Errorf("NewHandler(nil verifyCaller) should error")
	}
}

func TestTrigger_HappyPath(t *testing.T) {
	t.Parallel()
	h, orch, _, _ := newTestHandler(t)
	r := newRequest(http.MethodPost, "/v1/loops/inc-1/trigger", nil)
	r = r.WithContext(adminCtx(r))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("incident_id", "inc-1")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	h.trigger(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", w.Code, w.Body.String())
	}
	var resp TriggerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body err: %v", err)
	}
	if resp.IncidentID != "inc-1" {
		t.Errorf("resp.IncidentID = %q, want inc-1", resp.IncidentID)
	}
	if resp.FirstLoopEventID != 9999 {
		t.Errorf("resp.FirstLoopEventID = %d, want 9999", resp.FirstLoopEventID)
	}
	if orch.lastOpts.FromPhase != loopbiz.PhaseDetected {
		t.Errorf("orchestrator.FromPhase = %s, want detected", orch.lastOpts.FromPhase)
	}
	if orch.lastOpts.TriggeredBy != "ops" {
		t.Errorf("orchestrator.TriggeredBy = %q, want ops", orch.lastOpts.TriggeredBy)
	}
}

func TestTrigger_ChatPromoteFromApproved(t *testing.T) {
	t.Parallel()
	h, orch, _, _ := newTestHandler(t)
	body := []byte(`{"from_phase":"approved","triggered_by":"chat"}`)
	r := newRequest(http.MethodPost, "/v1/loops/inc-2/trigger", body)
	r = r.WithContext(adminCtx(r))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("incident_id", "inc-2")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	h.trigger(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if orch.lastOpts.FromPhase != loopbiz.PhaseApproved {
		t.Errorf("orchestrator.FromPhase = %s, want approved", orch.lastOpts.FromPhase)
	}
	if orch.lastOpts.TriggeredBy != "chat" {
		t.Errorf("orchestrator.TriggeredBy = %q, want chat", orch.lastOpts.TriggeredBy)
	}
}

func TestTrigger_RejectsNonAdmin(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newTestHandler(t)
	r := newRequest(http.MethodPost, "/v1/loops/inc-1/trigger", nil)
	r = r.WithContext(userCtx(r))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("incident_id", "inc-1")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	h.trigger(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestTrigger_RejectsUnknownPhase(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newTestHandler(t)
	body := []byte(`{"from_phase":"not_a_phase"}`)
	r := newRequest(http.MethodPost, "/v1/loops/inc-1/trigger", body)
	r = r.WithContext(adminCtx(r))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("incident_id", "inc-1")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	h.trigger(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

func TestTimeline_HappyPath(t *testing.T) {
	t.Parallel()
	h, _, events, _ := newTestHandler(t)
	events.events = []loopmodel.Event{
		{ID: 1, IncidentID: "inc-1", EventType: loopmodel.EventTypePhaseEntered, Phase: "detected"},
		{ID: 2, IncidentID: "inc-1", EventType: loopmodel.EventPhaseContractWritten, Phase: "detected"},
	}
	r := newRequest(http.MethodGet, "/v1/loops/inc-1/timeline", nil)
	r = r.WithContext(adminCtx(r))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("incident_id", "inc-1")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	h.timeline(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp TimelineResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body err: %v", err)
	}
	if len(resp.Events) != 2 {
		t.Errorf("resp.Events has %d entries, want 2", len(resp.Events))
	}
}

func TestTimeline_RejectsNonAdmin(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newTestHandler(t)
	r := newRequest(http.MethodGet, "/v1/loops/inc-1/timeline", nil)
	r = r.WithContext(userCtx(r))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("incident_id", "inc-1")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	h.timeline(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestVerifyRecovery_HappyPath(t *testing.T) {
	t.Parallel()
	h, _, _, verify := newTestHandler(t)
	body := []byte(`{"incident_id":"inc-3","tolerance":0.15,"metrics":["cpu_usage"]}`)
	r := newRequest(http.MethodPost, "/v1/recovery/verify", body)
	r = r.WithContext(adminCtx(r))

	w := httptest.NewRecorder()
	h.verifyRecovery(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp VerifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body err: %v", err)
	}
	if !resp.Passed {
		t.Errorf("resp.Passed = false, want true")
	}
	if resp.Delta == nil {
		t.Errorf("resp.Delta = nil, want populated")
	}
	if verify.calls != 1 {
		t.Errorf("verify.calls = %d, want 1", verify.calls)
	}
}

func TestVerifyRecovery_RequiresIncidentID(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newTestHandler(t)
	body := []byte(`{"tolerance":0.15}`)
	r := newRequest(http.MethodPost, "/v1/recovery/verify", body)
	r = r.WithContext(adminCtx(r))

	w := httptest.NewRecorder()
	h.verifyRecovery(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

func TestVerifyRecovery_ToolError(t *testing.T) {
	t.Parallel()
	h, _, _, verify := newTestHandler(t)
	verify.err = errors.New("tool down")
	body := []byte(`{"incident_id":"inc-3","tolerance":0.15}`)
	r := newRequest(http.MethodPost, "/v1/recovery/verify", body)
	r = r.WithContext(adminCtx(r))

	w := httptest.NewRecorder()
	h.verifyRecovery(w, r)
	if w.Code == http.StatusOK {
		t.Errorf("status = %d, want non-200; body = %s", w.Code, w.Body.String())
	}
	// The handler should map the error to the default 502 upstream.
	_ = errs.ErrConflict
}

// TestRegister_RoutesConfirmPaths covers the route registration
// shape (paths + methods) by issuing requests against the chi
// router. We don't exercise the full handler logic here — that's
// covered by the per-handler tests above — but we confirm the route
// table matches the spec.
func TestRegister_RoutesConfirmPaths(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newTestHandler(t)
	r := chi.NewRouter()
	h.Register(r)

	// Just check that routes are mounted (405 Method Not Allowed
	// for wrong-method proves the path exists).
	w := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "/v1/loops/inc-1/timeline", nil)
	req = req.WithContext(adminCtx(req))
	r.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Errorf("GET /v1/loops/inc-1/timeline returned 404, route not mounted")
	}
}

func TestHandler_StateReturnsDerivedSnapshot(t *testing.T) {
	t.Parallel()
	h, orch, events, _ := newTestHandler(t)
	now := time.Date(2026, 8, 22, 13, 0, 0, 0, time.UTC)
	orch.state = &loopmodel.State{
		IncidentID:   "inc-1",
		CurrentPhase: "recovered",
		LastEventID:  42,
		UpdatedAt:    now,
	}
	events.events = []loopmodel.Event{{ID: 1}, {ID: 42}}

	r := chi.NewRouter()
	h.Register(r)

	req := newRequest(http.MethodGet, "/v1/loops/inc-1/state", nil)
	req = req.WithContext(userCtx(req))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got StateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.IncidentID != "inc-1" {
		t.Errorf("IncidentID = %q, want inc-1", got.IncidentID)
	}
	if got.CurrentPhase != "recovered" {
		t.Errorf("CurrentPhase = %q, want recovered", got.CurrentPhase)
	}
	if got.LastEventID != 42 {
		t.Errorf("LastEventID = %d, want 42", got.LastEventID)
	}
	if got.EventCount != 2 {
		t.Errorf("EventCount = %d, want 2", got.EventCount)
	}
	if got.UpdatedAt == "" {
		t.Errorf("UpdatedAt empty; want RFC3339Nano")
	}
	if orch.lastIncidentID != "inc-1" {
		t.Errorf("State() called with %q, want inc-1", orch.lastIncidentID)
	}
}

func TestHandler_StateEmptyPhase(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newTestHandler(t)
	r := chi.NewRouter()
	h.Register(r)
	req := newRequest(http.MethodGet, "/v1/loops/inc-empty/state", nil)
	req = req.WithContext(userCtx(req))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no events yet)", w.Code)
	}
	var got StateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.CurrentPhase != "" {
		t.Errorf("CurrentPhase = %q, want empty (fresh start)", got.CurrentPhase)
	}
	if got.EventCount != 0 {
		t.Errorf("EventCount = %d, want 0", got.EventCount)
	}
}

func TestHandler_StateRequiresTenant(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newTestHandler(t)
	r := chi.NewRouter()
	h.Register(r)
	req := newRequest(http.MethodGet, "/v1/loops/inc-1/state", nil)
	// no tenant context
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Errorf("status = 200 without tenant; want 4xx")
	}
}

func TestRegister_StateRouteMounted(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newTestHandler(t)
	r := chi.NewRouter()
	h.Register(r)
	w := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "/v1/loops/inc-1/state", nil)
	req = req.WithContext(userCtx(req))
	r.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Errorf("GET /v1/loops/inc-1/state returned 404, route not mounted")
	}
}
