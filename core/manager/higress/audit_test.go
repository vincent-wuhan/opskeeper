package higress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// --- 决策 324：网关三条写路由上宿主链 -----------------------------------------
//
// 这个包此前一个测试文件都没有。三条写路由里最要紧的是登录：**它的失败行是
// 这个进程里唯一一份「有人在猜这个账号」的证据**，而在此之前链上一个字都没有。
//
// 夹具不装中间件，只装槽位。中间件在 cmd/higress-console 里，而**端到端的那
// 一条在 cmd/higress-console/auditsink_test.go**——因为 core/manager 不能 import
// core/domains，把「行真的落库了吗」这个问题问在本模块里是问不出来的。
func newTestServer(t *testing.T) (*Server, *Store) {
	t.Helper()
	store, err := NewStore(filepath(t.TempDir() + "/higress.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv, err := NewServer(Config{
		Store: store, JWTSecret: []byte("test-secret"),
		AdminUser: "admin", AdminPassword: "correct-password",
		CookieName: "_hi_sess", CookieMaxAge: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, store
}

func filepath(p string) string { return p }

func call(t *testing.T, srv *Server, method, path, body string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
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

// 1. 登录失败：这一行是这个进程里唯一的暴力破解证据，所以用户名必须在行上。
// 这条断言是为了钉住一个写错了的顺序——载荷一度建在解码之前，于是每一行都
// 带着空用户名，而一个空的「谁」读起来和「没人试过」完全一样。
func TestGatewayLogin_FailureNamesTheAccount(t *testing.T) {
	srv, _ := newTestServer(t)
	rec, ev, set := call(t, srv, http.MethodPost, "/session/login",
		`{"username":"admin","password":"hunter2"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !set {
		t.Fatal("a failed login wrote no row")
	}
	if ev.Action != auditport.ActionGatewayLogin {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	if ev.ResourceID != "admin" {
		t.Errorf("resource_id = %q, want admin", ev.ResourceID)
	}
	p := payloadOf(t, ev)
	if p["username"] != "admin" {
		t.Errorf("payload username = %v, want admin", p["username"])
	}
	if p["password_present"] != true {
		t.Errorf("password_present = %v, want true", p["password_present"])
	}
	// 口令本身与它的摘要都不在行上。
	for k, v := range p {
		if s, ok := v.(string); ok && strings.Contains(s, "hunter2") {
			t.Errorf("payload[%q] carries the password", k)
		}
	}
	if _, ok := p["password_digest"]; ok {
		t.Error("the row digests a typed password; a chain over that is a grind table")
	}
}

// 2. 登录成功行，且 minted 的会话令牌不进链。
func TestGatewayLogin_SuccessWithoutTheSessionToken(t *testing.T) {
	srv, _ := newTestServer(t)
	rec, ev, set := call(t, srv, http.MethodPost, "/session/login",
		`{"username":"admin","password":"correct-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on the one route that issues a credential")
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q", ev.Status)
	}
	var cookies []*http.Cookie
	for _, c := range rec.Result().Cookies() {
		cookies = append(cookies, c)
	}
	if len(cookies) == 0 || cookies[0].Value == "" {
		t.Fatal("no session cookie in the response — the test is measuring nothing")
	}
	token := cookies[0].Value
	for k, v := range payloadOf(t, ev) {
		if s, ok := v.(string); ok && strings.Contains(s, token) {
			t.Errorf("payload[%q] carries the session token", k)
		}
	}
	if strings.Contains(ev.ErrorMessage, token) {
		t.Error("the token leaked into the error message")
	}
}

// 3. 建 consumer：凭据与它的指纹都不进链。
func TestConsumerCreate_AuditedWithoutTheCredential(t *testing.T) {
	srv, store := newTestServer(t)
	const key = "sk_live_0123456789ABCDEF"
	rec, ev, set := call(t, srv, http.MethodPost, "/admin/consumers",
		`{"name":"opskeeper-worker","apikey":"`+key+`","jwt_required":true,"worker_claim":"worker-1"}`)
	// 没有会话 cookie 时这一路由是 401；带上会话才走得到 handler。
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a session", rec.Code)
	}
	if set {
		t.Error("the session gate wrote a row for the create action; a refused admin op should name itself")
	}

	// 先登录，再建。
	srv2, store2 := newTestServer(t)
	lr, _, _ := call(t, srv2, http.MethodPost, "/session/login",
		`{"username":"admin","password":"correct-password"}`)
	sess := lr.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodPost, "/admin/consumers",
		strings.NewReader(`{"name":"opskeeper-worker","apikey":"`+key+`","jwt_required":true,"worker_claim":"worker-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sess)
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec = httptest.NewRecorder()
	srv2.Routes().ServeHTTP(rec, req)
	ev, set = auditport.GetAuditEvent(req.Context())

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on credential creation")
	}
	if ev.Action != auditport.ActionConsumerCreate {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.ResourceType != auditport.ResourceGatewayConsumer || ev.ResourceID != "opskeeper-worker" {
		t.Errorf("resource = %q / %q", ev.ResourceType, ev.ResourceID)
	}
	p := payloadOf(t, ev)
	if p["worker_claim"] != "worker-1" || p["jwt_required"] != true {
		t.Errorf("payload = %v, want the claims recorded", p)
	}
	for k, v := range p {
		if s, ok := v.(string); ok && strings.Contains(s, key) {
			t.Errorf("payload[%q] carries the apikey", k)
		}
	}
	// 存储里那个查找用的指纹也不进链：它是 bearer 凭据的一半。
	fp := Fingerprint(key)
	for k, v := range p {
		if s, ok := v.(string); ok && strings.Contains(s, fp) {
			t.Errorf("payload[%q] carries the apikey fingerprint", k)
		}
	}
	// 而 consumer 真的建成了。
	if _, err := store2.Get(t.Context(), "opskeeper-worker"); err != nil {
		t.Errorf("the consumer was not actually created: %v", err)
	}
	_ = store
}

// 4. 删 consumer：带上被删掉的那条访问路径的 claims。
func TestConsumerDelete_AuditsTheRevokedPath(t *testing.T) {
	srv, _ := newTestServer(t)
	lr, _, _ := call(t, srv, http.MethodPost, "/session/login",
		`{"username":"admin","password":"correct-password"}`)
	sess := lr.Result().Cookies()[0]

	cr := httptest.NewRequest(http.MethodPost, "/admin/consumers",
		strings.NewReader(`{"name":"opskeeper-worker","apikey":"sk_live_XYZ","worker_claim":"worker-1","tenant_claim":"tenant-a"}`))
	cr.Header.Set("Content-Type", "application/json")
	cr.AddCookie(sess)
	crrec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(crrec, cr)
	if crrec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", crrec.Code, crrec.Body.String())
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/consumers/opskeeper-worker", nil)
	req.AddCookie(sess)
	req = req.WithContext(auditport.WithSlot(req.Context()))
	// chi 需要路由参数，Delete 用的是 {name}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "opskeeper-worker")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	ev, set := auditport.GetAuditEvent(req.Context())
	if !set {
		t.Fatal("no audit row on a revoked access path")
	}
	if ev.Action != auditport.ActionConsumerDelete {
		t.Errorf("action = %q", ev.Action)
	}
	p := payloadOf(t, ev)
	if p["worker_claim"] != "worker-1" || p["tenant_claim"] != "tenant-a" {
		t.Errorf("payload = %v, want the before-image of the revoked path", p)
	}
	if p["already_gone"] != false {
		t.Errorf("already_gone = %v, want false", p["already_gone"])
	}
}

// 5. 回归：一个手工构造的 Consumer（ApikeyHash 为空）渲染视图不许 panic。
// 决策 324 之前 viewOf 直接 `c.ApikeyHash[:16]`，而 Store.Create 按值接收
// consumer 并把指纹盖在自己的副本上——于是每一次成功的 create 都在响应渲染
// 时越界。这是这个包历史上第一个测试，而它找到的是一个每次调用都崩的缺陷。
func TestViewOfAShortFingerprintDoesNotPanic(t *testing.T) {
	for _, tc := range []struct{ name, hash string }{
		{"empty", ""},
		{"one char", "a"},
		{"exactly sixteen", "0123456789abcdef"},
		{"a real one", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	} {
		v := viewOf(Consumer{Name: "n", ApikeyHash: tc.hash})
		got, _ := v["apikey_hash"].(string)
		switch {
		case tc.hash == "" && got != "":
			t.Errorf("%s: hash = %q, want empty", tc.name, got)
		case len(tc.hash) <= 16 && got != tc.hash:
			t.Errorf("%s: hash = %q, want %q", tc.name, got, tc.hash)
		case len(tc.hash) > 16 && got != tc.hash[:16]+"…":
			t.Errorf("%s: hash = %q, want the 16-char prefix", tc.name, got)
		}
	}
}

// 6. 同一个进程里可以建两个 Server：Prometheus 的默认注册表是进程级的，
// MustRegister 遇到第二个就 panic，于是这个构造函数此前只能被调用一次。
func TestTwoServersInOneProcessShareTheRegisteredMetrics(t *testing.T) {
	first, _ := newTestServer(t)
	second, _ := newTestServer(t)
	if second.metrics.resolveMiss != first.metrics.resolveMiss {
		t.Error("the second server increments a counter nobody scrapes")
	}
	if second.metrics.adminOps != first.metrics.adminOps {
		t.Error("the second server's admin counters go nowhere")
	}
}
