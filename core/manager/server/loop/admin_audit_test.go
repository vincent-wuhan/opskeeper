package loop

import (
	"bytes"
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

// --- 决策 319：自愈循环的 retry_count 两条写路由上宿主链 ----------------------
//
// 闭环里最重的那个整数是 retry_count：它是「再试一次」和「交给人」的判据。这
// 两条路由直接改它，而链上此前没有一行关于它——于是一条无限循环的自愈是「自己
// 把自己升到危险级别」，还是「有人在按计划把它清零」，事后完全分不开。
//
// 夹具复用 admin_test.go 里的 fakeRecoveryStateAdmin，不另起 httptest.NewServer：
// 审计槽位是宿主中间件挂在请求上下文里的，跨一次真实 HTTP 就读不到了。

// auditCall 发一次请求并把行读回来。
func auditCall(t *testing.T, router http.Handler, method, path, body string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auditport.WithSlot(req.Context()))
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

func auditRouter(t *testing.T, store RecoveryStateAdmin) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	RegisterAdminRoutes(r, AdminRouteDeps{Enabled: true, StateStore: store})
	return r
}

// failAfterN 在第 n 次调用之后开始失败，用来复现「循环不是原子的」那条路径。
type failAfterN struct {
	*fakeRecoveryStateAdmin
	ok    int
	calls int
}

func (f *failAfterN) Increment(ctx context.Context, incidentID string) (int, error) {
	f.calls++
	if f.calls > f.ok {
		return 0, errors.New("db gone")
	}
	return f.fakeRecoveryStateAdmin.Increment(ctx, incidentID)
}

// 1. 动作名锁死。改名会让 17/18 两条断言一起变成同义反复，而它们读的是常量
// 不是字面量——所以这一条专门盯住字面量。
func TestRecoveryRetryActionVocabulary(t *testing.T) {
	if auditport.ActionRecoveryRetryIncrement != "recovery_retry_increment" {
		t.Errorf("increment action = %q", auditport.ActionRecoveryRetryIncrement)
	}
	if auditport.ActionRecoveryRetryReset != "recovery_retry_reset" {
		t.Errorf("reset action = %q", auditport.ActionRecoveryRetryReset)
	}
}

// 2. increment 成功行：落了几个、现在几、越线了吗。
func TestAdminIncrementRetryCount_Audited(t *testing.T) {
	store := newFakeRecoveryStateAdmin()
	r := auditRouter(t, store)
	rec, ev, set := auditCall(t, r, http.MethodPost,
		"/v1/admin/loops/recovery_state/INC-AUD/increment", `{"times":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on a mutating route")
	}
	if ev.Action != auditport.ActionRecoveryRetryIncrement {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.ResourceType != auditport.ResourceIncident {
		t.Errorf("resource_type = %q", ev.ResourceType)
	}
	if ev.ResourceID != "INC-AUD" {
		t.Errorf("resource_id = %q", ev.ResourceID)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q", ev.Status)
	}
	p := payloadOf(t, ev)
	if p["applied"] != 4 || p["requested"] != 4 {
		t.Errorf("applied/requested = %v/%v, want 4/4", p["applied"], p["requested"])
	}
	if p["retry_count"] != 4 {
		t.Errorf("retry_count = %v, want 4", p["retry_count"])
	}
	if p["escalated"] != true {
		t.Errorf("escalated = %v, want true", p["escalated"])
	}
}

// 3. 部分应用：四次里第三次炸了。行里必须是 applied=2，不能是 requested=4。
// 这是本轮唯一一个「不改就静默撒谎」的断言，所以它单独占一条。
func TestAdminIncrementRetryCount_PartialFailureRecordsApplied(t *testing.T) {
	store := &failAfterN{fakeRecoveryStateAdmin: newFakeRecoveryStateAdmin(), ok: 2}
	r := auditRouter(t, store)
	rec, ev, set := auditCall(t, r, http.MethodPost,
		"/v1/admin/loops/recovery_state/INC-PART/increment", `{"times":4}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !set {
		t.Fatal("a store failure wrote no row")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	p := payloadOf(t, ev)
	if p["applied"] != 2 {
		t.Errorf("applied = %v, want 2 (the counter really did move twice)", p["applied"])
	}
	if p["requested"] != 4 {
		t.Errorf("requested = %v, want 4", p["requested"])
	}
	if p["retry_count"] != 2 {
		t.Errorf("retry_count = %v, want 2", p["retry_count"])
	}
	if ev.ErrorMessage == "" {
		t.Error("failure row carries no error message")
	}
	// 夹具自身也要对得上：applied 这个数不是从响应里抄来的。
	if store.counts["INC-PART"] != 2 {
		t.Fatalf("store count = %d, want 2", store.counts["INC-PART"])
	}
}

// 4. 被夹掉的上限请求要留一行：有人要过 500 次，被拦住了。
func TestAdminIncrementRetryCount_RejectedTimesAudited(t *testing.T) {
	r := auditRouter(t, newFakeRecoveryStateAdmin())
	rec, ev, set := auditCall(t, r, http.MethodPost,
		"/v1/admin/loops/recovery_state/INC-BIG/increment", `{"times":500}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !set {
		t.Fatal("a rejected mutating request wrote no row")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	p := payloadOf(t, ev)
	if p["requested"] != 500 {
		t.Errorf("requested = %v, want 500", p["requested"])
	}
	if p["applied"] != 0 {
		t.Errorf("applied = %v, want 0", p["applied"])
	}
}

// 5. reset 是另一个动作，且带走被清掉的数——那正是事后再也查不到的东西。
func TestAdminResetRetryCount_AuditsPreviousValue(t *testing.T) {
	store := newFakeRecoveryStateAdmin()
	store.counts["INC-RES"] = 9
	r := auditRouter(t, store)
	rec, ev, set := auditCall(t, r, http.MethodPost,
		"/v1/admin/loops/recovery_state/INC-RES/reset", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on reset")
	}
	if ev.Action != auditport.ActionRecoveryRetryReset {
		t.Errorf("action = %q, want the reset action (merged into increment, this fails)",
			ev.Action)
	}
	if ev.Action == auditport.ActionRecoveryRetryIncrement {
		t.Error("reset is recorded as an increment — a reader cannot tell re-arming from escalating")
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q", ev.Status)
	}
	p := payloadOf(t, ev)
	if p["previous"] != 9 {
		t.Errorf("previous = %v, want 9 (the value the reset destroyed)", p["previous"])
	}
	if p["retry_count"] != 0 {
		t.Errorf("retry_count = %v, want 0", p["retry_count"])
	}
	if p["previous_known"] != true {
		t.Errorf("previous_known = %v, want true", p["previous_known"])
	}
}

// 6. reset 失败也要有行，而且仍带着清之前的数：失败行里的那个数同样能解释
// 后来为什么还在 escalating。
func TestAdminResetRetryCount_FailureAudited(t *testing.T) {
	store := newFakeRecoveryStateAdmin()
	store.counts["INC-RF"] = 4
	store.resetErr = errors.New("locked")
	r := auditRouter(t, store)
	rec, ev, set := auditCall(t, r, http.MethodPost,
		"/v1/admin/loops/recovery_state/INC-RF/reset", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !set {
		t.Fatal("a failed reset wrote no row")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	p := payloadOf(t, ev)
	if p["previous"] != 4 {
		t.Errorf("previous = %v, want 4", p["previous"])
	}
	if _, ok := p["retry_count"]; ok {
		t.Error("a failed reset claims retry_count = 0; it still holds 4")
	}
}

// 7. 读不到旧值时，reset 照做，但行里必须说「不知道」而不是写 0。
// 把未知写成 0 是本仓反复踩的那一类：0 和「本来就没有」读起来一模一样。
func TestAdminResetRetryCount_UnknownPreviousIsNotZero(t *testing.T) {
	store := newFakeRecoveryStateAdmin()
	store.getErr = errors.New("read replica down")
	store.counts["INC-UNK"] = 5
	r := auditRouter(t, store)
	rec, ev, set := auditCall(t, r, http.MethodPost,
		"/v1/admin/loops/recovery_state/INC-UNK/reset", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unreadable Get must not block the reset",
			rec.Code)
	}
	if !set {
		t.Fatal("no audit row")
	}
	if store.counts["INC-UNK"] != 0 {
		t.Errorf("store count = %d, want the reset to have happened", store.counts["INC-UNK"])
	}
	p := payloadOf(t, ev)
	if p["previous_known"] != false {
		t.Errorf("previous_known = %v, want false", p["previous_known"])
	}
	if v, ok := p["previous"]; ok {
		t.Errorf("previous = %v present, want absent when the read failed", v)
	}
}

// 8. 两条路由各写各的动作，且互不顶替——把其中一处的常量换成另一处，
// 上面两条就已经会红；这里再钉一次「都写了」，防止有人删掉一行。
func TestAdminRecoveryRetryRoutesBothAudited(t *testing.T) {
	r := auditRouter(t, newFakeRecoveryStateAdmin())
	for _, tc := range []struct {
		path, want string
	}{
		{"/v1/admin/loops/recovery_state/X/increment", auditport.ActionRecoveryRetryIncrement},
		{"/v1/admin/loops/recovery_state/X/reset", auditport.ActionRecoveryRetryReset},
	} {
		_, ev, set := auditCall(t, r, http.MethodPost, tc.path, `{"times":1}`)
		if !set {
			t.Errorf("%s wrote no row", tc.path)
			continue
		}
		if ev.Action != tc.want {
			t.Errorf("%s action = %q, want %q", tc.path, ev.Action, tc.want)
		}
	}
}

// 9. JSON 坏掉的请求也算一次尝试，不能悄悄没有行。
func TestAdminIncrementRetryCount_BadJSONAudited(t *testing.T) {
	r := auditRouter(t, newFakeRecoveryStateAdmin())
	rec, ev, set := auditCall(t, r, http.MethodPost,
		"/v1/admin/loops/recovery_state/INC-J/increment", `{"times":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !set {
		t.Fatal("a malformed mutating request wrote no row")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	if ev.ErrorMessage == "" {
		t.Error("failure row carries no error message")
	}
}

// bonus: 响应体形状没被审计改坏（载荷是行上的东西，不是响应里的东西）。
func TestAdminIncrementRetryCount_ResponseUnchanged(t *testing.T) {
	r := auditRouter(t, newFakeRecoveryStateAdmin())
	rec, _, _ := auditCall(t, r, http.MethodPost,
		"/v1/admin/loops/recovery_state/INC-RESP/increment", `{"times":2}`)
	var out IncrementResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.RetryCount != 2 || out.Incremented != 2 || out.Escalated {
		t.Errorf("response changed shape: %+v", out)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"retry_count":2`)) {
		t.Errorf("body = %s", rec.Body.String())
	}
}
