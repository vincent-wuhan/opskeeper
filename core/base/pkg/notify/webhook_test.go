package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 本文件覆盖响应体判定钩子（S-4）。
//
// 缺口 G-1：钉钉 / 飞书 / 企业微信在签名错、限流、机器人被收回时都回
// HTTP 200，只是 body 里的 errcode 非零。改动前 sender 只看状态码，
// 于是这三种故障全被记成"投递成功"——运维看到的现象是"消息再也不来了，
// 但系统说一切正常"。
//
// 本文件钉住三件事：
//   1. 三个聊天渠道读到非零错误码时报错，错误文本带上渠道给的原因；
//   2. 没有接到钩子的渠道（generic webhook / slack / telegram）行为逐位不变；
//   3. 请求本身的形状（body、query、签名）一个字没动——钩子只读响应。

// replyServer 起一个假渠道端点，回一个固定的 body，并记录收到的请求。
func replyServer(t *testing.T, status int, body string) (*httptest.Server, *http.Request, []byte) {
	t.Helper()
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, got, gotBody
}

// captureServer 起一个假渠道端点，把收到的请求存进一个可读的槽位。
func captureServer(t *testing.T, body string) (*httptest.Server, func() *http.Request, func() []byte) {
	t.Helper()
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() *http.Request { return got }, func() []byte { return gotBody }
}

// ── 三个聊天渠道：非零错误码判为失败 ────────────────────────────────────────

func TestDingTalkSenderRejectsANonZeroErrcode(t *testing.T) {
	srv, _, _ := replyServer(t, http.StatusOK, `{"errcode":310000,"errmsg":"sign not match"}`)

	s := NewDingTalkSender("dingtalk", srv.URL, "secret", srv.Client())
	err := s.Send(context.Background(), Message{Subject: "disk full", Severity: SeverityCritical})
	if err == nil {
		t.Fatal("a non-zero errcode must fail the send")
	}
	// 运维靠 errmsg 定位问题，所以原因必须进错误文本。
	if !strings.Contains(err.Error(), "310000") {
		t.Fatalf("err = %v; want the channel's error code", err)
	}
	if !strings.Contains(err.Error(), "sign not match") {
		t.Fatalf("err = %v; want the channel's reason", err)
	}
}

func TestFeishuSenderRejectsANonZeroCode(t *testing.T) {
	srv, _, _ := replyServer(t, http.StatusOK, `{"code":19021,"msg":"sign match fail"}`)

	s := NewFeishuSender("feishu", srv.URL, "secret", srv.Client())
	err := s.Send(context.Background(), Message{Subject: "disk full", Severity: SeverityCritical})
	if err == nil {
		t.Fatal("a non-zero code must fail the send")
	}
	if !strings.Contains(err.Error(), "19021") || !strings.Contains(err.Error(), "sign match fail") {
		t.Fatalf("err = %v; want the channel's code and reason", err)
	}
}

func TestWeComSenderRejectsANonZeroErrcode(t *testing.T) {
	srv, _, _ := replyServer(t, http.StatusOK, `{"errcode":60011,"errmsg":"no privilege to access"}`)

	s := NewWeComSender("wecom", srv.URL, srv.Client())
	err := s.Send(context.Background(), Message{Subject: "disk full", Severity: SeverityCritical})
	if err == nil {
		t.Fatal("a non-zero errcode must fail the send")
	}
	if !strings.Contains(err.Error(), "60011") || !strings.Contains(err.Error(), "no privilege to access") {
		t.Fatalf("err = %v; want the channel's code and reason", err)
	}
}

// 三个渠道的成功形状必须照旧通过——钩子不是把成功也变成失败。
func TestChatChannelsAcceptAZeroErrorCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"dingtalk", `{"errcode":0,"errmsg":"ok"}`},
		{"feishu", `{"code":0,"msg":"success"}`},
		{"feishu_legacy", `{"StatusCode":0,"StatusMessage":"success"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := replyServer(t, http.StatusOK, tc.body)
			var s Sender
			switch tc.name {
			case "dingtalk":
				s = NewDingTalkSender("dingtalk", srv.URL, "", srv.Client())
			default:
				s = NewFeishuSender("feishu", srv.URL, "", srv.Client())
			}
			if err := s.Send(context.Background(), Message{Subject: "ok", Severity: SeverityInfo}); err != nil {
				t.Fatalf("a zero error code must pass: %v", err)
			}
		})
	}
}

// 空 body 与不可解析的 body 都不是关于渠道的证据：有的接收方就回一个
// 空的 200。钩子不能把它们变成投递失败。
func TestChatChannelsAcceptAnEmptyOrUnreadableBody(t *testing.T) {
	for _, body := range []string{"", "   ", "not json at all"} {
		srv, _, _ := replyServer(t, http.StatusOK, body)
		s := NewDingTalkSender("dingtalk", srv.URL, "", srv.Client())
		if err := s.Send(context.Background(), Message{Subject: "ok", Severity: SeverityInfo}); err != nil {
			t.Fatalf("body %q must not be read as a channel failure: %v", body, err)
		}
	}
}

// 错误码也可以是带引号的数字——渠道不总是回一个 JSON number。
func TestChannelErrorCodeAcceptsAQuotedNumber(t *testing.T) {
	srv, _, _ := replyServer(t, http.StatusOK, `{"errcode":"310000","errmsg":"sign not match"}`)
	s := NewDingTalkSender("dingtalk", srv.URL, "", srv.Client())
	err := s.Send(context.Background(), Message{Subject: "x", Severity: SeverityInfo})
	if err == nil {
		t.Fatal("a quoted non-zero errcode must fail the send")
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Fatalf("err = %v; want the channel's error code", err)
	}
}

// ── 没接钩子的渠道：行为逐位不变 ────────────────────────────────────────────

func TestUnwiredChannelsIgnoreTheResponseBody(t *testing.T) {
	body := `{"errcode":310000,"errmsg":"sign not match"}`
	srv, _, _ := replyServer(t, http.StatusOK, body)

	cases := []struct {
		name string
		s    Sender
	}{
		{"generic_webhook", NewGenericWebhookSender("webhook", srv.URL, "", srv.Client())},
		{"slack", NewSlackSender("slack", srv.URL, srv.Client())},
		{"telegram", NewTelegramSender("telegram", srv.URL, "chat-1", srv.Client())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.s.Send(context.Background(), Message{Subject: "x", Severity: SeverityInfo}); err != nil {
				t.Fatalf("%s must not read the body: %v", tc.name, err)
			}
		})
	}
}

// 状态码判定没变：非 2xx 仍然是失败，与 body 无关。
func TestNonSuccessStatusStillFails(t *testing.T) {
	srv, _, _ := replyServer(t, http.StatusInternalServerError, `{"errcode":0,"errmsg":"ok"}`)
	s := NewDingTalkSender("dingtalk", srv.URL, "", srv.Client())
	err := s.Send(context.Background(), Message{Subject: "x", Severity: SeverityInfo})
	if err == nil {
		t.Fatal("a non-2xx status must fail the send")
	}
	if !strings.Contains(err.Error(), "unexpected status") {
		t.Fatalf("err = %v; want the status error", err)
	}
}

// ── 请求形状一个字没动 ──────────────────────────────────────────────────────

// 钩子只读响应。请求侧的 body、query、签名必须与改动前逐字相同——
// 否则已经按现行算法验签的接收方会在升级瞬间全部失败。
func TestWiredHookDoesNotChangeTheRequestShape(t *testing.T) {
	srv, getReq, getBody := captureServer(t, `{"errcode":0,"errmsg":"ok"}`)

	// 钉钉：签名走 URL query，body 里没有 timestamp / sign。
	s := NewDingTalkSender("dingtalk", srv.URL, "secret", srv.Client())
	if err := s.Send(context.Background(), Message{Subject: "x", Severity: SeverityInfo, Body: "b"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	req := getReq()
	if req == nil {
		t.Fatal("the fake endpoint received no request")
	}
	q, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if q.Get("timestamp") == "" || q.Get("sign") == "" {
		t.Fatalf("raw query = %q; want timestamp and sign", req.URL.RawQuery)
	}
	body := string(getBody())
	if strings.Contains(body, "timestamp") || strings.Contains(body, "\"sign\"") {
		t.Fatalf("body = %s; want the DingTalk signature to stay in the URL", body)
	}
	if !strings.Contains(body, `"msgtype":"text"`) {
		t.Fatalf("body = %s; want the DingTalk text payload", body)
	}

	// 飞书：签名走 body 顶层字段，URL 里没有签名。
	srv2, getReq2, getBody2 := captureServer(t, `{"code":0,"msg":"success"}`)
	s2 := NewFeishuSender("feishu", srv2.URL, "secret", srv2.Client())
	if err := s2.Send(context.Background(), Message{Subject: "x", Severity: SeverityInfo, Body: "b"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	req2 := getReq2()
	if req2 == nil {
		t.Fatal("the fake endpoint received no request")
	}
	if strings.Contains(req2.URL.RawQuery, "sign=") {
		t.Fatalf("raw query = %q; want the Feishu signature to stay in the body", req2.URL.RawQuery)
	}
	body2 := string(getBody2())
	if !strings.Contains(body2, `"timestamp"`) || !strings.Contains(body2, `"sign"`) {
		t.Fatalf("body = %s; want the Feishu signature in the body", body2)
	}
}
