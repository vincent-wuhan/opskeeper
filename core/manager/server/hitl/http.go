// Package hitl exposes the AgentTeams HITL Proposal HTTP API.
package hitl

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizhitl "github.com/vincent-wuhan/opskeeper/core/manager/biz/hitl"
	hitlmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/hitl"
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
		auditCreate(w, r, tenantctx.Tenant{}, nil, nil, errors.New("caller is not an admin"))
		return
	}
	var request createRequest
	if !decodeJSON(w, r, &request) {
		auditCreate(w, r, caller, &request, nil, errs.ErrInvalid)
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
		auditCreate(w, r, caller, &request, nil, err)
		writeError(w, err)
		return
	}
	auditCreate(w, r, caller, &request, &snapshot, nil)
	writeJSON(w, http.StatusCreated, &snapshot)
}

// auditCreate records a proposal being raised. It is the other half of the
// decide story: without the create row, an approval on the chain points at a
// proposal nobody can see was ever asked for, and "who asked for this in the
// first place" has no row to point at.
func auditCreate(w http.ResponseWriter, r *http.Request, caller tenantctx.Tenant, req *createRequest, snap *bizhitl.ProposalSnapshot, cause error) {
	payload := map[string]any{}
	if req != nil {
		if req.Kind != "" {
			payload["kind"] = req.Kind
		}
		if req.Title != "" {
			payload["title"] = req.Title
		}
		if req.Source != "" {
			payload["source"] = req.Source
		}
		if req.SessionID != "" {
			payload["session_id"] = req.SessionID
		}
		if req.MessageID != "" {
			payload["message_id"] = req.MessageID
		}
		if req.Severity != "" {
			payload["severity"] = req.Severity
		}
		if req.Sensitivity != "" {
			payload["sensitivity"] = req.Sensitivity
		}
	}
	if caller.UserID != 0 {
		payload["proposed_by"] = caller.UserID
	}
	if snap != nil {
		payload["proposal_id"] = snap.ID
		if snap.PayloadHash != "" {
			payload["payload_hash"] = snap.PayloadHash
		}
		if snap.State != "" {
			payload["state"] = snap.State
		}
	}
	id := ""
	if snap != nil {
		id = snap.ID
	}
	status := auditport.StatusSuccess
	if cause != nil {
		status = auditport.StatusFailure
	}
	ev := auditport.Event{
		Action:       auditport.ActionHITLProposalCreate,
		ResourceType: auditport.ResourceHITLProposal,
		ResourceID:   id,
		Status:       status,
		Payload:      payload,
	}
	if cause != nil {
		ev.ErrorMessage = cause.Error()
	}
	auditport.SetAuditEvent(r, ev)
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

// transition is the single door behind approve / reject / expire, and
// therefore the single place their audit has to be written.
//
// 载荷里带 `payload_hash`，它是这一行存在的原因：提案本身只是一句
// "有人请求做某事"，而 hash 钉住的是**批的是哪一份**。
// 「谁批准了这条命令」在链上要能被回答成
// (proposal_id, payload_hash, decided_by, when) 四元组，
// 光有 proposal_id 只能回答"某人批准了 3 号提案"。
//
// 三个动作而不是一个带 payload 的动作，理由与决策 309 一致：
// 「谁批准的」与「谁驳回的」是被分开问的问题。而 expire 单列还有一层理由——
// **过期根本不是人的决定**，把它和 approve 混在一类里，
// 「谁批准过」的答案里就会掺进若干条谁都没批过的东西。
func (h *Handler) transition(w http.ResponseWriter, r *http.Request, state string) {
	action := auditport.ActionHITLProposalApprove
	switch state {
	case hitlmodel.StateRejected:
		action = auditport.ActionHITLProposalReject
	case hitlmodel.StateExpired:
		action = auditport.ActionHITLProposalExpire
	}
	id := chi.URLParam(r, "id")
	caller, ok := requireAdmin(w, r)
	if !ok {
		auditTransition(r, action, id, tenantctx.Tenant{}, nil, nil, errors.New("caller is not an admin"))
		return
	}
	var request transitionRequest
	if !decodeJSON(w, r, &request) {
		auditTransition(r, action, id, caller, &request, nil, errs.ErrInvalid)
		return
	}
	snapshot, err := h.service.TransitionAgentTeams(r.Context(), bizhitl.AgentTeamsTransitionInput{
		ID:            id,
		ToState:       state,
		MessageID:     request.MessageID,
		PayloadHash:   request.PayloadHash,
		MatrixEventID: request.MatrixEventID,
		Reason:        request.Reason,
		DecidedBy:     caller.UserID,
	})
	if err != nil {
		auditTransition(r, action, id, caller, &request, nil, err)
		writeError(w, err)
		return
	}
	auditTransition(r, action, id, caller, &request, &snapshot, nil)
	writeJSON(w, http.StatusOK, &snapshot)
}

func auditTransition(r *http.Request, action, id string, caller tenantctx.Tenant, req *transitionRequest, snap *bizhitl.ProposalSnapshot, cause error) {
	// payload_hash is written twice on purpose. The request's copy is what a
	// rejected attempt brought with it (there is no snapshot then); the
	// snapshot's copy is the hash of the proposal that actually moved. On
	// success the service has already proven the two are equal — a mismatch
	// never gets past assertProposalBinding.
	payload := map[string]any{"proposal_id": id}
	if req != nil {
		if req.PayloadHash != "" {
			payload["payload_hash"] = req.PayloadHash
		}
		if req.MessageID != "" {
			payload["message_id"] = req.MessageID
		}
		if req.MatrixEventID != "" {
			payload["matrix_event_id"] = req.MatrixEventID
		}
		if req.Reason != "" {
			payload["reason"] = req.Reason
		}
	}
	if caller.UserID != 0 {
		payload["decided_by"] = caller.UserID
	}
	if snap != nil {
		payload["state"] = snap.State
		if snap.PayloadHash != "" {
			payload["payload_hash"] = snap.PayloadHash
		}
	}
	status := auditport.StatusSuccess
	if cause != nil {
		status = auditport.StatusFailure
	}
	ev := auditport.Event{
		Action:       action,
		ResourceType: auditport.ResourceHITLProposal,
		ResourceID:   id,
		Status:       status,
		Payload:      payload,
	}
	if cause != nil {
		ev.ErrorMessage = cause.Error()
	}
	auditport.SetAuditEvent(r, ev)
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
