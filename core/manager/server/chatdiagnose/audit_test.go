package chatdiagnose

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	chatdiagnosebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/chatdiagnose"
)

// --- 决策 320：聊天诊断的三条写路由上宿主链 ---------------------------------
//
// 这三条此前是裁决表里排在最前面的洞，理由是它们是剩下十条里唯一一条**出网**
// 的；读完代码之后理由变了，而且更硬：promote 把一句聊天结论变成执行面的一次真实
// 修复尝试，而链上没有一行写着「谁把哪句话变成了工单」——问「为什么没人报障障而系
// 统自己在修」的人，答案只在这条里。
//
// 夹具复用 http_test.go 的 fakeService 与 newRouter，不另起服务：审计槽位挂在请求上
// 下文的宿主中间件里，跨一次真实 HTTP 就读不到了。

// auditCall3 发一次请求并把行读回来。
func auditCall3(t *testing.T, router http.Handler, method, path, body string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	ev, set := auditport.GetAuditEvent(req.Context())
	return rec, ev, set
}

func payloadOf3(t *testing.T, ev auditport.Event) map[string]any {
	t.Helper()
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want a map", ev.Payload)
	}
	return p
}

// 1. 动作名与资源桶锁死。这一组是新加的词表，读的是常量而不是字面量，所以专门用
// 一条把字面量也钉住。
func TestChatAuditVocabulary(t *testing.T) {
	for got, want := range map[string]string{
		auditport.ActionChatDiagnose: "chat_diagnose",
		auditport.ActionChatPromote:  "chat_promote",
		auditport.ActionChatReport:   "chat_report",
	} {
		if got != want {
			t.Errorf("action = %q, want %q", got, want)
		}
	}
	if auditport.ResourceChatConversation != "chat_conversation" {
		t.Errorf("resource = %q", auditport.ResourceChatConversation)
	}
	// 这三个动作不许被折进 node agent 那一组：那条桶回答的是「哪台主机上的哪次
	// 会话」，而这三个的 id 是一个浏览器标签页里的会话。
	if auditport.ResourceChatConversation == auditport.ResourceAgentSession {
		t.Error("chat conversations are filed under node agent sessions")
	}
}

// 2. diagnose 成功行：会话 id 取自**响应**，因为第一次提问时请求里根本没有。
func TestDiagnose_Audited(t *testing.T) {
	svc := &fakeService{diagnoseResp: &chatdiagnosebiz.ChatDiagnoseResponse{
		ConversationID: "conv-new", TurnID: 42, Reply: "ok",
	}}
	r := newRouter(svc)
	rec, ev, set := auditCall3(t, r, http.MethodPost, "/chat/diagnose",
		`{"tenant_id":"t1","user_message":"why is the db slow","mentioned_agent":"sre"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on a mutating route")
	}
	if ev.Action != auditport.ActionChatDiagnose {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.ResourceType != auditport.ResourceChatConversation {
		t.Errorf("resource_type = %q", ev.ResourceType)
	}
	if ev.ResourceID != "conv-new" {
		t.Errorf("resource_id = %q, want conv-new (it comes from the response, not the request)",
			ev.ResourceID)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q", ev.Status)
	}
	p := payloadOf3(t, ev)
	if p["mentioned_agent"] != "sre" {
		t.Errorf("mentioned_agent = %v", p["mentioned_agent"])
	}
	if p["message_len"] != len("why is the db slow") {
		t.Errorf("message_len = %v", p["message_len"])
	}
	if p["message_digest"] == "" || p["message_digest"] == nil {
		t.Error("no message digest")
	}
}

// 3. 提问正文不进链。这一条用一个看起来像凭据的串：它是本轮唯一的「泄露」断言，
// 而决策 318 已经为同一件事付过一次学费。
func TestDiagnose_MessageTextNeverReachesTheChain(t *testing.T) {
	const secret = "ghp_AAAABBBBCCCCDDDDEEEEFFFF"
	svc := &fakeService{diagnoseResp: &chatdiagnosebiz.ChatDiagnoseResponse{
		ConversationID: "conv-sec", TurnID: 1,
	}}
	r := newRouter(svc)
	body := `{"tenant_id":"t1","user_message":"use token ` + secret + ` to debug"}`
	rec, ev, _ := auditCall3(t, r, http.MethodPost, "/chat/diagnose", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		// 响应里回显助手答复，不回显提问，所以这条只是提醒：断言的对象是
		// 载荷，不是响应。真正的检查在下面。
		t.Log("the secret appears in the response body; the assertion below is about the row")
	}
	if strings.Contains(ev.ErrorMessage, secret) {
		t.Error("the secret is in the row's error message")
	}
	assertNoSecretInPayload3(t, payloadOf3(t, ev), secret, "carries the message text")
	// 而摘要仍在：不可读，不等于不可比。
	if payloadOf3(t, ev)["message_digest"] != auditport.ValueDigest("use token "+secret+" to debug") {
		t.Error("the digest is not of the message that was actually sent")
	}
}

// 4. diagnose 失败也要有行，而且带原因。
func TestDiagnose_FailureAudited(t *testing.T) {
	svc := &fakeService{diagnoseErr: chatdiagnosebiz.ErrFeatureDisabled}
	r := newRouter(svc)
	rec, ev, set := auditCall3(t, r, http.MethodPost, "/chat/diagnose",
		`{"tenant_id":"t1","user_message":"hello"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !set {
		t.Fatal("a refused diagnose wrote no row")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	if ev.ErrorMessage == "" {
		t.Error("failure row carries no error message")
	}
}

// 5. 校验失败也是一次尝试：用户问了、没问成。
func TestDiagnose_ValidationFailureAudited(t *testing.T) {
	svc := &fakeService{}
	r := newRouter(svc)
	rec, ev, set := auditCall3(t, r, http.MethodPost, "/chat/diagnose", `{"user_message":"hi"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !set {
		t.Fatal("a rejected diagnose wrote no row")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
}

// 6. promote：这条是本轮最要紧的一行——它把一句结论变成执行面的一次修复尝试。
// turn_id 与它拉起的那次运行都必须在行上。
func TestPromote_Audited(t *testing.T) {
	svc := &fakeService{promoteResp: &chatdiagnosebiz.OrchestratorRunResult{
		IncidentID: "INC-1", FirstLoopEventID: 900, FinalPhase: "postmortem",
	}}
	r := newRouter(svc)
	rec, ev, set := auditCall3(t, r, http.MethodPost,
		"/chat/conversations/conv-7/promote", `{"tenant_id":"t1","turn_id":42}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on promote — this is the one that answers 'who filed the work'")
	}
	if ev.Action != auditport.ActionChatPromote {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.ResourceType != auditport.ResourceChatConversation {
		t.Errorf("resource_type = %q", ev.ResourceType)
	}
	if ev.ResourceID != "conv-7" {
		t.Errorf("resource_id = %q", ev.ResourceID)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q", ev.Status)
	}
	p := payloadOf3(t, ev)
	if p["turn_id"] != int64(42) {
		t.Errorf("turn_id = %v, want 42", p["turn_id"])
	}
	if p["incident_id"] != "INC-1" {
		t.Errorf("incident_id = %v", p["incident_id"])
	}
	if p["first_loop_event_id"] != int64(900) {
		t.Errorf("first_loop_event_id = %v", p["first_loop_event_id"])
	}
}

// 7. promote 失败必须有行，而且带上「它没能把哪句话变成工单」。
func TestPromote_FailureAudited(t *testing.T) {
	svc := &fakeService{promoteErr: chatdiagnosebiz.ErrConversationTenantMismatch}
	r := newRouter(svc)
	rec, ev, set := auditCall3(t, r, http.MethodPost,
		"/chat/conversations/conv-8/promote", `{"tenant_id":"t2","turn_id":9}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !set {
		t.Fatal("a refused promote wrote no row")
	}
	if ev.Action != auditport.ActionChatPromote {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q", ev.Status)
	}
	p := payloadOf3(t, ev)
	if p["turn_id"] != int64(9) {
		t.Errorf("turn_id = %v, want 9", p["turn_id"])
	}
}

// 8. push_report：报告正文同样不进链，而它是出网的那一条。
func TestPushReport_Audited(t *testing.T) {
	svc := &fakeService{}
	r := newRouter(svc)
	const md = "# postmortem\n\ntoken: hunter2\n"
	rec, ev, set := auditCall3(t, r, http.MethodPost,
		"/chat/conversations/conv-9/reports",
		`{"tenant_id":"t1","report_markdown":`+jsonLiteral(t, md)+`}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("no audit row on an outbound write")
	}
	if ev.Action != auditport.ActionChatReport {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.ResourceID != "conv-9" {
		t.Errorf("resource_id = %q", ev.ResourceID)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q", ev.Status)
	}
	p := payloadOf3(t, ev)
	if p["report_len"] != len(md) {
		t.Errorf("report_len = %v, want %d", p["report_len"], len(md))
	}
	if p["report_digest"] != auditport.ValueDigest(md) {
		t.Error("the digest is not of the report that was actually pushed")
	}
	assertNoSecretInPayload3(t, p, "hunter2", "carries the report text")
	// 服务端确实收到了正文——把正文抽掉不算修好。
	if svc.lastReport.Markdown != md {
		t.Errorf("the service was handed %q", svc.lastReport.Markdown)
	}
}

// 9. push_report 失败也要有行。
func TestPushReport_FailureAudited(t *testing.T) {
	svc := &fakeService{reportErr: errors.New("thread closed")}
	r := newRouter(svc)
	rec, ev, set := auditCall3(t, r, http.MethodPost,
		"/chat/conversations/conv-10/reports", `{"tenant_id":"t1","report_markdown":"# x"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !set {
		t.Fatal("a failed push wrote no row")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	if ev.ErrorMessage == "" {
		t.Error("failure row carries no error message")
	}
}

// assertNoSecretInPayload3 walks the payload with reflect rather than
// asserting v.(string). Decision 320 wrote the weak shape here and decision
// 322 found it the hard way: a mutation that put a *string into the payload
// passed a string type-assertion untouched, and a fmt.Sprint version passed
// it too by printing the pointer's address. This one dereferences all the way
// down, so a leak one pointer, slice or map deep is still a leak.
func assertNoSecretInPayload3(t *testing.T, payload map[string]any, needle, what string) {
	t.Helper()
	for k, v := range payload {
		if hit, ok := foundInPayload3(reflect.ValueOf(v), needle); ok {
			t.Errorf("payload[%q] %s: %q", k, what, hit)
		}
	}
}

func foundInPayload3(v reflect.Value, needle string) (string, bool) {
	if !v.IsValid() {
		return "", false
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return "", false
		}
		return foundInPayload3(v.Elem(), needle)
	case reflect.String:
		if strings.Contains(v.String(), needle) {
			return v.String(), true
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if hit, ok := foundInPayload3(v.Index(i), needle); ok {
				return hit, true
			}
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			if hit, ok := foundInPayload3(v.MapIndex(key), needle); ok {
				return hit, true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if hit, ok := foundInPayload3(v.Field(i), needle); ok {
				return hit, true
			}
		}
	}
	return "", false
}

// jsonLiteral 把一段 Markdown 变成合法 JSON 字符串字面量。用它而不是手写引号，
// 因为正文里有换行和冒号——而引号处理错了会让这条用例测成别的东西。
// 它故意叫 jsonLiteral 而不是 strconv：后者会遮蔽标准库包名，而这个文件
// 恰好要 import encoding/json。
func jsonLiteral(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshalling %q: %v", s, err)
	}
	return string(b)
}
