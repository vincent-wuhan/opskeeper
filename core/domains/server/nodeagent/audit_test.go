package nodeagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/domains/biz/nodeagent"
)

// --- 决策 318：节点 Agent 的五条写路由上宿主链 -------------------------------
//
// 这一组里最要紧的一条是 decide：它就是那个按钮。链上此前没有一行写着
// 「谁批准了这次调用、批的是哪一份」——而节点侧自己的账本虽然记了
// decided_by，却记不下「谁」是哪个主机上的哪次会话里的第几号请求。
//
// 夹具复用 http_test.go 里的 fakeFleet，但不用 httptest.NewServer：审计槽位
// 是宿主中间件挂在请求上下文里的，跨一次真实 HTTP 就拿不到了。

func newAuditRouter(t *testing.T) (http.Handler, *fakeFleet) {
	t.Helper()
	fleet := newFakeFleet()
	svc, err := nodeagent.New(nodeagent.Options{Fleet: fleet, Grace: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("nodeagent.New: %v", err)
	}
	r := chi.NewRouter()
	// The production prefix supplies the operator's identity; the audit slot
	// is created per request by the caller instead, so that the test can read
	// the row back off the request it built. Creating it in middleware would
	// work for the handler and not for the assertion, because the middleware
	// hands downstream a *new* context and the test still holds the old one.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := tenantctx.With(req.Context(), tenantctx.Tenant{
				UserID: 42, Email: "alice@example.com", Role: "admin",
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	NewHandler(svc).Register(r)
	return r, fleet
}

func auditCall2(t *testing.T, router http.Handler, method, path, body string) (*httptest.ResponseRecorder, auditport.Event, bool) {
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

func openSession2(t *testing.T, router http.Handler, body string) string {
	t.Helper()
	rec, _, _ := auditCall2(t, router, http.MethodPost, "/v1/node-agents/sessions", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	var out openResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode open: %v", err)
	}
	return out.SessionID
}

// attach starts reading the stream, which is what marks a conversation as
// having a console on it. Send refuses until that happens, and it refuses
// *before* prompting, so polling with a send is a safe way to wait for it.
func attach(t *testing.T, router http.Handler, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, "/v1/node-agents/sessions/"+sessionID+"/stream", nil).WithContext(ctx)
		router.ServeHTTP(httptest.NewRecorder(), req)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec, _, _ := auditCall2(t, router, http.MethodPost,
			"/v1/node-agents/sessions/"+sessionID+"/messages", `{"content":"ping"}`)
		if rec.Code != http.StatusConflict {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the stream never attached")
}

// 那个按钮。request_id 与 digest 合起来回答「批的是哪一份」：只有
// request_id 的一行会让读者以为「某人批准了某件事」，digest 才是把它钉到
// 节点验过的那一份参数上的那半句。
func TestTheApprovalButtonNamesWhatWasApproved(t *testing.T) {
	router, fleet := newAuditRouter(t)
	sid := openSession2(t, router, `{"edge_id":7,"session_id":"s-1"}`)

	rec, ev, set := auditCall2(t, router, http.MethodPost,
		"/v1/node-agents/sessions/"+sid+"/approvals/req-9/decide",
		`{"request_id":"req-9","digest":"sha256:abc123","grant":true,"note":"confirmed the disk is the cause"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: %d %s", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionAgentDecide || ev.Status != auditport.StatusSuccess {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if ev.ResourceID != sid {
		t.Fatalf("resource id = %q, want the session the answer belongs to", ev.ResourceID)
	}
	p := payloadOf(t, ev)
	if p["request_id"] != "req-9" || p["digest"] != "sha256:abc123" {
		t.Fatalf("payload = %v, want the request and the digest it was verified against", p)
	}
	if p["grant"] != true {
		t.Fatalf("grant = %v, want true", p["grant"])
	}
	if p["decided_by"] != "alice@example.com" {
		t.Fatalf("decided_by = %v, want the operator a human would recognise", p["decided_by"])
	}
	// The row is only worth anything if the answer actually went down.
	if fleet.decide.RequestID != "req-9" || !fleet.decide.Grant {
		t.Fatalf("the node never received the decision: %+v", fleet.decide)
	}
}

// 批准与拒绝是同一个动作上的一个布尔，不是两个动作：读者问的是
// 「对第 N 号请求他决定什么了」，那是一个问题一个答案，而不是一个
// 必须先猜对的过滤器。
func TestARefusalIsAnAnswerNotAnAbsence(t *testing.T) {
	router, _ := newAuditRouter(t)
	sid := openSession2(t, router, `{"edge_id":7,"session_id":"s-1"}`)
	rec, ev, set := auditCall2(t, router, http.MethodPost,
		"/v1/node-agents/sessions/"+sid+"/approvals/req-9/decide",
		`{"request_id":"req-9","digest":"sha256:abc123","grant":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: %d %s", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionAgentDecide {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if p := payloadOf(t, ev); p["grant"] != false {
		t.Fatalf("grant = %v, want false — a refusal that reads as absent is the failure", p["grant"])
	}
}

// 一个不指名任何请求的决定也是一次决定。
func TestADecisionNamingNoRequestIsAudited(t *testing.T) {
	router, _ := newAuditRouter(t)
	sid := openSession2(t, router, `{"edge_id":7,"session_id":"s-1"}`)
	rec, ev, set := auditCall2(t, router, http.MethodPost,
		"/v1/node-agents/sessions/"+sid+"/approvals/req-9/decide", `{"grant":true}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("a decision naming no request was accepted: %s", rec.Body.String())
	}
	if !set || ev.Status != auditport.StatusFailure || ev.Action != auditport.ActionAgentDecide {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
}

// 节点拒绝这次决定时，链上要留下一行失败，而不是什么都没有。
func TestANodeRefusalIsAudited(t *testing.T) {
	router, fleet := newAuditRouter(t)
	sid := openSession2(t, router, `{"edge_id":7,"session_id":"s-1"}`)
	fleet.decideErr = context.DeadlineExceeded
	rec, ev, set := auditCall2(t, router, http.MethodPost,
		"/v1/node-agents/sessions/"+sid+"/approvals/req-9/decide",
		`{"request_id":"req-9","digest":"sha256:abc123","grant":true}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("a refused decision returned 200")
	}
	if !set || ev.Status != auditport.StatusFailure {
		t.Fatalf("ev = %+v (set=%v) — an approval that never reached the node must not look like one that did", ev, set)
	}
	if p := payloadOf(t, ev); p["request_id"] != "req-9" {
		t.Fatalf("payload = %v, want the request even on the failure row", p)
	}
}

// 指令正文不进链：它是对一个能跑工具的 agent 说的话，没有长度上限，而且
// 运维从告警页往里粘的东西里经常就有一条凭据。行里放摘要与长度——两次
// 相同的指令可区分，两次都不可读。
func TestTheInstructionTextNeverReachesTheChain(t *testing.T) {
	const pasted = "restart the api pod, the token is ghp_AAAABBBBCCCC and the db password is hunter2"
	router, _ := newAuditRouter(t)
	sid := openSession2(t, router, `{"edge_id":7,"session_id":"s-1"}`)
	attach(t, router, sid)

	rec, ev, set := auditCall2(t, router, http.MethodPost,
		"/v1/node-agents/sessions/"+sid+"/messages",
		`{"content":`+mustJSON(t, pasted)+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionAgentMessageSend {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	blob, _ := json.Marshal(ev)
	for _, leak := range []string{pasted, "ghp_AAAABBBBCCCC", "hunter2"} {
		if strings.Contains(string(blob), leak) {
			t.Fatalf("the chain carries the instruction (or a credential pasted into it): %s", blob)
		}
	}
	p := payloadOf(t, ev)
	if p["content_len"] != len(pasted) {
		t.Fatalf("content_len = %v, want %d", p["content_len"], len(pasted))
	}
	if p["content_digest"] == "" || p["content_digest"] == nil {
		t.Fatalf("no digest: %v", p)
	}
	if p["steer"] != false {
		t.Fatalf("steer = %v, want false", p["steer"])
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 摘要要能区分两次不同的指令，也要认得出两次相同的——两个方向都要证。
func TestTheInstructionDigestDistinguishesAndIdentifies(t *testing.T) {
	router, _ := newAuditRouter(t)
	sid := openSession2(t, router, `{"edge_id":7,"session_id":"s-1"}`)
	attach(t, router, sid)
	send := func(content string) string {
		t.Helper()
		rec, ev, set := auditCall2(t, router, http.MethodPost,
			"/v1/node-agents/sessions/"+sid+"/messages",
			`{"content":`+mustJSON(t, content)+`}`)
		if rec.Code != http.StatusAccepted || !set {
			t.Fatalf("send %q: %d %s", content, rec.Code, rec.Body.String())
		}
		d, _ := payloadOf(t, ev)["content_digest"].(string)
		return d
	}
	if send("look at the disk") != send("look at the disk") {
		t.Fatal("the same instruction hashed two ways")
	}
	if send("look at the disk") == send("look at the memory") {
		t.Fatal("two different instructions hashed the same")
	}
}

// 急停与关会话都不能失败（一个 abort 一个本地句柄），所以它们永远写成功行。
// 一次没有留下任何东西的拆除，恰恰是事后只能靠「那行不见了」来重建的那种事。
func TestStopAndCloseAreOnTheRecord(t *testing.T) {
	router, fleet := newAuditRouter(t)
	sid := openSession2(t, router, `{"edge_id":7,"session_id":"s-1"}`)

	rec, ev, set := auditCall2(t, router, http.MethodPost, "/v1/node-agents/sessions/"+sid+"/stop", "")
	if rec.Code != http.StatusOK || !set || ev.Action != auditport.ActionAgentSessionStop {
		t.Fatalf("stop: code=%d ev=%+v set=%v", rec.Code, ev, set)
	}
	if fleet.aborts != 1 {
		t.Fatalf("aborts = %d, want the abort to have reached the node", fleet.aborts)
	}

	rec, ev, set = auditCall2(t, router, http.MethodDelete, "/v1/node-agents/sessions/"+sid, "")
	if rec.Code != http.StatusOK || !set || ev.Action != auditport.ActionAgentSessionClose {
		t.Fatalf("close: code=%d ev=%+v set=%v", rec.Code, ev, set)
	}
	if len(fleet.closed) != 1 || fleet.closed[0] != sid {
		t.Fatalf("closed = %v, want the session dropped", fleet.closed)
	}
}

// 开一个会话的那一行要说清「哪台机器、哪个模型、什么角色」——事故之后
// 第一个问题就是这三个，而它们没有一个能从 session id 里推出来。
func TestOpeningASessionNamesTheEdgeAndTheBrain(t *testing.T) {
	router, _ := newAuditRouter(t)
	rec, ev, set := auditCall2(t, router, http.MethodPost, "/v1/node-agents/sessions",
		`{"edge_id":7,"session_id":"s-1","role":"investigator","provider":"openai","model":"gpt-4o"}`)
	if rec.Code != http.StatusOK || !set || ev.Action != auditport.ActionAgentSessionOpen {
		t.Fatalf("open: code=%d ev=%+v set=%v", rec.Code, ev, set)
	}
	if ev.ResourceID != "s-1" {
		t.Fatalf("resource id = %q, want the session that was opened", ev.ResourceID)
	}
	p := payloadOf(t, ev)
	if p["edge_id"] != uint64(7) || p["role"] != "investigator" ||
		p["provider"] != "openai" || p["model"] != "gpt-4o" {
		t.Fatalf("payload = %v", p)
	}
}

// 节点打不开这个会话时，链上要留下一行失败。
func TestAFleetRefusalOnOpenIsAudited(t *testing.T) {
	router, fleet := newAuditRouter(t)
	fleet.openErr = context.DeadlineExceeded
	rec, ev, set := auditCall2(t, router, http.MethodPost, "/v1/node-agents/sessions", `{"edge_id":7}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("a refused open returned 200")
	}
	if !set || ev.Status != auditport.StatusFailure || ev.Action != auditport.ActionAgentSessionOpen {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if p := payloadOf(t, ev); p["edge_id"] != uint64(7) {
		t.Fatalf("payload = %v, want the edge even on the failure row", p)
	}
}
