package approval_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	store "github.com/vincent-wuhan/opskeeper/core/manager/data/approval/store"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/approval"
	srvapproval "github.com/vincent-wuhan/opskeeper/core/manager/server/approval"
)

// Why this file exists
// -------------------
// 决策 309 给审批面补上审计，而审批面此前是 `server/approval` 与
// `biz/approval` **两个零测试的包**——HITL 闸门整个没有直接测试。
// 那意味着下面这些断言不是"给已有行为加保险"，而是第一次把
// 「批准会不会真的把命令跑出去」「跑挂了链上长什么样」
// 这两件事变成可执行的事实。

func newUC(t *testing.T) *bizapproval.Usecase {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return bizapproval.NewUsecase(store.NewRepo(db), slog.Default())
}

// call drives one request through the real chi router with both context
// values the production chain installs: the caller identity (auth
// middleware) and the audit slot (audit middleware). It hands back the
// recorder, the request (so the test can read the slot back off the
// handler's context) and the stashed event.
func call(t *testing.T, h *srvapproval.Handler, method, path string, role string, userID uint64, body string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	r := chi.NewRouter()
	h.Register(r)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	// 槽先装，身份后装：生产里顺序相反也没关系——SetAuditEvent 只认 *slot，
	// 中间任何 r.WithContext 包装都不会把它弄丢（这正是 port.go 里记的那个坑）。
	ctx := auditport.WithSlot(req.Context())
	ctx = tenantctx.With(ctx, tenantctx.Tenant{UserID: userID, Role: role})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	ev, set := auditport.GetAuditEvent(req.Context())
	return rec, ev, set
}

func propose(t *testing.T, uc *bizapproval.Usecase, kind string) string {
	t.Helper()
	a, err := uc.Propose(context.Background(), bizapproval.ProposeInput{
		Kind: kind, Title: "kubectl delete pod api-7f", Source: model.SourceAgent, SessionID: "sess-1",
		Payload: map[string]any{"cmd": "kubectl delete pod api-7f", "ns": "prod"},
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return a.ID
}

// TestApproveAuditsTheCommandItLetLoose is the load-bearing one:
// 载荷里必须有那条命令的**逐词内容**，而不是"批准了 3 号提案"。
func TestApproveAuditsTheCommandItLetLoose(t *testing.T) {
	uc := newUC(t)
	id := propose(t, uc, "shell_command")
	uc.RegisterExecutor("shell_command", func(_ context.Context, payload string) (string, error) {
		return `{"stdout":"deleted"}`, nil
	})

	rec, ev, set := call(t, srvapproval.NewHandler(uc), http.MethodPost,
		"/v1/approvals/"+id+"/approve", tenantctx.RoleAdmin, 7, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if !set {
		t.Fatal("approve left no audit event")
	}
	if ev.Action != auditport.ActionApprovalApprove {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionApprovalApprove)
	}
	if ev.ResourceType != auditport.ResourceApproval || ev.ResourceID != id {
		t.Fatalf("resource = %q/%q, want approval/%s", ev.ResourceType, ev.ResourceID, id)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Fatalf("status = %q, want success", ev.Status)
	}
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map", ev.Payload)
	}
	if got := p["payload"]; !strings.Contains(got.(string), "kubectl delete pod api-7f") {
		t.Fatalf("payload does not carry the command verbatim: %v", got)
	}
	if p["kind"] != "shell_command" || p["result_status"] != model.StatusExecuted {
		t.Fatalf("kind/result_status = %v/%v, want shell_command/executed", p["kind"], p["result_status"])
	}
	if p["session_id"] != "sess-1" {
		t.Fatalf("session_id = %v, want sess-1 (没有它就无法把批准和那次会话对上)", p["session_id"])
	}
}

// TestApproveExecutorErrorIsAuditedAsFailureDespiteHTTP200 is the reason
// this decision exists. The HTTP surface reports the *decision*, and the
// decision succeeded; but the thing the operator meant by clicking —
// "run this command" — did not. Bucketing that as success is what turns
// "which approvals didn't run" into a payload scan.
func TestApproveExecutorErrorIsAuditedAsFailureDespiteHTTP200(t *testing.T) {
	uc := newUC(t)
	id := propose(t, uc, "shell_command")
	uc.RegisterExecutor("shell_command", func(_ context.Context, _ string) (string, error) {
		return "", errors.New("node unreachable")
	})

	rec, ev, set := call(t, srvapproval.NewHandler(uc), http.MethodPost,
		"/v1/approvals/"+id+"/approve", tenantctx.RoleAdmin, 7, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 — 决策本身成功了", rec.Code)
	}
	if !set {
		t.Fatal("a failed execution left no audit event at all")
	}
	if ev.Status != auditport.StatusFailure {
		t.Fatalf("status = %q, want failure despite HTTP 200", ev.Status)
	}
	p := ev.Payload.(map[string]any)
	if p["result_status"] != model.StatusFailed {
		t.Fatalf("result_status = %v, want failed", p["result_status"])
	}
	// 执行结果本体也要在链上可读，不能只留一个 failed 状态词。
	row, _ := uc.Get(context.Background(), id)
	if row == nil || row.ResultJSON == nil || !strings.Contains(*row.ResultJSON, "node unreachable") {
		t.Fatalf("executor error not recorded on the row: %+v", row)
	}
}

func TestRejectAuditsThePayloadItRefused(t *testing.T) {
	uc := newUC(t)
	id := propose(t, uc, "shell_command")
	called := false
	uc.RegisterExecutor("shell_command", func(_ context.Context, _ string) (string, error) {
		called = true
		return "{}", nil
	})

	rec, ev, set := call(t, srvapproval.NewHandler(uc), http.MethodPost,
		"/v1/approvals/"+id+"/reject", tenantctx.RoleAdmin, 9,
		`{"reason":"this is a prod pod"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if called {
		t.Fatal("reject executed the action — 驳回必须是零执行")
	}
	if !set || ev.Action != auditport.ActionApprovalReject || ev.Status != auditport.StatusSuccess {
		t.Fatalf("ev = %+v (set=%v), want a successful approval_reject", ev, set)
	}
	if got := ev.Payload.(map[string]any)["payload"]; !strings.Contains(got.(string), "kubectl delete pod") {
		t.Fatalf("reject did not record the payload it refused: %v", got)
	}
	row, _ := uc.Get(context.Background(), id)
	if row == nil || row.Status != model.StatusRejected {
		t.Fatalf("status = %+v, want rejected", row)
	}
	if row.Reason == nil || *row.Reason != "this is a prod pod" {
		t.Fatalf("reason not stored: %+v", row.Reason)
	}
}

// TestNonAdminApproveAttemptIsAudited: "有人一直在试" and "没人试过"
// 在链上原本长得一模一样 —— 因为 403 那条路直接 return 了。
func TestNonAdminApproveAttemptIsAudited(t *testing.T) {
	uc := newUC(t)
	id := propose(t, uc, "shell_command")

	rec, ev, set := call(t, srvapproval.NewHandler(uc), http.MethodPost,
		"/v1/approvals/"+id+"/approve", tenantctx.RoleViewer, 42, "")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if !set {
		t.Fatal("a non-admin approve attempt left no trace")
	}
	if ev.Status != auditport.StatusFailure {
		t.Fatalf("status = %q, want failure", ev.Status)
	}
	if !strings.Contains(ev.ErrorMessage, "not an admin") {
		t.Fatalf("error = %q, want it to say why", ev.ErrorMessage)
	}
	row, _ := uc.Get(context.Background(), id)
	if row.Status != model.StatusPending {
		t.Fatalf("status = %q, want the row still pending", row.Status)
	}
}

func TestApproveUnknownIDIsAudited(t *testing.T) {
	uc := newUC(t)
	rec, ev, set := call(t, srvapproval.NewHandler(uc), http.MethodPost,
		"/v1/approvals/does-not-exist/approve", tenantctx.RoleAdmin, 7, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
	if !set || ev.Status != auditport.StatusFailure || ev.ResourceID != "does-not-exist" {
		t.Fatalf("ev = %+v (set=%v), want a failure row for the missing id", ev, set)
	}
}

// TestApproveIsSingleUse 双重批准必须有迹可循：第二次尝试是一条
// failure 审计行，而不是又一次静默成功。
func TestApproveIsSingleUse(t *testing.T) {
	uc := newUC(t)
	id := propose(t, uc, "shell_command")
	h := srvapproval.NewHandler(uc)

	if rec, _, _ := call(t, h, http.MethodPost, "/v1/approvals/"+id+"/approve", tenantctx.RoleAdmin, 7, ""); rec.Code != http.StatusOK {
		t.Fatalf("first approve: code = %d", rec.Code)
	}
	rec, ev, set := call(t, h, http.MethodPost, "/v1/approvals/"+id+"/approve", tenantctx.RoleAdmin, 8, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second approve: code = %d, want 404 (Decide 只匹配 pending 行)", rec.Code)
	}
	if !set || ev.Status != auditport.StatusFailure {
		t.Fatalf("second approve left no failure row: %+v (set=%v)", ev, set)
	}
}

// TestNoExecutorStillAudits: 没有注册 executor 时 Approve 只标记不执行
// （Usecase 里那条 Warn）。这仍然是**一次批准**，必须入账。
func TestNoExecutorStillAudits(t *testing.T) {
	uc := newUC(t)
	id := propose(t, uc, "unregistered_kind")
	rec, ev, set := call(t, srvapproval.NewHandler(uc), http.MethodPost,
		"/v1/approvals/"+id+"/approve", tenantctx.RoleAdmin, 7, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !set || ev.Status != auditport.StatusSuccess {
		t.Fatalf("ev = %+v (set=%v), want success", ev, set)
	}
	if ev.Payload.(map[string]any)["result_status"] != model.StatusApproved {
		t.Fatalf("result_status = %v, want approved", ev.Payload.(map[string]any)["result_status"])
	}
}
