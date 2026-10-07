// Package report is the HTTP surface for scheduled reports (HLD-014).
// Mirrors the device handler's lean pattern — the chi-mounted Handler
// talks straight to the biz Usecase, pulls the caller from tenantctx,
// and gates writes on role via requireWriter. RBAC (ADR-022):
// admin/user may CRUD schedules + trigger generation; viewer is
// read-only. The public /r/{token} share route mounts separately
// without auth.
package report

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizreport "github.com/vincent-wuhan/opskeeper/core/manager/biz/report"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/report"
)

const roleViewer = "viewer"

// Handler serves the report API. uc is required; a nil uc makes every
// route 503 (binary built without the report stack wired).
type Handler struct {
	uc  *bizreport.Usecase
	now func() time.Time // injectable clock for tests
}

func NewHandler(uc *bizreport.Usecase) *Handler {
	return &Handler{uc: uc, now: func() time.Time { return time.Now().UTC() }}
}

// Register mounts the authenticated routes.
//
//	GET    /v1/reports                       list
//	POST   /v1/reports                       manual generate            (writer)
//	GET    /v1/reports/{id}                  detail
//	DELETE /v1/reports/{id}                  delete                     (writer)
//	POST   /v1/reports/{id}/share            mint share token           (writer)
//	GET    /v1/report-schedules              list
//	POST   /v1/report-schedules              create                     (writer)
//	GET    /v1/report-schedules/{id}         detail
//	PUT    /v1/report-schedules/{id}         update                     (writer)
//	DELETE /v1/report-schedules/{id}         delete                     (writer)
//	POST   /v1/report-schedules/{id}/toggle  enable/disable             (writer)
//	POST   /v1/report-schedules/{id}/run-now generate immediately       (writer)
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/reports", h.listReports)
	r.With(h.requireWriter).Post("/v1/reports", h.generateNow)
	r.Get("/v1/reports/{id}", h.getReport)
	r.With(h.requireWriter).Delete("/v1/reports/{id}", h.deleteReport)
	r.With(h.requireWriter).Post("/v1/reports/{id}/share", h.shareReport)

	r.Get("/v1/report-schedules", h.listSchedules)
	r.With(h.requireWriter).Post("/v1/report-schedules", h.createSchedule)
	r.Get("/v1/report-schedules/{id}", h.getSchedule)
	r.With(h.requireWriter).Put("/v1/report-schedules/{id}", h.updateSchedule)
	r.With(h.requireWriter).Delete("/v1/report-schedules/{id}", h.deleteSchedule)
	r.With(h.requireWriter).Post("/v1/report-schedules/{id}/toggle", h.toggleSchedule)
	r.With(h.requireWriter).Post("/v1/report-schedules/{id}/run-now", h.runNow)

	// HLD-022 Phase 2 — unified 任务 surface (recurring schedules ∪ oneoff tasks).
	r.Get("/v1/tasks", h.listTasks)
	r.With(h.requireWriter).Post("/v1/tasks/oneoff", h.createOneoffTask)
	r.Get("/v1/tasks/{id}", h.getTask)
	r.With(h.requireWriter).Post("/v1/tasks/{id}/run", h.rerunTask)
	r.With(h.requireWriter).Delete("/v1/tasks/{id}", h.deleteTask)
}

// RegisterPublic mounts the unauthenticated share route.
//
//	GET /r/{token}  shared report (read-only, 30d TTL)
func (h *Handler) RegisterPublic(r chi.Router) {
	r.Get("/r/{token}", h.sharedReport)
}

// requireWriter rejects viewer-role callers (ADR-022 read-only tier).
func (h *Handler) requireWriter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := tenantctx.From(r.Context())
		if !ok {
			writeErr(w, errs.ErrUnauthorized)
			return
		}
		if t.Role == roleViewer {
			writeErr(w, errs.ErrForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- reports ---

func (h *Handler) listReports(w http.ResponseWriter, r *http.Request) {
	if !h.authed(w, r) {
		return
	}
	q := r.URL.Query()
	f := bizreport.ReportFilter{
		Status: q.Get("status"),
		Kind:   q.Get("kind"),
		Limit:  atoiDefault(q.Get("limit"), 50),
		Offset: atoiDefault(q.Get("offset"), 0),
	}
	// schedule_id scopes the list to one schedule's reports.
	if sid := q.Get("schedule_id"); sid != "" {
		if v, err := strconv.ParseUint(sid, 10, 64); err == nil {
			f.ScheduleID = &v
		}
	}
	// task_id scopes to one task's reports (HLD-022) — the canonical 任务 detail
	// key, covering scheduled + run-now reports.
	f.TaskID = q.Get("task_id")
	rows, err := h.uc.ListReports(r.Context(), f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": toReportList(rows)})
}

func (h *Handler) getReport(w http.ResponseWriter, r *http.Request) {
	if !h.authed(w, r) {
		return
	}
	rpt, err := h.uc.GetReport(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toReportDetail(rpt))
}

func (h *Handler) deleteReport(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// 决策 337：先读标题再删。删完之后链上剩下的只是一个 uuid，而
	// 「删掉的是哪份报表」正是事后第一个要回答的问题。
	title := ""
	if rpt, err := h.uc.GetReport(r.Context(), id); err == nil && rpt != nil {
		title = rpt.Title
	}
	if err := h.uc.DeleteReport(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	// **A deleted report does not un-share itself.** The share token and its
	// expiry live on the row, so deleting the row is what closes the public
	// URL — which makes "who closed it and when" a question worth a row of its
	// own rather than a footnote on the delete.
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionReportDelete,
		ResourceType: auditport.ResourceReport,
		ResourceID:   id,
		ResourceName: title,
		Status:       auditport.StatusSuccess,
	})
	writeJSON(w, http.StatusNoContent, nil)
}

type generateNowReq struct {
	Kind      string `json:"kind"`
	Timezone  string `json:"timezone"`
	ScopeJSON string `json:"scope_json"`
}

func (h *Handler) generateNow(w http.ResponseWriter, r *http.Request) {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	var req generateNowReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	if req.Kind == "" {
		req.Kind = model.KindWeekly
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	loc, err := time.LoadLocation(req.Timezone)
	if err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	period, err := bizreport.PeriodFor(req.Kind, h.now(), loc, time.Time{})
	if err != nil {
		writeErr(w, err)
		return
	}
	scope := req.ScopeJSON
	if scope == "" {
		scope = "{}"
	}
	rpt, err := h.uc.GenerateNow(r.Context(), t.UserID, req.Kind, req.Timezone, scope, localeFromRequest(r), "", period)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 337：scope_json 不进链——它是筛选条件，可能含租户内部名字；
	// 进链的是「谁在什么时候要了一份什么周期、什么时区的报表」，
	// 报表本身随后会成为一行可查的 report。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionReportGenerate,
		ResourceType: auditport.ResourceReport,
		ResourceID:   rpt.ID,
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"kind": req.Kind, "timezone": req.Timezone},
	})
	writeJSON(w, http.StatusAccepted, toReportDetail(rpt))
}

func (h *Handler) shareReport(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// The report has to be read **before** sharing, because ShareReport writes
	// the token and the expiry onto the row: after the call the only thing left
	// to read is the secret itself.
	title := ""
	var expires *time.Time
	if rpt, err := h.uc.GetReport(r.Context(), id); err == nil && rpt != nil {
		title, expires = rpt.Title, rpt.ShareExpiresAt
	}
	token, err := h.uc.ShareReport(r.Context(), id, h.now())
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 337：这一行是整个 report 面后果最重的一条，而它**唯一不能包含的
	// 就是刚刚铸出来的那个 token**。
	//
	// token 是一条不需要认证就能读到这份报表的持有者凭证：它在 URL 里、在浏览器
	// 历史里、在任何抓过这个链接的东西里。而**链不能撤销它**——append-only 是
	// 这个设计的全部意义，写进去等于把一个可撤销的秘密换成一个永恒的秘密。
	// 事后要查的是「谁在什么时候公开了哪份报表、公开到什么时候」，这三样都留下了。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionReportShare,
		ResourceType: auditport.ResourceReport,
		ResourceID:   id,
		ResourceName: title,
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"expires_at": expires, "public_path": "/r/{token}"},
	})
	writeJSON(w, http.StatusOK, map[string]any{"share_token": token, "path": "/r/" + token})
}

func (h *Handler) sharedReport(w http.ResponseWriter, r *http.Request) {
	rpt, err := h.uc.GetSharedReport(r.Context(), chi.URLParam(r, "token"), h.now())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toReportDetail(rpt))
}

// --- schedules ---

func (h *Handler) listSchedules(w http.ResponseWriter, r *http.Request) {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	rows, err := h.uc.ListSchedules(r.Context(), t.UserID, t.Role != roleViewer)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": toScheduleList(rows)})
}

func (h *Handler) getSchedule(w http.ResponseWriter, r *http.Request) {
	if !h.authed(w, r) {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	s, err := h.uc.GetSchedule(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toScheduleView(s))
}

type scheduleReq struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Kind           string   `json:"kind"`
	CronSpec       string   `json:"cron_spec"`
	Timezone       string   `json:"timezone"`
	ScopeJSON      string   `json:"scope_json"`
	ChannelIDs     []uint64 `json:"channel_ids"`
	InAppVisible   *bool    `json:"in_app_visible"`
	PromptOverride string   `json:"prompt_override"`
}

func (h *Handler) createSchedule(w http.ResponseWriter, r *http.Request) {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	var req scheduleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	s := req.toModel(t.UserID)
	if err := h.uc.CreateSchedule(r.Context(), s, h.now()); err != nil {
		writeErr(w, err)
		return
	}
	// 决策 337：建一条 schedule 就是**装上一台没人盯着就会自己发报文的机器**。
	// 链上要留下的是「谁在什么时候装了什么、按什么周期、在哪个时区」——
	// cron_spec 与 timezone 决定它在运维人员不在场时会做什么，比 description 更要紧。
	// scope_json 不进链：那是筛选条件里的租户内部名字，而这台机器要发出去。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionScheduleCreate,
		ResourceType: auditport.ResourceReportSchedule,
		ResourceID:   strconv.FormatUint(s.ID, 10),
		ResourceName: s.Name,
		Status:       auditport.StatusSuccess,
		Payload: map[string]any{
			"kind":              s.Kind,
			"cron_spec":         s.CronSpec,
			"timezone":          s.Timezone,
			"enabled":           s.Enabled,
			"in_app_visible":    s.InAppVisible,
			"prompt_overridden": s.PromptOverride != nil,
		},
	})
	writeJSON(w, http.StatusCreated, toScheduleView(s))
}

func (h *Handler) updateSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	existing, err := h.uc.GetSchedule(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var req scheduleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	req.applyTo(existing)
	if err := h.uc.UpdateSchedule(r.Context(), existing, h.now()); err != nil {
		writeErr(w, err)
		return
	}
	// 决策 337：改 schedule 与建它是同一族后果——把周期从每天改成每分钟，
	// 或者把 prompt_override 换掉，都是在没有人在场的情况下改掉了将来会发什么。
	// payload 里带的是**改完之后**的形状：事后要问的是「出事那天它是按什么在跑」。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionScheduleUpdate,
		ResourceType: auditport.ResourceReportSchedule,
		ResourceID:   strconv.FormatUint(existing.ID, 10),
		ResourceName: existing.Name,
		Status:       auditport.StatusSuccess,
		Payload: map[string]any{
			"kind":              existing.Kind,
			"cron_spec":         existing.CronSpec,
			"timezone":          existing.Timezone,
			"in_app_visible":    existing.InAppVisible,
			"prompt_overridden": existing.PromptOverride != nil,
		},
	})
	writeJSON(w, http.StatusOK, toScheduleView(existing))
}

func (h *Handler) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 337：先读名字再删。删完之后链上剩下的只是一个自增 id，
	// 而「凌晨停掉的是哪台自动发报文的机器」正是事后第一个要回答的问题。
	name := ""
	if s, err := h.uc.GetSchedule(r.Context(), id); err == nil && s != nil {
		name = s.Name
	}
	if err := h.uc.DeleteSchedule(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionScheduleDelete,
		ResourceType: auditport.ResourceReportSchedule,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: name,
		Status:       auditport.StatusSuccess,
	})
	writeJSON(w, http.StatusNoContent, nil)
}

type toggleReq struct {
	Enabled bool `json:"enabled"`
}

func (h *Handler) toggleSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var req toggleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	// 决策 337：enabled 从来没有和名字一起被记下来过，而「这台机器现在还开着吗」
	// 是每一次事故前后都要问的问题。停用要先读名字：停掉之后链上只剩一个 id。
	name := ""
	if prev, err := h.uc.GetSchedule(r.Context(), id); err == nil && prev != nil {
		name = prev.Name
	}
	s, err := h.uc.SetScheduleEnabled(r.Context(), id, req.Enabled, h.now())
	if err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionScheduleToggle,
		ResourceType: auditport.ResourceReportSchedule,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: name,
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"enabled": req.Enabled, "next_fire_at": s.NextFireAt},
	})
	writeJSON(w, http.StatusOK, toScheduleView(s))
}

func (h *Handler) runNow(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	rpt, err := h.uc.RunNow(r.Context(), id, localeFromRequest(r), h.now())
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 337：手动跑一次与 cron 自己跑一次产生**同样一份会发出去的报文**，
	// 所以它必须有自己的一行，并指向新生成的 report id——事后「这份是哪次跑的」
	// 就是靠这个 id 而不是靠时间戳猜的。schedule 的名字一并读出来，因为
	// rpt.ScheduleID 是指针而报表自身没有可读的名字。
	name := ""
	if s, err := h.uc.GetSchedule(r.Context(), id); err == nil && s != nil {
		name = s.Name
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionScheduleRun,
		ResourceType: auditport.ResourceReportSchedule,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: name,
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"report_id": rpt.ID, "locale": localeFromRequest(r)},
	})
	writeJSON(w, http.StatusAccepted, toReportDetail(rpt))
}

// --- helpers ---

func (h *Handler) authed(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return false
	}
	return true
}

func pathID(r *http.Request) (uint64, error) {
	id, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		return 0, errors.Join(errs.ErrInvalid, err)
	}
	return id, nil
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// localeFromRequest picks the operator's UI language from Accept-Language
// (sent by the SPA from web/src/i18n/locale.ts). Returns "en" / "zh" or
// "" when unset/unknown — the generator's DefaultLocale catches "" for
// scheduled fires. Mirrors alert/http.go's helper. See
// feedback_ai_output_locale.
func localeFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	raw := strings.TrimSpace(r.Header.Get("Accept-Language"))
	if raw == "" {
		return ""
	}
	first := strings.SplitN(raw, ",", 2)[0]
	primary := strings.ToLower(strings.SplitN(strings.TrimSpace(first), "-", 2)[0])
	switch primary {
	case "en", "zh":
		return primary
	default:
		return ""
	}
}

// compile-time guard keeps context import lint-clean across edits.
var _ = context.Background
