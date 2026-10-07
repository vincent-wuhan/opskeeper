// Package loop exposes the closed-loop orchestrator HTTP API
// (zero-manual-ops-loop · Day 5 Task 5.6).
//
// Routes (mounted via Register under the auth-gated /api/v1 group):
//
//	POST /api/v1/loops/{incident_id}/trigger — start (or re-trigger)
//	  a closed-loop run for the incident. Body carries optional
//	  {from_phase?, triggered_by?, root_cause_json?} overrides; chat
//	  promote calls this with from_phase="approved".
//
//	GET  /api/v1/loops/{incident_id}/timeline — return the
//	  chronological loop_event_log list for the incident. The SPA
//	  uses this to render the closed-loop timeline (Day 8 web task).
//
//	POST /api/v1/recovery/verify — trigger a verify_recovery tool
//	  run for an incident independent of the orchestrator's
//	  recovered phase. Useful for ops-driven re-checks.
//
// All three endpoints require admin role (per platform admin policy
// for state-machine-write actions; the read endpoint is tenant-
// scoped, not admin).
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	loopbiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	loopmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/loop"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
)

// Orchestrator is the production seam this handler depends on. It
// mirrors loop.Orchestrator so production wires *loop.Orchestrator
// directly; tests inject a stub.
type Orchestrator interface {
	Run(ctx context.Context, opts loopbiz.RunOptions) (*loopbiz.RunResult, error)
	State(ctx context.Context, tenantID, incidentID string) (*loopmodel.State, error)
}

// EventRepo is the narrow seam for the timeline endpoint. Mirrors
// loop.EventRepo; production wires the *sql.DB-backed adapter.
type EventRepo interface {
	ReadEvents(ctx context.Context, tenantID, incidentID string) ([]loopmodel.Event, error)
}

// VerifyRecoveryCaller exposes the verify_recovery tool's invoke path
// for the standalone POST /api/v1/recovery/verify endpoint. The
// recovered phase's PhaseWorker uses the same seam; the HTTP
// endpoint allows ops to re-run verification outside the orchestrator
// (e.g. after a flaky sample window).
type VerifyRecoveryCaller interface {
	InvokeVerifyRecovery(ctx context.Context, argsJSON string) (string, error)
}

// Handler serves the three closed-loop endpoints.
type Handler struct {
	orchestrator Orchestrator
	eventRepo    EventRepo
	verifyCaller VerifyRecoveryCaller
}

// NewHandler constructs the handler. All three deps are required in
// production; tests can pass nil for the seam they don't exercise.
func NewHandler(o Orchestrator, e EventRepo, v VerifyRecoveryCaller) (*Handler, error) {
	if o == nil {
		return nil, errors.New("loop: handler requires Orchestrator")
	}
	if e == nil {
		return nil, errors.New("loop: handler requires EventRepo")
	}
	if v == nil {
		return nil, errors.New("loop: handler requires VerifyRecoveryCaller")
	}
	return &Handler{orchestrator: o, eventRepo: e, verifyCaller: v}, nil
}

// Register mounts the three routes on r (a chi.Router).
//
// The caller is responsible for placing r inside the auth-gated
// /api/v1 group; the handler does its own tenant / role check.
func (h *Handler) Register(r chi.Router) {
	r.Post("/v1/loops/{incident_id}/trigger", h.trigger)
	r.Get("/v1/loops/{incident_id}/timeline", h.timeline)
	r.Get("/v1/loops/{incident_id}/state", h.state)
	r.Post("/v1/recovery/verify", h.verifyRecovery)
}

// --- Request / Response shapes ------------------------------------------

// TriggerRequest is the POST /v1/loops/{incident_id}/trigger body.
type TriggerRequest struct {
	// FromPhase is the entry phase. Empty defaults to detected.
	FromPhase string `json:"from_phase,omitempty"`

	// TriggeredBy is the audit-trail annotation ("alert" /
	// "chat" / "harness" / "ops"). Defaults to "ops" when empty.
	TriggeredBy string `json:"triggered_by,omitempty"`

	// IdempotencyKey is the run-level idempotency key. When the
	// same key is seen twice within the orchestrator's cache
	// window, the prior result is returned.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// RootCauseJSON is the structured root cause for chat-promote
	// callers. The orchestrator stores it as the investigated-phase
	// contract; empty for alert entries (the alert pipeline
	// synthesises the contract separately).
	RootCauseJSON *loopbiz.RootCauseJSON `json:"root_cause_json,omitempty"`
}

// TriggerResponse is the POST /v1/loops/{incident_id}/trigger body.
type TriggerResponse struct {
	IncidentID       string            `json:"incident_id"`
	FinalPhase       string            `json:"final_phase"`
	FirstLoopEventID int64             `json:"first_loop_event_id"`
	LoopEvents       []loopmodel.Event `json:"loop_events"`
}

// TimelineResponse is the GET /v1/loops/{incident_id}/timeline body.
//
// Day 11 — the response is now backwards-compatible:
// the existing `events` field is preserved, and we add `phases`,
// `rubric`, and `chain` so the ClosedLoopTimeline SPA can render the
// closed loop without re-computing the contract/rubric client-side.
//
// `phases` is the 7-stage phase list (detected → postmortem) with
// per-phase tool calls, audit rows (dispatch/approval/execution/
// verification), skill versions, and knowledge references.
//
// `rubric` is the four-metric summary (rca_accuracy,
// time_to_remediate, approval_rate, recovery_pass_rate) plus
// presence flags for recovery_signal and incident closure.
//
// `chain` is a small footer block summarising coverage of the
// forward phases and the trace IDs that link the events together.
type TimelineResponse struct {
	IncidentID string            `json:"incident_id"`
	Events     []loopmodel.Event `json:"events"`
	Phases     []TimelinePhase   `json:"phases"`
	Rubric     *TimelineRubric   `json:"rubric,omitempty"`
	Chain      TimelineChainMeta `json:"chain"`
}

// TimelineChainMeta is a small footer block that lets the SPA show
// the chain's coverage (how many forward phases had real events)
// and the "next action" hint for the operator.
type TimelineChainMeta struct {
	PhasesObserved  int      `json:"phases_observed"`
	PhasesExpected  int      `json:"phases_expected"`
	CoveragePct     float64  `json:"coverage_pct"`
	CurrentPhase    string   `json:"current_phase"`
	FinalPhase      string   `json:"final_phase"`
	RecoverySignal  bool     `json:"recovery_signal"`
	Closed          bool     `json:"closed"`
	TraceIDs        []string `json:"trace_ids,omitempty"`
	IncidentIDAlias string   `json:"incident_id_alias,omitempty"`
}

// StateResponse is the GET /v1/loops/{incident_id}/state body.
//
// State is the O(1) "current phase" snapshot used by the AgentTeams
// state.json bridge (cmd/agentteams-state phase) and the closed-loop
// timeline view. Source of truth is loop_event_log; loop_state is a
// derived cache rebuilt on each call.
//
// StateError is non-empty when the recovered phase is ambiguous
// (events present but the state machine could not reconcile them);
// callers should re-trigger the orchestrator to recover.
type StateResponse struct {
	IncidentID    string `json:"incident_id"`
	CurrentPhase  string `json:"current_phase"`
	LastEventID   int64  `json:"last_event_id"`
	EventCount    int    `json:"event_count"`
	UpdatedAt     string `json:"updated_at"`
	RecoveredFrom string `json:"recovered_from,omitempty"`
	StateError    string `json:"state_error,omitempty"`
}

// VerifyRequest is the POST /v1/recovery/verify body.
type VerifyRequest struct {
	// IncidentID is the incident whose recovery we are verifying.
	IncidentID string `json:"incident_id"`

	// Tolerance is the pass threshold (default 0.15). Must be > 0.
	Tolerance float64 `json:"tolerance"`

	// Metrics is the optional subset of metric names to verify.
	// Empty means use the default subset for the resource type.
	Metrics []string `json:"metrics,omitempty"`

	// BaselineWindow is the human-readable window hint (e.g.
	// "1h", "24h"). Parsed by the verify_recovery tool; the
	// handler forwards it as-is.
	BaselineWindow string `json:"baseline_window,omitempty"`
}

// VerifyResponse is the POST /v1/recovery/verify body.
type VerifyResponse struct {
	IncidentID string                 `json:"incident_id"`
	Passed     bool                   `json:"passed"`
	Delta      *loopbiz.VerifiedDelta `json:"verified_delta,omitempty"`
	Raw        string                 `json:"raw,omitempty"`
}

// --- handlers ----------------------------------------------------------

// trigger godoc
// @Summary Trigger a closed-loop run
// @Description Start (or re-trigger) the seven-phase closed-loop orchestrator for an incident. The chat promote path passes from_phase=approved to skip the earlier phases per spec §"对话升级为闭环".
// @Router /v1/loops/{incident_id}/trigger [post]
// @Success 202 {object} loop.TriggerResponse
func (h *Handler) trigger(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	tenantID := tenantFromContext(w, r)
	if tenantID == "" {
		return
	}
	incidentID := chi.URLParam(r, "incident_id")
	if incidentID == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	var req TriggerRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, fmt.Errorf("%w: %s", errs.ErrInvalid, err))
			return
		}
	}
	if req.TriggeredBy == "" {
		req.TriggeredBy = "ops"
	}
	phase, err := phaseFromString(req.FromPhase)
	if err != nil {
		writeErr(w, err)
		return
	}

	res, err := h.orchestrator.Run(r.Context(), loopbiz.RunOptions{
		IncidentID:    incidentID,
		TenantID:      tenantID,
		FromPhase:     phase,
		TriggeredBy:   req.TriggeredBy,
		IdempotentKey: req.IdempotencyKey,
	})
	if err != nil {
		writeErr(w, err)
		return
	}

	firstLoopEventID := int64(0)
	if len(res.LoopEvents) > 0 {
		firstLoopEventID = res.LoopEvents[0].ID
	}
	writeJSON(w, http.StatusAccepted, TriggerResponse{
		IncidentID:       res.IncidentID,
		FinalPhase:       string(res.FinalPhase),
		FirstLoopEventID: firstLoopEventID,
		LoopEvents:       res.LoopEvents,
	})
}

// timeline godoc
// @Summary List loop timeline events
// @Description Returns the chronological loop_event_log entries for an incident, plus the aggregated per-phase trace (phases, rubric, chain metadata) used by the Day 8 ClosedLoopTimeline web view.
// @Router /v1/loops/{incident_id}/timeline [get]
// @Success 200 {object} loop.TimelineResponse
func (h *Handler) timeline(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	tenantID := tenantFromContext(w, r)
	if tenantID == "" {
		return
	}
	incidentID := chi.URLParam(r, "incident_id")
	if incidentID == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	events, err := h.eventRepo.ReadEvents(r.Context(), tenantID, incidentID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if events == nil {
		events = []loopmodel.Event{}
	}

	// Aggregate the raw event log into the 7-stage phase
	// view the SPA renders (process timeline + rubric cards + chain
	// footer). The aggregation is pure; see timeline_aggregate.go.
	phases := BuildTimelinePhases(events)
	rubric := BuildRubric(events)
	chain := BuildChainMeta(events, incidentID)
	writeJSON(w, http.StatusOK, TimelineResponse{
		IncidentID: incidentID,
		Events:     events,
		Phases:     phases,
		Rubric:     &rubric,
		Chain:      chain,
	})
}

// BuildChainMeta derives the small footer block from the raw event
// slice. It is exposed at package scope so the deployment handler
// can compose the same shape when a custom recovery action is
// referenced from the version page.
func BuildChainMeta(events []loopmodel.Event, incidentID string) TimelineChainMeta {
	chain := TimelineChainMeta{
		PhasesExpected: len(forwardPhaseOrder),
		FinalPhase:     "detected",
	}
	if len(events) == 0 {
		chain.CurrentPhase = "detected"
		return chain
	}
	// Use the same forward-phase list the phase renderer does so the
	// coverage counter matches what the user sees.
	seen := make(map[string]bool, len(forwardPhaseOrder))
	for _, ev := range events {
		for _, name := range forwardPhaseOrder {
			if ev.Phase == name {
				seen[name] = true
			}
		}
	}
	for _, ev := range events {
		if ev.TraceID != "" {
			chain.TraceIDs = append(chain.TraceIDs, ev.TraceID)
		}
	}
	chain.PhasesObserved = len(seen)
	if chain.PhasesExpected > 0 {
		chain.CoveragePct = float64(chain.PhasesObserved) / float64(chain.PhasesExpected)
	}

	// Final phase: prefer the last phase_contract_written; fall back
	// to the last event's phase. Skip correction events.
	final := ""
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.EventType == loopmodel.EventCorrection {
			continue
		}
		final = ev.Phase
		break
	}
	if final == "" {
		final = "detected"
	}
	chain.FinalPhase = final
	chain.CurrentPhase = final

	for _, ev := range events {
		if ev.Phase == string(loopbiz.PhasePostmortem) {
			chain.Closed = true
			break
		}
	}
	// Terminal states (failed / aborted) win over postmortem: a
	// loop that ended in failure must NOT be advertised as
	// `closed=true` even if it had a stale postmortem entry
	// beforehand. The Closed flag reflects a healthy terminal
	// transition through Postmortem, not just any Postmortem
	// event.
	terminal := false
	for _, ev := range events {
		if terminalPhases[ev.Phase] {
			terminal = true
			chain.FinalPhase = ev.Phase
			chain.CurrentPhase = ev.Phase
			break
		}
	}
	if terminal {
		chain.Closed = false
	}
	for _, ev := range events {
		if ev.Payload == "" {
			continue
		}
		if containsRecoverySignal(ev.Payload) {
			chain.RecoverySignal = true
			break
		}
	}
	chain.IncidentIDAlias = incidentID
	return chain
}

// containsRecoverySignal is a small string-search helper kept here
// (vs. imported from BuildRubric) so both helpers can be used
// independently by the deployment handler.
func containsRecoverySignal(payload string) bool {
	return strings.Contains(payload, `"recovery_signal":true`) ||
		strings.Contains(payload, `"recovery_signal": true`)
}

// state godoc
// @Summary Get the current closed-loop state for an incident
// @Description Returns the derived State snapshot (current_phase + last_event_id) for an incident, computed by replaying loop_event_log. Read-only endpoint, available to tenant users (not admin-only) so the AgentTeams Manager can poll state without holding admin credentials.
// @Router /v1/loops/{incident_id}/state [get]
// @Success 200 {object} loop.StateResponse
func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(w, r)
	if tenantID == "" {
		return
	}
	incidentID := chi.URLParam(r, "incident_id")
	if incidentID == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	snapshot, err := h.orchestrator.State(r.Context(), tenantID, incidentID)
	if err != nil && snapshot == nil {
		// Hard failure (DB read error, missing tenant): surface 5xx.
		writeErr(w, err)
		return
	}
	resp := StateResponse{IncidentID: incidentID}
	if snapshot != nil {
		resp.CurrentPhase = snapshot.CurrentPhase
		resp.LastEventID = snapshot.LastEventID
		if !snapshot.UpdatedAt.IsZero() {
			resp.UpdatedAt = snapshot.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
	}
	if events, err := h.eventRepo.ReadEvents(r.Context(), tenantID, incidentID); err == nil {
		resp.EventCount = len(events)
	}
	if err != nil {
		resp.StateError = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// verifyRecovery godoc
// @Summary Run verify_recovery on demand
// @Description Trigger a standalone verify_recovery tool run for an incident. Useful for ops-driven re-checks after a flaky sample window.
// @Router /v1/recovery/verify [post]
// @Success 200 {object} loop.VerifyResponse
func (h *Handler) verifyRecovery(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	tenantID := tenantFromContext(w, r)
	if tenantID == "" {
		return
	}
	var req VerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, fmt.Errorf("%w: %s", errs.ErrInvalid, err))
		return
	}
	if req.IncidentID == "" {
		writeErr(w, fmt.Errorf("%w: incident_id required", errs.ErrInvalid))
		return
	}
	if req.Tolerance <= 0 {
		req.Tolerance = 0.15
	}

	// Compose the args JSON expected by verify_recovery.
	argsMap := map[string]any{
		"incident_id":     req.IncidentID,
		"tenant_id":       tenantID,
		"tolerance":       req.Tolerance,
		"metrics":         req.Metrics,
		"baseline_window": req.BaselineWindow,
	}
	argsJSON, err := json.Marshal(argsMap)
	if err != nil {
		writeErr(w, fmt.Errorf("%w: marshal args: %s", errs.ErrInvalid, err))
		return
	}
	raw, err := h.verifyCaller.InvokeVerifyRecovery(r.Context(), string(argsJSON))
	if err != nil {
		writeErr(w, err)
		return
	}
	var delta loopbiz.VerifiedDelta
	if err := json.Unmarshal([]byte(raw), &delta); err != nil {
		// Don't fail — surface raw alongside the parse error so
		// callers can still inspect the response shape.
		writeJSON(w, http.StatusOK, VerifyResponse{
			IncidentID: req.IncidentID,
			Passed:     false,
			Raw:        raw,
		})
		return
	}
	writeJSON(w, http.StatusOK, VerifyResponse{
		IncidentID: req.IncidentID,
		Passed:     delta.Passed,
		Delta:      &delta,
	})
}

// --- helpers ------------------------------------------------------------

// phaseFromString maps a wire-level string to a loop.Phase.
// Whitelist validation keeps callers from passing typos.
func phaseFromString(s string) (loopbiz.Phase, error) {
	switch s {
	case "":
		return loopbiz.PhaseDetected, nil
	case "detected":
		return loopbiz.PhaseDetected, nil
	case "correlated":
		return loopbiz.PhaseCorrelated, nil
	case "investigated":
		return loopbiz.PhaseInvestigated, nil
	case "critiqued":
		return loopbiz.PhaseCritiqued, nil
	case "approved":
		return loopbiz.PhaseApproved, nil
	case "recovered":
		return loopbiz.PhaseRecovered, nil
	case "postmortem":
		return loopbiz.PhasePostmortem, nil
	}
	return "", fmt.Errorf("%w: unknown from_phase %q", errs.ErrInvalid, s)
}

func tenantFromContext(w http.ResponseWriter, r *http.Request) string {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return ""
	}
	// Single-tenant MVP — Tenant struct carries the user identity
	// but the loop tables are not yet tenant-partitioned. The
	// caller still passes a stable string so cross-event
	// correlation works. We use UserID as the tenant_id key —
	// Day 6+ will replace with the real multi-tenant resolver.
	if t.UserID != 0 {
		return fmt.Sprintf("user-%d", t.UserID)
	}
	return "default"
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return false
	}
	if t.Role != "admin" {
		writeErr(w, errs.ErrForbidden)
		return false
	}
	return true
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	slug := "internal"
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		status, slug = http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, errs.ErrForbidden):
		status, slug = http.StatusForbidden, "forbidden"
	case errors.Is(err, errs.ErrInvalid):
		status, slug = http.StatusBadRequest, "invalid"
	case errors.Is(err, errs.ErrNotFound):
		status, slug = http.StatusNotFound, "not-found"
	default:
		status, slug = http.StatusBadGateway, "upstream"
	}
	writeJSON(w, status, errorBody{Error: err.Error(), Code: slug})
}
