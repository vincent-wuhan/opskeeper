// Package skill is the manager-side HTTP layer for the skill framework.
// Routes:
//
//	GET  /v1/skills                    list all skills (optional ?category=)
//	GET  /v1/skills/{key}              one skill's metadata
//	POST /v1/skills/{key}/execute      execute on a target edge
//	                                   body: { edge_id, params }
package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	svc "github.com/vincent-wuhan/opskeeper/core/manager/biz/skill"
)

// Service is the narrow contract the handler depends on. Production
// passes the wired biz/skill.Service; tests inject a fake.
type Service interface {
	List(ctx context.Context, caller svc.Caller, category string) []svc.SkillSummary
	Get(ctx context.Context, caller svc.Caller, key string) (*svc.SkillSummary, error)
	Execute(ctx context.Context, caller svc.Caller, in svc.ExecuteInput) (*svc.ExecuteOutput, error)
}

// Handler holds the wired service.
type Handler struct{ svc Service }

// NewHandler builds the HTTP handler.
func NewHandler(s Service) *Handler { return &Handler{svc: s} }

// Register attaches routes to r. Caller must wrap r in the JWT auth
// middleware before calling this.
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/skills", h.list)
	r.Get("/v1/skills/{key}", h.get)
	r.Post("/v1/skills/{key}/execute", h.execute)
}

type listResp struct {
	Items []svc.SkillSummary `json:"items"`
	Total int                `json:"total"`
}

type executeReq struct {
	EdgeID uint64          `json:"edge_id"`
	Params json.RawMessage `json:"params,omitempty"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFromRequest(r)
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	items := h.svc.List(r.Context(), caller, r.URL.Query().Get("category"))
	writeJSON(w, http.StatusOK, listResp{Items: items, Total: len(items)})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFromRequest(r)
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	key := chi.URLParam(r, "key")
	item, err := h.svc.Get(r.Context(), caller, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// auditExecute puts one skill dispatch on the host chain.
//
// 这条路由**已经有一份审计**：`biz/skill` 的 GormAuditSink 为每次 Execute 写一行
// 到 `skill_executions`（含 skill_key / edge_id / caller / class / params / error）。
// 补这一行的理由不是"之前没有记录"，而是两个地方对"记录"的定义不同：
// 那张表可以按 skill_key 查，但它没有链，`core/base/pkg/audit` 的 VerifyChain
// 管不到它，而"谁在什么时候跑了哪个 skill"恰恰是事故之后第一个要问的问题。
//
// 载荷带 params 逐字内容，与 `skill_executions.params_json` 一致——
// **这不是新增一类暴露**：如果 params 里有密钥，那张表已经存了一份。
// 换句话说，这一行没有把什么东西放到比原来更低的地方。
func auditExecute(r *http.Request, caller svc.Caller, key string, edgeID uint64, params json.RawMessage, out *svc.ExecuteOutput, cause error) {
	payload := map[string]any{
		"skill_key": key,
		"edge_id":   edgeID,
		"params":    string(params),
	}
	if out != nil && out.Error != "" {
		payload["skill_error"] = out.Error
	}
	status := auditport.StatusSuccess
	if cause != nil {
		status = auditport.StatusFailure
	}
	ev := auditport.Event{
		Action:       auditport.ActionSkillExecute,
		ResourceType: auditport.ResourceSkill,
		ResourceID:   key,
		ResourceName: key,
		Status:       status,
		Payload:      payload,
	}
	if cause != nil {
		ev.ErrorMessage = cause.Error()
	}
	auditport.SetAuditEvent(r, ev)
}

func (h *Handler) execute(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFromRequest(r)
	if !ok {
		auditExecute(r, caller, chi.URLParam(r, "key"), 0, nil, nil, errs.ErrUnauthorized)
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	key := chi.URLParam(r, "key")
	if key == "" {
		auditExecute(r, caller, "", 0, nil, nil, fmt.Errorf("%w: skill key required", errs.ErrInvalid))
		writeErr(w, fmt.Errorf("%w: skill key required", errs.ErrInvalid))
		return
	}
	var req executeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auditExecute(r, caller, key, 0, nil, nil, errors.Join(errs.ErrInvalid, err))
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	// edge_id requirement is scope-dependent — let the service layer
	// decide. ScopeManager skills (web_search / subprocess packs) skip
	// the check there; ScopeHost skills still 400 when edge_id == 0.
	out, err := h.svc.Execute(r.Context(), caller, svc.ExecuteInput{
		Key:    key,
		EdgeID: req.EdgeID,
		Params: req.Params,
	})
	if err != nil {
		auditExecute(r, caller, key, req.EdgeID, req.Params, nil, err)
		writeErr(w, err)
		return
	}
	// skill 自己在体内报错时 out.Error 非空而 RPC 成功——那是**执行了并且失败了**，
	// 不是"没执行"。审计状态跟着它走，否则"跑了但没跑成"会被记成成功。
	statusOK := out == nil || out.Error == ""
	if statusOK {
		auditExecute(r, caller, key, req.EdgeID, req.Params, out, nil)
	} else {
		auditExecute(r, caller, key, req.EdgeID, req.Params, out,
			fmt.Errorf("skill returned an error: %s", out.Error))
	}
	writeJSON(w, http.StatusOK, out)
}

func callerFromRequest(r *http.Request) (svc.Caller, bool) {
	tenant, ok := tenantctx.From(r.Context())
	if !ok {
		return svc.Caller{}, false
	}
	return svc.Caller{UserID: tenant.UserID, Role: tenant.Role}, true
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeErr(w http.ResponseWriter, err error) {
	status := errs.HTTPStatus(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: err.Error(), Code: errCode(err)})
}

func errCode(err error) string {
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, errs.ErrForbidden):
		return "forbidden"
	case errors.Is(err, errs.ErrNotFound):
		return "not-found"
	case errors.Is(err, errs.ErrInvalid):
		return "invalid"
	default:
		return "internal"
	}
}

// parseUint64 is a small helper for tests that need to convert URL
// params manually. Not currently used by the handler but exported in
// case future routes (e.g. by-id) pick it up.
func parseUint64(s string) (uint64, error) {
	return strconv.ParseUint(s, 10, 64)
}
