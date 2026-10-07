// Package approval is the HTTP surface for the propose-confirm inbox
// (HLD-017). Read + decide routes are admin-only. Strictly additive.
package approval

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/approval"
)

// Handler serves /v1/approvals.
type Handler struct{ uc *bizapproval.Usecase }

// NewHandler wires the usecase.
func NewHandler(uc *bizapproval.Usecase) *Handler { return &Handler{uc: uc} }

// Register mounts the routes under an auth'd chi.Router.
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/approvals", h.list)
	r.Get("/v1/approvals/count", h.count)
	r.Get("/v1/approvals/{id}", h.get)
	r.Post("/v1/approvals/{id}/approve", h.approve)
	r.Post("/v1/approvals/{id}/reject", h.reject)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	status := r.URL.Query().Get("status")
	items, err := h.uc.List(r.Context(), status, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) count(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	n, err := h.uc.CountPending(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": n})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	a, err := h.uc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c, ok := requireAdmin(w, r)
	if !ok {
		// 一个**没有权限的人试图批准**一次执行，是这条链上最该被看见的一类行。
		// 不入账的话，"有人一直在试"与"没人试过"在链上长得一模一样。
		auditDecision(r, auditport.ActionApprovalApprove, auditport.StatusFailure,
			0, id, nil, errors.New("caller is not an admin"))
		return
	}
	a, decided, err := h.uc.Sign(r.Context(), bizapproval.Signer{
		UserID: c.UserID, Role: c.Role, At: time.Now().UTC(),
	}, id)
	if err != nil {
		auditDecision(r, auditport.ActionApprovalApprove, auditport.StatusFailure,
			c.UserID, id, nil, err)
		writeErr(w, err)
		return
	}
	if !decided {
		// 一次签名记下了，但这行还没到能执行的时候。**这不是失败**：审计
		// 记 success，HTTP 记 202，因为"我签了"是一件完成了的事，而把它
		// 报成错误会训练运维把双签当成故障。
		auditDecision(r, auditport.ActionApprovalApprove, auditport.StatusSuccess,
			c.UserID, id, a, nil)
		writeJSON(w, http.StatusAccepted, a)
		return
	}
	// 审计状态跟着**执行结果**走，而不是跟着 HTTP 走。
	//
	// Approve 会调用 executor：HTTP 200 的背后可能是"命令跑成功了"，
	// 也可能是"executor 报错"（approval 行的 status 是 failed 而 HTTP 仍然是 200，
	// 因为决策本身成功了）。把这两种合成一行 success，
	// "哪些批准没跑成"这个问题就退化成了 payload 扫描——
	// 而它恰恰是批准之后第一个会被问到的问题。
	status := auditport.StatusSuccess
	if a != nil && a.Status == model.StatusFailed {
		status = auditport.StatusFailure
	}
	auditDecision(r, auditport.ActionApprovalApprove, status, c.UserID, id, a, nil)
	writeJSON(w, http.StatusOK, a)
}

// auditDecision puts one inbox decision on the audit chain.
//
// 载荷里带 **payload**：那是被批准/驳回的东西本身——一条命令的逐词内容。
// 没有它，链上只能回答"某人在某时批准了 3 号提案"，
// 回答不了"他批准的那条命令是什么"，而后者才是审批这件事存在的理由。
//
// 也带 kind：它决定的是哪个 executor 会跑这条命令，
// 而"跑的是哪一类"决定了后果的量级。
func auditDecision(r *http.Request, action, status string, approverID uint64, id string, a *model.Approval, cause error) {
	payload := map[string]any{
		"approval_id": id,
		"approver_id": approverID,
	}
	if a != nil {
		payload["kind"] = a.Kind
		payload["title"] = a.Title
		payload["source"] = a.Source
		if a.SessionID != "" {
			payload["session_id"] = a.SessionID
		}
		if a.PayloadJSON != "" {
			payload["payload"] = a.PayloadJSON
		}
		if a.Status != "" {
			payload["result_status"] = a.Status
		}
	}
	ev := auditport.Event{
		Action:       action,
		ResourceType: auditport.ResourceApproval,
		ResourceID:   id,
		ResourceName: id,
		Status:       status,
		Payload:      payload,
	}
	if cause != nil {
		ev.ErrorMessage = cause.Error()
	}
	auditport.SetAuditEvent(r, ev)
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c, ok := requireAdmin(w, r)
	if !ok {
		auditDecision(r, auditport.ActionApprovalReject, auditport.StatusFailure,
			0, id, nil, errors.New("caller is not an admin"))
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in)
	// 先读再决定：链上要记的是**被决定的那一份 payload**，
	// 而不是决定之后可能已经变了的那一份。
	before, _ := h.uc.Get(r.Context(), id)
	if err := h.uc.Reject(r.Context(), c.UserID, id, in.Reason); err != nil {
		auditDecision(r, auditport.ActionApprovalReject, auditport.StatusFailure,
			c.UserID, id, before, err)
		writeErr(w, err)
		return
	}
	auditDecision(r, auditport.ActionApprovalReject, auditport.StatusSuccess,
		c.UserID, id, before, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- auth + json helpers (mirrors server/secret) ---

type caller struct {
	UserID uint64
	Role   string
}

func requireAdmin(w http.ResponseWriter, r *http.Request) (caller, bool) {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return caller{}, false
	}
	if t.Role != "admin" {
		writeErr(w, errs.ErrForbidden)
		return caller{}, false
	}
	return caller{UserID: t.UserID, Role: t.Role}, true
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	slug := "internal"
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		code, slug = http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, errs.ErrForbidden):
		code, slug = http.StatusForbidden, "forbidden"
	case errors.Is(err, errs.ErrNotFound):
		code, slug = http.StatusNotFound, "not_found"
	case errors.Is(err, errs.ErrInvalid):
		code, slug = http.StatusBadRequest, "invalid"
	}
	writeJSON(w, code, errorBody{Error: err.Error(), Code: slug})
}
