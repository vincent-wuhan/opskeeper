// Package incident exposes judge-facing incident metrics.
package incident

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"os"

	"github.com/go-chi/chi/v5"

	incidentcontrol "github.com/vincent-wuhan/opskeeper/core/domains/control/incident"
	repairpreview "github.com/vincent-wuhan/opskeeper/core/domains/control/repairpreview"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
)

type Repository interface {
	ListIncident(ctx context.Context, tenantID, incidentID string) ([]incidentcontrol.Event, error)
	ListTenant(ctx context.Context, tenantID string) ([]incidentcontrol.Event, error)
	ListRunbooks(ctx context.Context, tenantID, databaseType, faultFingerprint string) ([]incidentcontrol.Postmortem, error)
	ListIncidentRunbooks(ctx context.Context, tenantID, incidentID string) ([]incidentcontrol.Postmortem, error)
	ListRecallLogs(ctx context.Context, tenantID, incidentID string) ([]incidentcontrol.RecallLog, error)
}

type Handler struct {
	repository Repository
	previews   PreviewReadRepository
}

type PreviewReadRepository interface {
	ListByIncident(ctx context.Context, tenantID, incidentID string, limit int) ([]repairpreview.Run, error)
}

func NewHandler(repository Repository, previews ...PreviewReadRepository) *Handler {
	handler := &Handler{repository: repository}
	if len(previews) > 0 {
		handler.previews = previews[0]
	}
	return handler
}

func (h *Handler) Register(router chi.Router) {
	router.Get("/v1/incidents/metrics", h.metrics)
	router.Get("/v1/incidents/archive-index", h.archiveIndex)
	router.Get("/v1/incidents/runbooks", h.runbooks)
	router.Get("/v1/incidents/{incident_id}/archive", h.archive)
	router.Get("/v1/incidents/{incident_id}/repair-preview-summary", h.repairPreviewSummary)
	router.Get("/v1/incidents/{incident_id}/recall-logs", h.recallLogs)
}

// @Summary List judge-facing incident metrics.
// @Description Replays the append-only incident timeline and returns five operational evaluation metrics.
// @Tags incidents
// @Produce json
// @Param tenant_id query string false "Admin-only tenant override"
// @Success 200 {object} metricsResponse
// @Failure 400 {object} errorResponse
// @Failure 401 {object} errorResponse
// @Failure 403 {object} errorResponse
// @Router /v1/incidents/metrics [get]
func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	if tenantID == "" {
		writeError(w, http.StatusForbidden, "forbidden", "tenant could not be derived")
		return
	}
	if h.repository == nil {
		writeError(w, http.StatusServiceUnavailable, "not_wired", "incident repository is not wired")
		return
	}

	events, err := h.repository.ListTenant(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "repository_error", err.Error())
		return
	}
	report, err := incidentcontrol.ComputeReport(events)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_timeline", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, metricsResponse{Code: 0, Message: "ok", Data: report})
}

// @Summary List confirmed incident runbooks.
// @Tags incidents
// @Produce json
// @Param tenant_id query string false "Admin-only tenant override"
// @Param database_type query string true "Database type"
// @Param fault_fingerprint query string false "Fault fingerprint"
// @Success 200 {object} runbookResponse
// @Failure 400 {object} errorResponse
// @Failure 401 {object} errorResponse
// @Router /v1/incidents/runbooks [get]
func (h *Handler) runbooks(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	databaseType := r.URL.Query().Get("database_type")
	if databaseType == "" {
		writeError(w, http.StatusBadRequest, "invalid", "database_type is required")
		return
	}
	items, err := h.repository.ListRunbooks(r.Context(), tenantID, databaseType, r.URL.Query().Get("fault_fingerprint"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "repository_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runbookResponse{Code: 0, Message: "ok", Items: items, Total: len(items)})
}

// @Summary List RRF recall decisions for an incident.
// @Tags incidents
// @Produce json
// @Param tenant_id query string false "Admin-only tenant override"
// @Param incident_id path string true "Incident ID"
// @Success 200 {object} recallLogResponse
// @Failure 400 {object} errorResponse
// @Failure 401 {object} errorResponse
// @Router /v1/incidents/{incident_id}/recall-logs [get]
func (h *Handler) recallLogs(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	incidentID := chi.URLParam(r, "incident_id")
	if incidentID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "incident_id is required")
		return
	}
	items, err := h.repository.ListRecallLogs(r.Context(), tenantID, incidentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "repository_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recallLogResponse{Code: 0, Message: "ok", Items: items, Total: len(items)})
}

// @Summary Read the sanitized incident evidence archive.
// @Description Returns one incident's append-only control timeline, evidence completeness, similar incidents, and postmortem references. It intentionally does not expose raw diagnostic payloads or credentials.
// @Tags incidents
// @Produce json
// @Param tenant_id query string false "Admin-only tenant override"
// @Param incident_id path string true "Incident ID"
// @Success 200 {object} archiveResponse
// @Failure 400 {object} errorResponse
// @Failure 401 {object} errorResponse
// @Failure 403 {object} errorResponse
// @Failure 404 {object} errorResponse
// @Router /v1/incidents/{incident_id}/archive [get]
func (h *Handler) archive(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	if tenantID == "" {
		writeError(w, http.StatusForbidden, "forbidden", "tenant could not be derived")
		return
	}
	incidentID := chi.URLParam(r, "incident_id")
	if incidentID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "incident_id is required")
		return
	}
	if h.repository == nil {
		writeError(w, http.StatusServiceUnavailable, "not_wired", "incident repository is not wired")
		return
	}

	events, err := h.repository.ListIncident(r.Context(), tenantID, incidentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "repository_error", "incident archive lookup failed")
		return
	}
	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "incident archive is empty")
		return
	}
	tenantEvents, err := h.repository.ListTenant(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "repository_error", err.Error())
		return
	}
	runbooks, err := h.repository.ListIncidentRunbooks(r.Context(), tenantID, incidentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "repository_error", "incident archive lookup failed")
		return
	}
	repairPreviews := []repairpreview.Run{}
	if h.previews != nil {
		previewRuns, err := h.previews.ListByIncident(r.Context(), tenantID, incidentID, 3)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "repository_error", "repair preview archive lookup failed")
			return
		}
		repairPreviews = repairpreview.BoundArchiveRuns(previewRuns)
	}

	archive := buildArchive(tenantID, incidentID, events, tenantEvents)
	archive.PostmortemRefs = postmortemRefs(runbooks, incidentID)
	archive.RepairPreviews = repairPreviews
	writeJSON(w, http.StatusOK, archiveResponse{Code: 0, Message: "ok", Data: archive})
}

func (h *Handler) repairPreviewSummary(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	incidentID := chi.URLParam(r, "incident_id")
	if incidentID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "incident_id is required")
		return
	}
	if h.repository == nil {
		writeError(w, http.StatusServiceUnavailable, "not_wired", "incident repository is not wired")
		return
	}
	events, err := h.repository.ListIncident(r.Context(), tenantID, incidentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "repository_error", "incident archive lookup failed")
		return
	}
	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "incident archive is empty")
		return
	}
	summary := repairpreview.CompactSummary{}
	if h.previews != nil {
		runs, err := h.previews.ListByIncident(r.Context(), tenantID, incidentID, 3)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "repository_error", "repair preview lookup failed")
			return
		}
		summary, err = repairpreview.BuildCompactSummary(repairpreview.BoundArchiveRuns(runs))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "repository_error", "repair preview lookup failed")
			return
		}
	}
	writeJSON(w, http.StatusOK, repairPreviewSummaryResponse{Code: 0, Message: "ok", Data: summary})
}

func (h *Handler) tenantID(w http.ResponseWriter, r *http.Request) (string, bool) {
	caller, ok := tenantctx.From(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return "", false
	}
	tenantID := callerTenantID(caller)
	if caller.IsSuperuser || caller.Role == tenantctx.RoleAdmin {
		if requestedTenant := r.URL.Query().Get("tenant_id"); requestedTenant != "" {
			tenantID = requestedTenant
		}
	}
	if tenantID == "" {
		writeError(w, http.StatusForbidden, "forbidden", "tenant could not be derived")
		return "", false
	}
	if tenantID == "default" {
		if configured := os.Getenv("OPSKEEPER_DEFAULT_INCIDENT_TENANT_ID"); configured != "" {
			tenantID = configured
		}
	}
	return tenantID, true
}

type archiveIndexResponse struct {
	Code    string             `json:"code"`
	Message string             `json:"message"`
	Items   []archiveIndexItem `json:"items"`
	Total   int                `json:"total"`
}

type archiveIndexItem struct {
	IncidentID       string    `json:"incident_id"`
	EventCount       int       `json:"event_count"`
	FirstEventAt     time.Time `json:"first_event_at"`
	LastEventAt      time.Time `json:"last_event_at"`
	EvidenceComplete bool      `json:"evidence_complete"`
	Closed           bool      `json:"closed"`
}

func (h *Handler) archiveIndex(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	if tenantID == "" {
		writeError(w, http.StatusForbidden, "forbidden", "tenant could not be derived")
		return
	}
	if h.repository == nil {
		writeError(w, http.StatusServiceUnavailable, "not_wired", "incident repository is not wired")
		return
	}
	events, err := h.repository.ListTenant(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "repository_error", err.Error())
		return
	}
	items := archiveIndexFromEvents(events)
	writeJSON(w, http.StatusOK, archiveIndexResponse{Code: "0", Message: "ok", Items: items, Total: len(items)})
}

func archiveIndexFromEvents(events []incidentcontrol.Event) []archiveIndexItem {
	grouped := make(map[string][]incidentcontrol.Event)
	for _, event := range events {
		if event.IncidentID == "" {
			continue
		}
		grouped[event.IncidentID] = append(grouped[event.IncidentID], event)
	}
	items := make([]archiveIndexItem, 0, len(grouped))
	for incidentID, incidentEvents := range grouped {
		item := archiveIndexItem{IncidentID: incidentID, EventCount: len(incidentEvents)}
		found := make(map[string]bool)
		for index, event := range incidentEvents {
			if index == 0 || event.OccurredAt.Before(item.FirstEventAt) {
				item.FirstEventAt = event.OccurredAt
			}
			if index == 0 || event.OccurredAt.After(item.LastEventAt) {
				item.LastEventAt = event.OccurredAt
			}
			found[event.EventType] = true
		}
		required := []string{
			incidentcontrol.EventAlertReceived,
			incidentcontrol.EventRootCause,
			incidentcontrol.EventEvidenceRefreshed,
			incidentcontrol.EventApproved,
			incidentcontrol.EventAction,
			incidentcontrol.EventRecovery,
			incidentcontrol.EventClosed,
		}
		item.EvidenceComplete = true
		for _, eventType := range required {
			if !found[eventType] {
				item.EvidenceComplete = false
				break
			}
		}
		item.Closed = found[incidentcontrol.EventClosed]
		items = append(items, item)
	}
	sort.Slice(items, func(left, right int) bool {
		return items[left].LastEventAt.After(items[right].LastEventAt)
	})
	return items
}

type metricsResponse struct {
	Code    int                    `json:"code"`
	Message string                 `json:"message"`
	Data    incidentcontrol.Report `json:"data"`
}

type runbookResponse struct {
	Code    int                          `json:"code"`
	Message string                       `json:"message"`
	Items   []incidentcontrol.Postmortem `json:"items"`
	Total   int                          `json:"total"`
}

type recallLogResponse struct {
	Code    int                         `json:"code"`
	Message string                      `json:"message"`
	Items   []incidentcontrol.RecallLog `json:"items"`
	Total   int                         `json:"total"`
}

type archiveResponse struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    archiveSummary `json:"data"`
}

type archiveSummary struct {
	TenantID            string                  `json:"tenant_id"`
	IncidentID          string                  `json:"incident_id"`
	FirstEventAt        incidentEventTime       `json:"first_event_at"`
	LastEventAt         incidentEventTime       `json:"last_event_at"`
	EventCount          int                     `json:"event_count"`
	TraceIDs            []string                `json:"trace_ids"`
	RequiredEventTypes  []string                `json:"required_event_types"`
	MissingEventTypes   []string                `json:"missing_event_types"`
	EvidenceComplete    bool                    `json:"evidence_complete"`
	RecoveryObserved    bool                    `json:"recovery_observed"`
	Closed              bool                    `json:"closed"`
	LocalizationSeconds float64                 `json:"localization_seconds,omitempty"`
	RecoverySeconds     float64                 `json:"recovery_seconds,omitempty"`
	Timeline            []incidentcontrol.Event `json:"timeline"`
	SimilarIncidents    []similarIncident       `json:"similar_incidents"`
	PostmortemRefs      []postmortemRef         `json:"postmortem_refs"`
	RepairPreviews      []repairpreview.Run     `json:"repair_previews"`
}

type repairPreviewSummaryResponse struct {
	Code    int                          `json:"code"`
	Message string                       `json:"message"`
	Data    repairpreview.CompactSummary `json:"data"`
}

type similarIncident struct {
	IncidentID       string            `json:"incident_id"`
	LastEventAt      incidentEventTime `json:"last_event_at"`
	EventTypes       []string          `json:"event_types"`
	RecoveryObserved bool              `json:"recovery_observed"`
	Closed           bool              `json:"closed"`
}

type postmortemRef struct {
	ID          string            `json:"id"`
	IncidentID  string            `json:"incident_id"`
	RootCause   string            `json:"root_cause,omitempty"`
	ConfirmedBy string            `json:"confirmed_by,omitempty"`
	ConfirmedAt incidentEventTime `json:"confirmed_at,omitempty"`
}

type incidentEventTime time.Time

func (value incidentEventTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(value).UTC())
}

func (value *incidentEventTime) UnmarshalJSON(input []byte) error {
	var parsed time.Time
	if err := json.Unmarshal(input, &parsed); err != nil {
		return err
	}
	*value = incidentEventTime(parsed)
	return nil
}

func buildArchive(tenantID, incidentID string, events, tenantEvents []incidentcontrol.Event) archiveSummary {
	required := []string{
		incidentcontrol.EventAlertReceived,
		incidentcontrol.EventRootCause,
		incidentcontrol.EventEvidenceRefreshed,
		incidentcontrol.EventApproved,
		incidentcontrol.EventAction,
		incidentcontrol.EventRecovery,
		incidentcontrol.EventClosed,
	}
	found := make(map[string]bool, len(events))
	traceIDs := make([]string, 0, len(events))
	for _, event := range events {
		found[event.EventType] = true
		if event.TraceID != "" && !containsString(traceIDs, event.TraceID) {
			traceIDs = append(traceIDs, event.TraceID)
		}
	}
	missing := make([]string, 0)
	for _, eventType := range required {
		if !found[eventType] {
			missing = append(missing, eventType)
		}
	}

	first := events[0].OccurredAt.UTC()
	last := events[len(events)-1].OccurredAt.UTC()
	summary := archiveSummary{
		TenantID:           tenantID,
		IncidentID:         incidentID,
		FirstEventAt:       incidentEventTime(first),
		LastEventAt:        incidentEventTime(last),
		EventCount:         len(events),
		TraceIDs:           traceIDs,
		RequiredEventTypes: required,
		MissingEventTypes:  missing,
		EvidenceComplete:   len(missing) == 0,
		RecoveryObserved:   found[incidentcontrol.EventRecovery],
		Closed:             found[incidentcontrol.EventClosed],
		Timeline:           events,
		SimilarIncidents:   similarIncidents(tenantEvents, incidentID),
	}
	if alert := firstEventOfType(events, incidentcontrol.EventAlertReceived); alert != nil {
		if rootCause := firstEventOfType(events, incidentcontrol.EventRootCause); rootCause != nil {
			summary.LocalizationSeconds = rootCause.OccurredAt.Sub(alert.OccurredAt).Seconds()
		}
	}
	if action := firstEventOfType(events, incidentcontrol.EventAction); action != nil {
		if recovery := firstEventOfType(events, incidentcontrol.EventRecovery); recovery != nil {
			summary.RecoverySeconds = recovery.OccurredAt.Sub(action.OccurredAt).Seconds()
		}
	}
	return summary
}

func similarIncidents(events []incidentcontrol.Event, currentIncidentID string) []similarIncident {
	grouped := make(map[string][]incidentcontrol.Event)
	ids := make([]string, 0)
	for _, event := range events {
		if event.IncidentID == currentIncidentID {
			continue
		}
		if _, exists := grouped[event.IncidentID]; !exists {
			ids = append(ids, event.IncidentID)
		}
		grouped[event.IncidentID] = append(grouped[event.IncidentID], event)
	}
	result := make([]similarIncident, 0, len(ids))
	for _, incidentID := range ids {
		current := grouped[incidentID]
		eventTypes := make([]string, 0, len(current))
		seen := make(map[string]bool, len(current))
		for _, event := range current {
			if !seen[event.EventType] {
				seen[event.EventType] = true
				eventTypes = append(eventTypes, event.EventType)
			}
		}
		result = append(result, similarIncident{
			IncidentID:       incidentID,
			LastEventAt:      incidentEventTime(current[len(current)-1].OccurredAt.UTC()),
			EventTypes:       eventTypes,
			RecoveryObserved: seen[incidentcontrol.EventRecovery],
			Closed:           seen[incidentcontrol.EventClosed],
		})
	}
	return result
}

func postmortemRefs(runbooks []incidentcontrol.Postmortem, incidentID string) []postmortemRef {
	result := make([]postmortemRef, 0)
	for _, runbook := range runbooks {
		if runbook.IncidentID != incidentID {
			continue
		}
		result = append(result, postmortemRef{
			ID:          runbook.ID,
			IncidentID:  runbook.IncidentID,
			RootCause:   runbook.Diagnosis.RootCause,
			ConfirmedBy: runbook.ConfirmedBy,
			ConfirmedAt: incidentEventTime(runbook.ConfirmedAt.UTC()),
		})
	}
	return result
}

func firstEventOfType(events []incidentcontrol.Event, eventType string) *incidentcontrol.Event {
	for index := range events {
		if events[index].EventType == eventType {
			return &events[index]
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type errorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

func callerTenantID(caller tenantctx.Tenant) string {
	if caller.AgentTeams != nil && caller.AgentTeams.TenantID != "" {
		return caller.AgentTeams.TenantID
	}
	if caller.UserID != 0 {
		return strconv.FormatUint(caller.UserID, 10)
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Code: status, Message: message, Error: code})
}
