package main

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
)

// --- 决策 323：托管页面两条写路由 -------------------------------------------
//
// 这两条此前在裁决表里是「连一个能指认的函数都没有」——它们是就地闭包，闸门只
// 看得见 `func` 这个名字。这一刀先把它们变成具名 handler（否则没法判定），再
// 上链。**改代码形状在补守卫之前**，顺序反过来会让闸门指着一个不存在的名字。

type fakePageStore struct {
	deleted   []string
	readErr   error
	deleteErr error
	reads     int
}

func (f *fakePageStore) Delete(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakePageStore) readPageHTML(string) ([]byte, error) {
	f.reads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	return []byte("<html>incident</html>"), nil
}

// pageRouter mounts the handler under both route shapes and lets chi pick the
// right one, which is what the production router does too.
func pageRouter(h http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.MethodFunc(http.MethodDelete, "/v1/pages/{id}", h)
	r.MethodFunc(http.MethodPost, "/v1/pages/{id}/share", h)
	return r
}

// serve 发一次请求并把行读回来。槽位由宿主中间件装，测试自己装。
func serve(t *testing.T, h http.HandlerFunc, method, path string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	pageRouter(h).ServeHTTP(rec, req)
	ev, set := auditport.GetAuditEvent(req.Context())
	return rec, ev, set
}

// 1. delete 成功行。
func TestDeleteHostedPage_Audited(t *testing.T) {
	store := &fakePageStore{}
	rec, ev, set := serve(t, deleteHostedPage(store), http.MethodDelete, "/v1/pages/abc123")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if !set {
		t.Fatal("no audit row on a destructive route")
	}
	if ev.Action != auditport.ActionPageDelete {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.ResourceType != auditport.ResourceHostedPage || ev.ResourceID != "abc123" {
		t.Errorf("resource = %q / %q", ev.ResourceType, ev.ResourceID)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q", ev.Status)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "abc123" {
		t.Errorf("store deleted %v", store.deleted)
	}
}

// 2. delete 失败也要有行。
func TestDeleteHostedPage_FailureAudited(t *testing.T) {
	store := &fakePageStore{deleteErr: errors.New("no such page")}
	rec, ev, set := serve(t, deleteHostedPage(store), http.MethodDelete, "/v1/pages/gone")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !set {
		t.Fatal("a failed delete wrote no row")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	if ev.ErrorMessage == "" {
		t.Error("failure row carries no error message")
	}
}

// 3. share 成功行，且** minted 的令牌一个字都不进链**。这一条是本刀最要紧的：
// share 把一个只有登录用户能读的页面变成任何人拿到 URL 就能读三十天，令牌
// 本身就是那把钥匙。
func TestShareHostedPage_AuditedWithoutTheToken(t *testing.T) {
	store := &fakePageStore{}
	rec, ev, set := serve(t, shareHostedPage(store, "test-secret"), http.MethodPost, "/v1/pages/abc123/share")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on an outbound-facing action")
	}
	if ev.Action != auditport.ActionPageShare {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.ResourceID != "abc123" {
		t.Errorf("resource_id = %q", ev.ResourceID)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q", ev.Status)
	}
	var body struct {
		ShareToken string `json:"share_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if body.ShareToken == "" {
		t.Fatal("no token in the response — the test is measuring nothing")
	}
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T", ev.Payload)
	}
	if p["expires_at"] == nil {
		t.Error("no expiry on the row")
	}
	for k, v := range p {
		if s, ok := v.(string); ok && strings.Contains(s, body.ShareToken) {
			t.Errorf("payload[%q] carries the minted token", k)
		}
	}
	if strings.Contains(ev.ErrorMessage, body.ShareToken) {
		t.Error("the token leaked into the error message")
	}
}

// 4. 页面不存在时不签发，也要有行——「试图分享一个不存在的页面」是一条事实。
func TestShareHostedPage_MissingPageAudited(t *testing.T) {
	store := &fakePageStore{readErr: errors.New("no such page")}
	rec, ev, set := serve(t, shareHostedPage(store, "test-secret"), http.MethodPost, "/v1/pages/ghost/share")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !set {
		t.Fatal("a refused share wrote no row")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
}

// 5. 响应形状没被审计改坏。
func TestShareHostedPage_ResponseUnchanged(t *testing.T) {
	store := &fakePageStore{}
	rec, _, _ := serve(t, shareHostedPage(store, "test-secret"), http.MethodPost, "/v1/pages/abc123/share")
	if ct := rec.Header().Get("content-type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	for _, want := range []string{`"share_token"`, `"path"`, `"expires_at"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body is missing %s: %s", want, rec.Body.String())
		}
	}
	if store.reads != 1 {
		t.Errorf("the page was read %d times, want 1", store.reads)
	}
}
