// Package hitl exposes the AgentTeams HITL Proposal HTTP API.
package hitl

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	bizhitl "github.com/vincent-wuhan/opskeeper/internal/manager/biz/hitl"
	hitlmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

const maxRequestBodyBytes = 32 << 10

type Handler struct {
	service *bizhitl.Service
}

func NewHandler(service *bizhitl.Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Register(router chi.Router) {
	router.Group(func(versioned chi.Router) {
		versioned.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Opskeeper-Version") != "v1" {
					writeError(w, errs.ErrInvalid)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
		versioned.Post("/v1/hitl/proposals", h.create)
		versioned.Get("/v1/hitl/proposals/{id}", h.get)
		versioned.Post("/v1/hitl/proposals/{id}/approve", h.approve)
		versioned.Post("/v1/hitl/proposals/{id}/reject", h.reject)
		versioned.Post("/v1/hitl/proposals/{id}/expire", h.expire)
	})
}

type createRequest struct {
	Kind        string                      `json:"kind"`
	Title       string                      `json:"title"`
	Summary     string                      `json:"summary"`
	Payload     hitlmodel.AgentTeamsPayload `json:"payload"`
	Source      string                      `json:"source"`
	SessionID   string                      `json:"session_id"`
	MessageID   string                      `json:"message_id"`
	Severity    string                      `json:"severity"`
	Sensitivity string                      `json:"sensitivity"`
	IMThreadID  string                      `json:"im_thread_id"`
	ExpiresAt   time.Time                   `json:"expires_at"`
}

type transitionRequest struct {
	MessageID     string `json:"message_id"`
	PayloadHash   string `json:"payload_hash"`
	MatrixEventID string `json:"matrix_event_id"`
	Reason        string `json:"reason"`
}

type apiResponse struct {
	Code    int                       `json:"code"`
	Message string                    `json:"message"`
	Data    *bizhitl.ProposalSnapshot `json:"data"`
}

// create
// @Summary Create an AgentTeams HITL proposal
// @Tags hitl
// @Accept json
// @Produce json
// @Param request body hitl.createRequest true "Proposal request"
// @Success 201 {object} hitl.apiResponse
// @Router /api/v1/hitl/proposals [post]
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var request createRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	snapshot, err := h.service.CreateAgentTeams(r.Context(), bizhitl.AgentTeamsCreateInput{
		Kind:        request.Kind,
		Title:       request.Title,
		Summary:     request.Summary,
		Payload:     request.Payload,
		Source:      request.Source,
		SessionID:   request.SessionID,
		MessageID:   request.MessageID,
		Severity:    request.Severity,
		Sensitivity: request.Sensitivity,
		IMThreadID:  request.IMThreadID,
		ExpiresAt:   request.ExpiresAt,
		ProposedBy:  caller.UserID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, &snapshot)
}

// get
// @Summary Get an AgentTeams HITL proposal snapshot
// @Tags hitl
// @Produce json
// @Param id path string true "Proposal UUID"
// @Success 200 {object} hitl.apiResponse
// @Router /api/v1/hitl/proposals/{id} [get]
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	snapshot, err := h.service.Snapshot(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &snapshot)
}

// approve
// @Summary Approve an AgentTeams HITL proposal
// @Tags hitl
// @Accept json
// @Produce json
// @Param id path string true "Proposal UUID"
// @Param request body hitl.transitionRequest true "Verified transition"
// @Success 200 {object} hitl.apiResponse
// @Router /api/v1/hitl/proposals/{id}/approve [post]
func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, hitlmodel.StateApproved)
}

// reject
// @Summary Reject an AgentTeams HITL proposal
// @Tags hitl
// @Accept json
// @Produce json
// @Param id path string true "Proposal UUID"
// @Param request body hitl.transitionRequest true "Verified transition"
// @Success 200 {object} hitl.apiResponse
// @Router /api/v1/hitl/proposals/{id}/reject [post]
func (h *Handler) reject(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, hitlmodel.StateRejected)
}

// expire
// @Summary Expire an AgentTeams HITL proposal
// @Tags hitl
// @Accept json
// @Produce json
// @Param id path string true "Proposal UUID"
// @Param request body hitl.transitionRequest true "Verified transition"
// @Success 200 {object} hitl.apiResponse
// @Router /api/v1/hitl/proposals/{id}/expire [post]
func (h *Handler) expire(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, hitlmodel.StateExpired)
}

func (h *Handler) transition(w http.ResponseWriter, r *http.Request, state string) {
	caller, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var request transitionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	snapshot, err := h.service.TransitionAgentTeams(r.Context(), bizhitl.AgentTeamsTransitionInput{
		ID:            chi.URLParam(r, "id"),
		ToState:       state,
		MessageID:     request.MessageID,
		PayloadHash:   request.PayloadHash,
		MatrixEventID: request.MatrixEventID,
		Reason:        request.Reason,
		DecidedBy:     caller.UserID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &snapshot)
}

func requireAdmin(w http.ResponseWriter, r *http.Request) (tenantctx.Tenant, bool) {
	caller, ok := tenantctx.From(r.Context())
	if !ok {
		writeError(w, errs.ErrUnauthorized)
		return tenantctx.Tenant{}, false
	}
	if caller.Role != "admin" && !caller.IsSuperuser {
		writeError(w, errs.ErrForbidden)
		return tenantctx.Tenant{}, false
	}
	return caller, true
}

func decodeJSON[T any](w http.ResponseWriter, r *http.Request, target *T) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.More() {
		writeError(w, errs.ErrInvalid)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, data *bizhitl.ProposalSnapshot) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Opskeeper-Version", "v1")
	w.WriteHeader(status)
	message := "success"
	if status >= 400 {
		message = http.StatusText(status)
	}
	if err := json.NewEncoder(w).Encode(apiResponse{Code: status, Message: message, Data: data}); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, errs.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, errs.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, errs.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, errs.ErrConflict), errors.Is(err, bizhitl.ErrProposalMismatch):
		status = http.StatusConflict
	}
	writeJSON(w, status, nil)
}
