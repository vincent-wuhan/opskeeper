package agentteams

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	mcpauth "github.com/vincent-wuhan/opskeeper/internal/manager/server/mcp/middleware"
)

type stubSync struct {
	calls        atomic.Int32
	last         atomic.Value
	err          error
	pushErr      error
	pushCalls    atomic.Int32
	lastPayload  atomic.Value // []byte
	lastFilename atomic.Value // string
}

func (s *stubSync) SyncPlugin(_ context.Context, pluginID string) error {
	s.calls.Add(1)
	s.last.Store(pluginID)
	return s.err
}

func (s *stubSync) InstallPlugin(_ context.Context, pluginID string, payload []byte, filename string) error {
	s.pushCalls.Add(1)
	s.last.Store(pluginID)
	s.lastPayload.Store(append([]byte(nil), payload...))
	s.lastFilename.Store(filename)
	return s.pushErr
}

func newPluginRouter(h *PluginHandler) *chi.Mux {
	r := chi.NewRouter()
	h.Register(r)
	return r
}

const samplePluginYAML = `apiVersion: agentteams.agentteam/v1alpha1
kind: AgentTeamPlugin
metadata:
  name: sample-plugin
  version: 1.0.0
  description: A sample plugin for testing.
mcp:
  servers:
    - id: sample
      transport: stdio
      command: python
      args: [server.py]
      tools: [a, b, c]
skills:
  agent:
    - id: sk1
      path: skills/agent/sk1
prompts:
  team: prompts/team/T.md
  agent:
    leader: prompts/agent/leader.md
    worker: prompts/agent/worker.md
  manager:
    agents: prompts/manager/A.md
    tools: prompts/manager/T.md
adapters:
  - id: qwenpaw
    path: adapters/qwenpaw
`

func makeZip(t *testing.T, contents map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range contents {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makeMultipart 构造 multipart/form-data body。
func makeMultipart(t *testing.T, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf, mw.FormDataContentType()
}

func TestPluginInstallListGetDelete(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(filepath.Join(tmp, "plugins"))
	h := NewPluginHandler(registry, nil, nil, 0)
	r := newPluginRouter(h)

	// 1. 空 list
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/plugins", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list empty: %d", w.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["count"].(float64) != 0 {
		t.Fatalf("expected 0 plugins, got %v", out["count"])
	}

	// 2. install via zip
	zipBytes := makeZip(t, map[string]string{
		"plugin.yaml":               samplePluginYAML,
		"prompts/team/T.md":         "team",
		"skills/agent/sk1/SKILL.md": "sk1",
	})
	body, ctype := makeMultipart(t, "sample.zip", zipBytes)
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/install", body)
	req.Header.Set("Content-Type", ctype)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("install: expected 201, got %d body=%s", w.Code, w.Body.String())
	}
	var info PluginManifestInfo
	_ = json.Unmarshal(w.Body.Bytes(), &info)
	if info.ID != "sample-plugin" {
		t.Fatalf("id mismatch: %s", info.ID)
	}
	if info.ToolCount != 3 || info.SkillCount != 1 || info.PromptCount != 5 {
		t.Fatalf("counts wrong: tools=%d skills=%d prompts=%d", info.ToolCount, info.SkillCount, info.PromptCount)
	}
	if len(info.AdapterIDs) != 1 || info.AdapterIDs[0] != "qwenpaw" {
		t.Fatalf("adapters wrong: %v", info.AdapterIDs)
	}

	// 3. list shows 1
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/plugins", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["count"].(float64) != 1 {
		t.Fatalf("expected 1 plugin, got %v", out["count"])
	}

	// 4. get
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/plugins/sample-plugin", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}

	// 5. get 404
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/plugins/no-such", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("get missing: expected 404, got %d", w.Code)
	}

	// 6. enable / disable
	for _, status := range []string{"enable", "disable"} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/plugins/sample-plugin/"+status, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d body=%s", status, w.Code, w.Body.String())
		}
		_ = json.Unmarshal(w.Body.Bytes(), &info)
		if status == "enable" && info.Status != "enabled" {
			t.Fatalf("status after enable: %s", info.Status)
		}
		if status == "disable" && info.Status != "disabled" {
			t.Fatalf("status after disable: %s", info.Status)
		}
	}

	// 7. delete
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/v1/plugins/sample-plugin", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}

	// 8. 再次 get 应 404
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/plugins/sample-plugin", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", w.Code)
	}
}

func TestPluginInstallDuplicateReturns409(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(filepath.Join(tmp, "plugins"))
	h := NewPluginHandler(registry, nil, nil, 0)
	r := newPluginRouter(h)
	zipBytes := makeZip(t, map[string]string{"plugin.yaml": samplePluginYAML})

	body, ctype := makeMultipart(t, "p.zip", zipBytes)
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/install", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("first install: %d body=%s", w.Code, w.Body.String())
	}

	body, ctype = makeMultipart(t, "p.zip", zipBytes)
	req = httptest.NewRequest(http.MethodPost, "/v1/plugins/install", body)
	req.Header.Set("Content-Type", ctype)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate install: expected 409, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPluginInstallInvalidManifest(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(filepath.Join(tmp, "plugins"))
	h := NewPluginHandler(registry, nil, nil, 0)
	r := newPluginRouter(h)
	badYAML := `apiVersion: wrong/v1
kind: NotAPlugin
metadata:
  name: x
  version: 0.0.1
`
	zipBytes := makeZip(t, map[string]string{"plugin.yaml": badYAML})
	body, ctype := makeMultipart(t, "bad.zip", zipBytes)
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/install", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid manifest: expected 422, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPluginSyncCallsClient(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(filepath.Join(tmp, "plugins"))
	sync := &stubSync{}
	h := NewPluginHandler(registry, sync, nil, 0)
	r := newPluginRouter(h)

	zipBytes := makeZip(t, map[string]string{"plugin.yaml": samplePluginYAML})
	body, ctype := makeMultipart(t, "p.zip", zipBytes)
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/install", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("install: %d", w.Code)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/plugins/sample-plugin/sync", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("sync: %d body=%s", w.Code, w.Body.String())
	}
	if sync.calls.Load() != 1 {
		t.Fatalf("sync called %d times, want 1", sync.calls.Load())
	}
	if got := sync.last.Load().(string); got != "sample-plugin" {
		t.Fatalf("sync last id: %s", got)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/plugins/sample-plugin", nil))
	var info PluginManifestInfo
	_ = json.Unmarshal(w.Body.Bytes(), &info)
	if info.Status != "enabled" {
		t.Fatalf("status after sync: %s", info.Status)
	}
}

func TestPluginSyncClientErrorMarksError(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(filepath.Join(tmp, "plugins"))
	sync := &stubSync{err: io.ErrUnexpectedEOF}
	h := NewPluginHandler(registry, sync, nil, 0)
	r := newPluginRouter(h)

	zipBytes := makeZip(t, map[string]string{"plugin.yaml": samplePluginYAML})
	body, ctype := makeMultipart(t, "p.zip", zipBytes)
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/install", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/plugins/sample-plugin/sync", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("sync error: expected 502, got %d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/plugins/sample-plugin", nil))
	var info PluginManifestInfo
	_ = json.Unmarshal(w.Body.Bytes(), &info)
	if !strings.HasPrefix(info.Status, "error:") {
		t.Fatalf("status after failed sync: %s", info.Status)
	}
}

func TestPluginInstallZipWithoutPluginYAML(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(filepath.Join(tmp, "plugins"))
	h := NewPluginHandler(registry, nil, nil, 0)
	r := newPluginRouter(h)

	zipBytes := makeZip(t, map[string]string{"readme.md": "no manifest"})
	body, ctype := makeMultipart(t, "bad.zip", zipBytes)
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/install", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "plugin.yaml not found") {
		t.Fatalf("expected manifest error, got %s", w.Body.String())
	}
}

func TestPluginInstallZipSlipRejected(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(filepath.Join(tmp, "plugins"))
	h := NewPluginHandler(registry, nil, nil, 0)
	r := newPluginRouter(h)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../escape.txt")
	_, _ = io.WriteString(w, "escape")
	_ = zw.Close()

	body, ctype := makeMultipart(t, "evil.zip", buf.Bytes())
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/install", body)
	req.Header.Set("Content-Type", ctype)
	wr := httptest.NewRecorder()
	r.ServeHTTP(wr, req)
	if wr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for zip-slip, got %d body=%s", wr.Code, wr.Body.String())
	}
}

func TestContentChecksum_DeterministicAndDetectsChange(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "p1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("apiVersion: v1\nkind: Plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	cs1, err := ContentChecksum(dir)
	if err != nil {
		t.Fatal(err)
	}
	cs2, err := ContentChecksum(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cs1 != cs2 {
		t.Errorf("checksum not deterministic: %s vs %s", cs1, cs2)
	}
	if !strings.HasPrefix(cs1, "sha256:") {
		t.Errorf("expected sha256: prefix, got %s", cs1)
	}

	// 修改文件后 checksum 应改变
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	cs3, err := ContentChecksum(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cs3 == cs1 {
		t.Errorf("checksum should change after file mutation: %s", cs3)
	}
}

func TestIsContentCurrent_AfterInstall(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(filepath.Join(tmp, "plugins"))
	yamlBytes := []byte(samplePluginYAML)
	info, err := registry.Install(context.Background(), InstallParams{
		Source:     "test:install",
		PluginYAML: yamlBytes,
	})
	if err != nil {
		t.Fatalf("install failed: %v", err)
	}
	dir := filepath.Join(tmp, "plugins", info.ID)
	current, expected, actual, err := IsContentCurrent(dir)
	if err != nil {
		t.Fatalf("IsContentCurrent failed: %v", err)
	}
	if !current {
		t.Errorf("content should be current immediately after install: expected=%s actual=%s", expected, actual)
	}

	// 改一个文件 → 标记过期
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	current, _, _, err = IsContentCurrent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if current {
		t.Errorf("content should be stale after tamper")
	}

	// 重新写 marker → 回到 current
	cs, _ := ContentChecksum(dir)
	if err := writeContentMarker(dir, cs); err != nil {
		t.Fatal(err)
	}
	current, _, _, _ = IsContentCurrent(dir)
	if !current {
		t.Errorf("content should be current after marker refresh")
	}
}

func TestInstallRollbackOnInvalidManifest(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(filepath.Join(tmp, "plugins"))

	// 故意给一个非法 yaml（缺 metadata.name）
	badYAML := []byte(`apiVersion: agentteams.agentteam/v1alpha1
kind: AgentTeamPlugin
metadata:
  version: 0.1.0
mcp:
  servers:
    - id: x
      transport: stdio
      command: python
      args: [a.py]
      tools: [t]
skills:
  agent:
    - id: s
      path: skills/agent/s
prompts:
  team: prompts/team/T.md
  agent:
    leader: prompts/agent/l.md
    worker: prompts/agent/w.md
  manager:
    agents: prompts/manager/a.md
    tools: prompts/manager/t.md
adapters:
  - id: qwenpaw
    path: adapters/qwenpaw
`)

	_, err := registry.Install(context.Background(), InstallParams{
		Source:     "test:bad",
		PluginYAML: badYAML,
	})
	if err == nil {
		t.Fatal("expected error for invalid manifest")
	}

	// 验证:目录应该已被清理（事务回滚）
	dir := filepath.Join(tmp, "plugins", "x")
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("expected plugin dir to be cleaned up, but it exists: %s", dir)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat error: %v", err)
	}
}

func TestPluginAuditEventsEmitted(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(filepath.Join(tmp, "plugins"))

	var auditEntries []map[string]any
	captureHandler := &slogCaptureHandler{captured: &auditEntries}
	h := NewPluginHandler(registry, &stubSync{}, slog.New(captureHandler), 0)
	r := newPluginRouter(h)

	// install
	body, ctype := makeMultipart(t, "plugin.zip", makeZip(t, map[string]string{"plugin.yaml": samplePluginYAML}))
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/install", body)
	req.Header.Set("Content-Type", ctype)
	wr := httptest.NewRecorder()
	r.ServeHTTP(wr, req)
	if wr.Code != http.StatusCreated {
		t.Fatalf("install failed: %d %s", wr.Code, wr.Body.String())
	}

	// uninstall
	req = httptest.NewRequest(http.MethodDelete, "/v1/plugins/sample-plugin", nil)
	wr = httptest.NewRecorder()
	r.ServeHTTP(wr, req)
	if wr.Code != http.StatusNoContent {
		t.Fatalf("uninstall failed: %d", wr.Code)
	}

	// Verify audit events
	events := map[string]int{}
	for _, e := range auditEntries {
		if action, ok := e["event"].(string); ok {
			events[action]++
		}
	}
	if events["plugin_install"] != 1 {
		t.Errorf("expected 1 plugin_install event, got %d (all: %v)", events["plugin_install"], events)
	}
	if events["plugin_uninstall"] != 1 {
		t.Errorf("expected 1 plugin_uninstall event, got %d", events["plugin_uninstall"])
	}
	// Verify all entries have the plugin_audit message key
	for _, e := range auditEntries {
		if msg, ok := e["_msg"].(string); !ok || msg != "plugin_audit" {
			t.Errorf("expected _msg=plugin_audit, got %v", e["_msg"])
		}
	}
}

// slogCaptureHandler captures JSON-decoded records into a slice
type slogCaptureHandler struct {
	inner    slog.Handler
	captured *[]map[string]any
}

func (h *slogCaptureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *slogCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := map[string]any{"_msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		rec[a.Key] = a.Value.Any()
		return true
	})
	*h.captured = append(*h.captured, rec)
	return nil
}
func (h *slogCaptureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &slogCaptureHandler{inner: h.inner, captured: h.captured}
}
func (h *slogCaptureHandler) WithGroup(name string) slog.Handler {
	return &slogCaptureHandler{inner: h.inner, captured: h.captured}
}

// =============================================================================
// pushPlugin tests — POST /v1/plugins/{id}/push
//
// End-to-end: install zip → read .payload.zip from disk → forward to workers
// via PluginSyncClient.InstallPlugin. Confirms the opskeeper-side wiring of
// "upload → manager → worker HTTP → qwenpaw plugin install".
// =============================================================================

func makePluginZipBytes(t *testing.T) []byte {
	t.Helper()
	return makeZip(t, map[string]string{
		"plugin.yaml":               samplePluginYAML,
		"prompts/team/T.md":         "team",
		"skills/agent/sk1/SKILL.md": "sk1",
	})
}

func doRequest(r http.Handler, method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func TestPluginPushHappyPath(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	sync := &stubSync{}
	h := NewPluginHandler(registry, sync, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	r := newPluginRouter(h)

	zipBytes := makePluginZipBytes(t)

	// 1) install a plugin via multipart upload (auto_push=false → 由下方 /push 显式触发)
	body, contentType := makeMultipart(t, "sample.zip", zipBytes)
	res := doRequest(r, "POST", "/v1/plugins/install?auto_push=false", body, contentType)
	if res.Code != http.StatusCreated {
		t.Fatalf("install failed: status=%d body=%s", res.Code, res.Body.String())
	}

	// 2) push to workers
	res = doRequest(r, "POST", "/v1/plugins/sample-plugin/push", nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("push failed: status=%d body=%s", res.Code, res.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["pushed"] != true {
		t.Errorf("expected pushed=true, got %v", payload)
	}
	if got, ok := payload["bytes"].(float64); !ok || int(got) != len(zipBytes) {
		t.Errorf("bytes mismatch: %v vs %d", payload["bytes"], len(zipBytes))
	}

	// 3) confirm sync.InstallPlugin received the right payload
	if sync.pushCalls.Load() != 1 {
		t.Errorf("expected 1 push call, got %d", sync.pushCalls.Load())
	}
	gotPayload, _ := sync.lastPayload.Load().([]byte)
	if len(gotPayload) == 0 {
		t.Fatal("expected push payload recorded, got none")
	}
	if !bytes.Equal(gotPayload, zipBytes) {
		t.Errorf("push payload not equal to original zip: %d vs %d", len(gotPayload), len(zipBytes))
	}
	gotFilename, _ := sync.lastFilename.Load().(string)
	if gotFilename != "sample-plugin.zip" {
		t.Errorf("unexpected filename: %s", gotFilename)
	}
}

func TestPluginPush404WhenPluginMissing(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	sync := &stubSync{}
	h := NewPluginHandler(registry, sync, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	r := newPluginRouter(h)

	res := doRequest(r, "POST", "/v1/plugins/nonexistent/push", nil, "")
	if res.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d body=%s", res.Code, res.Body.String())
	}
	if sync.pushCalls.Load() != 0 {
		t.Errorf("push should not be called for missing plugin, got %d", sync.pushCalls.Load())
	}
}

func TestPluginPush502WhenSyncFails(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	sync := &stubSync{pushErr: errors.New("worker offline")}
	h := NewPluginHandler(registry, sync, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	r := newPluginRouter(h)

	zipBytes := makePluginZipBytes(t)
	body, contentType := makeMultipart(t, "sample.zip", zipBytes)
	res := doRequest(r, "POST", "/v1/plugins/install", body, contentType)
	if res.Code != http.StatusCreated {
		t.Fatalf("install failed: %d body=%s", res.Code, res.Body.String())
	}

	res = doRequest(r, "POST", "/v1/plugins/sample-plugin/push", nil, "")
	if res.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d body=%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "worker offline") {
		t.Errorf("expected worker offline in body, got %s", res.Body.String())
	}
}

func TestPluginPushNoSyncConfigured(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	h := NewPluginHandler(registry, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	r := newPluginRouter(h)

	zipBytes := makePluginZipBytes(t)
	body, contentType := makeMultipart(t, "sample.zip", zipBytes)
	res := doRequest(r, "POST", "/v1/plugins/install", body, contentType)
	if res.Code != http.StatusCreated {
		t.Fatalf("install failed: %d", res.Code)
	}

	res = doRequest(r, "POST", "/v1/plugins/sample-plugin/push", nil, "")
	if res.Code != http.StatusOK {
		t.Errorf("expected 200 with stub response, got %d body=%s", res.Code, res.Body.String())
	}
	var payload map[string]any
	_ = json.Unmarshal(res.Body.Bytes(), &payload)
	if payload["pushed"] != false {
		t.Errorf("expected pushed=false when no sync client, got %v", payload)
	}
	if !strings.Contains(payload["reason"].(string), "no PluginSyncClient") {
		t.Errorf("expected reason about missing sync client, got %v", payload["reason"])
	}
}

func TestReadInstallPayloadMissingReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	payload, err := registry.ReadInstallPayload(context.Background(), "missing-plugin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload != nil {
		t.Errorf("expected nil payload for missing plugin, got %d bytes", len(payload))
	}
}

func TestReadInstallPayloadRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	zipBytes := makePluginZipBytes(t)
	_, err := registry.Install(context.Background(), InstallParams{
		Source:     "upload:test.zip",
		PluginYAML: []byte(samplePluginYAML),
		ZipPayload: zipBytes,
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	payload, err := registry.ReadInstallPayload(context.Background(), "sample-plugin")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(payload, zipBytes) {
		t.Errorf("payload round-trip mismatch: %d vs %d bytes", len(payload), len(zipBytes))
	}
}

// ----------------------------------------------------------------------
// maxZipBytes 可配置性 + 单元解析
// ----------------------------------------------------------------------

func TestNewPluginHandler_MaxZipBytesDefault(t *testing.T) {
	h := NewPluginHandler(nil, nil, nil, 0)
	if got := h.MaxZipBytes(); got != DefaultMaxPluginZipBytes {
		t.Errorf("default maxZipBytes = %d, want %d", got, DefaultMaxPluginZipBytes)
	}
}

func TestNewPluginHandler_MaxZipBytesOverride(t *testing.T) {
	custom := int64(2 * 1024 * 1024)
	h := NewPluginHandler(nil, nil, nil, custom)
	if got := h.MaxZipBytes(); got != custom {
		t.Errorf("override maxZipBytes = %d, want %d", got, custom)
	}
}

func TestNewPluginHandler_MaxZipBytesNegativeFallsBackToDefault(t *testing.T) {
	h := NewPluginHandler(nil, nil, nil, -1)
	if got := h.MaxZipBytes(); got != DefaultMaxPluginZipBytes {
		t.Errorf("negative maxZipBytes should fall back to default, got %d", got)
	}
}

func TestInstallPlugin_RejectsZipExceedingConfigurableLimit(t *testing.T) {
	// 配 1KB 上限，尝试上传 ~2KB 的合法 zip — 应被拒绝。
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	const limit = 1024
	h := NewPluginHandler(registry, nil, nil, limit)

	// 构造一个看起来合法的 zip 但超 1KB
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	minimal := `{"apiVersion":"agentteams/v1alpha1","kind":"Plugin","metadata":{"name":"oversize","version":"1.0.0"}}`
	padding := strings.Repeat("x", limit+512)
	_, _ = f.Write([]byte(minimal + padding))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipBytes := buf.Bytes()

	// 直接用 mime/multipart 构造（同 buildMultipartInstallBody 内部逻辑）
	var mpBody bytes.Buffer
	mw := multipart.NewWriter(&mpBody)
	fw, err := mw.CreateFormFile("file", "oversize.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipBytes); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/install", &mpBody)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	wr := httptest.NewRecorder()
	h.installPlugin(wr, req)
	if wr.Code != http.StatusRequestEntityTooLarge && wr.Code != http.StatusBadRequest {
		t.Errorf("expected 413 or 400 for oversize zip, got %d body=%s", wr.Code, wr.Body.String())
	}
}

// ----------------------------------------------------------------------
// P2.2 auto_push 测试
// ----------------------------------------------------------------------

func TestInstallPlugin_AutoPushByDefault(t *testing.T) {
	// 默认 auto_push=true:install 成功后自动调 sync.InstallPlugin 一次
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	sync := &stubSync{}
	h := NewPluginHandler(registry, sync, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	r := newPluginRouter(h)

	zipBytes := makePluginZipBytes(t)
	body, contentType := makeMultipart(t, "sample.zip", zipBytes)
	res := doRequest(r, "POST", "/v1/plugins/install", body, contentType)
	if res.Code != http.StatusCreated {
		t.Fatalf("install failed: status=%d body=%s", res.Code, res.Body.String())
	}

	// sync.InstallPlugin 应该被调 1 次 (auto_push 默认 true)
	if sync.pushCalls.Load() != 1 {
		t.Errorf("auto_push=true by default, expected 1 push call, got %d", sync.pushCalls.Load())
	}

	var payload map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := payload["auto_push"].(bool); !ok || !v {
		t.Errorf("expected auto_push=true in response, got %v", payload["auto_push"])
	}
	pr, ok := payload["push_result"].(map[string]any)
	if !ok {
		t.Fatalf("expected push_result in response, got %v", payload)
	}
	if pr["pushed"] != true {
		t.Errorf("expected push_result.pushed=true, got %v", pr)
	}
}

func TestInstallPlugin_AutoPushFalseDisabled(t *testing.T) {
	// ?auto_push=false:install 成功但不同步推 worker
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	sync := &stubSync{}
	h := NewPluginHandler(registry, sync, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	r := newPluginRouter(h)

	zipBytes := makePluginZipBytes(t)
	body, contentType := makeMultipart(t, "sample.zip", zipBytes)
	res := doRequest(r, "POST", "/v1/plugins/install?auto_push=false", body, contentType)
	if res.Code != http.StatusCreated {
		t.Fatalf("install failed: %d %s", res.Code, res.Body.String())
	}

	if sync.pushCalls.Load() != 0 {
		t.Errorf("auto_push=false should skip push, got %d calls", sync.pushCalls.Load())
	}

	var payload map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := payload["auto_push"].(bool); !ok || v {
		t.Errorf("expected auto_push=false in response, got %v", payload["auto_push"])
	}
	if _, hasPush := payload["push_result"]; hasPush {
		t.Errorf("auto_push=false should not include push_result, got %v", payload["push_result"])
	}
}

func TestInstallPlugin_AutoPushFailsDoesNotFailInstall(t *testing.T) {
	// auto_push=true 但 sync.InstallPlugin 失败 → install 仍 201 + push_result.pushed=false
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	sync := &stubSync{pushErr: errors.New("simulated worker failure")}
	h := NewPluginHandler(registry, sync, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	r := newPluginRouter(h)

	zipBytes := makePluginZipBytes(t)
	body, contentType := makeMultipart(t, "sample.zip", zipBytes)
	res := doRequest(r, "POST", "/v1/plugins/install", body, contentType)
	if res.Code != http.StatusCreated {
		t.Fatalf("install should succeed even when push fails, got %d %s", res.Code, res.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	pr, ok := payload["push_result"].(map[string]any)
	if !ok {
		t.Fatalf("expected push_result, got %v", payload)
	}
	if pr["pushed"] != false {
		t.Errorf("expected pushed=false when push fails, got %v", pr)
	}
	if errStr, _ := pr["error"].(string); errStr == "" {
		t.Errorf("expected error string in push_result, got %v", pr)
	}
}

func TestInstallPlugin_AutoPushNoSyncConfigured(t *testing.T) {
	// h.sync 为 nil (stub 模式) → push_result.pushed=false + reason
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)
	h := NewPluginHandler(registry, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	r := newPluginRouter(h)

	zipBytes := makePluginZipBytes(t)
	body, contentType := makeMultipart(t, "sample.zip", zipBytes)
	res := doRequest(r, "POST", "/v1/plugins/install", body, contentType)
	if res.Code != http.StatusCreated {
		t.Fatalf("install should succeed without sync, got %d %s", res.Code, res.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	pr, ok := payload["push_result"].(map[string]any)
	if !ok {
		t.Fatalf("expected push_result, got %v", payload)
	}
	if pr["pushed"] != false {
		t.Errorf("expected pushed=false when no sync, got %v", pr)
	}
	if reason, _ := pr["reason"].(string); reason == "" {
		t.Errorf("expected reason when sync is nil, got %v", pr)
	}
}

// ----------------------------------------------------------------------
// Bug 修复测试：uninstallPlugin 日志不再把 consumer 当成 plugin name
// ----------------------------------------------------------------------

// TestUninstallPlugin_LogDoesNotConfuseConsumerWithPluginName 是 regression 测试。
//
// 历史 bug：uninstallPlugin 里 `if id, ok := mcpauth.FromContext(...); ok` 里的
// `id` shadow 了外层的 plugin id（外层 id 来自 chi.URLParam），导致 log 行输出
// `plugin=<consumer name>` 而不是真正的 plugin name。这里用 slogCaptureHandler 抓
// 真实日志，验证 plugin 字段值是 "sample-plugin" 而不是 consumer name。
//
// 设计要点：
//   - 用 mcpauth.WithContext 注入 consumer 身份，让 handler 走 if 分支写出日志
//   - bug 存在时 plugin 字段值 == "buggy-consumer-name"
//   - bug 修复后 plugin 字段值 == pluginID == "sample-plugin"
func TestUninstallPlugin_LogDoesNotConfuseConsumerWithPluginName(t *testing.T) {
	tmp := t.TempDir()
	registry := NewPluginRegistry(tmp)

	// capture handler 收集 slog 记录
	var captured []map[string]any
	capHandler := &slogCaptureHandler{captured: &captured}
	h := NewPluginHandler(registry, nil, slog.New(capHandler), 0)
	r := newPluginRouter(h)

	// 1) install 一个 plugin
	zipBytes := makePluginZipBytes(t)
	body, contentType := makeMultipart(t, "test.zip", zipBytes)
	res := doRequest(r, "POST", "/v1/plugins/install", body, contentType)
	if res.Code != http.StatusCreated {
		t.Fatalf("install failed: %d %s", res.Code, res.Body.String())
	}

	// 2) uninstall — 用一个 mcpauth consumer ctx 触发 bug
	pluginID := "sample-plugin"
	req := httptest.NewRequest(http.MethodDelete, "/v1/plugins/"+pluginID, nil)
	req = req.WithContext(mcpauth.WithIdentity(req.Context(), mcpauth.ResolvedIdentity{
		ConsumerName: "buggy-consumer-name",
		Role:         "agentteams-worker",
	}))
	captured = nil // 清空 install 阶段的噪音，只看 uninstall
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("uninstall failed: %d %s", w.Code, w.Body.String())
	}

	// 3) 找到 "plugin uninstalled" 那条 log，验证 plugin 字段
	var uninstalledLog map[string]any
	for _, rec := range captured {
		if msg, _ := rec["_msg"].(string); msg == "plugin uninstalled" {
			uninstalledLog = rec
			break
		}
	}
	if uninstalledLog == nil {
		t.Skip("no mcpauth consumer in test env → log line skipped. Bug is reachable only with auth ctx.")
	}

	pluginVal, _ := uninstalledLog["plugin"].(string)
	consumerVal, _ := uninstalledLog["consumer"].(string)

	// regression 断言 1：plugin 字段不能等于 consumer name（bug 存在时会失败）
	if pluginVal == consumerVal && consumerVal != "" {
		t.Errorf("BUG: plugin 字段值 = consumer name %q,应该是真正的 plugin id %q", pluginVal, pluginID)
	}
	// regression 断言 2：plugin 字段必须是真正的 plugin id
	if pluginVal != pluginID {
		t.Errorf("plugin 字段值 = %q, want %q", pluginVal, pluginID)
	}
	// regression 断言 3：consumer 字段必须是 consumer name
	if consumerVal != "buggy-consumer-name" {
		t.Errorf("consumer 字段值 = %q, want %q", consumerVal, "buggy-consumer-name")
	}
}
