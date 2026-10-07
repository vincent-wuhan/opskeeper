package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io"
	"log/slog"

	"gorm.io/gorm"

	managerbizaudit "github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	auditmodel "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
	managermiddleware "github.com/vincent-wuhan/opskeeper/core/domains/server/middleware"
	"github.com/vincent-wuhan/opskeeper/core/manager/higress"
)

// The gateway's chain can finally be asked whether it is intact (决策 328).
//
// 决策 324 gave this process a chain of its own, for a reason that still
// holds: it holds OPSKEEPER_JWT_SECRET, so letting it write the control
// plane's chain would let it forge control-plane audit. What 324 did not do
// is give anything a way to **verify** that chain — and the comment it left
// behind claimed there were two verifiers when there was one, on the other
// side of the system.
//
// **一条只写不验的链是装饰品。** It costs an HMAC per row and buys nothing,
// because anyone who can edit the database can recompute the digest of the
// row they edited — as long as nobody walks the chain and notices. So the
// test here is deliberately an attack test rather than a smoke test: it
// writes rows, **edits one of them in the database**, and asserts the
// gateway's own surface reports the break and the exact row. A verifier that
// only ever says "yes" would pass every other test in the repository.

// newChainedGateway is newWiredGateway plus the Chain the route needs, and
// plus the database handle the tamper step writes through.
func newChainedGateway(t *testing.T, hmacKey string) (http.Handler, *managerbizaudit.Usecase, *gorm.DB) {
	t.Helper()
	// The key is a parameter, not a constant. The first version set it here
	// unconditionally, which meant the "no key configured" case could not be
	// expressed — the fixture overwrote the very variable the test was about.
	// **一个夹具把自己的被测变量写死，被测的那条分支就不存在了。**
	t.Setenv("OPSKEEPER_HIGRESS_AUDIT_HMAC_KEY", hmacKey)
	// Exactly main.go's order: the store first, then the audit sink over
	// the store's own handle. The tamper step below writes through that
	// same handle, which is the point — "someone with database access" has
	// to be a real way in, not a simulated one.
	store, err := higress.NewStore(filepath.Join(t.TempDir(), "higress.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	sink, err := buildAuditSink(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildAuditSink: %v", err)
	}
	srv, err := higress.NewServer(higress.Config{
		Store:         store,
		JWTSecret:     []byte("test-secret"),
		AdminUser:     "admin",
		AdminPassword: "correct-password",
		CookieName:    "_hi_sess",
		CookieMaxAge:  time.Hour,
		Chain:         sink,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return managermiddleware.AuditMiddleware(sink)(srv.Routes()), sink, db
}

type chainReply struct {
	Enabled     bool   `json:"enabled"`
	Intact      *bool  `json:"intact"`
	HeadSeq     uint64 `json:"head_seq"`
	BrokenAtSeq uint64 `json:"broken_at_seq"`
	Reason      string `json:"reason"`
}

// adminCookie logs in the way an operator does and returns the cookie.
func adminCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := post(t, h, "/session/login", `{"username":"admin","password":"correct-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "_hi_sess" {
			return c
		}
	}
	t.Fatal("the login set no session cookie")
	return nil
}

// postAs is post plus the session cookie, because every mutating admin
// route is behind one and a test that forgot it would be measuring 401s.
func postAs(t *testing.T, h http.Handler, cookie *http.Cookie, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getChain(t *testing.T, h http.Handler, cookie *http.Cookie) chainReply {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/audit-chain", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an operator asking whether their records were altered must not get a 5xx either way: %s", rec.Code, rec.Body.String())
	}
	var out chainReply
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// 1. 一次真实的写入之后，这条链必须能回答「完整」。
func TestTheGatewayChainReportsItselfIntactAfterRealWrites(t *testing.T) {
	handler, _, _ := newChainedGateway(t, "test-hmac-key")
	cookie := adminCookie(t, handler)

	// A real mutating call, so the row comes from a handler through the
	// middleware rather than from a fixture.
	if rec := postAs(t, handler, cookie, "/admin/consumers", `{"name":"svc-a","apikey":"k-1"}`); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create consumer: status %d: %s", rec.Code, rec.Body.String())
	}

	got := getChain(t, handler, cookie)
	if !got.Enabled {
		t.Fatalf("the chain reports itself disabled although a key is configured: %q", got.Reason)
	}
	if got.Intact == nil {
		t.Fatalf("no verdict at all: %+v — \"could not check\" and \"checked and fine\" must not look alike", got)
	}
	if !*got.Intact {
		t.Fatalf("a freshly written chain reports itself broken at seq %d: %s", got.BrokenAtSeq, got.Reason)
	}
	if got.HeadSeq == 0 {
		t.Error("head_seq = 0 although rows were just written")
	}
}

// 2. **本刀的正身**：改一行，链必须说出来，而且说得出是哪一行。
func TestATamperedRowBreaksTheGatewayChainAndIsLocated(t *testing.T) {
	handler, _, db := newChainedGateway(t, "test-hmac-key")
	cookie := adminCookie(t, handler)
	if rec := postAs(t, handler, cookie, "/admin/consumers", `{"name":"svc-a","apikey":"k-1"}`); rec.Code/100 != 2 {
		t.Fatalf("create consumer: status %d: %s", rec.Code, rec.Body.String())
	}

	// The tamper: someone with database access rewrites history. The
	// digest is a keyed hash over every field, so a rewritten row cannot
	// keep its own Hash — which is the entire point of storing one.
	if err := db.Model(&auditmodel.Log{}).
		Where("seq = ?", 1).
		Update("resource_name", "svc-a-RENAMED").Error; err != nil {
		t.Fatalf("tamper: %v", err)
	}

	got := getChain(t, handler, cookie)
	if got.Intact == nil || *got.Intact {
		t.Fatalf("a rewritten row still verifies: %+v", got)
	}
	if got.BrokenAtSeq != 1 {
		t.Errorf("broken_at_seq = %d, want 1 — \"your audit is untrustworthy\" and \"untrustworthy from row 1\" call for different responses", got.BrokenAtSeq)
	}
	if got.Reason == "" {
		t.Error("no reason given for a broken chain")
	}
}

// 3. 没配密钥时要说「没开」，不能说「检查通过」——**从一次缺席里递出一张
// 健康证明，是这类端点最容易犯也最贵的错**。
func TestAChainWithNoKeySaysDisabledRatherThanIntact(t *testing.T) {
	handler, _, _ := newChainedGateway(t, "")
	cookie := adminCookie(t, handler)
	got := getChain(t, handler, cookie)
	if got.Enabled {
		t.Fatal("a chain with no key reports itself enabled")
	}
	if got.Intact != nil {
		t.Fatalf("intact = %v with no key configured; an absent check must not render as a passing one", *got.Intact)
	}
	if !strings.Contains(got.Reason, "HMAC_KEY") {
		t.Errorf("reason = %q, want it to name the variable the operator has to set", got.Reason)
	}
}

// 4. 这条路由在 /admin 底下，因为它回答的是「谁动过我的数据」。
func TestTheChainRouteIsBehindTheSession(t *testing.T) {
	handler, _, _ := newChainedGateway(t, "test-hmac-key")
	req := httptest.NewRequest(http.MethodGet, "/admin/audit-chain", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: chain state says who touched the data, so it is not public", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "intact") {
		t.Errorf("the refusal leaked the chain state: %s", rec.Body.String())
	}
}
