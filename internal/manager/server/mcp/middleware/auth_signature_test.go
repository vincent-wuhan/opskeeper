package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// helper: 构造一个 ResolveConsumer 已知的 mock，key 形如 "<key>:<consumer>:<apiKeyId>:<role>"
func signatureMock() *mockHigress {
	return &mockHigress{
		resolved: map[string]struct {
			name, key, role string
		}{
			"opskey:opskeeper-investigator:ak-001:investigator": {
				name: "opskeeper-investigator",
				key:  "ak-001",
				role: "investigator",
			},
		},
	}
}

// helper: 拼一份合规签名头
func signForToken(token string, h http.Header, body []byte) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	bodySha := sha256.Sum256(body)
	bodyHex := hex.EncodeToString(bodySha[:])
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write([]byte(bodyHex))
	h.Set("X-Opskeeper-Timestamp", ts)
	h.Set("X-Opskeeper-Signature", hex.EncodeToString(mac.Sum(nil)))
	h.Set("X-Opskeeper-Body-SHA256", bodyHex)
}

// ----- F1+F2: 必填 HMAC 签名，缺失/伪造返 401 -----

func TestResolve_RequireSignature_MissingSig(t *testing.T) {
	h := signatureMock()
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = true

	req := newReqWithBearer("opskey:opskeeper-investigator:ak-001:investigator")
	// 不设 X-Opskeeper-Signature / X-Opskeeper-Body-SHA256 / X-Opskeeper-Timestamp
	_, err := a.Resolve(context.Background(), req.Header)
	if err != ErrSignatureMissing {
		t.Fatalf("expected ErrSignatureMissing, got %v", err)
	}
}

func TestResolve_RequireSignature_TamperedBody(t *testing.T) {
	h := signatureMock()
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = true

	req := newReqWithBearer("opskey:opskeeper-investigator:ak-001:investigator")
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"loop.investigate","arguments":{}}}`)
	signForToken("opskey:opskeeper-investigator:ak-001:investigator", req.Header, body)
	// 篡改 body 后摘要：服务器端从 header 拿到的 bodySha256 与签名中的不匹配
	req.Header.Set("X-Opskeeper-Body-SHA256", sha256Hex([]byte("tampered")))

	_, err := a.Resolve(context.Background(), req.Header)
	if err != ErrSignatureMismatch {
		t.Fatalf("expected ErrSignatureMismatch, got %v", err)
	}
}

func TestResolve_RequireSignature_OK(t *testing.T) {
	h := signatureMock()
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = true

	req := newReqWithBearer("opskey:opskeeper-investigator:ak-001:investigator")
	body := []byte(`{"jsonrpc":"2.0","id":1}`)
	signForToken("opskey:opskeeper-investigator:ak-001:investigator", req.Header, body)

	id, err := a.Resolve(context.Background(), req.Header)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if id.ConsumerName != "opskeeper-investigator" || id.Role != "investigator" {
		t.Fatalf("got %+v", id)
	}
}

func TestResolve_RequireSignature_CacheHitStillRequiresSignature(t *testing.T) {
	h := signatureMock()
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = true
	token := "opskey:opskeeper-investigator:ak-001:investigator"

	first := newReqWithBearer(token)
	signForToken(token, first.Header, []byte(`{"jsonrpc":"2.0","id":1}`))
	if _, err := a.Resolve(context.Background(), first.Header); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	replay := newReqWithBearer(token)
	if _, err := a.Resolve(context.Background(), replay.Header); err != ErrSignatureMissing {
		t.Fatalf("cached resolve without signature: expected ErrSignatureMissing, got %v", err)
	}
	if h.called != 1 {
		t.Fatalf("cached resolve should not call Higress again, calls=%d", h.called)
	}
}

// ----- F3: timestamp replay 防护 -----

func TestResolve_RequireSignature_StaleTimestamp(t *testing.T) {
	h := signatureMock()
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = true
	a.ReplayWindow = 300 * time.Second

	req := newReqWithBearer("opskey:opskeeper-investigator:ak-001:investigator")
	body := []byte(`{"jsonrpc":"2.0"}`)
	bodySha := sha256Hex(body)
	// 把 ts 拨到 1 小时前
	ts := strconv.FormatInt(time.Now().Add(-1*time.Hour).Unix(), 10)
	mac := hmac.New(sha256.New, []byte("opskey:opskeeper-investigator:ak-001:investigator"))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write([]byte(bodySha))
	req.Header.Set("X-Opskeeper-Timestamp", ts)
	req.Header.Set("X-Opskeeper-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Opskeeper-Body-SHA256", bodySha)

	_, err := a.Resolve(context.Background(), req.Header)
	if err != ErrTimestampSkew {
		t.Fatalf("expected ErrTimestampSkew, got %v", err)
	}
}

// ----- F4: key 角色后缀与 consumer.name 一致性 -----

func TestResolve_RequireSignature_RoleMismatch(t *testing.T) {
	// apiKey 第 4 段 role=bogus，但 Higress 返回 name=opskeeper-investigator → 不匹配
	h := &mockHigress{
		resolved: map[string]struct {
			name, key, role string
		}{
			"opskey:opskeeper-investigator:ak-001:bogus": {
				name: "opskeeper-investigator",
				key:  "ak-001",
				role: "investigator",
			},
		},
	}
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = false
	a.RequireSignature = true

	req := newReqWithBearer("opskey:opskeeper-investigator:ak-001:bogus")
	body := []byte(`{"jsonrpc":"2.0"}`)
	signForToken("opskey:opskeeper-investigator:ak-001:bogus", req.Header, body)

	_, err := a.Resolve(context.Background(), req.Header)
	if err != ErrRoleMismatch {
		t.Fatalf("expected ErrRoleMismatch, got %v", err)
	}
}

// ----- F5: tenant 一致性 -----

func TestResolve_RequireSignature_TenantMismatch(t *testing.T) {
	// apiKey 自带 "tenant-a" 前缀（不是 canonical opskeeper-），但 X-Opskeeper-Tenant=tenant-b
	h := &mockHigress{
		resolved: map[string]struct {
			name, key, role string
		}{
			"opskey:tenant-a-investigator:ak-001:investigator": {
				name: "tenant-a-investigator",
				key:  "ak-001",
				role: "investigator",
			},
		},
	}
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = true

	req := newReqWithBearer("opskey:tenant-a-investigator:ak-001:investigator")
	req.Header.Set("X-Opskeeper-Tenant", "tenant-b")
	body := []byte(`{"jsonrpc":"2.0"}`)
	signForToken("opskey:tenant-a-investigator:ak-001:investigator", req.Header, body)

	_, err := a.Resolve(context.Background(), req.Header)
	if err != ErrTenantMismatch {
		t.Fatalf("expected ErrTenantMismatch, got %v", err)
	}
}

func TestResolve_RequireSignature_CanonicalWorkerCannotOverrideTenant(t *testing.T) {
	a := NewAuthenticator(signatureMock(), nopLogger{})
	a.RequireSignature = true

	req := newReqWithBearer("opskey:opskeeper-investigator:ak-001:investigator")
	req.Header.Set("X-Opskeeper-Tenant", "tenant-a")
	signForToken("opskey:opskeeper-investigator:ak-001:investigator", req.Header, []byte(`{"jsonrpc":"2.0"}`))

	if _, err := a.Resolve(context.Background(), req.Header); err != ErrTenantMismatch {
		t.Fatalf("expected ErrTenantMismatch, got %v", err)
	}
}

func TestResolve_TenantConsistencyAppliesWithoutRequiredSignature(t *testing.T) {
	h := &mockHigress{
		resolved: map[string]struct {
			name, key, role string
		}{
			"opskey:tenant-a-investigator:ak-001:investigator": {
				name: "tenant-a-investigator", key: "ak-001", role: "investigator",
			},
			"opskey:unknown:ak-002:investigator": {
				name: "unknown", key: "ak-002", role: "investigator",
			},
		},
	}
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = false

	req := newReqWithBearer("opskey:tenant-a-investigator:ak-001:investigator")
	req.Header.Set("X-Opskeeper-Tenant", "tenant-a")
	identity, err := a.Resolve(context.Background(), req.Header)
	if err != nil {
		t.Fatalf("expected matching tenant to pass, got %v", err)
	}
	if identity.TenantID != "tenant-a" {
		t.Fatalf("expected consumer tenant tenant-a, got %+v", identity)
	}

	req = newReqWithBearer("opskey:unknown:ak-002:investigator")
	if _, err := a.Resolve(context.Background(), req.Header); err != ErrTenantMismatch {
		t.Fatalf("expected unresolved tenant to fail closed, got %v", err)
	}
}

func TestResolve_CacheHitStillEnforcesTenantConsistency(t *testing.T) {
	h := signatureMock()
	a := NewAuthenticator(h, nopLogger{})
	a.RequireSignature = false
	token := "opskey:opskeeper-investigator:ak-001:investigator"

	first := newReqWithBearer(token)
	if _, err := a.Resolve(context.Background(), first.Header); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	replay := newReqWithBearer(token)
	replay.Header.Set("X-Opskeeper-Tenant", "tenant-a")
	if _, err := a.Resolve(context.Background(), replay.Header); err != ErrTenantMismatch {
		t.Fatalf("cached tenant override: expected ErrTenantMismatch, got %v", err)
	}
	if h.called != 1 {
		t.Fatalf("cached resolve should not call Higress again, calls=%d", h.called)
	}
}

// ----- helpers -----

func newReqWithBearer(token string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/v1/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Opskeeper-Version", "v1")
	return r
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
