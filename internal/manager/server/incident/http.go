// Package incident exposes judge-facing incident metrics.
package incident

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	incidentcontrol "github.com/vincent-wuhan/opskeeper/internal/control/incident"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

type Repository interface {
	ListTenant(ctx context.Context, tenantID string) ([]incidentcontrol.Event, error)
	ListRunbooks(ctx context.Context, tenantID, databaseType, faultFingerprint string) ([]incidentcontrol.Postmortem, error)
	ListRecallLogs(ctx context.Context, tenantID, incidentID string) ([]incidentcontrol.RecallLog, error)
}

type Handler struct {
	repository Repository
}

func NewHandler(repository Repository) *Handler {
	return &Handler{repository: repository}
}

func (h *Handler) Register(router chi.Router) {
	router.Get("/v1/incidents/metrics", h.metrics)
	router.Get("/v1/incidents/runbooks", h.runbooks)
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

func (h *Handler) tenantID(w http.ResponseWriter, r *http.Request) (string, bool) {
	caller, ok := tenantctx.From(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return "", false
	}
	tenantID := callerTenantID(caller)
	if caller.IsSuperuser || caller.Role == "admin" {
		if requestedTenant := r.URL.Query().Get("tenant_id"); requestedTenant != "" {
			tenantID = requestedTenant
		}
	}
	if tenantID == "" {
		writeError(w, http.StatusForbidden, "forbidden", "tenant could not be derived")
		return "", false
	}
	return tenantID, true
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
