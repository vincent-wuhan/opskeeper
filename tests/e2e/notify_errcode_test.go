//go:build e2e

// Catalog: G8 — 渠道在 200 里拒收：钉钉回 HTTP 200 + body 里非零
//
//	errcode 时，「测试通道」必须报 accepted=false，并把渠道给的错误码
//	与原因带回给运维。
//
// 改动前 sender 只看状态码，于是钉钉/飞书/企业微信在签名错、限流、
// 机器人被收回这三种故障下（它们都回 HTTP 200，只是 body 里的
// errcode 非零）全被记成「投递成功」——运维看到的现象是「消息再也不
// 来了，但系统说一切正常」。
//
// 这条用例验证 core/base/pkg/notify/webhook.go 的响应体判定钩子
// （channelAPIError）接进钉钉 sender 之后，manager 的这条既有通道
// （POST /v1/notification-channels/{id}/test）能把渠道的拒收如实报给
// 调用方。G4 的签名断言一个字没动——本文件只新增一条用例。
package e2e

import (
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/tests/e2e/testenv"
)

func TestNotify_ChannelErrcodeRejected_G8(t *testing.T) {
	env := testenv.Start(t)
	pair := env.LoginAdmin()

	channelID := createChannel(t, env, pair.AccessToken, channelSpec{
		name:     "e2e-dingtalk-errcode",
		channel:  "dingtalk",
		endpoint: env.FakeSlack().WebhookURL(),
		secret:   "e2e-secret-errcode",
	})

	// 渠道回 HTTP 200，但 body 里的 errcode 非零。
	env.FakeSlack().SetReply(&testenv.FakeReply{
		Status: 200,
		Body:   `{"errcode":310000,"errmsg":"sign not match"}`,
	})
	t.Cleanup(func() { env.FakeSlack().SetReply(nil) })

	body := fireTestSendBody(t, env, pair.AccessToken, channelID)
	if accepted, _ := body["accepted"].(bool); accepted {
		t.Fatalf("a channel that rejected the message must not be reported as accepted (body=%v)", body)
	}
	// 运维靠这两样定位问题：哪一条渠道、为什么被拒。
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "310000") {
		t.Errorf("message = %q; want the channel's error code", msg)
	}
	if !strings.Contains(msg, "sign not match") {
		t.Errorf("message = %q; want the channel's reason", msg)
	}

	// 渠道恢复正常后同一个通道必须又能测通——钩子不是把成功也变成失败。
	env.FakeSlack().SetReply(nil)
	body = fireTestSendBody(t, env, pair.AccessToken, channelID)
	if accepted, _ := body["accepted"].(bool); !accepted {
		t.Fatalf("a channel that accepted the message must be reported as accepted (body=%v)", body)
	}
}

// fireTestSendBody fires the channel test probe and returns the decoded
// response body. The existing fireTestSend helper asserts the status and
// throws the body away; this case needs the body, because the whole
// point is what the manager reports back about the channel's verdict.
func fireTestSendBody(t *testing.T, env *testenv.Env, bearer, channelID string) map[string]any {
	t.Helper()
	status, body, err := env.DoJSON("POST", "/api/v1/notification-channels/"+channelID+"/test", nil, bearer)
	if err != nil {
		t.Fatalf("test channel %s: %v", channelID, err)
	}
	if status != 200 {
		t.Fatalf("test channel %s: status=%d body=%v", channelID, status, body)
	}
	return body
}
