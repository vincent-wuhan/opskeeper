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
//	GET    /v1/data-guard/compliance/frameworks 五个框架的推荐控制项 + 本构建的真实状态
package dataguard

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard"
	dglabel "github.com/vincent-wuhan/opskeeper/core/manager/dataguard/label"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard/store"
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
	r.Get("/v1/data-guard/compliance/frameworks", h.listFrameworks)
}

// FrameworkCatalogResponse 是 GET /v1/data-guard/compliance/frameworks 的响应体。
//
// 关键在于每一条控制项都带着 status：目录函数的注释写着"用作 UI 提示 /
// 一键加载按钮"，于是控制台会把这一串名字摆到操作员面前。**如果不同时告诉她
// 这 16 条里本构建强制了 0 条，这个端点就是同一个谎的第二个来源**——
// 而且比注释更难驳回，因为它穿着 API 的外衣。
type FrameworkCatalogResponse struct {
	Frameworks []dataguard.FrameworkCatalog `json:"frameworks"`
}

// listFrameworks 返回目录，每条控制项附它在登记表里的下落。
//
// admin-only，与本包其余路由一致。它是静态参考数据、不含任何租户数据，
// 所以"只读"本身并不构成放开它的理由；真正的理由是**它的消费者就是打标页面**，
// 而打标页面本身是 admin-only 的。放低权限只会让控制台在另一处再写一遍
// "这个页面需要 admin"——而那种权限判断写在文档里从来保不住。
func (h *Handler) listFrameworks(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, FrameworkCatalogResponse{
		Frameworks: dataguard.ControlCatalog(),
	})
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

// auditLabel puts one data-guard label change on the chain.
//
// 这三个路由改的是**数据脱敏规则本身**——它是安全控制，不是元数据。
// 一个 override 可以把某个资源的 effective sensitivity 从 SECRET 降成 PUBLIC，
// 于是链上必须能回答「谁把什么从什么降到了什么」。这也是载荷里带
// `effective_before` 的唯一理由：只记 after 的行答不出降级这件事发生过。
//
// 非 admin 的尝试同样入账：拒绝发生在链上之前，而「有人在反复试着摘掉一条脱敏规则」
// 恰恰是最该被看见的模式。
func auditLabel(r *http.Request, action string, cl caller, rt, rid string, req *LabelRequest, before, after string, cause error) {
	payload := map[string]any{}
	if rt != "" {
		payload["resource_type"] = rt
	}
	if rid != "" {
		payload["resource_id"] = rid
	}
	if cl.UserID != 0 {
		payload["actor_user_id"] = cl.UserID
	}
	if req != nil {
		if req.Sensitivity != "" {
			payload["sensitivity"] = req.Sensitivity
		}
		if req.Override {
			payload["override"] = true
		}
		if req.OverrideReason != "" {
			payload["override_reason"] = req.OverrideReason
		}
		if len(req.ComplianceTags) > 0 {
			payload["compliance_tags"] = req.ComplianceTags
		}
		if req.Notes != "" {
			payload["notes"] = req.Notes
		}
	}
	if before != "" {
		payload["effective_before"] = before
	}
	if after != "" {
		payload["effective_after"] = after
	}
	status := auditport.StatusSuccess
	if cause != nil {
		status = auditport.StatusFailure
	}
	ev := auditport.Event{
		Action:       action,
		ResourceType: auditport.ResourceDataGuardLabel,
		ResourceID:   rt + "/" + rid,
		Status:       status,
		Payload:      payload,
	}
	if cause != nil {
		ev.ErrorMessage = cause.Error()
	}
	auditport.SetAuditEvent(r, ev)
}

func (h *Handler) upsertLabel(w http.ResponseWriter, r *http.Request) {
	cl, ok := requireAdmin(w, r)
	if !ok {
		auditLabel(r, actionForLabelReq(nil, ""), cl, "", "", nil, "", "", errors.New("caller is not an admin"))
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

	// 降级之前先把当前生效值取出来：override 的全部信息量都在「从什么降到什么」，
	// 而那件事发生之后就查不到了。
	beforeEff, _, _, _ := h.mgr.ResolveEffective(r.Context(), req.ResourceType, req.ResourceID)
	before := ""
	if beforeEff.String() != "" {
		before = beforeEff.String()
	}

	var saved *store.DataSensitivityLabel
	if req.Override {
		saved, err = h.mgr.UpdateOverride(r.Context(), req.ResourceType, req.ResourceID, dataguard.MustParse(req.Sensitivity), usernameFromCaller(cl), req.OverrideReason)
	} else {
		err = h.mgr.CreateManual(r.Context(), l, usernameFromCaller(cl))
		saved = l
	}
	if err != nil {
		auditLabel(r, actionForLabelReq(&req, ""), cl, req.ResourceType, req.ResourceID, &req, before, "", err)
		writeErr(w, err)
		return
	}
	eff, conf, via, _ := h.mgr.ResolveEffective(r.Context(), req.ResourceType, req.ResourceID)
	auditLabel(r, actionForLabelReq(&req, ""), cl, req.ResourceType, req.ResourceID, &req, before, eff.String(), nil)
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
		auditLabel(r, auditport.ActionDataGuardLabelOverride, cl, "", "", nil, "", "", errors.New("caller is not an admin"))
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
	req.Override = true
	beforeEff, _, _, _ := h.mgr.ResolveEffective(r.Context(), rt, rid)
	before := ""
	if beforeEff.String() != "" {
		before = beforeEff.String()
	}
	saved, err := h.mgr.UpdateOverride(r.Context(), rt, rid, dataguard.MustParse(req.Sensitivity), usernameFromCaller(cl), req.OverrideReason)
	if err != nil {
		auditLabel(r, auditport.ActionDataGuardLabelOverride, cl, rt, rid, &req, before, "", err)
		writeErr(w, err)
		return
	}
	eff, conf, via, _ := h.mgr.ResolveEffective(r.Context(), rt, rid)
	auditLabel(r, auditport.ActionDataGuardLabelOverride, cl, rt, rid, &req, before, eff.String(), nil)
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
			// 错误不吞。一个解析不了的列不是"这个资源没有标签"，而是
			// "我不知道这一列里写了什么"——前者让控制台显示一个空列表，
			// 后者会让一个贴了 GDPR 标签的资源看起来从未被打过标。
			tags, tagErr := dglabel.DecodeJSONTags(l.ComplianceTags)
			if tagErr != nil {
				writeErr(w, tagErr)
				return
			}
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
	cl, ok := requireAdmin(w, r)
	if !ok {
		auditLabel(r, auditport.ActionDataGuardLabelDelete, cl, "", "", nil, "", "", errors.New("caller is not an admin"))
		return
	}
	rt := chi.URLParam(r, "type")
	rid := chi.URLParam(r, "id")
	// 删掉之后这条资源就不再被脱敏，而"删之前它是什么"是唯一问得出的问题。
	beforeEff, _, _, _ := h.mgr.ResolveEffective(r.Context(), rt, rid)
	before := ""
	if beforeEff.String() != "" {
		before = beforeEff.String()
	}
	if err := h.mgr.Delete(r.Context(), rt, rid); err != nil {
		auditLabel(r, auditport.ActionDataGuardLabelDelete, cl, rt, rid, nil, before, "", err)
		writeErr(w, err)
		return
	}
	auditLabel(r, auditport.ActionDataGuardLabelDelete, cl, rt, rid, nil, before, "unset", nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// actionForLabelReq picks the action for a POST that may be either a fresh
// label or an override. Override is separated because it can only *lower* a
// classification, and "谁把 SECRET 降成了 PUBLIC" is a question the chain
// should answer with its own filter rather than a payload scan.
func actionForLabelReq(req *LabelRequest, _ string) string {
	if req != nil && req.Override {
		return auditport.ActionDataGuardLabelOverride
	}
	return auditport.ActionDataGuardLabelSet
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
// 与 LabelResponse 区别：列表场景下 ComplianceTags 已从存储列解出（避免前端
// 二次 unmarshal）。
//
// 它的类型是 []string 而不是 []dataguard.ComplianceTag，理由是这一列里装的
// 就是框架名：写路径只有 label.EncodeJSONTags 一个写入方，它产出的形状是
// `["GDPR","PCI-DSS"]`。**曾经这里声明成富标签类型，读路径用一个形状对不上的
// 解码器去解析并把错误丢掉，于是合规标签在读回来时永远是空的**（决策 368）。
// `controls` 与 `enforced` 至今没有写入方，它们是登记表
// `compliance.enforced-tag` 那一行 declared 的内容，不是这一行要假装的东西。
type EffectiveLabel struct {
	Label               *store.DataSensitivityLabel `json:"label"`
	ComplianceTags      []string                    `json:"compliance_tags,omitempty"`
	Effective           string                      `json:"effective_sensitivity"`
	EffectiveConfidence float64                     `json:"effective_confidence"`
	ViaInherited        bool                        `json:"via_inherited"`
}
