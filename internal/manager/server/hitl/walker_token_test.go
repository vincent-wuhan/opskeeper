package hitl

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// requireAdminOrWalkerToken：service token 等值放行；错值回落 admin 鉴权；
// 未配置 env 时行为与 requireAdmin 一致。
func TestWalkerTokenAuth(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		auth    string
		expect  bool // requireAdminOrWalkerToken 返回值（env 空时无 JWT 必然 false）
	}{
		{"token match", "s3cret", "Bearer s3cret", true},
		{"token mismatch", "s3cret", "Bearer wrong", false}, // 无 JWT → admin 鉴权失败
		{"missing header", "s3cret", "", false},
		{"non-bearer", "s3cret", "Basic s3cret", false},
		{"env unset", "", "Bearer s3cret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPSKEEPER_WALKER_TOKEN", tc.env)
			req := httptest.NewRequest(http.MethodPost, "/v1/hitl/proposals/x/decision", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			got := requireAdminOrWalkerToken(rec, req)
			if got != tc.expect {
				t.Fatalf("requireAdminOrWalkerToken = %v, want %v (status=%d)", got, tc.expect, rec.Code)
			}
			if !got && rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Fatalf("reject 应写 401/403，got %d", rec.Code)
			}
		})
	}
}
