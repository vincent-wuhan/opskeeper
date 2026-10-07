package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizaudit "github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	auditmodel "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
)

// recordingRepo is the writer's seam. The point of the test below is the
// path from a handler's SetAuditEvent to the row, so the row has to land
// somewhere observable, and an in-memory repo is the only honest way to
// observe it without a database.
type recordingRepo struct{ rows []*auditmodel.Log }

func (r *recordingRepo) Insert(_ context.Context, row *auditmodel.Log) error {
	r.rows = append(r.rows, row)
	return nil
}

func (r *recordingRepo) List(context.Context, bizaudit.ListFilters) ([]auditmodel.Log, int64, error) {
	return nil, 0, nil
}

func (r *recordingRepo) DeleteOlderThan(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func newAuditStack(t *testing.T, repo bizaudit.Repo, handler http.Handler) http.Handler {
	t.Helper()
	uc := bizaudit.New(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// rewrap stands in for auth / tenant / otel, each of which re-wraps
	// the request on its way down. They are the reason the audit slot is a
	// pointer in the context, so a stack without them would not exercise
	// the property being claimed.
	rewrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), struct{ mid string }{"tenant"}, "t-1")
			ctx = context.WithValue(ctx, struct{ mid string }{"otel"}, "s-1")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	return chimw.RequestID(AuditMiddleware(uc)(rewrap(handler)))
}

// TestTheRowAHandlerAsksForIsTheRowTheLedgerGets is the end-to-end claim
// behind moving the shape into a port: a handler in a bounded context that
// knows nothing about the ledger can still get a row into it, and the row
// carries what the handler set plus what only the host can know.
//
// The handler below sets an action, a resource and an actor, and nothing
// else. The status, the client IP and the request id are the middleware's
// to fill, which is why a handler cannot be trusted to write them: it has
// no way to see the response.
func TestTheRowAHandlerAsksForIsTheRowTheLedgerGets(t *testing.T) {
	repo := &recordingRepo{}
	uid := uint64(7)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auditport.SetAuditEvent(r, auditport.Event{
			UserID:       &uid,
			Action:       auditport.ActionUserCreate,
			ResourceType: auditport.ResourceUser,
			ResourceID:   "u-7",
			Payload:      map[string]string{"field": "role", "to": "admin"},
		})
		w.WriteHeader(http.StatusCreated)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/iam/users", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	// The id an upstream proxy already stamped: the row has to carry the
	// same one the request's log lines do, or joining them is guesswork.
	req.Header.Set(chimw.RequestIDHeader, "req-1")
	newAuditStack(t, repo, handler).ServeHTTP(httptest.NewRecorder(), req)

	if len(repo.rows) != 1 {
		t.Fatalf("expected exactly one row, got %d", len(repo.rows))
	}
	row := repo.rows[0]
	if row.Action != auditport.ActionUserCreate {
		t.Errorf("action = %q, want the one the handler set", row.Action)
	}
	if row.ResourceType != auditport.ResourceUser || row.ResourceID != "u-7" {
		t.Errorf("resource = %q/%q, want user/u-7", row.ResourceType, row.ResourceID)
	}
	if row.UserID == nil || *row.UserID != uid {
		t.Errorf("actor = %v, want the one the handler set", row.UserID)
	}
	if row.Status != auditmodel.StatusSuccess {
		t.Errorf("status = %q, want the middleware to bucket 201 as success", row.Status)
	}
	// First XFF hop only: the rest of the chain is our own proxies, and
	// recording them as the client would make the trail useless.
	if row.IP != "203.0.113.7" {
		t.Errorf("ip = %q, want the first XFF hop", row.IP)
	}
	if row.RequestID != "req-1" {
		t.Errorf("request id = %q, want the id stamped on the request's log lines", row.RequestID)
	}
	if row.OccurredAt.IsZero() {
		t.Error("occurred_at was not stamped by the writer")
	}
	if row.PayloadJSON == "" {
		t.Error("payload was dropped; the row no longer says what changed")
	}
}

// TestAnUnannotatedRequestIsNotAudited holds the curation rule this
// middleware was written around: audit_logs is a trail of user-meaningful
// actions, not an access log. A GET that nobody annotated must not become a
// row, or the signal drowns and the retention job spends its budget on
// noise.
func TestAnUnannotatedRequestIsNotAudited(t *testing.T) {
	repo := &recordingRepo{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	newAuditStack(t, repo, handler).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/api/v1/iam/users", nil))

	if len(repo.rows) != 0 {
		t.Fatalf("an unannotated request produced %d rows: %+v", len(repo.rows), repo.rows[0])
	}
}

// TestAFailingRequestIsAuditedAsAFailure is the other half of the status
// bucket. A handler that annotates and then fails must not leave a
// success-shaped row behind, because the status is decided by the response
// the handler never sees.
func TestAFailingRequestIsAuditedAsAFailure(t *testing.T) {
	repo := &recordingRepo{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auditport.SetAuditEvent(r, auditport.Event{
			Action:       auditport.ActionUserDelete,
			ResourceType: auditport.ResourceUser,
			ResourceID:   "u-9",
		})
		w.WriteHeader(http.StatusInternalServerError)
	})
	newAuditStack(t, repo, handler).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodDelete, "/api/v1/iam/users/u-9", nil))

	if len(repo.rows) != 1 {
		t.Fatalf("expected exactly one row, got %d", len(repo.rows))
	}
	if got := repo.rows[0].Status; got != auditmodel.StatusFailure {
		t.Errorf("status = %q, want failure for a 500", got)
	}
}

// --- 决策 333：一个请求可以产生多行 ------------------------------------------------
//
// 批量升级一次动五百台机器。审计槽里原本只有一行，于是「哪台没升上去」这个问题
// 只能靠不带摘要、不跟链走的日志去回答——**而审计链存在的全部意义，就是那些日志
// 回答不了的问题**。
//
// 下面这条用例钉住四件事，缺一件这条能力就没意义：
//
//	追加行真的落到链上（不是攒在内存里）；
//	主行仍然只落一行（否则一次点按变成两行）；
//	只有追加行、没有主行的请求也能落（处理器不必硬造一条主行）；
//	**每一行各自带 actor** —— 五百行里只有一行能回答「谁干的」，那五百行就是噪声。

func TestABatchRequestLandsOneRowPerThingItChanged(t *testing.T) {
	repo := &recordingRepo{}
	// The actor arrives the way it does in production: auth middleware
	// mutates the tenant slot, and the audit middleware fills it into every
	// row afterwards. Setting it here rather than in the handler is the whole
	// point — **if the handler had to name the actor, "each row carries it"
	// would be trivially true and prove nothing.**
	stack := newAuditStack(t, repo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantctx.SetOnSlot(r.Context(), tenantctx.Tenant{UserID: 42, Email: "ops@example.com", Role: "admin"})
		auditport.SetAuditEvent(r, auditport.Event{
			Action:       auditport.ActionEdgePackageUpgrade,
			ResourceType: auditport.ResourceEdge,
			ResourceID:   "0",
			Status:       auditport.StatusSuccess,
			Payload:      map[string]any{"scope": "batch"},
		})
		for _, id := range []string{"11", "12", "13"} {
			auditport.AddAuditEvent(r, auditport.Event{
				Action:       auditport.ActionEdgePackageUpgrade,
				ResourceType: auditport.ResourceEdge,
				ResourceID:   id,
				Status:       auditport.StatusSuccess,
				Payload:      map[string]any{"version": "v1.2.3"},
			})
		}
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/edges/batch/upgrade-package", nil)
	stack.ServeHTTP(rec, req)

	if len(repo.rows) != 4 {
		t.Fatalf("landed %d rows, want 4 (one primary + three appended): %+v", len(repo.rows), repo.rows)
	}
	if repo.rows[0].ResourceID != "0" || repo.rows[1].ResourceID != "11" || repo.rows[3].ResourceID != "13" {
		t.Errorf("row order = %s,%s,%s,%s; the primary must come first and the rest in the order they were added",
			repo.rows[0].ResourceID, repo.rows[1].ResourceID, repo.rows[2].ResourceID, repo.rows[3].ResourceID)
	}
	for i, row := range repo.rows {
		if row.UserID == nil || *row.UserID == 0 {
			t.Errorf("row %d carries no actor: a batch of 500 rows where only one answers \"who did this\" is noise", i)
		}
		if row.Action != auditport.ActionEdgePackageUpgrade {
			t.Errorf("row %d action = %q", i, row.Action)
		}
	}
}

func TestExtraRowsAloneAreEnoughToRecordARequest(t *testing.T) {
	repo := &recordingRepo{}
	stack := newAuditStack(t, repo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantctx.SetOnSlot(r.Context(), tenantctx.Tenant{UserID: 42, Role: "admin"})
		auditport.AddAuditEvent(r, auditport.Event{
			Action:       auditport.ActionWebshellSessionKill,
			ResourceType: auditport.ResourceWebshellSession,
			ResourceID:   "sess-9",
			Status:       auditport.StatusSuccess,
		})
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/webshell/sessions/sess-9", nil))

	if len(repo.rows) != 1 || repo.rows[0].ResourceID != "sess-9" {
		t.Fatalf("rows = %+v, want the one appended row and nothing invented", repo.rows)
	}
	if repo.rows[0].Status != auditport.StatusSuccess {
		t.Errorf("status = %q, want the 2xx-derived success", repo.rows[0].Status)
	}
}

// A request that audits nothing must still land nothing. The loop version of
// the middleware can forget this — `for range nil` is a no-op, which is right,
// but only as long as nobody replaces it with something that emits a blank row
// per iteration.
func TestARequestThatAuditsNothingLandsNothing(t *testing.T) {
	repo := &recordingRepo{}
	stack := newAuditStack(t, repo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	stack.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/edges", nil))
	if len(repo.rows) != 0 {
		t.Fatalf("landed %d rows for a request nobody annotated", len(repo.rows))
	}
}
