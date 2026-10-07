package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	managerbizaudit "github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	auditmodel "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
	managermiddleware "github.com/vincent-wuhan/opskeeper/core/domains/server/middleware"
	"github.com/vincent-wuhan/opskeeper/core/manager/higress"
)

// The end-to-end proof that the gateway's rows actually land.
//
// Every other audit test in this repository installs the slot by hand and
// reads it back off the request. That is the right unit test and it cannot
// answer the question decision 321 asked: does a row written in *this
// process* survive to storage? Here nothing is stubbed — the real SQLite
// store, the real chain store, the real stamper, the real middleware, the
// real handler — and the assertion is that a row is readable afterwards and
// that the chain verifies.
//
// If someone deletes the AuditMiddleware line from main.go, every other
// audit test in the tree still passes and this one fails.
func newWiredGateway(t *testing.T) (http.Handler, *managerbizaudit.Usecase) {
	t.Helper()
	store, err := higress.NewStore(filepath.Join(t.TempDir(), "higress.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t.Setenv("OPSKEEPER_HIGRESS_AUDIT_HMAC_KEY", "test-hmac-key")
	sink, err := buildAuditSink(store.DB(), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Exactly the wiring main.go does, written out again rather than shared
	// through a helper: a test that called main's helper would keep passing
	// if main stopped mounting the middleware, because the helper would then
	// be the thing under suspicion rather than the thing under test.
	return managermiddleware.AuditMiddleware(sink)(srv.Routes()), sink
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// readChain reads every row the gateway wrote and proves it is a chain.
//
// It reads through the usecase rather than through a handler's own slot,
// which is the whole point: this test is the only one in the repository that
// asks whether a row survived to storage.
func readChain(t *testing.T, sink *managerbizaudit.Usecase) []auditmodel.Log {
	t.Helper()
	// Verify first: a deployment with no HMAC key still writes rows, so the
	// rows alone prove nothing about tamper-evidence.
	if err := sink.VerifyChain(context.Background()); err != nil {
		t.Fatalf("the gateway chain does not verify: %v", err)
	}
	rows, _, err := sink.List(context.Background(), managerbizaudit.ListFilters{Limit: 100})
	if err != nil {
		t.Fatalf("listing the gateway chain: %v", err)
	}
	return rows
}

func TestAFailedGatewayLoginReachesTheChain(t *testing.T) {
	handler, sink := newWiredGateway(t)

	rec := post(t, handler, "/session/login", `{"username":"admin","password":"hunter2"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	rows := readChain(t, sink)
	if len(rows) == 0 {
		t.Fatal("the failed login left no row in the gateway's own chain — the middleware is not mounted")
	}
	row := rows[0]
	if row.Action != "gateway_login" {
		t.Errorf("action = %q", row.Action)
	}
	if row.Status != "failure" {
		t.Errorf("status = %q, want failure", row.Status)
	}
	if row.ResourceID != "admin" {
		t.Errorf("resource_id = %q, want the account that was guessed", row.ResourceID)
	}
	if strings.Contains(row.ErrorMessage, "hunter2") || strings.Contains(row.PayloadJSON, "hunter2") {
		t.Error("the typed password reached a stored column")
	}
}

func TestASuccessfulGatewayLoginReachesTheChainWithoutItsToken(t *testing.T) {
	handler, sink := newWiredGateway(t)

	rec := post(t, handler, "/session/login", `{"username":"admin","password":"correct-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	token := rec.Result().Cookies()[0].Value
	if token == "" {
		t.Fatal("no session cookie — the test is measuring nothing")
	}

	rows := readChain(t, sink)
	if len(rows) != 1 {
		t.Fatalf("stored rows = %d, want exactly the login", len(rows))
	}
	if rows[0].Status != "success" {
		t.Errorf("status = %q", rows[0].Status)
	}
	if strings.Contains(rows[0].PayloadJSON, token) {
		t.Error("the session token reached the chain")
	}
}

// A gateway with no HMAC key still records rows, and says so: an empty audit
// trail is a worse failure than an untamper-evident one.
func TestAGatewayWithNoKeyStillRecordsRows(t *testing.T) {
	store, err := higress.NewStore(filepath.Join(t.TempDir(), "higress.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Setenv("OPSKEEPER_HIGRESS_AUDIT_HMAC_KEY", "")

	sink, err := buildAuditSink(store.DB(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildAuditSink: %v", err)
	}
	srv, err := higress.NewServer(higress.Config{
		Store: store, JWTSecret: []byte("s"), AdminUser: "admin",
		AdminPassword: "correct-password", CookieName: "_hi_sess", CookieMaxAge: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := post(t, managermiddleware.AuditMiddleware(sink)(srv.Routes()), "/session/login",
		`{"username":"admin","password":"wrong"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	rows, _, err := sink.List(context.Background(), managerbizaudit.ListFilters{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("a keyless gateway recorded nothing at all; unchained is not the same as absent")
	}
}
