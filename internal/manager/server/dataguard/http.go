// Package dataguard 是 Data-Guard 业务层防护的 HTTP 接入。
//
// 路径 A P1-3 阶段 1 任务 1.4 — POST /api/v1/data-guard/labels（人工打标）。
// 后续 Phase 还会加 GET（查询）等。
//
// 路由（全部 admin-only，因为人工打标 / override 都需要 admin 审计）：
//
//	POST   /v1/data-guard/labels         人工打标 / 覆盖
//	PUT    /v1/data-guard/labels/{t}/{id} 显式 override
//	GET    /v1/data-guard/labels?resource_type=&resource_id= 查询
//	GET    /v1/data-guard/labels?sensitivity=&source= 列表筛选
//	DELETE /v1/data-guard/labels/{t}/{id} 强制清理（admin 审计）
package dataguard

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
	dglabel "github.com/vincent-wuhan/opskeeper/internal/dataguard/label"
	"github.com/vincent-wuhan/opskeeper/internal/dataguard/store"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// Handler serves /v1/data-guard/labels.
type Handler struct {
	mgr  *dglabel.LabelManager
	repo dglabel.Repo
}

// NewHandler wires the manager.
func NewHandler(mgr *dglabel.LabelManager) *Handler {
	return &Handler{mgr: mgr, repo: mgr.Repo()}
}

// Register mounts routes.
func (h *Handler) Register(r chi.Router) {
	r.Post("/v1/data-guard/labels", h.upsertLabel)
	r.Put("/v1/data-guard/labels/{type}/{id}", h.overrideLabel)
	r.Get("/v1/data-guard/labels", h.listOrGet)
	r.Delete("/v1/data-guard/labels/{type}/{id}", h.deleteLabel)
}

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

// LabelRequest 是 POST /v1/data-guard/labels 的请求体。
//
// 支持两种语义：
//   - 人工打标（fill_create=true 或留空）：写一条新的 manual label
//   - 显式 override（fill_override=true）：保留旧敏感性，写入 override 记录
type LabelRequest struct {
	ResourceType   string   `json:"resource_type"`
	ResourceID     string   `json:"resource_id"`
	Sensitivity    string   `json:"sensitivity"`
	ComplianceTags []string `json:"compliance_tags,omitempty"`
	Notes          string   `json:"notes,omitempty"`
	// Override 标记：true 时走 UpdateOverride，Notes 自动追加 override_of=<prev>
	Override       bool   `json:"override,omitempty"`
	OverrideReason string `json:"override_reason,omitempty"`
}

// LabelResponse 是写入后返回的 label + Effective sensitivity（解析后值）。
type LabelResponse struct {
	Label               *store.DataSensitivityLabel `json:"label"`
	Effective           string                      `json:"effective_sensitivity"`
	EffectiveConfidence float64                     `json:"effective_confidence"`
	ViaInherited        bool                        `json:"via_inherited"`
}

func (h *Handler) upsertLabel(w http.ResponseWriter, r *http.Request) {
	cl, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var req LabelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, errs.ErrInvalid)
		return
	}
	if req.ResourceType == "" || req.ResourceID == "" || req.Sensitivity == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "resource_type/resource_id/sensitivity required", "code": "invalid"})
		return
	}
	if !dataguard.IsValid(req.Sensitivity) {
		writeErr(w, errs.ErrInvalid)
		return
	}

	tagsJSON, err := dglabel.EncodeJSONTags(req.ComplianceTags)
	if err != nil {
		writeErr(w, errs.ErrInvalid)
		return
	}
	l := &store.DataSensitivityLabel{
		ResourceType:   req.ResourceType,
		ResourceID:     req.ResourceID,
		Sensitivity:    req.Sensitivity,
		ComplianceTags: tagsJSON,
		Notes:          req.Notes,
	}

	var saved *store.DataSensitivityLabel
	if req.Override {
		saved, err = h.mgr.UpdateOverride(r.Context(), req.ResourceType, req.ResourceID, dataguard.MustParse(req.Sensitivity), usernameFromCaller(cl), req.OverrideReason)
	} else {
		err = h.mgr.CreateManual(r.Context(), l, usernameFromCaller(cl))
		saved = l
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	eff, conf, via, _ := h.mgr.ResolveEffective(r.Context(), req.ResourceType, req.ResourceID)
	writeJSON(w, http.StatusOK, LabelResponse{
		Label:               saved,
		Effective:           eff.String(),
		EffectiveConfidence: conf,
		ViaInherited:        via,
	})
}

func (h *Handler) overrideLabel(w http.ResponseWriter, r *http.Request) {
	cl, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	rt := chi.URLParam(r, "type")
	rid := chi.URLParam(r, "id")
	if rt == "" || rid == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	var req LabelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, errs.ErrInvalid)
		return
	}
	if req.Sensitivity == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	saved, err := h.mgr.UpdateOverride(r.Context(), rt, rid, dataguard.MustParse(req.Sensitivity), usernameFromCaller(cl), req.OverrideReason)
	if err != nil {
		writeErr(w, err)
		return
	}
	eff, conf, via, _ := h.mgr.ResolveEffective(r.Context(), rt, rid)
	writeJSON(w, http.StatusOK, LabelResponse{
		Label:               saved,
		Effective:           eff.String(),
		EffectiveConfidence: conf,
		ViaInherited:        via,
	})
}

func (h *Handler) listOrGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	q := r.URL.Query()
	rt := q.Get("resource_type")
	rid := q.Get("resource_id")
	if rt != "" && rid != "" {
		// 单条查询
		l, err := h.mgr.Get(r.Context(), rt, rid)
		if err != nil {
			writeErr(w, err)
			return
		}
		eff, conf, via, _ := h.mgr.ResolveEffective(r.Context(), rt, rid)
		writeJSON(w, http.StatusOK, LabelResponse{
			Label:               l,
			Effective:           eff.String(),
			EffectiveConfidence: conf,
			ViaInherited:        via,
		})
		return
	}
	// 列表（task 2.7：resource_type 过滤 + sensitivity/source 维度）
	sens := q.Get("sensitivity")
	src := q.Get("source")
	rtFilter := q.Get("resource_type")
	limit := 100
	offset := 0
	var (
		items []*store.DataSensitivityLabel
		total int64
		err   error
	)
	if rtFilter != "" {
		items, err = h.repo.ListByResourceType(r.Context(), rtFilter, "", limit, offset)
		total = int64(len(items))
	} else {
		items, total, err = h.mgr.List(r.Context(), sens, src, limit, offset)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	// effective=true 时附带 Effective 解析
	if q.Get("effective") == "true" {
		out := make([]EffectiveLabel, 0, len(items))
		for _, l := range items {
			eff, conf, via, _ := h.mgr.ResolveEffective(r.Context(), l.ResourceType, l.ResourceID)
			tags, _ := dataguard.UnmarshalComplianceTags(l.ComplianceTags)
			out = append(out, EffectiveLabel{
				Label:               l,
				ComplianceTags:      tags,
				Effective:           eff.String(),
				EffectiveConfidence: conf,
				ViaInherited:        via,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": out,
			"total": total,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"total": total,
	})
}

func (h *Handler) deleteLabel(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rt := chi.URLParam(r, "type")
	rid := chi.URLParam(r, "id")
	if err := h.mgr.Delete(r.Context(), rt, rid); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func usernameFromCaller(c caller) string {
	if c.UserID == 0 {
		return "admin"
	}
	return "admin:" + uintToString(c.UserID)
}

func uintToString(u uint64) string {
	const digits = "0123456789"
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = digits[u%10]
		u /= 10
	}
	return string(b[i:])
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

// EffectiveLabel 是 task 2.7：GET 列表 + effective=true 时返回的扩展行。
//
// 与 LabelResponse 区别：列表场景下 ComplianceTags 已是反序列化后的 []ComplianceTag
// （避免前端再二次 unmarshal）。
type EffectiveLabel struct {
	Label               *store.DataSensitivityLabel `json:"label"`
	ComplianceTags      []dataguard.ComplianceTag   `json:"compliance_tags,omitempty"`
	Effective           string                      `json:"effective_sensitivity"`
	EffectiveConfidence float64                     `json:"effective_confidence"`
	ViaInherited        bool                        `json:"via_inherited"`
}
