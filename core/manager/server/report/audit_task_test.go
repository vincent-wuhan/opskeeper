package report

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	bizreport "github.com/vincent-wuhan/opskeeper/core/manager/biz/report"
	reportstore "github.com/vincent-wuhan/opskeeper/core/manager/data/report/store"
)

// newHandlerOnDB builds a second Handler over the **same** database, so a task
// created through a working generator can be re-run through a broken one.
func newHandlerOnDB(t *testing.T, db *gorm.DB, gen bizreport.Generator) *Handler {
	t.Helper()
	repo := reportstore.NewRepo(db)
	uc := bizreport.NewUsecase(repo, gen, func() string { return "rpt-" + t.Name() }).
		WithReadRepo(repo)
	return NewHandler(uc)
}

func newDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := reportstore.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestCreateOneoffTaskAuditsTheFailureHiddenInsideA201 is the load-bearing one
// for 决策 338.
//
// CreateOneoffTaskAndRun persists the task row first and generates the report
// best-effort. So a generation failure returns `(task, err)` — and the handler
// answers **201 Created with a task in the body**. Booking that as success is
// what turns "which tasks produced a report" into a payload scan: the chain
// says someone asked, and says nothing about whether anything came out.
func TestCreateOneoffTaskAuditsTheFailureHiddenInsideA201(t *testing.T) {
	db := newDB(t)
	broken := newHandlerOnDB(t, db, bizreport.NewUnavailableGenerator("LLM provider not configured"))

	rec, ev, set := auditServe(broken, auditReq("POST", "/v1/tasks/oneoff",
		`{"title":"周一晨会","kind":"weekly","timezone":"Asia/Shanghai"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201（任务行确实落了库）", rec.Code)
	}
	var task taskDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.ID == "" {
		t.Fatal("no task id in the response — 没有可记的资源")
	}
	if !set {
		t.Fatal("create oneoff left no audit event")
	}
	if ev.Action != auditport.ActionReportTaskCreate {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionReportTaskCreate)
	}
	// The rows on rerun/delete carry the **bare** uuid (they are keyed by what
	// the path parses to), so create must too — three rows about one task that
	// spell its id three ways is a payload scan.
	if ev.ResourceType != auditport.ResourceReportTask || ev.ResourceID != bareID(task.ID) {
		t.Fatalf("resource = %q/%q, want report_task/%s", ev.ResourceType, ev.ResourceID, bareID(task.ID))
	}
	// The whole point: HTTP said 201, the chain must say failure.
	if ev.Status != auditport.StatusFailure {
		t.Fatalf("status = %q, want failure —— 报表没生成出来，201 只是一个外壳", ev.Status)
	}
	if ev.ErrorMessage == "" || !strings.Contains(ev.ErrorMessage, "LLM provider not configured") {
		t.Errorf("error message = %q, want the generator's reason（链上只剩一个 failure 让人猜是不够的）", ev.ErrorMessage)
	}
	p, _ := ev.Payload.(map[string]any)
	if got, ok := p["report_generated"].(bool); !ok || got {
		t.Errorf("report_generated = %v (present=%v), want false", p["report_generated"], ok)
	}
}

// TestCreateOneoffTaskSuccessIsStillSuccess is the other half: the failure path
// must not be the only one the test can see, or "always failure" passes it.
func TestCreateOneoffTaskSuccessIsStillSuccess(t *testing.T) {
	h, _ := newTestHandler(t)
	rec, ev, set := auditServe(h, auditReq("POST", "/v1/tasks/oneoff",
		`{"title":"周一晨会","kind":"weekly","timezone":"UTC"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("create oneoff left no audit event")
	}
	if ev.Status != auditport.StatusSuccess {
		t.Fatalf("status = %q, want success", ev.Status)
	}
	if ev.ErrorMessage != "" {
		t.Errorf("error message = %q, want empty on the success path", ev.ErrorMessage)
	}
	p, _ := ev.Payload.(map[string]any)
	if got, ok := p["report_generated"].(bool); !ok || !got {
		t.Errorf("report_generated = %v (present=%v), want true", p["report_generated"], ok)
	}
	if ev.ResourceName != "周一晨会" {
		t.Errorf("name = %q, want the task title", ev.ResourceName)
	}
}

// TestRerunOneoffTaskAuditsTheFailureHiddenInsideA200: same in-band error on the
// re-run path. A re-run does **not** overwrite the previous artifact — it
// produces another one — so the chain has to be able to tell three runs from
// one.
func TestRerunOneoffTaskAuditsTheFailureHiddenInsideA200(t *testing.T) {
	db := newDB(t)
	working := newHandlerOnDB(t, db, nil)
	rec, _, _ := auditServe(working, auditReq("POST", "/v1/tasks/oneoff",
		`{"title":"季度复盘","kind":"monthly","timezone":"UTC"}`))
	var task taskDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}

	broken := newHandlerOnDB(t, db, bizreport.NewUnavailableGenerator("provider down"))
	ref := strings.Replace(task.ID, ":", "%3A", 1)
	rec2, ev, set := auditServe(broken, auditReq("POST", "/v1/tasks/"+ref+"/run", ""))
	if rec2.Code != http.StatusOK {
		t.Fatalf("rerun status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	if !set {
		t.Fatal("rerun left no audit event")
	}
	if ev.Action != auditport.ActionReportTaskRerun {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionReportTaskRerun)
	}
	if ev.Status != auditport.StatusFailure {
		t.Fatalf("status = %q, want failure（200 只是一个外壳）", ev.Status)
	}
	if ev.ErrorMessage == "" {
		t.Error("no error message on the rerun failure row")
	}
	if ev.ResourceID != bareID(task.ID) {
		t.Errorf("resource id = %q, want the bare oneoff uuid %s", ev.ResourceID, bareID(task.ID))
	}
	p, _ := ev.Payload.(map[string]any)
	if got, ok := p["report_generated"].(bool); !ok || got {
		t.Errorf("report_generated = %v (present=%v), want false", p["report_generated"], ok)
	}
}

// TestDeleteTaskAuditRowNamesTheTask: after the delete the chain holds only a
// uuid.
func TestDeleteTaskAuditRowNamesTheTask(t *testing.T) {
	h, _ := newTestHandler(t)
	rec, _, _ := auditServe(h, auditReq("POST", "/v1/tasks/oneoff",
		`{"title":"临时排查","kind":"daily","timezone":"UTC"}`))
	var task taskDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	ref := strings.Replace(task.ID, ":", "%3A", 1)

	rec2, ev, set := auditServe(h, auditReq("DELETE", "/v1/tasks/"+ref, ""))
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	if !set {
		t.Fatal("delete task left no audit event")
	}
	if ev.Action != auditport.ActionReportTaskDelete {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionReportTaskDelete)
	}
	if ev.ResourceName != "临时排查" {
		t.Fatalf("row name = %q, want the task title", ev.ResourceName)
	}
	// Deleting the task does NOT delete the reports it produced. Saying so on
	// the row is what stops a reader from concluding the artifacts are gone.
	p, _ := ev.Payload.(map[string]any)
	if got, ok := p["reports_kept"].(bool); !ok || !got {
		t.Errorf("reports_kept = %v (present=%v), want true", p["reports_kept"], ok)
	}
}

// bareID strips the "oneoff:" prefix the DTO carries, matching what the path
// param parses to on the rerun and delete routes.
func bareID(ref string) string {
	if n, ok := strings.CutPrefix(ref, "oneoff:"); ok {
		return n
	}
	return ref
}
