package hitl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 授权校验四分支：approved+窗口内放行；rejected/过期/无决议 fail-closed。
func TestCheckAuthorization(t *testing.T) {
	var decision string
	var windowTo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues"):
			if r.Method == "GET" {
				w.Write([]byte(`{"issues":[{"id":"i1","title":"[change-proposal] prp:cert-renewal:x.example:2026Q4 cert-renewal"}]}`))
				return
			}
		case strings.HasSuffix(r.URL.Path, "/issues/i1/comments"):
			body := "# ==== change-decision v1 ====\nschema: change-decision/1\ndecision: " + decision + "\nconstraints: { window: { from: 2026-10-01T00:00:00Z, to: " + windowTo + " }, max_duration_sec: null }\n# ==== end change-decision ===="
			b, _ := json.Marshal([]map[string]string{{"body": body}})
			w.Write(b)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	up := &PaperclipUplink{baseURL: srv.URL, token: "t", company: "c", client: srv.Client()}
	ex := NewCertExecutor(up, slogNop{})
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)

	decision, windowTo = "approved", "2026-12-31T00:00:00Z"
	if _, err := ex.CheckAuthorization(context.Background(), "x.example", now); err != nil {
		t.Fatalf("approved in-window should pass: %v", err)
	}
	decision = "rejected"
	if _, err := ex.CheckAuthorization(context.Background(), "x.example", now); err == nil {
		t.Fatal("rejected must fail-closed")
	}
	decision, windowTo = "approved", "2026-11-01T00:00:00Z"
	if _, err := ex.CheckAuthorization(context.Background(), "x.example", now); err == nil {
		t.Fatal("expired window must fail-closed")
	}
	decision = "approved"
	if _, err := ex.CheckAuthorization(context.Background(), "unknown.example", now); err == nil {
		t.Fatal("missing proposal must fail-closed")
	}
}

// 白名单：Renew 只调 certbot renew --cert-name + deploy-hook。
func TestRenewWhitelist(t *testing.T) {
	var called []string
	ex := &CertExecutor{certbot: "/bin/echo", reloadCmd: "true", log: slogNop{}}
	ex.run = func(_ context.Context, name string, args ...string) (string, error) {
		called = append([]string{name}, args...)
		return "ok", nil
	}
	if _, err := ex.Renew(context.Background(), "x.example"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(called, " ")
	if !strings.Contains(joined, "renew --cert-name x.example --deploy-hook") {
		t.Fatalf("unexpected command: %v", called)
	}
}

type slogNop struct{}

func (slogNop) Info(string, ...any) {}
func (slogNop) Warn(string, ...any) {}
