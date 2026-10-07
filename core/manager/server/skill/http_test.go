package skill_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	svc "github.com/vincent-wuhan/opskeeper/core/manager/biz/skill"
	srvskill "github.com/vincent-wuhan/opskeeper/core/manager/server/skill"
)

// 这个包此前零测试。决策 312 给它补上第一批。
//
// 这条路由**已经有**一份审计——biz/skill 的 GormAuditSink 每次执行都往
// skill_executions 写一行。所以这里要钉住的不是"有没有记录"，而是
// "执行失败"在链上**长什么样"，因为那条路有三种不同的失败形状。

type fakeService struct {
	out     *svc.ExecuteOutput
	err     error
	gotKey  string
	gotEdge uint64
	calls   int
}

func (f *fakeService) List(context.Context, svc.Caller, string) []svc.SkillSummary { return nil }
func (f *fakeService) Get(context.Context, svc.Caller, string) (*svc.SkillSummary, error) {
	return nil, errs.ErrNotFound
}
func (f *fakeService) Execute(_ context.Context, _ svc.Caller, in svc.ExecuteInput) (*svc.ExecuteOutput, error) {
	f.calls++
	f.gotKey, f.gotEdge = in.Key, in.EdgeID
	return f.out, f.err
}

func post(t *testing.T, s *srvskill.Handler, path, body string, role string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	r := chi.NewRouter()
	s.Register(r)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx := auditport.WithSlot(req.Context())
	if role != "" {
		ctx = tenantctx.With(ctx, tenantctx.Tenant{UserID: 7, Role: role})
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	ev, set := auditport.GetAuditEvent(req.Context())
	return rec, ev, set
}

func TestExecuteAuditCarriesWhatActuallyRan(t *testing.T) {
	f := &fakeService{out: &svc.ExecuteOutput{Result: json.RawMessage(`{"ok":true}`)}}
	rec, ev, set := post(t, srvskill.NewHandler(f), "/v1/skills/dmesg_tail/execute",
		`{"edge_id":9,"params":{"lines":200}}`, tenantctx.RoleAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionSkillExecute || ev.Status != auditport.StatusSuccess {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if ev.ResourceType != auditport.ResourceSkill || ev.ResourceID != "dmesg_tail" {
		t.Fatalf("resource = %q/%q", ev.ResourceType, ev.ResourceID)
	}
	p := ev.Payload.(map[string]any)
	if p["edge_id"] != uint64(9) {
		t.Fatalf("edge_id = %v — 不带它就答不出「跑在哪台机器上」", p["edge_id"])
	}
	// params 逐字入链：事故之后要回答的是「跑的到底是什么」，
	// 不是一个字节数。
	if !strings.Contains(p["params"].(string), `"lines":200`) {
		t.Fatalf("params not carried verbatim: %v", p["params"])
	}
}

// 三种失败形状必须分得开：RPC 报错（没跑）、skill 体内报错（跑了但没跑成）、
// 未授权（没跑，且不是这个调用者的错）。
func TestSkillBodyErrorIsAuditedAsFailureDespiteHTTP200(t *testing.T) {
	f := &fakeService{out: &svc.ExecuteOutput{Error: "permission denied reading /var/log"}}
	rec, ev, set := post(t, srvskill.NewHandler(f), "/v1/skills/tail_file/execute",
		`{"edge_id":9}`, tenantctx.RoleAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d — 决策本身成功了，RPC 也是成功的", rec.Code)
	}
	if !set || ev.Status != auditport.StatusFailure {
		t.Fatalf("a failed skill run was recorded as success: %+v (set=%v)", ev, set)
	}
	if ev.Payload.(map[string]any)["skill_error"] != "permission denied reading /var/log" {
		t.Fatalf("the skill's own error is not on the chain: %v", ev.Payload)
	}
}

func TestExecuteFailureIsAudited(t *testing.T) {
	f := &fakeService{err: errs.ErrForbidden}
	rec, ev, set := post(t, srvskill.NewHandler(f), "/v1/skills/restart_service/execute",
		`{"edge_id":9}`, tenantctx.RoleUser)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if !set || ev.Status != auditport.StatusFailure {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if ev.Payload.(map[string]any)["edge_id"] != uint64(9) {
		t.Fatalf("a refused call does not record what was refused: %v", ev.Payload)
	}
}

func TestUnauthenticatedExecuteIsAudited(t *testing.T) {
	f := &fakeService{out: &svc.ExecuteOutput{}}
	rec, ev, set := post(t, srvskill.NewHandler(f), "/v1/skills/dmesg_tail/execute", `{"edge_id":9}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if !set || ev.Status != auditport.StatusFailure {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if f.calls != 0 {
		t.Fatal("an unauthenticated execute reached the service")
	}
}

// skill 不存在是一次尝试，"key 不存在"在链上要能被回答。
func TestUnknownSkillIsAudited(t *testing.T) {
	f := &fakeService{err: errs.ErrNotFound}
	_, ev, set := post(t, srvskill.NewHandler(f), "/v1/skills/nope/execute", `{"edge_id":9}`, tenantctx.RoleAdmin)
	if !set || ev.Status != auditport.StatusFailure || ev.ResourceID != "nope" {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
}

// ensure the fake's unused error import is meaningful: a service that fails
// for a reason unrelated to auth still has to produce a failure row.
func TestServiceErrorIsNotSilentlySuccess(t *testing.T) {
	f := &fakeService{err: errors.New("tunnel closed")}
	_, ev, set := post(t, srvskill.NewHandler(f), "/v1/skills/x/execute", `{"edge_id":9}`, tenantctx.RoleAdmin)
	if !set || ev.Status != auditport.StatusFailure {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
}
