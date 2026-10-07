package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	managerbizaudit "github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	"github.com/vincent-wuhan/opskeeper/core/manager/higress"
)

// 决策 330 的那一半：**被限流器挡下的请求，不能往链上写任何东西。**
//
// 这一条是整个改动里最容易做反的一条。一个很自然、看起来很有帮助的写法是
// 「把每一次限流也记下来」——于是链上有了更多关于攻击的细节。**而那恰恰是错的**：
// 限流器存在的理由是「一条不需要任何凭据的调用方不能按他发包的速度往一条
// append-only 的防篡改表里写行」，而一个把限流本身也写成行的限流器，是同一个
// 放大器换了个名字，只是速率低一点。
//
// 所以被挡下的请求走的是计数器与日志，不是链。这条用例把这件事钉住：
// 打 40 次失败登录，链上必须只多出 burst 那么多行，而不是 40 行。

func TestAThrottledLoginWritesNothingToTheChain(t *testing.T) {
	handler, sink, _ := newChainedGateway(t, "test-hmac-key")

	const attempts = 40
	throttled := 0
	for i := 0; i < attempts; i++ {
		req := httptest.NewRequest(http.MethodPost, "/session/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusTooManyRequests:
			throttled++
		case http.StatusUnauthorized:
		default:
			t.Fatalf("attempt %d: unexpected status %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	if throttled == 0 {
		t.Fatalf("%d attempts and not one was throttled: an unauthenticated caller can write this chain as fast as he can send packets", attempts)
	}

	rows, _, err := sink.List(context.Background(), managerbizaudit.ListFilters{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The first helper call in newChainedGateway does not log in, so the
	// chain starts empty and every row here is a login attempt.
	if len(rows) > higress.DefaultLoginBurst {
		t.Fatalf("%d rows in the chain after %d attempts (%d throttled): the limiter refused the request but the row still landed",
			len(rows), attempts, throttled)
	}
	if len(rows) == 0 {
		t.Fatal("no rows at all: the limiter is in front of the audit slot rather than behind the handler")
	}
	t.Logf("%d attempts, %d throttled, %d rows in the chain", attempts, throttled, len(rows))
}
