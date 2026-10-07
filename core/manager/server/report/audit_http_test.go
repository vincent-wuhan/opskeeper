package report

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizreport "github.com/vincent-wuhan/opskeeper/core/manager/biz/report"
	reportstore "github.com/vincent-wuhan/opskeeper/core/manager/data/report/store"
)

// auditServe is `serve` plus an audit slot, so these tests can read the row the
// handler declared. 生产里槽由中间件装上；这里装在身份之前，顺序与 port.go
// 记的那个坑一致（中间任何 WithContext 包装都不会丢 *slot）。
func auditServe(h *Handler, r *http.Request) (*httptest.ResponseRecorder, auditport.Event, bool) {
	router := chi.NewRouter()
	h.Register(router)
	h.RegisterPublic(router)
	ctx := auditport.WithSlot(r.Context())
	ctx = tenantctx.With(ctx, tenantctx.Tenant{UserID: 42, Role: "user"})
	r = r.WithContext(ctx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	ev, set := auditport.GetAuditEvent(r.Context())
	return rec, ev, set
}

func auditReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// makeReport generates a report through the HTTP surface so the id under test
// is one a real caller could have gotten.
func makeReport(t *testing.T, h *Handler) string {
	t.Helper()
	rec, _, _ := auditServe(h, auditReq("POST", "/v1/reports", `{"kind":"weekly","timezone":"UTC"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("generate status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var d reportDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d.ID
}

func makeSchedule(t *testing.T, h *Handler, name string) uint64 {
	t.Helper()
	body := `{"name":"` + name + `","kind":"weekly","timezone":"Asia/Shanghai"}`
	rec, ev, set := auditServe(h, auditReq("POST", "/v1/report-schedules", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create schedule status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("createSchedule left no audit event")
	}
	id := ev.ResourceID
	if id == "" {
		t.Fatal("createSchedule audit row carries no resource id")
	}
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		t.Fatalf("resource id %q is not a schedule id: %v", id, err)
	}
	return n
}

// TestShareAuditRowDoesNotCarryTheToken is the load-bearing one for 决策 337.
//
// shareReport mints a credential that reads this report **with no
// authentication at all**. The chain is append-only and cannot revoke it, so
// writing the token down would trade a revocable secret for an eternal one.
// The row must still answer the two questions that matter — which report, and
// until when.
func TestShareAuditRowDoesNotCarryTheToken(t *testing.T) {
	h, _ := newTestHandler(t)
	id := makeReport(t, h)

	rec, ev, set := auditServe(h, auditReq("POST", "/v1/reports/"+id+"/share", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("share status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("share left no audit event")
	}
	var sh struct {
		Token string `json:"share_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sh); err != nil {
		t.Fatal(err)
	}
	if sh.Token == "" {
		t.Fatal("no share token returned — nothing to keep out of the chain")
	}

	if ev.Action != auditport.ActionReportShare {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionReportShare)
	}
	if ev.ResourceType != auditport.ResourceReport || ev.ResourceID != id {
		t.Fatalf("resource = %q/%q, want report/%s", ev.ResourceType, ev.ResourceID, id)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Fatalf("status = %q, want success", ev.Status)
	}

	// The token must appear nowhere in the row: not as a payload value, not in
	// the resource id, not in the name.
	payload, _ := json.Marshal(ev.Payload)
	if strings.Contains(string(payload), sh.Token) {
		t.Fatalf("audit payload carries the share token verbatim: %s", payload)
	}
	whole, _ := json.Marshal(ev)
	if strings.Contains(string(whole), sh.Token) {
		t.Fatalf("audit row carries the share token: %s", whole)
	}
	if ev.ResourceID == sh.Token || ev.ResourceName == sh.Token {
		t.Fatal("share token leaked into a resource field")
	}

	// …but the two things an operator actually asks are still there.
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map", ev.Payload)
	}
	// Key presence, not value: a `*time.Time` nil inside an `any` is not
	// `== nil`, and a dropped key is not a smaller mistake than a wrong one.
	if _, ok := p["expires_at"]; !ok {
		t.Error("share row has no expires_at key — 公开到什么时候答不出来")
	}
	if p["public_path"] != "/r/{token}" {
		t.Errorf("public_path = %v, want the literal /r/{token}", p["public_path"])
	}
}

// TestDeleteReportAuditRowNamesTheReport: after the delete the chain holds only
// a uuid, and "which report was deleted" is the first question asked.
func TestDeleteReportAuditRowNamesTheReport(t *testing.T) {
	h, db := newTestHandler(t)
	repo := reportstore.NewRepo(db)
	uc := bizreport.NewUsecase(repo, nil, func() string { return "fixed-id" }).WithReadRepo(repo)
	h = NewHandler(uc)
	id := makeReport(t, h)

	// Give it a human title, which is the whole point of the name field.
	rpt, err := uc.GetReport(context.Background(), id)
	if err != nil || rpt == nil {
		t.Fatalf("read back report: %v", err)
	}
	rpt.Title = "SRE 周报 · 10 月第 1 周"
	if err := repo.UpdateReport(context.Background(), rpt); err != nil {
		t.Fatalf("seed title: %v", err)
	}

	rec, ev, set := auditServe(h, auditReq("DELETE", "/v1/reports/"+id, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("delete left no audit event")
	}
	if ev.Action != auditport.ActionReportDelete {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionReportDelete)
	}
	if ev.ResourceID != id {
		t.Fatalf("resource id = %q, want %s", ev.ResourceID, id)
	}
	if !strings.Contains(ev.ResourceName, "SRE 周报") {
		t.Fatalf("delete row name = %q, want the report title（删完之后只剩一个 uuid）", ev.ResourceName)
	}
}

// TestToggleScheduleAuditRowCarriesEnabledAndName: "is this automation still
// armed" is asked before every incident. A uuid does not answer it.
func TestToggleScheduleAuditRowCarriesEnabledAndName(t *testing.T) {
	h, _ := newTestHandler(t)
	id := makeSchedule(t, h, "每日 09:00 运维周报")

	rec, ev, set := auditServe(h, auditReq("POST", "/v1/report-schedules/"+
		itoa(id)+"/toggle", `{"enabled":false}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("toggle left no audit event")
	}
	if ev.Action != auditport.ActionScheduleToggle {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionScheduleToggle)
	}
	if ev.ResourceType != auditport.ResourceReportSchedule {
		t.Fatalf("resource type = %q, want %q", ev.ResourceType, auditport.ResourceReportSchedule)
	}
	if ev.ResourceName != "每日 09:00 运维周报" {
		t.Fatalf("toggle row name = %q, want the schedule name", ev.ResourceName)
	}
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map", ev.Payload)
	}
	// The key must be **present**: a missing key and a `false` value are both
	// "not armed" to a reader skimming the chain, and only one of them is a
	// fact about this request. Asserting the value alone lets the field be
	// deleted outright.
	raw, ok := p["enabled"]
	if !ok {
		t.Fatal("toggle row has no `enabled` key — 链上答不出「这台机器还开着吗」")
	}
	got, isBool := raw.(bool)
	if !isBool {
		t.Fatalf("enabled type = %T, want bool", raw)
	}
	if got {
		t.Fatalf("enabled = true, want false（这正是被问的问题）")
	}
	// A typed nil (*time.Time) inside an `any` is NOT == nil, so ask the type:
	// "下一次什么时候" 必须在停用后真的没有答案，而不是有一个装着 nil 的盒子。
	if nf, ok := p["next_fire_at"].(*time.Time); ok && nf != nil {
		t.Errorf("next_fire_at = %v, want nil after disabling", nf)
	} else if !ok && p["next_fire_at"] != nil {
		t.Errorf("next_fire_at type = %T, want *time.Time or nil", p["next_fire_at"])
	}
}

// TestDeleteScheduleAuditRowNamesTheSchedule: same reasoning on the automation
// side — which machine stopped is not answerable from an autoincrement id.
func TestDeleteScheduleAuditRowNamesTheSchedule(t *testing.T) {
	h, _ := newTestHandler(t)
	id := makeSchedule(t, h, "夜间容量周报")

	rec, ev, set := auditServe(h, auditReq("DELETE", "/v1/report-schedules/"+itoa(id), ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("schedule delete left no audit event")
	}
	if ev.Action != auditport.ActionScheduleDelete {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionScheduleDelete)
	}
	if ev.ResourceName != "夜间容量周报" {
		t.Fatalf("row name = %q, want the schedule name", ev.ResourceName)
	}
}

// TestUpdateScheduleAuditRowCarriesThePostChangeShape: "what was it running as
// on the day of the incident" is the question, so the row holds the cron and
// timezone **after** the change.
func TestUpdateScheduleAuditRowCarriesThePostChangeShape(t *testing.T) {
	h, _ := newTestHandler(t)
	id := makeSchedule(t, h, "周报")

	rec, ev, set := auditServe(h, auditReq("PUT", "/v1/report-schedules/"+itoa(id),
		`{"name":"周报（每 5 分钟）","kind":"custom","cron_spec":"*/5 * * * *","timezone":"UTC"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("update left no audit event")
	}
	if ev.Action != auditport.ActionScheduleUpdate {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionScheduleUpdate)
	}
	p, _ := ev.Payload.(map[string]any)
	if p["cron_spec"] != "*/5 * * * *" {
		t.Errorf("cron_spec = %v, want the post-change */5 * * * *", p["cron_spec"])
	}
	if p["timezone"] != "UTC" {
		t.Errorf("timezone = %v, want UTC", p["timezone"])
	}
	if ev.ResourceName != "周报（每 5 分钟）" {
		t.Errorf("name = %q, want the post-change name", ev.ResourceName)
	}
	// The row must point at **this** schedule. `existing` is a mutable struct
	// the handler hands to the usecase, so an id taken from the wrong place
	// still type-checks and still looks right — only the chain tells.
	if ev.ResourceID != itoa(id) {
		t.Errorf("resource id = %q, want %d（链上挂错了 schedule 就等于什么都没记）", ev.ResourceID, id)
	}
}

// TestCreateScheduleAuditRowCarriesCronAndTimezone: creating a schedule arms
// something that publishes unattended. The cadence and the timezone decide what
// it will do while nobody is watching.
func TestCreateScheduleAuditRowCarriesCronAndTimezone(t *testing.T) {
	h, _ := newTestHandler(t)
	rec, ev, set := auditServe(h, auditReq("POST", "/v1/report-schedules",
		`{"name":"凌晨对账","kind":"custom","cron_spec":"30 3 * * *","timezone":"Asia/Shanghai"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("create left no audit event")
	}
	if ev.Action != auditport.ActionScheduleCreate {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionScheduleCreate)
	}
	p, _ := ev.Payload.(map[string]any)
	if p["cron_spec"] != "30 3 * * *" {
		t.Errorf("cron_spec = %v, want 30 3 * * *", p["cron_spec"])
	}
	if p["timezone"] != "Asia/Shanghai" {
		t.Errorf("timezone = %v, want Asia/Shanghai", p["timezone"])
	}
	if got, ok := p["enabled"].(bool); !ok || !got {
		t.Errorf("enabled = %v (present=%v), want true on a freshly armed schedule", p["enabled"], ok)
	}
}

// TestGenerateNowAuditRowKeepsScopeOutOfTheChain: scope_json is a filter over
// tenant-internal names; kind + timezone are what actually happened.
func TestGenerateNowAuditRowKeepsScopeOutOfTheChain(t *testing.T) {
	h, _ := newTestHandler(t)
	rec, ev, set := auditServe(h, auditReq("POST", "/v1/reports",
		`{"kind":"weekly","timezone":"Asia/Shanghai","scope_json":"{\"edge_ids\":[7]}"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("generate status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("generate left no audit event")
	}
	if ev.Action != auditport.ActionReportGenerate {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionReportGenerate)
	}
	if ev.ResourceID == "" {
		t.Error("generate row carries no report id")
	}
	whole, _ := json.Marshal(ev)
	if strings.Contains(string(whole), "edge_ids") {
		t.Fatalf("audit row carries scope_json: %s", whole)
	}
	p, _ := ev.Payload.(map[string]any)
	if p["kind"] != "weekly" || p["timezone"] != "Asia/Shanghai" {
		t.Errorf("payload = %v, want kind=weekly timezone=Asia/Shanghai", p)
	}
}

// TestRunNowAuditRowPointsAtTheProducedReport: a manual run publishes the same
// artifact a cron tick would, and the row has to say which run produced it.
func TestRunNowAuditRowPointsAtTheProducedReport(t *testing.T) {
	h, _ := newTestHandler(t)
	id := makeSchedule(t, h, "每周复盘")

	rec, ev, set := auditServe(h, auditReq("POST", "/v1/report-schedules/"+itoa(id)+"/run-now", ""))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run-now status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("run-now left no audit event")
	}
	if ev.Action != auditport.ActionScheduleRun {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionScheduleRun)
	}
	var d reportDetail
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	p, _ := ev.Payload.(map[string]any)
	if p["report_id"] != d.ID {
		t.Fatalf("report_id = %v, want the produced report %s", p["report_id"], d.ID)
	}
	if ev.ResourceName != "每周复盘" {
		t.Errorf("row name = %q, want the schedule name", ev.ResourceName)
	}
}

// TestFailedShareLeavesNoSuccessRow: the 403 path must not look audited as a
// published report.
func TestViewerCannotShareAndNothingIsClaimed(t *testing.T) {
	h, _ := newTestHandler(t)
	id := makeReport(t, h)

	r := auditReq("POST", "/v1/reports/"+id+"/share", "")
	r = r.WithContext(tenantctx.With(auditport.WithSlot(r.Context()),
		tenantctx.Tenant{UserID: 42, Role: "viewer"}))
	router := chi.NewRouter()
	h.Register(router)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer share status = %d, want 403", rec.Code)
	}
	if ev, set := auditport.GetAuditEvent(r.Context()); set {
		t.Fatalf("denied share claimed an audit row: %+v", ev)
	}
}

func itoa(v uint64) string { return strconv.FormatUint(v, 10) }
