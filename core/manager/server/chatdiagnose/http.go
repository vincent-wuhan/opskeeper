// Package chatdiagnose — HTTP handler for the chat diagnose entry
// point. Mirrors core/manager/server/loop/http.go shape (chi
// router, narrow Service interface, Swagger annotations).
package chatdiagnose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	chatdiagnosebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/chatdiagnose"
)

// ChatDiagnoseService is the narrow contract the handler needs.
// Implemented by *chatdiagnose.ChatDiagnoseService. Kept narrow so
// tests can inject a fake without pulling the full biz graph.
type ChatDiagnoseService interface {
	Diagnose(ctx context.Context, req chatdiagnosebiz.ChatDiagnoseRequest) (*chatdiagnosebiz.ChatDiagnoseResponse, error)
	PromoteToLoop(ctx context.Context, conversationID string, turnID int64, tenantID string) (*chatdiagnosebiz.OrchestratorRunResult, error)
	PushReportToConversation(ctx context.Context, conversationID, tenantID, reportMarkdown string) error
}

// ChatDiagnoseRequestBody is the wire-level DTO for POST /diagnose.
// Slightly looser than the biz DTO so the handler can do per-field
// validation + feature-flag rejection before reaching the service.
type ChatDiagnoseRequestBody struct {
	UserMessage    string   `json:"user_message"`
	MentionedAgent string   `json:"mentioned_agent,omitempty"`
	ContextRefs    []string `json:"context_refs,omitempty"`
	ConversationID string   `json:"conversation_id,omitempty"`
	TenantID       string   `json:"tenant_id"`
	UserID         string   `json:"user_id"`
}

// PromoteRequestBody is the wire-level DTO for POST /conversations/{id}/promote.
type PromoteRequestBody struct {
	TurnID   int64  `json:"turn_id"`
	TenantID string `json:"tenant_id"`
}

// ReportRequestBody is the wire-level DTO for POST /conversations/{id}/reports.
type ReportRequestBody struct {
	TenantID       string `json:"tenant_id"`
	ReportMarkdown string `json:"report_markdown"`
}

// response envelope
type apiResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

type Handler struct {
	svc ChatDiagnoseService
}

func NewHandler(svc ChatDiagnoseService) *Handler {
	return &Handler{svc: svc}
}

// Register mounts the three endpoints under r. Caller is responsible
// for auth/tenant middleware (we don't double-wrap).
func (h *Handler) Register(r chi.Router) {
	r.Route("/chat", func(sub chi.Router) {
		sub.Post("/diagnose", h.diagnose)
		sub.Post("/conversations/{id}/promote", h.promote)
		sub.Post("/conversations/{id}/reports", h.pushReport)
	})
}

// diagnose — POST /api/v1/chat/diagnose
//
// @Summary  Diagnose an alert via chat
// @Description  Conversational entry point: take a user message with
// @Description  @-agent and @-resource mentions, run a chat ReAct cycle,
// @Description  and (optionally) surface a promotion hint to the loop.
// @Tags     chatdiagnose
// @Accept   json
// @Produce  json
// @Param    body  body  ChatDiagnoseRequestBody  true  "Diagnose request"
// @Success  200  {object}  apiResponse{data=chatdiagnosebiz.ChatDiagnoseResponse}
// @Failure  400  {object}  apiResponse  "validation: missing tenant/message/agent"
// @Failure  403  {object}  apiResponse  "feature flag off / tenant mismatch"
// @Failure  500  {object}  apiResponse  "internal error"
// @Router   /v1/chat/diagnose [post]
func (h *Handler) diagnose(w http.ResponseWriter, r *http.Request) {
	var body ChatDiagnoseRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionChatDiagnose,
			ResourceType: auditport.ResourceChatConversation,
			Payload:      map[string]any{"message_len": 0},
		}, err)
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// The message itself is deliberately absent from the row. It is an
	// instruction to an agent that can run tools, it has no length limit, and
	// operators paste it from whatever the incident page showed them — which
	// is regularly a credential or a token. The row carries the length and a
	// digest, so two identical questions are comparable and neither is
	// readable. The conversation's own turns and the tool calls they caused
	// carry what actually happened.
	//
	// The cost is stated rather than hidden: on its own this row cannot say
	// what was asked. It says that an investigation was started, by whom,
	// against which conversation, and the tool rows answer the rest.
	diagPayload := map[string]any{
		"message_len":     len(body.UserMessage),
		"message_digest":  auditport.ValueDigest(body.UserMessage),
		"mentioned_agent": body.MentionedAgent,
	}
	if body.ConversationID != "" {
		diagPayload["requested_conversation_id"] = body.ConversationID
	}
	if strings.TrimSpace(body.TenantID) == "" {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionChatDiagnose,
			ResourceType: auditport.ResourceChatConversation,
			Payload:      diagPayload,
		}, chatdiagnosebiz.ErrMissingTenant)
		writeErr(w, http.StatusBadRequest, "tenant_id is required")
		return
	}
	if strings.TrimSpace(body.UserMessage) == "" {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionChatDiagnose,
			ResourceType: auditport.ResourceChatConversation,
			Payload:      diagPayload,
		}, chatdiagnosebiz.ErrEmptyMessage)
		writeErr(w, http.StatusBadRequest, "user_message is required")
		return
	}
	// build biz request — context_refs are pre-validated strings; service parses further
	refs := make([]chatdiagnosebiz.ResourceRef, 0, len(body.ContextRefs))
	for _, raw := range body.ContextRefs {
		t, id, err := splitWireRef(raw)
		if err != nil {
			auditFail(r, auditport.Event{
				Action:       auditport.ActionChatDiagnose,
				ResourceType: auditport.ResourceChatConversation,
				ResourceID:   body.ConversationID,
				Payload:      diagPayload,
			}, fmt.Errorf("bad context_ref %q: %w", raw, err))
			writeErr(w, http.StatusBadRequest, "bad context_ref: "+err.Error())
			return
		}
		refs = append(refs, chatdiagnosebiz.ResourceRef{Type: t, ID: id})
	}
	req := chatdiagnosebiz.ChatDiagnoseRequest{
		UserMessage:    body.UserMessage,
		MentionedAgent: body.MentionedAgent,
		ContextRefs:    refs,
		ConversationID: body.ConversationID,
		TenantID:       body.TenantID,
		UserID:         body.UserID,
	}
	diagPayload["context_ref_count"] = len(refs)
	resp, err := h.svc.Diagnose(r.Context(), req)
	if err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionChatDiagnose,
			ResourceType: auditport.ResourceChatConversation,
			ResourceID:   body.ConversationID,
			Payload:      diagPayload,
		}, err)
		writeErr(w, mapStatus(err), err.Error())
		return
	}
	// The conversation id comes from the response, not the request: the first
	// message of a thread creates it, and a row keyed on the request's empty
	// string would leave every new investigation filed under no thread at
	// all — which is exactly the one an operator searches for.
	auditOK(r, auditport.Event{
		Action:       auditport.ActionChatDiagnose,
		ResourceType: auditport.ResourceChatConversation,
		ResourceID:   resp.ConversationID,
		Payload:      diagPayload,
	})
	writeOK(w, resp)
}

// promote — POST /api/v1/chat/conversations/{id}/promote
//
// @Summary  Promote a chat conversation to a closed-loop run
// @Description  PromoteToLoop kicks off an Orchestrator.Run from
// @Description  PhaseCorrelated, skipping the detected phase (per
// @Description  spec §"升级为闭环"). Returns the first loop event id so
// @Description  the SPA can subscribe to the timeline.
// @Tags     chatdiagnose
// @Accept   json
// @Produce  json
// @Param    id    path  string                  true  "Conversation ID"
// @Param    body  body  PromoteRequestBody     true  "Promote request"
// @Success  200  {object}  apiResponse{data=chatdiagnosebiz.OrchestratorRunResult}
// @Failure  400  {object}  apiResponse  "missing fields"
// @Failure  403  {object}  apiResponse  "feature flag off / tenant mismatch"
// @Failure  500  {object}  apiResponse  "internal error"
// @Router   /v1/chat/conversations/{id}/promote [post]
func (h *Handler) promote(w http.ResponseWriter, r *http.Request) {
	convID := chi.URLParam(r, "id")
	var body PromoteRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionChatPromote,
			ResourceType: auditport.ResourceChatConversation,
			ResourceID:   convID,
		}, err)
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// The turn id is load-bearing: promote is "this reply in this thread
	// becomes an execution-plane run", and without the turn a row says only
	// that some conversation was promoted, which is any of its turns.
	promotePayload := map[string]any{"turn_id": body.TurnID}
	if strings.TrimSpace(body.TenantID) == "" || convID == "" {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionChatPromote,
			ResourceType: auditport.ResourceChatConversation,
			ResourceID:   convID,
			Payload:      promotePayload,
		}, chatdiagnosebiz.ErrMissingTenant)
		writeErr(w, http.StatusBadRequest, "conversation_id and tenant_id are required")
		return
	}
	res, err := h.svc.PromoteToLoop(r.Context(), convID, body.TurnID, body.TenantID)
	if err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionChatPromote,
			ResourceType: auditport.ResourceChatConversation,
			ResourceID:   convID,
			Payload:      promotePayload,
		}, err)
		writeErr(w, mapStatus(err), err.Error())
		return
	}
	// The run it started is named on the row, so the chain answers "which
	// incident did this conversation cause to be worked" without a join into
	// the loop's own tables.
	promotePayload["incident_id"] = res.IncidentID
	promotePayload["first_loop_event_id"] = res.FirstLoopEventID
	promotePayload["final_phase"] = res.FinalPhase
	auditOK(r, auditport.Event{
		Action:       auditport.ActionChatPromote,
		ResourceType: auditport.ResourceChatConversation,
		ResourceID:   convID,
		Payload:      promotePayload,
	})
	writeOK(w, res)
}

// pushReport — POST /api/v1/chat/conversations/{id}/reports
//
// @Summary  Push a post-loop postmortem report into a conversation
// @Description  Async write-back: orchestrator finished a run and the
// @Description  postmortem markdown is being appended to the chat thread
// @Description  that triggered the run. Spec §"自动化完成后回灌 Postmortem
// @Description  摘要到对话".
// @Tags     chatdiagnose
// @Accept   json
// @Produce  json
// @Param    id    path  string                  true  "Conversation ID"
// @Param    body  body  ReportRequestBody      true  "Report payload"
// @Success  204  "no content"
// @Failure  400  {object}  apiResponse  "missing fields"
// @Failure  403  {object}  apiResponse  "tenant mismatch"
// @Failure  500  {object}  apiResponse  "internal error"
// @Router   /v1/chat/conversations/{id}/reports [post]
func (h *Handler) pushReport(w http.ResponseWriter, r *http.Request) {
	convID := chi.URLParam(r, "id")
	var body ReportRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionChatReport,
			ResourceType: auditport.ResourceChatConversation,
			ResourceID:   convID,
			Payload:      map[string]any{"report_len": 0},
		}, err)
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Same bargain as diagnose, for the same reason: the postmortem is
	// unbounded markdown assembled from an automated run, and it is written
	// into a thread that other people read. Length and digest, not the text.
	reportPayload := map[string]any{
		"report_len":    len(body.ReportMarkdown),
		"report_digest": auditport.ValueDigest(body.ReportMarkdown),
	}
	if strings.TrimSpace(body.TenantID) == "" || convID == "" {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionChatReport,
			ResourceType: auditport.ResourceChatConversation,
			ResourceID:   convID,
			Payload:      reportPayload,
		}, chatdiagnosebiz.ErrMissingTenant)
		writeErr(w, http.StatusBadRequest, "conversation_id and tenant_id are required")
		return
	}
	if err := h.svc.PushReportToConversation(r.Context(), convID, body.TenantID, body.ReportMarkdown); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionChatReport,
			ResourceType: auditport.ResourceChatConversation,
			ResourceID:   convID,
			Payload:      reportPayload,
		}, err)
		writeErr(w, mapStatus(err), err.Error())
		return
	}
	auditOK(r, auditport.Event{
		Action:       auditport.ActionChatReport,
		ResourceType: auditport.ResourceChatConversation,
		ResourceID:   convID,
		Payload:      reportPayload,
	})
	w.WriteHeader(http.StatusNoContent)
}

// helpers
func writeOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(apiResponse{Code: 0, Message: "ok", Data: data})
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResponse{Code: status, Message: msg})
}

// mapStatus maps chatdiagnose sentinel errors to HTTP status codes.
func mapStatus(err error) int {
	switch {
	case errors.Is(err, chatdiagnosebiz.ErrFeatureDisabled),
		errors.Is(err, chatdiagnosebiz.ErrConversationTenantMismatch):
		return http.StatusForbidden
	case errors.Is(err, chatdiagnosebiz.ErrMissingTenant),
		errors.Is(err, chatdiagnosebiz.ErrEmptyMessage),
		errors.Is(err, chatdiagnosebiz.ErrMissingMentionedAgent),
		errors.Is(err, chatdiagnosebiz.ErrUnknownAgent):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// splitWireRef 把 "type:id" 拆成 type, id（service 端的 wire 形态）。
func splitWireRef(raw string) (string, string, error) {
	if raw == "" {
		return "", "", nil
	}
	for i, c := range raw {
		if c == ':' {
			if i == 0 || i == len(raw)-1 {
				return "", "", errors.New("empty type or id")
			}
			return raw[:i], raw[i+1:], nil
		}
	}
	return "", "", errors.New("want type:id format")
}
