package gitartifact

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/model"
)

func newTestServer(t *testing.T) (*Server, *LinkerRegistry) {
	t.Helper()
	reg := NewLinkerRegistry()
	_ = reg.Register(NewPGQueryLinker())
	_ = reg.Register(NewRedisCmdLinker())
	_ = reg.Register(NewK8sImageLinker())
	_ = reg.Register(NewHTTPRouteLinker())
	return NewServer(reg, nil), reg
}

func TestServerHandler(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-link", strings.NewReader(`{"query":{}}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestServer_PostArtifact_Success(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	body := `{
		"artifact": {
			"repo_url": "https://github.com/example/order-svc",
			"commit": "abc123def456789012345678901234567890abcd",
			"branch": "main",
			"artifact_url": "s3://bucket/abc.tar.gz",
			"meta": {"build_id": "gh-12345"},
			"build_at": "2026-07-13T10:00:00Z"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/git-artifacts", strings.NewReader(body))
	req.Header.Set("X-GitArtifact-Version", "v0")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status=%d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-GitArtifact-Version") != "v0" {
		t.Error("missing protocol version header")
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data := resp["data"].(map[string]interface{})
	if data["index_status"] != "queued" {
		t.Errorf("index_status=%v, want queued", data["index_status"])
	}
}

func TestServer_PostArtifact_MissingBuildID(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	body := `{
		"artifact": {
			"repo_url": "https://github.com/example/x",
			"commit": "abc123def456789012345678901234567890abcd",
			"branch": "main",
			"artifact_url": "s3://bucket/x",
			"build_at": "2026-07-13T10:00:00Z"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/git-artifacts", strings.NewReader(body))
	req.Header.Set("X-GitArtifact-Version", "v0")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (missing build_id)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "build_id") {
		t.Errorf("expected build_id in error: %s", rec.Body.String())
	}
}

func TestServer_PostArtifact_Duplicate(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	body := `{
		"artifact": {
			"repo_url": "https://github.com/x",
			"commit": "abc123def456789012345678901234567890abcd",
			"branch": "main",
			"artifact_url": "s3://bucket/x",
			"meta": {"build_id": "gh-1"},
			"build_at": "2026-07-13T10:00:00Z"
		}
	}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/git-artifacts", strings.NewReader(body))
		req.Header.Set("X-GitArtifact-Version", "v0")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if i == 0 && rec.Code != http.StatusCreated {
			t.Fatalf("first POST: status=%d, want 201", rec.Code)
		}
		if i == 1 && rec.Code != http.StatusConflict {
			t.Errorf("second POST: status=%d, want 409", rec.Code)
		}
	}
}

func TestServer_GetArtifact_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/git-artifacts/nonexistent", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rec.Code)
	}
}

func TestServer_GetArtifact_Success(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	// 先 POST
	body := `{"artifact":{"repo_url":"https://x","commit":"abc123def456789012345678901234567890abcd","branch":"main","artifact_url":"s3://x","meta":{"build_id":"b1"},"build_at":"2026-07-13T10:00:00Z"}}`
	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/git-artifacts", strings.NewReader(body))
	postReq.Header.Set("X-GitArtifact-Version", "v0")
	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, postReq)

	var postResp map[string]interface{}
	json.Unmarshal(postRec.Body.Bytes(), &postResp)
	id := postResp["data"].(map[string]interface{})["id"].(string)

	// 再 GET
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/git-artifacts/"+id, nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Errorf("GET status=%d, want 200", getRec.Code)
	}
}

func TestServer_RuntimeLink_PGQueryHit(t *testing.T) {
	s, reg := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	// 预填索引
	func() *PGQueryLinker { l, _ := reg.Get(SymbolTypePGQuery); return l.(*PGQueryLinker) }().AddIndex(
		"SELECT * FROM orders", &LinkResult{
			Commit: "abc", FilePath: "queries.go", LineStart: 10, LineEnd: 20, Confidence: 0.95,
		})

	body := `{"query":{"symbol_type":"pg_query","symbol":"SELECT * FROM orders","tenant_id":1}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-link", strings.NewReader(body))
	req.Header.Set("X-GitArtifact-Version", "v0")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	link := data["link"].(map[string]interface{})
	if link["commit"] != "abc" {
		t.Errorf("commit=%v, want abc", link["commit"])
	}
	if link["file_path"] != "queries.go" {
		t.Errorf("file_path=%v, want queries.go", link["file_path"])
	}
}

func TestServer_RuntimeLink_LowConfidenceFlag(t *testing.T) {
	s, reg := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	// 预填低置信度索引
	func() *PGQueryLinker { l, _ := reg.Get(SymbolTypePGQuery); return l.(*PGQueryLinker) }().AddIndex(
		"SELECT unknown", &LinkResult{Commit: "abc", FilePath: "x", Confidence: 0.4},
	)

	body := `{"query":{"symbol_type":"pg_query","symbol":"SELECT unknown","tenant_id":1}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-link", strings.NewReader(body))
	req.Header.Set("X-GitArtifact-Version", "v0")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	link := data["link"].(map[string]interface{})
	if link["flag"] != "needs_human_confirm" {
		t.Errorf("expected flag=needs_human_confirm, got %v", link["flag"])
	}
}

func TestServer_RuntimeLink_NoMatch(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	body := `{"query":{"symbol_type":"pg_query","symbol":"SELECT nothing","tenant_id":1}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-link", strings.NewReader(body))
	req.Header.Set("X-GitArtifact-Version", "v0")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	if data["link"] != nil {
		t.Errorf("expected link=nil, got %v", data["link"])
	}
	if data["reason"] != "no_match_in_index" {
		t.Errorf("expected reason=no_match_in_index, got %v", data["reason"])
	}
}

func TestServer_MethodNotAllowed(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/git-artifacts", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/v1/git-artifacts: status=%d, want 405", rec.Code)
	}
}

func TestServer_UnsupportedProtocolVersion(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/git-artifacts", strings.NewReader(body))
	req.Header.Set("X-GitArtifact-Version", "v99")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (unsupported version)", rec.Code)
	}
}

func TestServer_InvalidJSON(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/git-artifacts", bytes.NewReader([]byte("not json")))
	req.Header.Set("X-GitArtifact-Version", "v0")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

// 测试 symbol 解析
func TestParseSymbolInput_PGQuery(t *testing.T) {
	in, err := parseSymbolInput(SymbolTypePGQuery, "SELECT * FROM x")
	if err != nil {
		t.Fatal(err)
	}
	q, ok := in.(PGQuery)
	if !ok || q.Query != "SELECT * FROM x" {
		t.Errorf("got %v", in)
	}
}

func TestParseSymbolInput_RedisCmd(t *testing.T) {
	in, _ := parseSymbolInput(SymbolTypeRedisCmd, "GET foo:bar")
	c, ok := in.(RedisCmd)
	if !ok || c.Cmd != "GET" || c.Key != "foo:bar" {
		t.Errorf("got %v", in)
	}
}

func TestParseSymbolInput_HTTPRoute(t *testing.T) {
	in, _ := parseSymbolInput(SymbolTypeHTTPRoute, "GET /orders/{id}")
	r, ok := in.(HTTPRoute)
	if !ok || r.Method != "GET" || r.Path != "/orders/{id}" {
		t.Errorf("got %v", in)
	}
}

func TestSimpleHash_Deterministic(t *testing.T) {
	h1 := simpleHash("abc|123")
	h2 := simpleHash("abc|123")
	if h1 != h2 {
		t.Errorf("hash not deterministic: %s vs %s", h1, h2)
	}
	h3 := simpleHash("abc|124")
	if h1 == h3 {
		t.Error("different inputs should produce different hashes")
	}
}

// 测试 buildIndex（异步）
func TestServer_BuildIndex(t *testing.T) {
	s, _ := newTestServer(t)
	a := &StoredArtifact{
		PublicID:    "ga-test",
		RepoURL:     "https://github.com/x",
		Commit:      "abc",
		Branch:      "main",
		ArtifactURL: "s3://x",
		Meta:        map[string]interface{}{"build_id": "b1"},
		BuildAt:     time.Now(),
		IndexStatus: "queued",
	}
	// 新 API：先 Put 入库，再按 publicID 触发 buildIndex
	if err := s.store.Put(context.Background(), a); err != nil {
		t.Fatalf("put: %v", err)
	}
	s.buildIndex(a.PublicID)
	got, err := s.store.Get(context.Background(), a.PublicID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.IndexStatus != model.IndexStatusCompleted {
		t.Errorf("IndexStatus=%s, want completed", got.IndexStatus)
	}
	if got.IndexedAt == nil {
		t.Error("IndexedAt not set")
	}
}
