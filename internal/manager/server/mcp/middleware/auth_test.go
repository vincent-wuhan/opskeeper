package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type mockHigress struct {
	called   int
	resolved map[string]struct {
		name, key, role string
	}
}

func (m *mockHigress) ResolveConsumer(ctx context.Context, apiKey string) (string, string, string, error) {
	m.called++
	r, ok := m.resolved[apiKey]
	if !ok {
		return "", "", "", errNotFound
	}
	return r.name, r.key, r.role, nil
}

var errNotFound = &higressNotFoundError{}

type higressNotFoundError struct{}

func (e *higressNotFoundError) Error() string { return "consumer not found" }

type nopLogger struct{}

func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}

func TestAuthenticator_Middleware_MissingHeader(t *testing.T) {
	h := &mockHigress{}
	a := NewAuthenticator(h, nopLogger{})

	var sawIdentity bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); ok {
			sawIdentity = true
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := a.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if sawIdentity {
		t.Fatal("identity should not be set on missing auth")
	}
}

func TestAuthenticator_Middleware_RequiresIncidentEventAuth(t *testing.T) {
	handler := NewAuthenticator(&mockHigress{}, nopLogger{}).Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/events", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthenticator_ResolveAndCache(t *testing.T) {
	h := &mockHigress{
		resolved: map[string]struct {
			name, key, role string
		}{
			"tok-alice": {"tenant-a-investigator", "key-1", "worker"},
		},
	}
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = false // 老式 Bearer-only 流程，关闭完整性护栏

	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", nil)
	req.Header.Set("Authorization", "Bearer tok-alice")
	req.Header.Set("X-Opskeeper-Tenant", "tenant-a")

	id, err := a.Resolve(context.Background(), req.Header)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id.ConsumerName != "tenant-a-investigator" {
		t.Fatalf("expected consumer=tenant-a-investigator, got %q", id.ConsumerName)
	}
	if id.TenantID != "tenant-a" {
		t.Fatalf("expected tenant-a, got %q", id.TenantID)
	}

	// 第二次走 cache，Higress 不再被调
	id2, _ := a.Resolve(context.Background(), req.Header)
	if id2.ConsumerName != "tenant-a-investigator" {
		t.Fatalf("cache miss: %q", id2.ConsumerName)
	}
	if h.called != 1 {
		t.Fatalf("expected 1 Higress call (cached on 2nd), got %d", h.called)
	}
}

func TestAuthenticator_Middleware_BypassesAdminCRUD(t *testing.T) {
	h := &mockHigress{}
	a := NewAuthenticator(h, nopLogger{})

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// next handler 应当被调用，证明 auth bypass
		w.WriteHeader(http.StatusOK)
	})
	handler := a.Middleware(next)

	// /v1/mcp/servers 不需要 Bearer（admin CRUD 走原有 admin auth）
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp/servers", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/mcp/servers should bypass auth, got %d", w.Code)
	}
	if h.called != 0 {
		t.Fatalf("Higress should not be called for bypassed paths, called=%d", h.called)
	}

	// /v1/skills/* 不需要 Bearer（公开下载）
	req = httptest.NewRequest(http.MethodGet, "/v1/skills/opskeeper-alerter", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/skills/* should bypass auth, got %d", w.Code)
	}

	// /v1/state/{id} 需要 Bearer
	req = httptest.NewRequest(http.MethodGet, "/v1/state/incident-123", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/state/{id} without Bearer should be 401, got %d", w.Code)
	}

	// /v1/hitl/decide 需要 Bearer
	req = httptest.NewRequest(http.MethodPost, "/v1/hitl/decide", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/hitl/decide without Bearer should be 401, got %d", w.Code)
	}

	// /api/v1/knowledge/docs 是 plugin-native 写入口，也必须走 Bearer。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/docs", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1/knowledge/docs without Bearer should be 401, got %d", w.Code)
	}
}

func TestExtractTrace_W3CTraceparent(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	tc := ExtractTrace(h)
	if !tc.HasTrace() {
		t.Fatalf("expected HasTrace()=true for W3C traceparent")
	}
	if tc.TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("TraceID=%q", tc.TraceID)
	}
	if tc.SpanID != "b7ad6b7169203331" {
		t.Fatalf("SpanID=%q", tc.SpanID)
	}
	if tc.Raw == "" {
		t.Fatalf("Raw should be set to original traceparent")
	}
}

func TestExtractTrace_InvalidW3CFallsBack(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "garbage-not-w3c")
	h.Set("X-Trace-Id", "abc123def456abc123def456abc123de") // 32 hex
	h.Set("X-Span-Id", "7890123456789012")                  // 16 hex
	tc := ExtractTrace(h)
	if !tc.HasTrace() {
		t.Fatalf("should fall back to X-Trace-Id")
	}
	if tc.TraceID != "abc123def456abc123def456abc123de" {
		t.Fatalf("TraceID fallback=%q", tc.TraceID)
	}
	if tc.SpanID != "7890123456789012" {
		t.Fatalf("SpanID fallback=%q", tc.SpanID)
	}
}

func TestExtractTrace_ShortHexNormalized(t *testing.T) {
	// LoongSuite 旧协议可能传 16 字符短 trace id；前补 0 到 32 位。
	h := http.Header{}
	h.Set("X-Trace-Id", "abcdef1234")
	h.Set("X-Span-Id", "12345678")
	tc := ExtractTrace(h)
	if !tc.HasTrace() {
		t.Fatalf("expected HasTrace()=true")
	}
	wantTrace := "0000000000000000000000abcdef1234"
	if tc.TraceID != wantTrace {
		t.Fatalf("TraceID short-hex normalized=%q want %q", tc.TraceID, wantTrace)
	}
	wantSpan := "0000000012345678"
	if tc.SpanID != wantSpan {
		t.Fatalf("SpanID short-hex normalized=%q want %q", tc.SpanID, wantSpan)
	}
}

func TestExtractTrace_Empty(t *testing.T) {
	tc := ExtractTrace(http.Header{})
	if tc.HasTrace() {
		t.Fatalf("expected HasTrace()=false for empty header")
	}
}

func TestExtractTrace_NonHexRejected(t *testing.T) {
	h := http.Header{}
	h.Set("X-Trace-Id", "zzzz-not-hex")
	tc := ExtractTrace(h)
	if tc.HasTrace() {
		t.Fatalf("non-hex X-Trace-Id should be rejected (no trace context)")
	}
}

func TestAuthenticator_Middleware_PropagatesTrace(t *testing.T) {
	// Worker plugin stdio MCP server 带 W3C traceparent + 正确 Bearer，
	// backend middleware 应该把 ResolvedIdentity 和 TraceContext 都放进 ctx。
	h := &mockHigress{
		resolved: map[string]struct {
			name, key, role string
		}{"test-key": {"tenant-a-investigator", "ak1", "worker"}},
	}
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = false // 这个测试只验证 trace context 透传，不关心签名

	var sawIdentity, sawTrace bool
	var traceOut TraceContext
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); ok {
			sawIdentity = true
		}
		if tc, ok := TraceFromContext(r.Context()); ok && tc.HasTrace() {
			sawTrace = true
			traceOut = tc
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := a.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("X-Opskeeper-Tenant", "tenant-a")
	req.Header.Set("traceparent", "00-aaaa1111aaaa1111aaaa1111aaaa1111-bbbb2222bbbb2222-01")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if !sawIdentity {
		t.Fatal("identity should be in ctx")
	}
	if !sawTrace {
		t.Fatal("trace context should be in ctx")
	}
	if traceOut.TraceID != "aaaa1111aaaa1111aaaa1111aaaa1111" {
		t.Fatalf("TraceID=%q", traceOut.TraceID)
	}
}

func TestCanonicalAgentTeamsWorkerName(t *testing.T) {
	cases := []struct {
		consumer string
		role     string
		want     string
	}{
		{"manager", "manager", "opskeeper-manager"},
		{"worker", "worker", "opskeeper-worker"},
		{"admin", "admin", "opskeeper-admin"},
		{"worker-lumos", "worker", "worker-lumos"},
		{"opskeeper-investigator", "investigator", "opskeeper-investigator"},
	}
	for _, test := range cases {
		if got := canonicalAgentTeamsWorkerName(test.consumer, test.role); got != test.want {
			t.Errorf("canonicalAgentTeamsWorkerName(%q, %q) = %q, want %q", test.consumer, test.role, got, test.want)
		}
	}
}

func TestAllowedToolsForSuperWorkerIncludesDatabaseDiagnostics(t *testing.T) {
	tools := allowedToolsForRole("worker")
	found := false
	for _, tool := range tools {
		if tool == "analyze_database_status" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("worker derived allowlist excludes analyze_database_status: %v", tools)
	}
}

func TestCheckTenantConsistencyAgentTeamsSuperWorkers(t *testing.T) {
	cases := []struct {
		name       string
		tenantID   string
		wantTenant string
		wantErr    bool
	}{
		{name: "worker-lumos", tenantID: "default", wantTenant: "default"},
		{name: "worker-monitor", tenantID: "default", wantTenant: "default"},
		{name: "manager", tenantID: "default", wantTenant: "default"},
		{name: "worker-lumos", tenantID: "other", wantErr: true},
		{name: "tenant-a-investigator", tenantID: "tenant-a", wantTenant: "tenant-a"},
	}

	for _, test := range cases {
		got, err := checkTenantConsistency(test.name, test.tenantID)
		if test.wantErr {
			if err == nil {
				t.Fatalf("checkTenantConsistency(%q, %q) expected an error", test.name, test.tenantID)
			}
			continue
		}
		if err != nil {
			t.Fatalf("checkTenantConsistency(%q, %q) returned error: %v", test.name, test.tenantID, err)
		}
		if got != test.wantTenant {
			t.Fatalf("checkTenantConsistency(%q, %q) = %q, want %q", test.name, test.tenantID, got, test.wantTenant)
		}
	}
}
