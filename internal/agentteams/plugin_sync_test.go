package agentteams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerHTTPClient_AllSuccess(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/api/opskeeper-teamharness/sync" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing bearer token")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	c := NewWorkerHTTPClient(
		[]WorkerEndpoint{{WorkerName: "w1", BaseURL: srv.URL}},
		"test-token",
		nil,
	)
	if err := c.SyncPlugin(context.Background(), "opskeeper-teamharness"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected 1 hit, got %d", hits)
	}
}

func TestWorkerHTTPClient_PartialFailure(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer bad.Close()

	c := NewWorkerHTTPClient(
		[]WorkerEndpoint{
			{WorkerName: "w-good", BaseURL: ok.URL},
			{WorkerName: "w-bad", BaseURL: bad.URL},
		},
		"",
		nil,
	)
	err := c.SyncPlugin(context.Background(), "opskeeper-teamharness")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, err) { // sanity
		t.Logf("got expected error: %v", err)
	}
}

func TestWorkerHTTPClient_NoWorkers(t *testing.T) {
	c := NewWorkerHTTPClient(nil, "", nil)
	if err := c.SyncPlugin(context.Background(), "opskeeper-teamharness"); err != nil {
		t.Errorf("no workers should be no-op, got: %v", err)
	}
}

func TestWorkerHTTPClient_PluginPathOverride(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWorkerHTTPClient(
		[]WorkerEndpoint{
			{WorkerName: "w1", BaseURL: srv.URL, PluginPath: "/custom/sync"},
		},
		"",
		nil,
	)
	if err := c.SyncPlugin(context.Background(), "opskeeper-teamharness"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/custom/sync" {
		t.Errorf("expected /custom/sync, got %s", got)
	}
}

func TestMarshalUnmarshalWorkerEndpoints(t *testing.T) {
	in := []WorkerEndpoint{
		{WorkerName: "a", BaseURL: "http://a:8088", PluginPath: "/api/foo/sync"},
		{WorkerName: "b", BaseURL: "http://b:8088"},
	}
	data, err := MarshalWorkerEndpoints(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := UnmarshalWorkerEndpoints(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].WorkerName != "a" || out[1].BaseURL != "http://b:8088" {
		t.Errorf("roundtrip mismatch: %+v", out)
	}
}

func TestLoggingSyncClient_StubOK(t *testing.T) {
	c := &LoggingSyncClient{}
	if err := c.SyncPlugin(context.Background(), "any-plugin"); err != nil {
		t.Errorf("logging stub should always return nil: %v", err)
	}
}

// ensure http.Client default timeout applied
func TestWorkerHTTPClient_DefaultTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewWorkerHTTPClient(
		[]WorkerEndpoint{{WorkerName: "w", BaseURL: srv.URL}},
		"",
		nil,
	)
	if err := c.SyncPlugin(context.Background(), "opskeeper-teamharness"); err != nil {
		t.Errorf("default 5s timeout should be enough: %v", err)
	}
}

// =============================================================================
// InstallPlugin tests — Phase 4 worker-side install automation
// =============================================================================

func TestWorkerHTTPClient_InstallPlugin_AllSuccess(t *testing.T) {
	var hits int32
	var lastContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		lastContentType = r.Header.Get("Content-Type")
		if r.URL.Path != "/api/opskeeper-teamharness/install-plugin" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("get form file: %v", err)
		}
		defer file.Close()
		buf := make([]byte, header.Size)
		_, _ = file.Read(buf)
		if string(buf) != "zip-bytes-payload" {
			t.Errorf("unexpected payload: %s", string(buf))
		}
		if header.Filename != "opskeeper-teamharness.zip" {
			t.Errorf("unexpected filename: %s", header.Filename)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewWorkerHTTPClient([]WorkerEndpoint{{WorkerName: "w1", BaseURL: srv.URL}}, "tok", nil)
	if err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("zip-bytes-payload"), "opskeeper-teamharness.zip"); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected 1 hit, got %d", hits)
	}
	if !strings.Contains(lastContentType, "multipart/form-data") {
		t.Errorf("expected multipart content-type, got %q", lastContentType)
	}
}

func TestWorkerHTTPClient_InstallPlugin_PartialFailure(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("install failed: missing manifest"))
	}))
	defer bad.Close()

	c := NewWorkerHTTPClient([]WorkerEndpoint{
		{WorkerName: "w1", BaseURL: good.URL},
		{WorkerName: "w2", BaseURL: bad.URL},
	}, "", nil)
	err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("payload"), "p.zip")
	if err == nil {
		t.Fatal("expected partial failure error")
	}
	if !strings.Contains(err.Error(), "w2") {
		t.Errorf("error should mention failing worker w2: %v", err)
	}
	if !strings.Contains(err.Error(), "1/2 ok") {
		t.Errorf("error should report 1/2 ok: %v", err)
	}
}

func TestWorkerHTTPClient_InstallPlugin_NoWorkers(t *testing.T) {
	c := NewWorkerHTTPClient(nil, "", nil)
	if err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("x"), "x.zip"); err != nil {
		t.Errorf("expected no error with no workers, got %v", err)
	}
}

func TestWorkerHTTPClient_InstallPlugin_DefaultFilename(t *testing.T) {
	var gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, header, err := r.FormFile("file")
		if err == nil {
			gotFilename = header.Filename
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWorkerHTTPClient([]WorkerEndpoint{{WorkerName: "w1", BaseURL: srv.URL}}, "", nil)
	if err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("p"), ""); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if gotFilename != "opskeeper-teamharness.zip" {
		t.Errorf("default filename should be pluginID.zip, got %s", gotFilename)
	}
}

func TestWorkerHTTPClient_InstallPlugin_CustomPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWorkerHTTPClient([]WorkerEndpoint{
		{WorkerName: "w1", BaseURL: srv.URL, PluginPath: "/custom/install"},
	}, "", nil)
	if err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("p"), "p.zip"); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if gotPath != "/custom/install" {
		t.Errorf("custom path not respected: got %s", gotPath)
	}
}

func TestWorkerHTTPClient_InstallPlugin_BearerAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWorkerHTTPClient([]WorkerEndpoint{{WorkerName: "w1", BaseURL: srv.URL}}, "secret-token", nil)
	if err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("p"), "p.zip"); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("bearer not set: got %q", gotAuth)
	}
}

func TestLoggingSyncClient_InstallPlugin(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	c := &LoggingSyncClient{Log: logger}
	if err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("p"), "p.zip"); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "plugin install (stub)") {
		t.Errorf("expected stub log, got %s", logged)
	}
	if !strings.Contains(logged, "opskeeper-teamharness") {
		t.Errorf("expected plugin id in log, got %s", logged)
	}
}

func TestBuildMultipartInstallBody_RoundTrip(t *testing.T) {
	body, ct, err := buildMultipartInstallBody("pkg.zip", []byte("hello"))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(ct, "multipart/form-data; boundary=") {
		t.Errorf("unexpected content-type: %s", ct)
	}
	if !bytes.Contains(body, []byte(`name="file"`)) {
		t.Errorf("missing form field name")
	}
	if !bytes.Contains(body, []byte(`filename="pkg.zip"`)) {
		t.Errorf("missing filename")
	}
	if !bytes.Contains(body, []byte("hello")) {
		t.Errorf("missing payload")
	}
}

// ----------------------------------------------------------------------
// 重试相关测试
// ----------------------------------------------------------------------

// flaky-server helper：前 N 次返回 503，后续返回 200；记录调用次数。
type flakyInstallServer struct {
	hits       int32
	failFirst  int
	failStatus int
}

func (s *flakyInstallServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&s.hits, 1)
		if int(n) <= s.failFirst {
			w.WriteHeader(s.failStatus)
			_, _ = w.Write([]byte("simulated transient failure"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

func TestWorkerHTTPClient_InstallPlugin_RetryOn5xxThenSuccess(t *testing.T) {
	srv := &flakyInstallServer{failFirst: 2, failStatus: http.StatusServiceUnavailable}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := NewWorkerHTTPClient([]WorkerEndpoint{{WorkerName: "w1", BaseURL: ts.URL}}, "", nil)
	c.InstallRetryBaseDelay = 1 * time.Millisecond // 加速测试
	err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("p"), "p.zip")
	if err != nil {
		t.Fatalf("expected eventual success after retries, got %v", err)
	}
	if got := atomic.LoadInt32(&srv.hits); got != 3 {
		t.Errorf("expected 3 hits (1 + 2 retries), got %d", got)
	}
}

func TestWorkerHTTPClient_InstallPlugin_RetryOnNetworkThenSuccess(t *testing.T) {
	// 第一台 worker 关闭 -> network error -> 重试仍失败 -> 计入 failures
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close() // 立刻关闭 = connection refused
	// 第二台 worker 健康
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()

	c := NewWorkerHTTPClient([]WorkerEndpoint{
		{WorkerName: "dead", BaseURL: dead.URL},
		{WorkerName: "live", BaseURL: live.URL},
	}, "", nil)
	c.InstallRetryBaseDelay = 1 * time.Millisecond
	err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("p"), "p.zip")
	if err == nil {
		t.Fatal("expected partial failure from dead worker")
	}
	if !strings.Contains(err.Error(), "dead") {
		t.Errorf("error should mention dead worker, got %v", err)
	}
	if !strings.Contains(err.Error(), "1/2 ok") {
		t.Errorf("expected 1/2 ok, got %v", err)
	}
}

func TestWorkerHTTPClient_InstallPlugin_NoRetryOn4xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid manifest"))
	}))
	defer srv.Close()

	c := NewWorkerHTTPClient([]WorkerEndpoint{{WorkerName: "w1", BaseURL: srv.URL}}, "", nil)
	c.InstallRetryBaseDelay = 1 * time.Millisecond
	err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("p"), "p.zip")
	if err == nil {
		t.Fatal("expected 400 to surface as error")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("4xx should NOT retry, expected 1 hit, got %d", got)
	}
}

func TestWorkerHTTPClient_InstallPlugin_RetryOn429(t *testing.T) {
	srv := &flakyInstallServer{failFirst: 1, failStatus: http.StatusTooManyRequests}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := NewWorkerHTTPClient([]WorkerEndpoint{{WorkerName: "w1", BaseURL: ts.URL}}, "", nil)
	c.InstallRetryBaseDelay = 1 * time.Millisecond
	err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("p"), "p.zip")
	if err != nil {
		t.Fatalf("expected retry on 429, got %v", err)
	}
	if got := atomic.LoadInt32(&srv.hits); got != 2 {
		t.Errorf("expected 2 hits (1 fail + 1 retry success), got %d", got)
	}
}

func TestWorkerHTTPClient_InstallPlugin_ExhaustRetries(t *testing.T) {
	srv := &flakyInstallServer{failFirst: 100, failStatus: http.StatusBadGateway}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := NewWorkerHTTPClient([]WorkerEndpoint{{WorkerName: "w1", BaseURL: ts.URL}}, "", nil)
	c.InstallRetryBaseDelay = 1 * time.Millisecond
	err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("p"), "p.zip")
	if err == nil {
		t.Fatal("expected failure after retries exhausted")
	}
	if got := atomic.LoadInt32(&srv.hits); got != 3 {
		t.Errorf("expected 3 hits (initial + 2 retries), got %d", got)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error should report retry exhaustion, got %v", err)
	}
}

func TestInstallRetryDelay(t *testing.T) {
	tests := []struct {
		base time.Duration
		n    int
		want time.Duration
	}{
		{0, 1, 500 * time.Millisecond}, // base 0 -> default 500ms
		{100 * time.Millisecond, 1, 100 * time.Millisecond},
		{100 * time.Millisecond, 2, 200 * time.Millisecond},
		{100 * time.Millisecond, 3, 400 * time.Millisecond},
		{1 * time.Second, 10, 10 * time.Second}, // cap
	}
	for _, tt := range tests {
		got := installRetryDelay(tt.base, tt.n)
		if got != tt.want {
			t.Errorf("installRetryDelay(%v, %d) = %v, want %v", tt.base, tt.n, got, tt.want)
		}
	}
}

func TestIsTransientHTTPStatus(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{200, false},
		{201, false},
		{400, false},
		{404, false},
		{409, false},
		{422, false},
		{429, true}, // rate limit -> retry
		{500, true},
		{502, true},
		{503, true},
		{504, true},
	}
	for _, tt := range tests {
		got := isTransientHTTPStatus(tt.code)
		if got != tt.want {
			t.Errorf("isTransientHTTPStatus(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}

// ----------------------------------------------------------------------
// LocalHarnessSyncClient tests
// ----------------------------------------------------------------------

// fakeHarnessServer returns a minimal qwenpaw-style harness:
// GET /api/opskeeper-teamharness/health     → 200 OK
// POST /api/opskeeper-teamharness/sync      → 200 OK
// POST /api/opskeeper-teamharness/install-plugin → 200 OK
//
// hit log records every request line so tests can assert URL paths /
// bearer tokens.
type fakeHarnessServer struct {
	*httptest.Server
	hits []string
}

func newFakeHarnessServer() *fakeHarnessServer {
	f := &fakeHarnessServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/opskeeper-teamharness/health", func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/opskeeper-teamharness/sync", func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/opskeeper-teamharness/install-plugin", func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"installed":true}`))
	})
	f.Server = httptest.NewServer(mux)
	return f
}

func TestNewLocalHarnessSyncClient_EmptyURL(t *testing.T) {
	c, err := NewLocalHarnessSyncClient("", "", nil)
	if err == nil {
		t.Fatal("expected error on empty baseURL")
	}
	if c != nil {
		t.Errorf("expected nil client, got %+v", c)
	}
}

func TestNewLocalHarnessSyncClient_Defaults(t *testing.T) {
	c, err := NewLocalHarnessSyncClient("http://127.0.0.1:8088/", "tok", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BaseURL != "http://127.0.0.1:8088" {
		t.Errorf("trailing slash should be trimmed, got %q", c.BaseURL)
	}
	if c.PluginPath != "/api/opskeeper-teamharness/sync" {
		t.Errorf("default plugin path wrong: %q", c.PluginPath)
	}
	if c.InstallPath != "/api/opskeeper-teamharness/install-plugin" {
		t.Errorf("default install path wrong: %q", c.InstallPath)
	}
	if c.HealthPath != "/api/opskeeper-teamharness/health" {
		t.Errorf("default health path wrong: %q", c.HealthPath)
	}
}

func TestLocalHarnessSyncClient_HealthCheck_OK(t *testing.T) {
	f := newFakeHarnessServer()
	defer f.Close()
	c, err := NewLocalHarnessSyncClient(f.URL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.HealthCheck(context.Background()); err != nil {
		t.Fatalf("healthcheck failed: %v", err)
	}
	if len(f.hits) != 1 || f.hits[0] != "GET /api/opskeeper-teamharness/health" {
		t.Errorf("expected health hit, got %v", f.hits)
	}
}

func TestLocalHarnessSyncClient_HealthCheck_Unreachable(t *testing.T) {
	// Use a closed server to simulate unreachable harness.
	f := newFakeHarnessServer()
	url := f.URL
	f.Close() // immediately close
	c := &LocalHarnessSyncClient{
		BaseURL:    url,
		HealthPath: "/api/opskeeper-teamharness/health",
		Log:        slog.Default(),
		HTTPClient: &http.Client{Timeout: 500 * time.Millisecond},
	}
	if err := c.HealthCheck(context.Background()); err == nil {
		t.Fatal("expected error when harness closed")
	}
}

func TestLocalHarnessSyncClient_SyncPlugin_OK(t *testing.T) {
	f := newFakeHarnessServer()
	defer f.Close()
	c, err := NewLocalHarnessSyncClient(f.URL, "tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SyncPlugin(context.Background(), "opskeeper-teamharness"); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	found := false
	for _, h := range f.hits {
		if h == "POST /api/opskeeper-teamharness/sync" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected POST sync, hits=%v", f.hits)
	}
}

func TestLocalHarnessSyncClient_InstallPlugin_OK(t *testing.T) {
	f := newFakeHarnessServer()
	defer f.Close()
	c, err := NewLocalHarnessSyncClient(f.URL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("zip-bytes"), "teamharness.zip"); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	found := false
	for _, h := range f.hits {
		if h == "POST /api/opskeeper-teamharness/install-plugin" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected POST install, hits=%v", f.hits)
	}
}

func TestLocalHarnessSyncClient_InstallPlugin_DefaultFilename(t *testing.T) {
	f := newFakeHarnessServer()
	defer f.Close()
	c, err := NewLocalHarnessSyncClient(f.URL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.InstallPlugin(context.Background(), "opskeeper-teamharness", []byte("p"), ""); err != nil {
		t.Fatalf("install failed: %v", err)
	}
}

func TestProbeLocalHarness_Empty(t *testing.T) {
	ok, err := ProbeLocalHarness(context.Background(), "", "", time.Second)
	if ok {
		t.Error("expected ok=false for empty baseURL")
	}
	if err == nil {
		t.Error("expected non-nil error")
	}
}

func TestProbeLocalHarness_OK(t *testing.T) {
	f := newFakeHarnessServer()
	defer f.Close()
	ok, err := ProbeLocalHarness(context.Background(), f.URL, "", time.Second)
	if !ok {
		t.Errorf("expected ok=true, got err=%v", err)
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
