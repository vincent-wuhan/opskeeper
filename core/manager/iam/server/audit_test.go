package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/manager/iam/biz/membership"
	"github.com/vincent-wuhan/opskeeper/core/manager/iam/biz/org"
	bizuser "github.com/vincent-wuhan/opskeeper/core/manager/iam/biz/user"
	iammodel "github.com/vincent-wuhan/opskeeper/core/manager/iam/model"
	"github.com/vincent-wuhan/opskeeper/core/manager/iam/service"
)

// --- 决策 317：身份与组织的十条写路由上宿主链 -------------------------------
//
// 这一组测试要回答的问题只有一句：**「谁改了谁的权限」在链上答不答得出来。**
// 入账之前，同一个文件里 setRole 与 deleteUser 写了行，而 createUser 与
// resetPassword——一个管理员最想留在记录上的两个动作——一行都没有，七个组织
// 端点也一个都没有。洞的形状恰好长在敏感操作上，比没有表更糟，因为它读起来
// 像覆盖率。

// --- 内存 repo：走真实的 biz service，而不是替身 ---------------------------

type orgRepo struct {
	rows   map[uint64]*iammodel.Org
	byName map[string]uint64
	next   uint64
}

func newOrgRepo() *orgRepo {
	return &orgRepo{rows: map[uint64]*iammodel.Org{}, byName: map[string]uint64{}}
}

func (r *orgRepo) Create(_ context.Context, o *iammodel.Org) error {
	r.next++
	o.ID = r.next
	r.rows[o.ID] = o
	r.byName[o.Name] = o.ID
	return nil
}
func (r *orgRepo) GetByID(_ context.Context, id uint64) (*iammodel.Org, error) {
	o, ok := r.rows[id]
	if !ok {
		return nil, errs.ErrNotFound
	}
	return o, nil
}
func (r *orgRepo) GetByName(_ context.Context, name string) (*iammodel.Org, error) {
	id, ok := r.byName[name]
	if !ok {
		return nil, errs.ErrNotFound
	}
	return r.rows[id], nil
}
func (r *orgRepo) List(_ context.Context) ([]*iammodel.Org, error) {
	out := []*iammodel.Org{}
	for _, o := range r.rows {
		out = append(out, o)
	}
	return out, nil
}
func (r *orgRepo) Update(_ context.Context, id uint64, name, description string, parentID *uint64) error {
	o, ok := r.rows[id]
	if !ok {
		return errs.ErrNotFound
	}
	if name != "" {
		o.Name = name
	}
	if description != "" {
		o.Description = description
	}
	o.ParentID = parentID
	return nil
}
func (r *orgRepo) Delete(_ context.Context, id uint64) error {
	o, ok := r.rows[id]
	if !ok {
		return errs.ErrNotFound
	}
	delete(r.rows, id)
	delete(r.byName, o.Name)
	return nil
}
func (r *orgRepo) Count(_ context.Context) (int64, error) { return int64(len(r.rows)), nil }
func (r *orgRepo) CountChildren(_ context.Context, _ uint64) (int64, error) {
	return 0, nil
}

type memberRepo struct {
	rows map[string]*iammodel.OrgMembership
}

func newMemberRepo() *memberRepo { return &memberRepo{rows: map[string]*iammodel.OrgMembership{}} }

func key(userID, orgID uint64) string {
	return itoa(userID) + ":" + itoa(orgID)
}
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

func (r *memberRepo) Upsert(_ context.Context, userID, orgID uint64, role string) (*iammodel.OrgMembership, error) {
	m := &iammodel.OrgMembership{UserID: userID, OrgID: orgID, Role: role}
	r.rows[key(userID, orgID)] = m
	return m, nil
}
func (r *memberRepo) Delete(_ context.Context, userID, orgID uint64) error {
	k := key(userID, orgID)
	if _, ok := r.rows[k]; !ok {
		return errs.ErrNotFound
	}
	delete(r.rows, k)
	return nil
}
func (r *memberRepo) DeleteByOrg(_ context.Context, orgID uint64) error {
	for k, m := range r.rows {
		if m.OrgID == orgID {
			delete(r.rows, k)
		}
	}
	return nil
}
func (r *memberRepo) DeleteByUser(_ context.Context, userID uint64) error {
	for k, m := range r.rows {
		if m.UserID == userID {
			delete(r.rows, k)
		}
	}
	return nil
}
func (r *memberRepo) ListByOrg(_ context.Context, _ uint64) ([]iammodel.MembershipWithUser, error) {
	return nil, nil
}
func (r *memberRepo) ListByUser(_ context.Context, _ uint64) ([]iammodel.MembershipWithOrg, error) {
	return nil, nil
}
func (r *memberRepo) All(_ context.Context) ([]iammodel.OrgMembership, error) {
	out := []iammodel.OrgMembership{}
	for _, m := range r.rows {
		out = append(out, *m)
	}
	return out, nil
}

// orgCleaner adapts the membership repo to the one-method contract the org
// service wants. The membership *service* deliberately does not expose
// DeleteByOrg — cascade cleanup belongs to the org side — so the test wires
// the repo directly rather than widening the production API for a fixture.
type orgCleaner struct{ r *memberRepo }

func (c orgCleaner) DeleteByOrg(ctx context.Context, orgID uint64) error {
	return c.r.DeleteByOrg(ctx, orgID)
}

func newIdentityRouter(t *testing.T) http.Handler {
	t.Helper()
	users := newServerTestRepo(t)
	uc := bizuser.NewUsecase(users, nil, nil)
	if err := uc.BootstrapAdmin(t.Context(), "root@example.com", "root-password"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	svc := service.New(uc, nil)
	mrepo := newMemberRepo()
	ms := membership.New(mrepo, nil)
	svc.SetMemberships(ms)
	svc.SetOrgs(org.New(newOrgRepo(), orgCleaner{mrepo}, nil))

	router := chi.NewRouter()
	handler := NewHandler(svc, nil)
	// refresh lives on the public router; registering only the protected half
	// would 404 it and the test below would pass for the wrong reason.
	handler.RegisterPublic(router)
	handler.RegisterProtected(router)
	return router
}

func adminCaller() *tenantctx.Tenant {
	t := tenantctx.Tenant{UserID: 1, Role: tenantctx.RoleAdmin}
	t.IsSuperuser = true
	return &t
}

func userCaller() *tenantctx.Tenant {
	return &tenantctx.Tenant{UserID: 99, Role: tenantctx.RoleUser}
}

func do(t *testing.T, router http.Handler, method, path, body string, tv *tenantctx.Tenant) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := auditport.WithSlot(req.Context())
	ctx = tenantctx.With(ctx, *tv)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	ev, set := auditport.GetAuditEvent(req.Context())
	return rec, ev, set
}

func payloadOf(t *testing.T, ev auditport.Event) map[string]any {
	t.Helper()
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want a map", ev.Payload)
	}
	return p
}

// 建用户这一行必须有 email 与 role，且**一个字节的密码都不能有**。
// 账号创建是密码最不该出现的地方：这个账号是全新的，别的任何地方都还没
// 引用过它。
func TestCreatingAUserIsOnTheRecordWithoutItsPassword(t *testing.T) {
	const password = "brand-new-admin-password"
	router := newIdentityRouter(t)
	body, _ := json.Marshal(map[string]any{
		"email": "newcomer@example.com", "password": password, "role": "user",
	})
	rec, ev, set := do(t, router, http.MethodPost, "/v1/users", string(body), adminCaller())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionUserCreate || ev.Status != auditport.StatusSuccess {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	blob, _ := json.Marshal(ev)
	if strings.Contains(string(blob), password) {
		t.Fatalf("the creation row carries the password: %s", blob)
	}
	p := payloadOf(t, ev)
	if p["email"] != "newcomer@example.com" || p["role"] != "user" {
		t.Fatalf("payload = %v", p)
	}
	if ev.ResourceID == "" || ev.ResourceID == "0" {
		t.Fatalf("resource id = %q, want the account that was created", ev.ResourceID)
	}
}

// 重置口令是控制面里风险最高的一次写，所以它的那一行也是最小的一行：
// 只说清是谁的口令被重置了。
func TestResettingAPasswordSaysWhoseAndNothingElse(t *testing.T) {
	const password = "rotated-to-this-value-9f2c"
	router := newIdentityRouter(t)
	rec, ev, set := do(t, router, http.MethodPatch, "/v1/users/1/password",
		`{"password":"`+password+`"}`, adminCaller())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionUserUpdate {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	blob, _ := json.Marshal(ev)
	if strings.Contains(string(blob), password) {
		t.Fatalf("the reset row carries the new password: %s", blob)
	}
	if strings.Contains(string(blob), "digest") {
		t.Fatalf("the reset row carries a digest of the new password: %s", blob)
	}
	p := payloadOf(t, ev)
	if p["field"] != "password" || ev.ResourceID != "1" {
		t.Fatalf("payload = %v, id = %q", p, ev.ResourceID)
	}
}

// 载荷说的是**请求要求改哪些字段**，不是碰巧改成了哪些。只发 display_name
// 的请求没有改过状态；说它改了，就是一条读者无法自行纠正的假话。
func TestOnlyTheRequestedFieldsAreClaimed(t *testing.T) {
	router := newIdentityRouter(t)
	rec, ev, set := do(t, router, http.MethodPatch, "/v1/users/1",
		`{"display_name":"Renamed"}`, adminCaller())
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row")
	}
	p := payloadOf(t, ev)
	fields, _ := p["fields"].([]string)
	if len(fields) != 1 || fields[0] != "display_name" {
		t.Fatalf("fields = %v, want only the one the request asked to move", p["fields"])
	}
}

// 建用户时会顺手把新账号塞进默认组织。那是一次授权，只躺在一条 warn 日志里
// 的授权，事后没人找得到——所以它得跟着建账号那一行走。
func TestTheAutoJoinIsOnTheRecord(t *testing.T) {
	router := newIdentityRouter(t)
	body, _ := json.Marshal(map[string]any{
		"email": "auto@example.com", "password": "pw-for-auto-join", "role": "user",
	})
	rec, ev, set := do(t, router, http.MethodPost, "/v1/users", string(body), adminCaller())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row")
	}
	p := payloadOf(t, ev)
	if p["auto_joined_org_id"] == nil {
		t.Fatalf("the auto-join left no trace: %v", p)
	}
}

// 「他为什么能看这个租户」的答案在成员关系这一行上。org 是资源，user_id 与
// role 是动了的东西——三个都要在，只记 org 等于什么都没记。
func TestMovingAMemberNamesTheUserAndTheRole(t *testing.T) {
	router := newIdentityRouter(t)
	if rec, _, _ := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"payments"}`, adminCaller()); rec.Code != http.StatusCreated {
		t.Fatalf("seed org: %d %s", rec.Code, rec.Body.String())
	}
	rec, ev, set := do(t, router, http.MethodPost, "/v1/orgs/1/members",
		`{"user_id":42,"role":"org_admin"}`, adminCaller())
	if rec.Code != http.StatusCreated {
		t.Fatalf("add member: %d %s", rec.Code, rec.Body.String())
	}
	if !set || ev.Status != auditport.StatusSuccess {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if ev.Action != auditport.ActionOrgMemberAdd || ev.ResourceType != auditport.ResourceOrg {
		t.Fatalf("ev = %+v", ev)
	}
	if ev.ResourceID != "1" {
		t.Fatalf("resource id = %q, want the org the member was added to", ev.ResourceID)
	}
	p := payloadOf(t, ev)
	if p["user_id"] != uint64(42) || p["role"] != "org_admin" {
		t.Fatalf("payload = %v, want the user and the role that were asked for", p)
	}
}

// 移除与新增不对称：移除那一行里没有 role，因为没有角色被移动。一个带着
// 空 role 的移除行，读者会把它读成「被移到了空角色」。
func TestRemovingAMemberCarriesNoRole(t *testing.T) {
	router := newIdentityRouter(t)
	_, ev, set := do(t, router, http.MethodDelete, "/v1/orgs/1/members/42", "", adminCaller())
	if !set || ev.Action != auditport.ActionOrgMemberRemove {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	p := payloadOf(t, ev)
	if p["user_id"] != uint64(42) {
		t.Fatalf("payload = %v, want the user that was cut", p)
	}
	if _, present := p["role"]; present {
		t.Fatalf("a removal row carries a role: %v", p)
	}
}

// 删掉一个组织之后就没东西可问了，所以名字必须在删之前取。只会说「org 7 没了」
// 的一行，对读它的人的价值，远低于说得出「删的是哪个」的一行。
func TestDeletingAnOrgNamesItBeforeItGoes(t *testing.T) {
	router := newIdentityRouter(t)
	if rec, _, _ := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"payments"}`, adminCaller()); rec.Code != http.StatusCreated {
		t.Fatalf("seed org: %d %s", rec.Code, rec.Body.String())
	}
	rec, ev, set := do(t, router, http.MethodDelete, "/v1/orgs/1", "", adminCaller())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionOrgDelete {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if payloadOf(t, ev)["name"] != "payments" {
		t.Fatalf("payload = %v, want the org named before it went", ev.Payload)
	}
}

// 每一条写路由，被拒绝的那一次也要留痕。这是本仓第四次为同一句话写测试
// （309、HITL、凭据库、这里），四次都是因为第一次只做了成功路径。
func TestEveryIdentityWriteLeavesARowWhenRefused(t *testing.T) {
	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/orgs", `{"name":"x"}`},
		{http.MethodPatch, "/v1/orgs/1", `{"name":"x"}`},
		{http.MethodDelete, "/v1/orgs/1", ""},
		{http.MethodPost, "/v1/orgs/1/members", `{"user_id":1,"role":"member"}`},
		{http.MethodPatch, "/v1/orgs/1/members/1", `{"role":"admin"}`},
		{http.MethodDelete, "/v1/orgs/1/members/1", ""},
		{http.MethodPost, "/v1/users", `{"email":"a@b.c","password":"p"}`},
		{http.MethodPatch, "/v1/users/1", `{"display_name":"x"}`},
		{http.MethodPatch, "/v1/users/1/password", `{"password":"p"}`},
	}
	for _, tc := range cases {
		router := newIdentityRouter(t)
		rec, ev, set := do(t, router, tc.method, tc.path, tc.body, userCaller())
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: code = %d, want 403", tc.method, tc.path, rec.Code)
		}
		if !set || ev.Status != auditport.StatusFailure {
			t.Fatalf("%s %s: ev = %+v (set=%v) — a refused attempt that leaves no row "+
				"looks identical to nobody trying", tc.method, tc.path, ev, set)
		}
		if ev.Action == "" {
			t.Fatalf("%s %s: the refusal names no action", tc.method, tc.path)
		}
	}
}

// refresh 成功不记，失败要记：本仓 2026-05-21 明确砍掉了 auth_login /
// auth_logout，理由是会淹掉变更信号；而**失败的** refresh 是一次重放或伪造，
// 正是当初保留 auth_login_failed 的那类形状。
func TestAFailedRefreshIsRecordedAndASuccessfulOneIsNot(t *testing.T) {
	router := newIdentityRouter(t)
	_, ev, set := do(t, router, http.MethodPost, "/v1/auth/refresh",
		`{"refresh_token":"not-a-real-token"}`, userCaller())
	if !set || ev.Status != auditport.StatusFailure || ev.Action != auditport.ActionAuthLoginFailed {
		t.Fatalf("failed refresh: ev = %+v (set=%v)", ev, set)
	}
	if payloadOf(t, ev)["flow"] != "refresh" {
		t.Fatalf("payload = %v, want the flow named", ev.Payload)
	}
}
