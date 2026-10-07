package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// ── a fake database/sql driver ─────────────────────────────────────────
//
// The adapter's contract is "bind arguments, run the statement, decode the
// rows". A mock of the Adapter's own methods would test nothing, and a live
// PostgreSQL would make the test suite depend on a server. Registering a
// driver instead means the real database/sql path — argument conversion,
// Rows.Next, Scan into *any, ErrNoRows — is what runs here.

// fakeDriverSeq makes each pool's driver name unique. Registering the same
// name twice panics, and a per-test name also means a test's recorder can
// never see another test's statements.
var fakeDriverSeq atomic.Int64

// recordedCall is one statement the adapter sent.
type recordedCall struct {
	Query string
	Args  []driver.NamedValue
}

type fakeDriver struct {
	mu       sync.Mutex
	calls    []recordedCall
	cols     []string
	rows     [][]driver.Value
	execErr  error
	queryErr error
}

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{d: d}, nil }

func (d *fakeDriver) record(query string, args []driver.NamedValue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, recordedCall{Query: query, Args: args})
}

func (d *fakeDriver) last() (recordedCall, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.calls) == 0 {
		return recordedCall{}, false
	}
	return d.calls[len(d.calls)-1], true
}

func (d *fakeDriver) all() []recordedCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recordedCall(nil), d.calls...)
}

func (d *fakeDriver) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = nil
}

type fakeConn struct{ d *fakeDriver }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{d: c.d, query: query}, nil
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return fakeTx{}, nil }

// fakeTx stands in for a transaction. No test opens one — the adapter never
// does, and a fake that cannot actually commit is what proves that: if a
// statement were wrapped in a transaction, VACUUM would fail against this
// driver rather than pass silently.
type fakeTx struct{}

func (fakeTx) Commit() error   { return errors.New("postgres: the adapter must not open a transaction") }
func (fakeTx) Rollback() error { return nil }
func (c *fakeConn) Ping(context.Context) error {
	c.d.record("PING", nil)
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	return c.d.queryErr
}

type fakeStmt struct {
	d     *fakeDriver
	query string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }
func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.d.record(s.query, nil)
	if s.d.execErr != nil {
		return nil, s.d.execErr
	}
	return driver.RowsAffected(1), nil
}

func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	nv := make([]driver.NamedValue, len(args))
	for i, a := range args {
		nv[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	s.d.record(s.query, nv)
	if s.d.queryErr != nil {
		return nil, s.d.queryErr
	}
	return &fakeRows{cols: s.d.cols, data: s.d.rows}, nil
}

type fakeRows struct {
	cols []string
	data [][]driver.Value
	pos  int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// newFakeDB registers the fake driver once and returns a pool plus the
// recorder behind it.
func newFakeDB(t *testing.T) (*sql.DB, *fakeDriver) {
	t.Helper()
	name := "opskeeper-pg-fake-" + strconv.FormatInt(fakeDriverSeq.Add(1), 10)
	d := &fakeDriver{}
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, d
}

// connected returns an adapter backed by the fake pool.
func connected(t *testing.T) (*Adapter, *fakeDriver) {
	t.Helper()
	db, d := newFakeDB(t)
	return NewWithDB(db), d
}

func TestAdapter_Type(t *testing.T) {
	if got := New().Type(); got != adapter.TypePostgres {
		t.Errorf("Type() = %s, want postgres", got)
	}
}

// Connect must refuse an empty DSN.
//
// This assertion is the opposite of what the skeleton asserted. The skeleton
// accepted a spec with no DSN, marked itself connected, and answered Health
// with a fixed "healthy" — so a deployment with no database configured
// reported itself healthy, and every test written against it encoded that
// behaviour as correct. Refusing the connection is the honest answer: there
// is no target, so there is nothing to be healthy about.
func TestAdapter_Connect_RefusesEmptyDSN(t *testing.T) {
	a := New()
	err := a.Connect(context.Background(), adapter.ConnectionSpec{})
	if err == nil {
		t.Fatal("Connect with an empty DSN must fail; it would otherwise report a healthy adapter that reaches nothing")
	}
	if _, err := a.Health(context.Background()); !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Health after a refused Connect = %v, want ErrNotConnected", err)
	}
}

func TestAdapter_Connect_PingsBeforeReportingConnected(t *testing.T) {
	a := New()
	err := a.Connect(context.Background(), adapter.ConnectionSpec{DSN: "postgres://nobody@127.0.0.1:1/none"})
	if err == nil {
		t.Fatal("Connect must fail when the target does not answer")
	}
	if !strings.Contains(err.Error(), "ping") {
		t.Errorf("Connect error should name the probe that failed, got %v", err)
	}
	if _, err := a.Health(context.Background()); !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("a failed Connect must not leave the adapter connected, got %v", err)
	}
}

func TestAdapter_Connect_DefaultsPoolSettings(t *testing.T) {
	// The defaults are asserted on the spec rather than through a live pool:
	// this is a configuration contract, and a test that needed a server to
	// check it would skip itself exactly where it matters.
	if defaultPoolSize != 10 {
		t.Errorf("defaultPoolSize = %d, want 10", defaultPoolSize)
	}
	if defaultTimeout != 30*time.Second {
		t.Errorf("defaultTimeout = %v, want 30s", defaultTimeout)
	}
}

func TestAdapter_Health_NotConnected(t *testing.T) {
	_, err := New().Health(context.Background())
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Health on a fresh adapter = %v, want ErrNotConnected", err)
	}
}

func TestAdapter_Health_Probes(t *testing.T) {
	a, d := connected(t)
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Status != "healthy" {
		t.Errorf("Status = %s, want healthy", h.Status)
	}
	if h.CheckedAt.IsZero() {
		t.Error("CheckedAt must be set; a health report with no timestamp cannot be aged")
	}
	if c, ok := d.last(); !ok || c.Query != "PING" {
		t.Errorf("Health must probe the server, got %+v", c)
	}
}

func TestAdapter_Health_ReportsDownWithoutError(t *testing.T) {
	a, d := connected(t)
	d.queryErr = errors.New("connection refused")
	h, err := a.Health(context.Background())
	// A down server is a health result, not a call failure: returning the
	// error would make every caller treat "the database is unreachable" as
	// "the health check is broken", which are different pages.
	if err != nil {
		t.Fatalf("Health must report down as a status, got error %v", err)
	}
	if h.Status != "down" {
		t.Errorf("Status = %s, want down", h.Status)
	}
}

func TestAdapter_Close_IsIdempotent(t *testing.T) {
	a, _ := connected(t)
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close runs on the teardown path whether or not Connect ever succeeded,
	// so a second call must not panic on an already-nil pool.
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := a.Health(context.Background()); !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Health after Close = %v, want ErrNotConnected", err)
	}
}

func TestAdapter_Execute_RequiresApproval(t *testing.T) {
	a, d := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{Operation: "kill_session", Params: map[string]interface{}{"pid": 42}})
	if !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Fatalf("Execute without approval = %v, want ErrApprovalRequired", err)
	}
	// The gate runs before anything is sent. An unapproved call that reached
	// the server would be a production change made without a decision, and a
	// driver mock that recorded the statement would prove it.
	if calls := d.all(); len(calls) != 0 {
		t.Errorf("an unapproved Execute must send nothing, got %d statement(s): %+v", len(calls), calls)
	}
}

func TestAdapter_Execute_NotConnected(t *testing.T) {
	_, err := New().Execute(context.Background(), adapter.ExecOp{Operation: "kill_session", ApprovedBy: "u1"})
	// Approval is checked first, so an approved call on an unconnected
	// adapter is what reaches ErrNotConnected.
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Execute on a fresh adapter = %v, want ErrNotConnected", err)
	}
}

func TestAdapter_Execute_UnknownOperation(t *testing.T) {
	a, d := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{Operation: "drop_everything", ApprovedBy: "u1"})
	if !errors.Is(err, ErrUnknownOperation) {
		t.Fatalf("Execute with an unknown op = %v, want ErrUnknownOperation", err)
	}
	// A vocabulary mismatch is a deployment fault, and it must not be able to
	// reach the database on its way to being reported.
	if calls := d.all(); len(calls) != 0 {
		t.Errorf("an unknown operation must send nothing, got %+v", calls)
	}
}

func TestExecute_KillSession(t *testing.T) {
	a, d := connected(t)
	d.cols = []string{"pg_terminate_backend"}
	d.rows = [][]driver.Value{{true}}

	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "kill_session",
		Params:     map[string]interface{}{"pid": 4242},
		ApprovedBy: "alice",
		Reason:     "blocking the vacuum",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Success || res.Impacted != 1 {
		t.Errorf("Success=%v Impacted=%d, want true/1", res.Success, res.Impacted)
	}
	if res.Metadata["approved_by"] != "alice" {
		t.Errorf("approved_by must be recorded, got %q", res.Metadata["approved_by"])
	}
	call, _ := d.last()
	if !strings.Contains(call.Query, "pg_terminate_backend") {
		t.Errorf("wrong statement: %s", call.Query)
	}
	if len(call.Args) != 1 || call.Args[0].Value != int64(4242) {
		t.Errorf("pid must be bound as a parameter, got %+v", call.Args)
	}
}

// A refused terminate reports zero impacted and says why.
//
// pg_terminate_backend returns false both when the backend is gone and when it
// refused to die. Reporting either as a row count of zero would tell the
// operator the remediation did nothing without distinguishing a no-op from a
// failure.
func TestExecute_KillSession_ReportsRefusal(t *testing.T) {
	a, d := connected(t)
	d.cols = []string{"pg_terminate_backend"}
	d.rows = [][]driver.Value{{false}}

	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation: "kill_session", Params: map[string]interface{}{"pid": 7}, ApprovedBy: "alice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Success {
		t.Error("Success must be false when the backend was not terminated")
	}
	if res.Impacted != 0 {
		t.Errorf("Impacted = %d, want 0", res.Impacted)
	}
	if !strings.Contains(res.Message, "not terminated") {
		t.Errorf("Message must say the backend was not terminated, got %q", res.Message)
	}
}

func TestExecute_TerminateLongTx_UsesTheDiagnosticThreshold(t *testing.T) {
	a, d := connected(t)
	d.cols = []string{"pid", "terminated"}
	d.rows = [][]driver.Value{{int64(1), true}, {int64(2), true}, {int64(3), false}}

	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "terminate_long_tx",
		Params:     map[string]interface{}{"min_age_seconds": 45},
		ApprovedBy: "alice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Impacted != 2 {
		t.Errorf("Impacted = %d, want 2 (three attempted, one refused)", res.Impacted)
	}
	if !strings.Contains(res.Message, "refused") {
		t.Errorf("Message must account for the refusal, got %q", res.Message)
	}
	call, _ := d.last()
	// One round trip, not a select followed by a terminate: between two
	// statements a pid can exit and be recycled, and terminating a recycled
	// pid kills an unrelated session.
	if !strings.Contains(call.Query, "pg_terminate_backend") {
		t.Errorf("the remedy must terminate in the same statement that selects: %s", call.Query)
	}
	// database/sql normalises an int argument to int64 on the way in, which
	// is the same conversion pgx performs.
	if len(call.Args) != 1 || call.Args[0].Value != int64(45) {
		t.Errorf("the threshold must be bound, got %+v", call.Args)
	}
}

func TestExecute_TerminateLongTx_RejectsNonPositiveThreshold(t *testing.T) {
	a, d := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "terminate_long_tx",
		Params:     map[string]interface{}{"min_age_seconds": 0},
		ApprovedBy: "alice",
	})
	if err == nil {
		t.Fatal("a zero threshold would terminate every open transaction, including the caller's own neighbours")
	}
	if calls := d.all(); len(calls) != 0 {
		t.Errorf("nothing may be sent for a rejected threshold, got %+v", calls)
	}
}

func TestExecute_VacuumAnalyze_QuotesTheTable(t *testing.T) {
	a, d := connected(t)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "vacuum_analyze",
		Params:     map[string]interface{}{"table": "public.orders"},
		ApprovedBy: "alice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Impacted != 1 {
		t.Errorf("Impacted = %d, want 1 for a single named table", res.Impacted)
	}
	call, _ := d.last()
	// VACUUM cannot take bind parameters, so the identifier is the only place
	// injection could enter. It must be quoted, and it must be the only thing
	// interpolated.
	if !strings.Contains(call.Query, `"public"."orders"`) {
		t.Errorf("table name must be quoted: %s", call.Query)
	}
	if !strings.HasPrefix(strings.TrimSpace(call.Query), "VACUUM (ANALYZE)") {
		t.Errorf("unexpected statement: %s", call.Query)
	}
}

// The identifier whitelist is the security boundary for every statement that
// cannot bind its target. It is tested directly because an injection that
// only shows up against a live database is an injection that ships.
func TestQuoteQualified_RejectsInjection(t *testing.T) {
	bad := []string{
		`orders; DROP DATABASE production`,
		`"orders"`,
		`orders"`,
		`1orders`,
		``,
		`a.b.c`,
		`orders --`,
		`orders/*x*/`,
	}
	for _, name := range bad {
		if got, err := quoteQualified(name); err == nil {
			t.Errorf("quoteQualified(%q) = %q, want an error", name, got)
		}
	}
	good := map[string]string{
		"orders":          `"orders"`,
		"public.orders":   `"public"."orders"`,
		"_tmp2024":        `"_tmp2024"`,
		"my-table":        `"my-table"`,
		"public.my_table": `"public"."my_table"`,
	}
	for in, want := range good {
		got, err := quoteQualified(in)
		if err != nil {
			t.Errorf("quoteQualified(%q) failed: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("quoteQualified(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestExecute_ConnectionPause_SetsLimitBeforeEvicting(t *testing.T) {
	a, d := connected(t)
	d.cols = []string{"pid", "terminated"}
	d.rows = [][]driver.Value{{int64(11), true}, {int64(12), true}}

	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "connection_pause",
		Params:     map[string]interface{}{"role": "reporting_ro"},
		ApprovedBy: "alice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Impacted != 2 {
		t.Errorf("Impacted = %d, want 2 evicted sessions", res.Impacted)
	}
	// The message must name the way back. A remediation that can lock a role
	// out of a database and does not say how to undo it is a hazard.
	if !strings.Contains(res.Message, "connection_resume") {
		t.Errorf("Message must name the inverse operation, got %q", res.Message)
	}
	calls := d.all()
	if len(calls) < 2 {
		t.Fatalf("expected the eviction and the ALTER, got %+v", calls)
	}
	alterIdx := -1
	for i, c := range calls {
		if strings.Contains(c.Query, "CONNECTION LIMIT 0") {
			alterIdx = i
		}
	}
	if alterIdx < 0 {
		t.Fatalf("no CONNECTION LIMIT 0 statement was sent: %+v", calls)
	}
	// Set the limit first: a connection that races in between would otherwise
	// survive the sweep and the "pause" would not have paused anything.
	if alterIdx > 0 && !strings.Contains(calls[0].Query, "pg_terminate_backend") {
		t.Errorf("the limit must be set before the eviction, order was %+v", calls)
	}
	if !strings.Contains(calls[alterIdx].Query, `"reporting_ro"`) {
		t.Errorf("the role must be quoted: %s", calls[alterIdx].Query)
	}
}

func TestExecute_ConnectionResume_RestoresTheDefault(t *testing.T) {
	a, d := connected(t)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "connection_resume",
		Params:     map[string]interface{}{"role": "reporting_ro"},
		ApprovedBy: "alice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Message, "may open connections again") {
		t.Errorf("Message = %q, want the resume confirmation", res.Message)
	}
	call, _ := d.last()
	// -1 rather than the role's original limit: the original is not recorded
	// anywhere, and restoring an invented number could leave the role worse
	// than it was found.
	if !strings.Contains(call.Query, "CONNECTION LIMIT -1") {
		t.Errorf("resume must restore the server default: %s", call.Query)
	}
}

func TestExecute_RejectsBadIdentifiers(t *testing.T) {
	a, d := connected(t)
	for _, op := range []string{"connection_pause", "connection_resume"} {
		_, err := a.Execute(context.Background(), adapter.ExecOp{
			Operation:  op,
			Params:     map[string]interface{}{"role": `ro"; DROP DATABASE x; --`},
			ApprovedBy: "alice",
		})
		if err == nil {
			t.Errorf("%s must reject an identifier that is not one", op)
		}
	}
	if calls := d.all(); len(calls) != 0 {
		t.Errorf("a rejected identifier must send nothing, got %+v", calls)
	}
}

func TestAdapter_Diagnose_RoutesCategories(t *testing.T) {
	a, d := connected(t)
	cases := []struct {
		category string
		wantIn   string
	}{
		{catDatabases, "pg_database"},
		{catSchemas, "pg_namespace"},
		{catTables, "pg_class"},
		{catSessions, "pg_stat_activity"},
		{catLongTxns, "xact_start"},
		{catSlowByTime, "total_exec_time"},
		{catLockWaits, "blocker_pid"},
		{catBloat, "n_dead_tup"},
		{catIndexUsage, "idx_scan"},
		{catVacuumStatus, "pg_stat_progress_vacuum"},
		{catSlowLog, "mean_exec_time"},
		{catReplication, "pg_stat_replication"},
	}
	for _, tc := range cases {
		d.reset()
		d.cols = []string{"x"}
		d.rows = [][]driver.Value{{int64(1)}}
		r, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: tc.category})
		if err != nil {
			t.Errorf("Diagnose(%s): %v", tc.category, err)
			continue
		}
		if r.Category != tc.category {
			t.Errorf("Category = %s, want %s", r.Category, tc.category)
		}
		call, _ := d.last()
		if !strings.Contains(call.Query, tc.wantIn) {
			t.Errorf("Diagnose(%s) sent the wrong query; wanted %q in:\n%s", tc.category, tc.wantIn, call.Query)
		}
		if strings.Contains(r.Summary, "skeleton") {
			t.Errorf("Diagnose(%s) still reports a skeleton: %s", tc.category, r.Summary)
		}
	}
}

func TestAdapter_Diagnose_UnknownCategoryNamesTheRest(t *testing.T) {
	a, _ := connected(t)
	_, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: "made_up"})
	if err == nil {
		t.Fatal("an unknown category must be refused")
	}
	// A caller that cannot discover the vocabulary has no way to use this
	// method at all, so the error lists what exists.
	for _, name := range []string{catSessions, catLockWaits, catBloat} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error should list %q among the known categories: %v", name, err)
		}
	}
}

// The long-transaction threshold is the shared contract between the
// diagnostic and the remedy, and it has to be parameterised in both or an
// operator shown one set of rows gets a remedy pointed at another.
func TestAdapter_Diagnose_LongTxnsBindsThreshold(t *testing.T) {
	a, d := connected(t)
	d.cols = []string{"pid"}
	d.rows = nil
	if _, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{
		Category: catLongTxns,
		Params:   map[string]interface{}{"min_age_seconds": 90},
	}); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	call, _ := d.last()
	if len(call.Args) != 1 || call.Args[0].Value != int64(90) {
		t.Errorf("the threshold must be bound, got %+v", call.Args)
	}
}

func TestAdapter_Diagnose_NotConnected(t *testing.T) {
	_, err := New().Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catSessions})
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Diagnose on a fresh adapter = %v, want ErrNotConnected", err)
	}
}

func TestAdapter_Collect_IsASnapshot(t *testing.T) {
	a, d := connected(t)
	d.cols = []string{"version", "database", "user", "started_at", "connections", "total_backends"}
	d.rows = [][]driver.Value{{"PostgreSQL 17", "app", "ops", time.Unix(0, 0).UTC(), int64(12), int64(30)}}

	res, err := a.Collect(context.Background(), adapter.CollectQuery{Metrics: []string{"connections"}})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Metrics["database"] != "app" {
		t.Errorf("Metrics = %+v, want the catalog snapshot", res.Metrics)
	}
	// A catalog view has no history. Filling Samples with the same snapshot
	// would let a dashboard draw a flat line and call it a time series.
	if len(res.Samples) != 0 {
		t.Errorf("Samples must stay empty for a snapshot, got %d rows", len(res.Samples))
	}
	if !strings.Contains(res.Metadata["sampling"], "not a time series") {
		t.Errorf("Metadata must say the reading is a snapshot, got %q", res.Metadata["sampling"])
	}
	if res.Metadata["requested"] != "connections" {
		t.Errorf("the requested metric names must be echoed, got %q", res.Metadata["requested"])
	}
}

// explain_query is the one read that takes SQL text, and it stays safe
// because ANALYZE is not requested: the plan is produced without running the
// statement.
func TestExecute_ExplainDoesNotExecute(t *testing.T) {
	a, d := connected(t)
	if _, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catExplain}); err != nil {
		t.Fatalf("Diagnose(explain): %v", err)
	}
	call, _ := d.last()
	if strings.Contains(strings.ToUpper(call.Query), "ANALYZE") {
		t.Errorf("EXPLAIN must not request ANALYZE: %s", call.Query)
	}
}

// The registered tool set is the platform's public promise about what this
// adapter can do, and the closed loop dispatches remediation by these exact
// names. A name that is registered here but nowhere else is a run that
// prescribes a remedy nothing can perform.
func TestRegisterTools_ClosedLoopVocabularyIsPresent(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	// The four names the closed loop's PostgreSQL investigator emits.
	for _, name := range []string{
		"pg.kill_session",
		"pg.terminate_long_tx",
		"pg.connection_pause",
		"pg.vacuum_analyze",
	} {
		tool, ok := reg.GetTool(name)
		if !ok {
			t.Errorf("%s is emitted by the loop but is not registered", name)
			continue
		}
		if tool.Handler == nil {
			t.Errorf("%s has no handler", name)
		}
		if tool.Description == "" {
			t.Errorf("%s has no description for the model to read", name)
		}
	}
}

// Every tool the closed loop can name must reach the approval gate. A
// registered write that bypassed Execute would be an unapproved production
// change wearing a tool name.
func TestRegisterTools_WriteToolsGoThroughTheGate(t *testing.T) {
	a, d := connected(t)
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, a); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	writes := []string{
		"pg.kill_session", "pg.cancel_query", "pg.terminate_long_tx",
		"pg.connection_pause", "pg.connection_resume",
		"pg.vacuum_analyze", "pg.vacuum_table", "pg.analyze_table",
	}
	for _, name := range writes {
		tool, ok := reg.GetTool(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if tool.RiskLevel == adapter.RiskL0ReadOnly || tool.RiskLevel == adapter.RiskL1Diagnostic {
			t.Errorf("%s changes server state but is graded %s", name, tool.RiskLevel)
		}
		d.reset()
		// No approved_by: the gate must refuse, and nothing may be sent.
		if _, err := tool.Handler(context.Background(), map[string]interface{}{"pid": 1}); err == nil {
			t.Errorf("%s ran without approval", name)
		}
		if calls := d.all(); len(calls) != 0 {
			t.Errorf("%s reached the database without approval: %+v", name, calls)
		}
	}
}

func TestRegisterTools_ReadToolsRefuseWhenNotConnected(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	// A registered read on an adapter with no pool must say so, rather than
	// returning empty rows that read as "nothing is wrong".
	for _, name := range []string{
		"pg.active_sessions", "pg.lock_waits", "pg.table_bloat", "pg.list_tables",
	} {
		tool, ok := reg.GetTool(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		_, err := tool.Handler(context.Background(), map[string]interface{}{})
		if !errors.Is(err, adapter.ErrNotConnected) {
			t.Errorf("%s on an unconnected adapter = %v, want ErrNotConnected", name, err)
		}
	}
}

func TestOpRiskLevel(t *testing.T) {
	a := New()
	// vacuum_analyze is the one action the loop marks safe and auto-approve.
	// Grading it L3 here would force an operator to approve the one action the
	// platform already decided was harmless.
	if got := a.OpRiskLevel("vacuum_analyze"); got != adapter.RiskL2SoftWrite {
		t.Errorf("OpRiskLevel(vacuum_analyze) = %s, want L2", got)
	}
	for _, op := range []string{"kill_session", "terminate_long_tx", "connection_pause"} {
		if got := a.OpRiskLevel(op); got != adapter.RiskL3HardWrite {
			t.Errorf("OpRiskLevel(%s) = %s, want L3", op, got)
		}
	}
}

func TestToInt_AcceptsTheShapesJSONProduces(t *testing.T) {
	// A JSON number may arrive as float64, json.Number or int depending on
	// which path decoded it. A remediation that fails on "expected number,
	// got float64" fails at the moment it is most needed.
	for _, in := range []any{3, int32(3), int64(3), float64(3)} {
		got, err := toInt(in)
		if err != nil || got != 3 {
			t.Errorf("toInt(%T(%v)) = %d, %v; want 3", in, in, got, err)
		}
	}
	if _, err := toInt(3.5); err == nil {
		t.Error("toInt(3.5) must fail; a truncated pid is a different backend")
	}
	if _, err := toInt("3"); err == nil {
		t.Error(`toInt("3") must fail; a string pid is not a number`)
	}
}

func TestNormaliseCell(t *testing.T) {
	// The consumer is a model, not a Go program: a []byte marshals to base64,
	// which reads as noise rather than as the value it is.
	if got := normaliseCell([]byte(`{"a":1}`)); got == nil {
		t.Error("json bytes should decode")
	} else if m, ok := got.(map[string]any); !ok || m["a"] != float64(1) {
		t.Errorf("json bytes decoded to %#v, want map[a:1]", got)
	}
	if got := normaliseCell([]byte("plain text")); got != "plain text" {
		t.Errorf("non-json bytes = %#v, want the text", got)
	}
	ts := time.Date(2026, 3, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	if got := normaliseCell(ts); got != "2026-03-01T11:00:00Z" {
		t.Errorf("time = %#v, want RFC3339 in UTC", got)
	}
	if normaliseCell(nil) != nil {
		t.Error("NULL must stay nil")
	}
}

// ── replication_status ─────────────────────────────────────────────────

// A standby that has not yet reported its replay position has an unknown
// lag, not a zero one. The query computes the difference with
// pg_wal_lsn_diff, which is NULL when either side is NULL, and the adapter
// must carry that NULL through instead of printing a healthy-looking 0.
func TestReplicationStatus_NullLagIsUnknownNotZero(t *testing.T) {
	a, d := connected(t)
	d.cols = []string{"application_name", "client_addr", "state", "sync_state", "sent_lsn", "replay_lsn", "replay_lag_bytes", "replay_lag_seconds"}
	// A standby that just connected: positions not reported yet.
	d.rows = [][]driver.Value{{"standby-1", "10.0.0.2", "startup", "async", "0/5000000", nil, nil, nil}}

	rows, err := replicationStatus(context.Background(), a, nil, 0)
	if err != nil {
		t.Fatalf("replicationStatus: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	for _, key := range []string{"replay_lsn", "replay_lag_bytes", "replay_lag_seconds"} {
		v, present := rows[0][key]
		if !present {
			t.Errorf("%s must be present as an explicit unknown, not dropped", key)
			continue
		}
		if v != nil {
			t.Errorf("%s = %v (%T); a NULL position is unknown, and 0 would read as caught up", key, v, v)
		}
	}
	call, _ := d.last()
	if !strings.Contains(call.Query, "pg_wal_lsn_diff") {
		t.Errorf("the lag must be computed as a LSN difference so a NULL stays NULL:\n%s", call.Query)
	}
	if !strings.Contains(call.Query, "pg_stat_replication") {
		t.Errorf("this must read the primary's WAL sender view:\n%s", call.Query)
	}
}

// The two lag figures are reported separately because they answer different
// questions: bytes is what grows during an incident, seconds is what an RPO
// statement is written in, and neither is derivable from the other.
func TestReplicationStatus_ReportsBothLagFigures(t *testing.T) {
	a, d := connected(t)
	d.cols = []string{"application_name", "client_addr", "state", "sync_state", "sent_lsn", "replay_lsn", "replay_lag_bytes", "replay_lag_seconds"}
	d.rows = [][]driver.Value{{"standby-1", "10.0.0.2", "streaming", "sync", "0/6000000", "0/5000000", int64(1048576), 2.5}}

	rows, err := replicationStatus(context.Background(), a, nil, 0)
	if err != nil {
		t.Fatalf("replicationStatus: %v", err)
	}
	if rows[0]["replay_lag_bytes"] != int64(1048576) {
		t.Errorf("replay_lag_bytes = %v (%T)", rows[0]["replay_lag_bytes"], rows[0]["replay_lag_bytes"])
	}
	if rows[0]["replay_lag_seconds"] != 2.5 {
		t.Errorf("replay_lag_seconds = %v (%T)", rows[0]["replay_lag_seconds"], rows[0]["replay_lag_seconds"])
	}
	if rows[0]["sync_state"] != "sync" {
		t.Errorf("sync_state = %v, want the replication mode kept", rows[0]["sync_state"])
	}
}

// No rows means this instance has no connected standbys — often because it is
// itself a standby, or because the DSN points at the wrong host. That is a
// different finding from "replication is perfectly caught up", and the
// category's suggestion has to say so.
func TestReplicationStatus_NoRowsIsNotZeroLag(t *testing.T) {
	a, d := connected(t)
	d.cols = []string{"application_name"}
	d.rows = nil

	res, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catReplication})
	if err != nil {
		t.Fatalf("Diagnose(replication): %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected no findings, got %#v", res.Findings)
	}
	joined := strings.Join(res.Suggestions, " ")
	if !strings.Contains(joined, "zero lag") {
		t.Errorf("suggestions = %q, want the no-standby answer distinguished from zero lag", res.Suggestions)
	}
}
