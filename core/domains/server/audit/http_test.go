package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	bizaudit "github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	store "github.com/vincent-wuhan/opskeeper/core/domains/data/audit/store"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
)

const testKey = "audit-http-test-key-0123456789"

func newTestHandler(t *testing.T, key string) (*Handler, *bizaudit.Usecase, *gorm.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	db, err := gorm.Open(sqlite.Open("file:"+path+"?_pragma=busy_timeout(5000)"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	uc := bizaudit.New(store.New(db), nil, bizaudit.WithChain(key, store.NewChainStore(db)))
	return NewHandler(uc), uc, db
}

func authedRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	h.Register(r)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := tenantctx.With(req.Context(), tenantctx.Tenant{UserID: 42, Role: "admin"})
		r.ServeHTTP(w, req.WithContext(ctx))
	})
}

func getChain(t *testing.T, router http.Handler) (int, chainResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit-logs/chain", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var out chainResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func emitRows(t *testing.T, uc *bizaudit.Usecase, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		uc.Emit(context.Background(), bizaudit.Event{
			UserEmail: "op@example.com", Role: "admin", IP: "10.0.0.7",
			Action: "device_update", ResourceType: "device", ResourceID: "dev-1",
			ResourceName: "web-01", Status: "success", RequestID: "req-1",
		})
	}
}

func TestChainEndpointReportsIntactLedger(t *testing.T) {
	h, uc, _ := newTestHandler(t, testKey)
	emitRows(t, uc, 3)

	code, out := getChain(t, authedRouter(h))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !out.Enabled {
		t.Fatal("enabled = false with a key configured")
	}
	if out.Intact == nil || !*out.Intact {
		t.Fatalf("intact = %v, want true", out.Intact)
	}
	if out.HeadSeq != 3 {
		t.Errorf("head_seq = %d, want 3", out.HeadSeq)
	}
	if out.AnchorSeq != 1 {
		t.Errorf("anchor_seq = %d, want 1", out.AnchorSeq)
	}
}

func TestChainEndpointReportsTampering(t *testing.T) {
	h, uc, db := newTestHandler(t, testKey)
	emitRows(t, uc, 3)

	if err := db.Model(&model.Log{}).Where("seq = ?", 2).
		Update("resource_name", "web-01-pwned").Error; err != nil {
		t.Fatalf("tamper: %v", err)
	}
	code, out := getChain(t, authedRouter(h))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a broken chain is an answer, not a server error", code)
	}
	if out.Intact == nil || *out.Intact {
		t.Fatalf("intact = %v, want false", out.Intact)
	}
	if out.BrokenAtSeq != 2 {
		t.Errorf("broken_at_seq = %d, want 2", out.BrokenAtSeq)
	}
	if out.Reason == "" {
		t.Error("reason is empty; an operator cannot act on a bare flag")
	}
}

// TestChainEndpointReportsDisabledAsNullNotTrue is the distinction the
// response shape exists for: a deployment with no key must not render as
// a verified trail.
func TestChainEndpointReportsDisabledAsNullNotTrue(t *testing.T) {
	h, uc, _ := newTestHandler(t, "")
	emitRows(t, uc, 2)

	code, out := getChain(t, authedRouter(h))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out.Enabled {
		t.Error("enabled = true with no key")
	}
	if out.Intact != nil {
		t.Errorf("intact = %v, want null — an absent chain is not an intact chain", *out.Intact)
	}
	if out.Reason == "" {
		t.Error("reason is empty; the operator is not told why the trail is unverifiable")
	}
}

func TestChainEndpointRequiresAuthentication(t *testing.T) {
	h, _, _ := newTestHandler(t, testKey)
	r := chi.NewRouter()
	h.Register(r)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit-logs/chain", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req) // no tenant on the context
	if rec.Code == http.StatusOK {
		t.Fatal("chain status answered an unauthenticated request")
	}
}

func TestChainEndpointIsNotItselfAudited(t *testing.T) {
	// Polling the status must not add rows. A self-auditing status
	// endpoint changes the chain it reports on every poll, which makes
	// head_seq a function of how often somebody looked.
	h, uc, db := newTestHandler(t, testKey)
	emitRows(t, uc, 1)
	router := authedRouter(h)

	for i := 0; i < 3; i++ {
		if code, _ := getChain(t, router); code != http.StatusOK {
			t.Fatalf("poll %d: status = %d", i, code)
		}
	}
	var total int64
	if err := db.Model(&model.Log{}).Count(&total).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Errorf("audit rows = %d, want 1 — polling must not write", total)
	}
}
