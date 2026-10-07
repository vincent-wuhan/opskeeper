package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	biz "github.com/vincent-wuhan/opskeeper/core/domains/biz/monitor"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/monitor"
)

// --- 决策 322：面板三条写路由上宿主链 -----------------------------------------
//
// 这是 core/domains/server/monitor 的**第一个**测试文件：这个包此前既没有
// 守卫也没有测试，所以三条写路由从未被验证过任何东西——不只是审计。夹具因此
// 从零写，代价是这个包第一次被 go test 跑过时它已经有了一整套断言而不是一个
// 冒烟。
//
// 三条路由里最要紧的是 delete：删掉一个面板之后，板子上什么痕迹都不留。

// fakePanels 是 PanelService 的内存实现。
type fakePanels struct {
	panels map[uint64]*model.Panel
	next   uint64

	createErr error
	updateErr error
	deleteErr error
	getErr    error
	// calls 记下每个方法被调过几次，用来断言「delete 之前真的读了」。
	calls map[string]int
}

func newFakePanels() *fakePanels {
	return &fakePanels{panels: map[uint64]*model.Panel{}, next: 100, calls: map[string]int{}}
}

func (f *fakePanels) List(context.Context) ([]*model.Panel, error) {
	f.calls["List"]++
	return nil, nil
}

func (f *fakePanels) Get(_ context.Context, id uint64) (*model.Panel, error) {
	f.calls["Get"]++
	if f.getErr != nil {
		return nil, f.getErr
	}
	p, ok := f.panels[id]
	if !ok {
		return nil, errs.ErrNotFound
	}
	return p, nil
}

func (f *fakePanels) Create(_ context.Context, in biz.CreateInput) (*model.Panel, error) {
	f.calls["Create"]++
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.next++
	p := &model.Panel{ID: f.next, Title: in.Title, Type: in.Type, PromQL: in.PromQL}
	f.panels[p.ID] = p
	return p, nil
}

func (f *fakePanels) Update(_ context.Context, id uint64, in biz.UpdateInput) (*model.Panel, error) {
	f.calls["Update"]++
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	p, ok := f.panels[id]
	if !ok {
		return nil, errs.ErrNotFound
	}
	if in.Title != nil {
		p.Title = *in.Title
	}
	if in.PromQL != nil {
		p.PromQL = *in.PromQL
	}
	return p, nil
}

func (f *fakePanels) Delete(_ context.Context, id uint64) error {
	f.calls["Delete"]++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.panels, id)
	return nil
}

// auditCall 发一次请求并把行读回来。槽位是宿主中间件挂的，所以测试自己装。
func auditCall(t *testing.T, router http.Handler, method, path, body string, tenant *tenantctx.Tenant) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tenant != nil {
		req = req.WithContext(tenantctx.With(req.Context(), *tenant))
	}
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	ev, set := auditport.GetAuditEvent(req.Context())
	return rec, ev, set
}

func admin() *tenantctx.Tenant { return &tenantctx.Tenant{UserID: 7, Role: tenantctx.RoleAdmin} }
func viewer() *tenantctx.Tenant {
	return &tenantctx.Tenant{UserID: 8, Role: tenantctx.RoleUser}
}

func panelRouter(svc PanelService) http.Handler {
	r := chi.NewRouter()
	NewHandler(svc).Register(r)
	return r
}

func payloadOf(t *testing.T, ev auditport.Event) map[string]any {
	t.Helper()
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want a map", ev.Payload)
	}
	return p
}

// 1. 词表锁死。断言读的是常量，字面量要单独钉一次。
func TestPanelAuditVocabulary(t *testing.T) {
	for got, want := range map[string]string{
		auditport.ActionPanelCreate: "panel_create",
		auditport.ActionPanelUpdate: "panel_update",
		auditport.ActionPanelDelete: "panel_delete",
	} {
		if got != want {
			t.Errorf("action = %q, want %q", got, want)
		}
	}
	if auditport.ResourcePanel != "panel" {
		t.Errorf("resource = %q", auditport.ResourcePanel)
	}
}

// 2. create 成功行。
func TestPanelCreate_Audited(t *testing.T) {
	svc := newFakePanels()
	r := panelRouter(svc)
	rec, ev, set := auditCall(t, r, http.MethodPost, "/v1/monitor/panels",
		`{"title":"disk latency","type":"timeseries","promql":"rate(node_disk_x[5m])","unit":"ms"}`, admin())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on a mutating route")
	}
	if ev.Action != auditport.ActionPanelCreate {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.ResourceType != auditport.ResourcePanel || ev.ResourceID != "101" {
		t.Errorf("resource = %q / %q, want panel / 101", ev.ResourceType, ev.ResourceID)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q", ev.Status)
	}
	p := payloadOf(t, ev)
	if p["title"] != "disk latency" || p["type"] != "timeseries" {
		t.Errorf("payload = %v", p)
	}
	if p["promql_len"] != len("rate(node_disk_x[5m])") {
		t.Errorf("promql_len = %v", p["promql_len"])
	}
	if p["promql_digest"] != auditport.ValueDigest("rate(node_disk_x[5m])") {
		t.Error("the digest is not of the query that was saved")
	}
}

// 3. PromQL 正文不进链：仪表盘查询里经常有一个标签，值是别人的令牌。
func TestPanelCreate_PromQLTextNeverReachesTheChain(t *testing.T) {
	const q = `rate(node_disk{tenant="tok_live_ABCDEF0123456789"}[5m])`
	svc := newFakePanels()
	rec, ev, _ := auditCall(t, panelRouter(svc), http.MethodPost, "/v1/monitor/panels",
		`{"title":"t","promql":`+jsonLiteral(t, q)+`}`, admin())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	assertNoSecret(t, payloadOf(t, ev), "tok_live_")
	if payloadOf(t, ev)["promql_digest"] != auditport.ValueDigest(q) {
		t.Error("digest does not match the saved query")
	}
}

// 4. update 记「哪些列动了」，不记「动成了什么」。
func TestPanelUpdate_AuditsFieldNamesNotValues(t *testing.T) {
	svc := newFakePanels()
	svc.panels[7] = &model.Panel{ID: 7, Title: "old"}
	r := panelRouter(svc)
	body := `{"title":"new title","promql":"tok_live_SECRET"}`
	rec, ev, set := auditCall(t, r, http.MethodPatch, "/v1/monitor/panels/7", body, admin())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on update")
	}
	if ev.Action != auditport.ActionPanelUpdate {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.ResourceID != "7" {
		t.Errorf("resource_id = %q", ev.ResourceID)
	}
	p := payloadOf(t, ev)
	fields, ok := p["fields"].([]string)
	if !ok {
		t.Fatalf("fields is %T, want []string", p["fields"])
	}
	if len(fields) != 2 || fields[0] != "title" || fields[1] != "promql" {
		t.Errorf("fields = %v, want [title promql] in a fixed order", fields)
	}
	assertNoSecret(t, p, "tok_live_")
	if strings.Contains(ev.ErrorMessage, "tok_live_") {
		t.Error("the value leaked into the error message")
	}
}

// 5. delete 必须带上被删掉的那一块板子的标题——这是这条行存在的全部理由。
func TestPanelDelete_AuditsWhatWasRemoved(t *testing.T) {
	svc := newFakePanels()
	svc.panels[7] = &model.Panel{ID: 7, Title: "disk latency", Type: "timeseries"}
	r := panelRouter(svc)
	rec, ev, set := auditCall(t, r, http.MethodDelete, "/v1/monitor/panels/7", "", admin())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on delete")
	}
	if ev.Action != auditport.ActionPanelDelete {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.ResourceID != "7" {
		t.Errorf("resource_id = %q", ev.ResourceID)
	}
	p := payloadOf(t, ev)
	if p["title"] != "disk latency" || p["type"] != "timeseries" {
		t.Errorf("payload = %v, want the removed panel named", p)
	}
	// 夹具侧也要对得上：读的是真的先发生了，不是记完再补的一次读。
	if svc.calls["Get"] != 1 {
		t.Errorf("Get called %d times before Delete, want exactly 1", svc.calls["Get"])
	}
	if svc.calls["Delete"] != 1 {
		t.Errorf("Delete called %d times, want 1", svc.calls["Delete"])
	}
}

// 6. 面板本来就不存在时，行里必须说「本来就不在」，不能带一个空标题
// 读起来像一个没有名字的面板。
func TestPanelDelete_AlreadyGoneSaysSo(t *testing.T) {
	svc := newFakePanels()
	svc.getErr = errors.New("panel store unreachable")
	r := panelRouter(svc)
	rec, ev, set := auditCall(t, r, http.MethodDelete, "/v1/monitor/panels/7", "", admin())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unreadable panel must not block the delete", rec.Code)
	}
	if !set {
		t.Fatal("no audit row")
	}
	p := payloadOf(t, ev)
	if p["already_gone"] != true {
		t.Errorf("already_gone = %v, want true", p["already_gone"])
	}
	if v, ok := p["title"]; ok {
		t.Errorf("title = %v present, want absent when the read failed", v)
	}
	if svc.calls["Delete"] != 1 {
		t.Errorf("Delete called %d times, want 1", svc.calls["Delete"])
	}
}

// 7. 非管理员的尝试也是事实：denied 行与没有行必须长得不一样。
func TestPanelMutations_RecordDeniedAttempts(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{http.MethodPost, "/v1/monitor/panels", auditport.ActionPanelCreate},
		{http.MethodPatch, "/v1/monitor/panels/7", auditport.ActionPanelUpdate},
		{http.MethodDelete, "/v1/monitor/panels/7", auditport.ActionPanelDelete},
	} {
		svc := newFakePanels()
		svc.panels[7] = &model.Panel{ID: 7, Title: "disk latency"}
		rec, ev, set := auditCall(t, panelRouter(svc), tc.method, tc.path, `{"title":"x"}`, viewer())
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", tc.method, tc.path, rec.Code)
		}
		if !set {
			t.Errorf("%s %s: a refused attempt wrote no row", tc.method, tc.path)
			continue
		}
		if ev.Status != auditport.StatusDenied {
			t.Errorf("%s %s: status = %q, want denied", tc.method, tc.path, ev.Status)
		}
		if ev.Action != tc.want {
			t.Errorf("%s %s: action = %q, want %q", tc.method, tc.path, ev.Action, tc.want)
		}
		// 而被拒的请求什么也没做。denied 行如果没有配一条「服务没被碰到」
		// 的断言，就只是一行声称。
		if len(svc.calls) != 0 {
			t.Errorf("%s %s: a refused request still reached the service: %v",
				tc.method, tc.path, svc.calls)
		}
	}
}

// 8. 服务失败要留行。
func TestPanelMutations_RecordServiceFailures(t *testing.T) {
	svc := newFakePanels()
	svc.createErr = errors.New("disk full")
	rec, ev, set := auditCall(t, panelRouter(svc), http.MethodPost, "/v1/monitor/panels",
		`{"title":"t","promql":"up"}`, admin())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !set {
		t.Fatal("a failed create wrote no row")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	if ev.ErrorMessage == "" {
		t.Error("failure row carries no error message")
	}
}

// assertNoSecret scans every payload value for needle.
//
// It walks values with reflect rather than asserting v.(string), and it took
// two failures to arrive here. The first version type-asserted to string, so
// a mutation that put UpdateInput.PromQL (a *string) into the payload sailed
// through: a pointer is not a string and the assertion declined to look. The
// second used fmt.Sprint, which is worse in a quieter way — it *did* look at
// the pointer and printed its address, so a payload full of hex addresses
// passed a check for a token. Dereferencing has to be explicit, and it has to
// go all the way down: a leak hidden one pointer, a map or a slice deep is
// the same leak.
func assertNoSecret(t *testing.T, payload map[string]any, needle string) {
	t.Helper()
	for k, v := range payload {
		if hit, ok := foundInValue(reflect.ValueOf(v), needle); ok {
			t.Errorf("payload[%q] carries the sensitive text: %q", k, hit)
		}
	}
}

// foundInValue returns the offending text itself, not just a yes. The first
// version of this reported fmt.Sprint(v) and printed a pointer address
// alongside the word "sensitive", which is the kind of failure message that
// makes a reader doubt the finding rather than the code.
func foundInValue(v reflect.Value, needle string) (string, bool) {
	if !v.IsValid() {
		return "", false
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return "", false
		}
		return foundInValue(v.Elem(), needle)
	case reflect.String:
		if strings.Contains(v.String(), needle) {
			return v.String(), true
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if hit, ok := foundInValue(v.Index(i), needle); ok {
				return hit, true
			}
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			if hit, ok := foundInValue(v.MapIndex(key), needle); ok {
				return hit, true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if hit, ok := foundInValue(v.Field(i), needle); ok {
				return hit, true
			}
		}
	}
	return "", false
}

// jsonLiteral 把一段查询变成合法 JSON 字符串字面量。
func jsonLiteral(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshalling %q: %v", s, err)
	}
	return string(b)
}
