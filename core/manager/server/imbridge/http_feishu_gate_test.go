package imbridge_test

// S-3 of the marketplace-imbridge-fail-closed change: an ImApp whose
// encrypt_key is empty must not have its events processed. validate()
// already refuses empty keys on save, but legacy rows / direct DB edits /
// stream rows switched to webhook all bypass that gate, and with no key
// there is no signature to verify — so before this gate anyone who could
// POST the endpoint drove the agent (running, pre-S-2, as the superuser).
// These tests pin the three-posture truth table at the HTTP boundary and
// assert the bridge is unreachable in the failing postures.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	bizbridge "github.com/vincent-wuhan/opskeeper/core/manager/biz/imbridge"
	store "github.com/vincent-wuhan/opskeeper/core/manager/data/imbridge/store"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/imbridge"
	srvim "github.com/vincent-wuhan/opskeeper/core/manager/server/imbridge"
)

// recordingAgent fails the test if the bridge ever reaches it in a
// posture the test asserts must be closed.
type recordingAgent struct {
	mu    sync.Mutex
	calls int
}

func (a *recordingAgent) EnsureSession(context.Context, uint64, string) (string, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	return "sess-1", nil
}

func (a *recordingAgent) StreamMessage(_ context.Context, _ string, _ string, _ bizbridge.Emit) error {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	return nil
}

func (a *recordingAgent) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func newGateHandler(t *testing.T, encryptKey string) (*srvim.Handler, *recordingAgent) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repo := store.New(db)
	// Bypass UC validation deliberately: the whole point of the runtime
	// gate is to catch rows that did NOT come through the validate()
	// front door (legacy / direct DB writes).
	if err := db.Create(&model.ImApp{
		Provider:    model.ProviderFeishu,
		Mode:        model.ModeWebhook,
		Name:        "gate test app",
		AppID:       "cli_gate",
		AppSecret:   "s",
		VerifyToken: "vtok",
		EncryptKey:  encryptKey,
		Enabled:     true,
	}).Error; err != nil {
		t.Fatalf("seed app: %v", err)
	}
	ag := &recordingAgent{}
	bridge := bizbridge.NewBridge(repo, ag, 1, nil)
	return srvim.NewHandler(bridge, repo, bizbridge.NewUC(repo), nil), ag
}

// eventBody is a plaintext im.message.receive_v1 envelope — exactly what
// an attacker without the key would POST.
func eventBody(t *testing.T, text string) []byte {
	t.Helper()
	content, _ := json.Marshal(map[string]string{"text": text})
	env := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id":   "evt-" + time.Now().Format("150405.000000000"),
			"event_type": "im.message.receive_v1",
			"app_id":     "cli_gate",
		},
		"event": map[string]any{
			"message": map[string]any{
				"chat_id":      "oc_gate",
				"message_id":   "om_gate",
				"message_type": "text",
				"content":      string(content),
			},
			"sender": map[string]any{"sender_id": map[string]string{"open_id": "ou_attacker"}},
		},
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return b
}

func postEvent(t *testing.T, h *srvim.Handler, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	h.RegisterPublic(r)
	req := httptest.NewRequest(http.MethodPost, "/v1/im/feishu/events", strings.NewReader(string(body)))
	req.Header.Set("X-Lark-Source-App", "cli_gate")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func signFeishu(key, ts, nonce string, body []byte) string {
	hh := sha256.New()
	hh.Write([]byte(ts))
	hh.Write([]byte(nonce))
	hh.Write([]byte(key))
	hh.Write(body)
	return hex.EncodeToString(hh.Sum(nil))
}

func TestFeishuGate_EmptyEncryptKeyRejectedAndBridgeNotCalled(t *testing.T) {
	h, ag := newGateHandler(t, "")
	body := eventBody(t, "forge me an admin session")

	// No signature headers at all — an attacker without the key has none.
	rec := postEvent(t, h, nil, body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 (empty encrypt_key must fail closed)", rec.Code)
	}
	// The bridge dispatches on a goroutine; give the scheduler a beat,
	// then require zero agent reach-through. A flake here would mean the
	// gate raced the dispatch, which itself would be a finding.
	for i := 0; i < 50 && ag.count() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ag.count(); got != 0 {
		t.Fatalf("agent reached %d time(s) — the empty-key gate let an event through to dispatch", got)
	}
}

func TestFeishuGate_ValidSignaturePassesToBridge(t *testing.T) {
	const key = "gate-key-9"
	h, ag := newGateHandler(t, key)
	body := eventBody(t, "hello bot")
	ts, nonce := "1700000000", "n-1"
	rec := postEvent(t, h, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Request-Nonce":     nonce,
		"X-Lark-Signature":         signFeishu(key, ts, nonce, body),
	}, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (correct signature must pass) body=%s", rec.Code, rec.Body)
	}
	// The dispatch is async; wait for it rather than asserting instantly.
	for i := 0; i < 100 && ag.count() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ag.count(); got == 0 {
		t.Fatal("bridge never reached the agent — a correctly signed event was swallowed")
	}
}

func TestFeishuGate_BadSignatureRejected(t *testing.T) {
	h, ag := newGateHandler(t, "gate-key-9")
	body := eventBody(t, "tampered")
	ts, nonce := "1700000000", "n-1"
	rec := postEvent(t, h, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Request-Nonce":     nonce,
		"X-Lark-Signature":         signFeishu("wrong-key", ts, nonce, body),
	}, body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 (signature mismatch must be rejected)", rec.Code)
	}
	for i := 0; i < 50 && ag.count() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ag.count(); got != 0 {
		t.Fatalf("agent reached %d time(s) behind a bad signature", got)
	}
}

func TestFeishuGate_MissingSignatureHeadersRejected(t *testing.T) {
	h, ag := newGateHandler(t, "gate-key-9")
	body := eventBody(t, "no headers at all")
	rec := postEvent(t, h, nil, body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 (verification is now unconditional, so absent headers must fail)", rec.Code)
	}
	for i := 0; i < 50 && ag.count() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ag.count(); got != 0 {
		t.Fatalf("agent reached %d time(s) with no signature headers", got)
	}
}

// url_verification lands BEFORE app resolution, so it must keep working
// even while the gate is closed for events — an admin configuring the
// callback URL in the Feishu console cannot have filled the key yet, and
// rejecting the handshake would make the console show red forever.
func TestFeishuGate_UrlVerificationHandshakeUnaffected(t *testing.T) {
	h, _ := newGateHandler(t, "")
	rec := postEvent(t, h, nil, []byte(`{"type":"url_verification","challenge":"cha-123"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (handshake precedes the key gate)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cha-123") {
		t.Fatalf("challenge not echoed: %s", rec.Body)
	}
}
