package secret

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
	bizsecret "github.com/vincent-wuhan/opskeeper/core/domains/biz/secret"
	secretmodel "github.com/vincent-wuhan/opskeeper/core/domains/model/secret"
)

// --- 决策 316：凭据库的三条写路由上宿主链 -----------------------------------
//
// 这一组测试存在的理由只有一个：**链上不能出现凭据明文**。其余断言都是
// 为它服务的。凭据库此前不在任何人的视野里（决策 315 才把它列成洞），
// 而一个把明文口令写进 HMAC 签名审计日志的修复，会比不修更糟。

type memRepo struct {
	rows map[uint64]*secretmodel.Secret
	next uint64
}

func newMemRepo() *memRepo { return &memRepo{rows: map[uint64]*secretmodel.Secret{}} }

func (m *memRepo) Create(_ context.Context, s *secretmodel.Secret) error {
	m.next++
	s.ID = m.next
	m.rows[s.ID] = s
	return nil
}

func (m *memRepo) Update(_ context.Context, id uint64, data, description string) error {
	s, ok := m.rows[id]
	if !ok {
		return errs.ErrNotFound
	}
	s.Data = data
	if description != "" {
		s.Description = description
	}
	return nil
}

func (m *memRepo) Delete(_ context.Context, id uint64) error {
	if _, ok := m.rows[id]; !ok {
		return errs.ErrNotFound
	}
	delete(m.rows, id)
	return nil
}

func (m *memRepo) List(_ context.Context) ([]*secretmodel.Secret, error) {
	out := []*secretmodel.Secret{}
	for _, s := range m.rows {
		out = append(out, s)
	}
	return out, nil
}

func (m *memRepo) GetByName(_ context.Context, name string) (*secretmodel.Secret, error) {
	for _, s := range m.rows {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, errs.ErrNotFound
}

func newRouter() (http.Handler, *memRepo) {
	repo := newMemRepo()
	router := chi.NewMux()
	NewHandler(bizsecret.NewUsecase(repo)).Register(router)
	return router, repo
}

func admin() *tenantctx.Tenant {
	t := tenantctx.Tenant{UserID: 7, Role: tenantctx.RoleAdmin}
	t.IsSuperuser = true
	return &t
}

func call(t *testing.T, router http.Handler, method, path, body string, tv *tenantctx.Tenant) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := auditport.WithSlot(req.Context())
	if tv != nil {
		ctx = tenantctx.With(ctx, *tv)
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	ev, set := auditport.GetAuditEvent(req.Context())
	return rec, ev, set
}

// 凭据的每一个字节都不得出现在审计事件里。这条断言把整个事件序列化之后
// 再找明文，而不是逐个字段检查——逐字段检查会漏掉将来新增的字段。
func TestTheChainNeverHoldsTheCredential(t *testing.T) {
	const password = "hunter2-correct-horse-battery"
	const token = "ghp_AAAABBBBCCCCDDDDEEEEFFFF"
	router, _ := newRouter()

	body, err := json.Marshal(map[string]any{
		"name": "tencent-prod", "type": "custom", "description": "prod",
		"fields": map[string]string{"password": password, "token": token},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, ev, set := call(t, router, http.MethodPost, "/v1/secrets", string(body), admin())
	if rec.Code != http.StatusOK {
		t.Fatalf("create: code = %d (%s)", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionSecretCreate {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	blob, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	for _, secretValue := range []string{password, token} {
		if strings.Contains(string(blob), secretValue) {
			t.Fatalf("the audit event carries the credential verbatim: %s", blob)
		}
	}
	// 但字段名必须在——不然这一行只说明「有人动过某个东西」。
	p := ev.Payload.(map[string]any)
	names, _ := p["field_names"].([]string)
	if len(names) != 2 || names[0] != "password" || names[1] != "token" {
		t.Fatalf("field_names = %v, want the two keys sorted", p["field_names"])
	}
	if p["name"] != "tencent-prod" {
		t.Fatalf("name = %v", p["name"])
	}
}

// 摘要存在的唯一理由是回答「轮换过没有」。同样的值必须给出同样的摘要，
// 不同的值必须给出不同的摘要——两个方向都要证，只证一个等于没证。
func TestTheDigestAnswersWasItRotated(t *testing.T) {
	post := func(t *testing.T, fields map[string]string) map[string]any {
		t.Helper()
		router, _ := newRouter()
		body, err := json.Marshal(map[string]any{"name": "c", "fields": fields})
		if err != nil {
			t.Fatal(err)
		}
		rec, ev, _ := call(t, router, http.MethodPost, "/v1/secrets", string(body), admin())
		if rec.Code != http.StatusOK {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		return ev.Payload.(map[string]any)
	}

	same := post(t, map[string]string{"password": "aaa", "user": "root"})
	again := post(t, map[string]string{"user": "root", "password": "aaa"})
	rotated := post(t, map[string]string{"user": "root", "password": "bbb"})

	if same["fields_digest"] != again["fields_digest"] {
		t.Fatalf("map order changed the digest: %v vs %v", same["fields_digest"], again["fields_digest"])
	}
	if same["fields_digest"] == rotated["fields_digest"] {
		t.Fatalf("a rotated credential hashed the same: %v", rotated["fields_digest"])
	}
}

// 分隔符不是为了好看。少了它，{"ab":"c"} 与 {"a":"bc"} 会算出同一个摘要，
// 于是「轮换过」这件事在一次挪动键名之后会静悄悄地消失。
func TestTheDigestRespectsFieldBoundaries(t *testing.T) {
	post := func(t *testing.T, fields map[string]string) string {
		t.Helper()
		router, _ := newRouter()
		body, _ := json.Marshal(map[string]any{"name": "c", "fields": fields})
		rec, ev, _ := call(t, router, http.MethodPost, "/v1/secrets", string(body), admin())
		if rec.Code != http.StatusOK {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		return ev.Payload.(map[string]any)["fields_digest"].(string)
	}
	if post(t, map[string]string{"ab": "c"}) == post(t, map[string]string{"a": "bc"}) {
		t.Fatal("moving a character across the key/value boundary left the digest unchanged")
	}
}

// 改写与新建同样是一次决定：前者让线上服务突然拿到新口令，后者让它们
// 第一次拿到。
func TestUpdateAndDeleteAreAudited(t *testing.T) {
	router, _ := newRouter()
	body, _ := json.Marshal(map[string]any{
		"name": "pg", "fields": map[string]string{"password": "s3cr3t-value"},
	})
	if rec, _, _ := call(t, router, http.MethodPost, "/v1/secrets", string(body), admin()); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d", rec.Code)
	}

	rec, ev, set := call(t, router, http.MethodPut, "/v1/secrets/1",
		`{"fields":{"password":"rotated-value"}}`, admin())
	if rec.Code != http.StatusOK || !set || ev.Action != auditport.ActionSecretUpdate {
		t.Fatalf("update: code=%d ev=%+v set=%v", rec.Code, ev, set)
	}
	if ev.ResourceID != "1" {
		t.Fatalf("resource id = %q, want the credential that moved", ev.ResourceID)
	}
	blob, _ := json.Marshal(ev)
	if strings.Contains(string(blob), "rotated-value") {
		t.Fatalf("the update row carries the new credential: %s", blob)
	}

	rec, ev, set = call(t, router, http.MethodDelete, "/v1/secrets/1", "", admin())
	if rec.Code != http.StatusOK || !set || ev.Action != auditport.ActionSecretDelete {
		t.Fatalf("delete: code=%d ev=%+v set=%v", rec.Code, ev, set)
	}
	if ev.Status != auditport.StatusSuccess || ev.ResourceID != "1" {
		t.Fatalf("delete row = %+v", ev)
	}
}

// 「有人在一直试」与「没人试过」在链上必须长得不一样。
func TestANonAdminWriteLeavesAFailureRow(t *testing.T) {
	router, _ := newRouter()
	user := tenantctx.Tenant{UserID: 3, Role: tenantctx.RoleUser}
	rec, ev, set := call(t, router, http.MethodPost, "/v1/secrets",
		`{"name":"x","fields":{"password":"p"}}`, &user)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if !set || ev.Status != auditport.StatusFailure || ev.Action != auditport.ActionSecretCreate {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
}

// 删一条不存在的凭据也要留痕：有人拿着一个已经失效的 id 反复试，本身
// 就是需要被看见的事。
func TestDeletingAnAbsentCredentialIsAudited(t *testing.T) {
	router, _ := newRouter()
	rec, ev, set := call(t, router, http.MethodDelete, "/v1/secrets/404", "", admin())
	if rec.Code == http.StatusOK {
		t.Fatalf("deleting an absent credential returned 200")
	}
	if !set || ev.Status != auditport.StatusFailure || ev.ResourceID != "404" {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
}
