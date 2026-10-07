package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/ports"

	bizaudit "github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	auditmodel "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	auditmw "github.com/vincent-wuhan/opskeeper/core/domains/server/middleware"
	release "github.com/vincent-wuhan/opskeeper/core/domains/service/plugin"
)

// fakeService is a scripted release manager. The rollout's own behaviour is
// tested in service/plugin; what these tests are about is that the HTTP
// layer does not flatten the answers a console has to branch on.
type fakeService struct {
	start       func() (release.Status, error)
	list        []release.Status
	status      func(name string) (release.Status, error)
	advance     func(name string) (bool, release.Status, error)
	halt        func(name, reason string) (release.Status, error)
	rollback    func(name string) (release.Status, error)
	compat      func(req release.Requirement) (release.Matrix, error)
	lastStart   release.StartRequest
	lastHalt    string
	lastHaltWhy string
	lastCompat  release.Requirement
}

func (f *fakeService) Start(_ context.Context, req release.StartRequest) (release.Status, error) {
	f.lastStart = req
	return f.start()
}

func (f *fakeService) List() []release.Status { return f.list }

func (f *fakeService) Status(name string) (release.Status, error) { return f.status(name) }

func (f *fakeService) Advance(_ context.Context, name string) (bool, release.Status, error) {
	return f.advance(name)
}

func (f *fakeService) Halt(name, reason string) (release.Status, error) {
	f.lastHalt, f.lastHaltWhy = name, reason
	return f.halt(name, reason)
}

func (f *fakeService) Rollback(_ context.Context, name string) (release.Status, error) {
	return f.rollback(name)
}

// Compatibility defaults to refusing nothing so a test that is about
// something else does not have to script it — the zero Service has to be
// usable, and a fake whose every method panics is a fake that gets
// half-scripted and then read as coverage.
func (f *fakeService) Compatibility(_ context.Context, req release.Requirement) (release.Matrix, error) {
	f.lastCompat = req
	if f.compat == nil {
		return release.Matrix{Requirement: req}, nil
	}
	return f.compat(req)
}

// asRole stands in for the auth middleware, which every route here sits
// behind. It writes the tenant both to the plain context value and to
// the mutable slot, exactly as core/base/pkg/auth does — the slot is the
// path the audit middleware reads, so a helper that only did With()
// would let the audit row come out with an empty role while the
// handler still saw the tenant.
func asRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := tenantctx.Tenant{UserID: 1, Role: role}
			tenantctx.SetOnSlot(r.Context(), t)
			ctx := tenantctx.With(r.Context(), t)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func newServer(t *testing.T, svc Service, role string) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Use(asRole(role))
	NewHandler(svc).Register(r)
	return httptest.NewServer(r)
}

func post(t *testing.T, srv *httptest.Server, path, body string) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	resp, err := srv.Client().Post(srv.URL+path, "application/json", rdr)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	return resp
}

func get(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func okService() *fakeService {
	return &fakeService{
		start: func() (release.Status, error) {
			return release.Status{Plugin: "acme", Version: "1.0.0", Wave: 1, Waves: 3}, nil
		},
		status: func(string) (release.Status, error) {
			return release.Status{Plugin: "acme", Version: "1.0.0", Wave: 2, Waves: 3}, nil
		},
		advance: func(string) (bool, release.Status, error) {
			return true, release.Status{Plugin: "acme", Wave: 2, Waves: 3}, nil
		},
		halt: func(string, string) (release.Status, error) {
			return release.Status{Plugin: "acme", Halted: true, Reason: "canary alerting"}, nil
		},
		rollback: func(string) (release.Status, error) { return release.Status{Plugin: "acme", RolledBack: true}, nil },
	}
}

// ---------------------------------------------------------------------
// auth
// ---------------------------------------------------------------------

func TestAReleaseCannotBeStartedByANonAdmin(t *testing.T) {
	// A release puts new code — including L2 tools that can restart
	// services — onto hosts. Every route here is admin, and the check is
	// on the group rather than only on start, because a viewer who could
	// call rollback could take a working package off the fleet.
	srv := newServer(t, okService(), "viewer")
	defer srv.Close()

	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/plugins/releases"},
		{"GET", "/v1/plugins/releases"},
		{"GET", "/v1/plugins/releases/acme"},
		{"POST", "/v1/plugins/releases/acme/advance"},
		{"POST", "/v1/plugins/releases/acme/halt"},
		{"POST", "/v1/plugins/releases/acme/rollback"},
	} {
		var resp *http.Response
		if c.method == "GET" {
			resp = get(t, srv, c.path)
		} else {
			resp = post(t, srv, c.path, "{}")
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403 for a non-admin", c.method, c.path, resp.StatusCode)
		}
	}
}

func TestAnUnauthenticatedReleaseIsRefused(t *testing.T) {
	// No tenantctx means no caller. Reporting it as 500 would send an
	// operator to the manager's logs for a missing session.
	r := chi.NewRouter()
	NewHandler(okService()).Register(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp := post(t, srv, "/v1/plugins/releases", "{}")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with no caller attached", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------
// the happy path
// ---------------------------------------------------------------------

func TestStartForwardsEveryFieldTheNodeReviewNeeds(t *testing.T) {
	// The signature, the digest and the key id have to survive the HTTP
	// hop. A console that could not send the envelope would make every
	// signed node refuse the release, and it would read as a fleet-wide
	// policy problem.
	svc := okService()
	srv := newServer(t, svc, "admin")
	defer srv.Close()

	body := `{"plugin":"acme","version":"1.0.0","url":"https://m/x.tar.gz",` +
		`"sha256":"abc","signature":"ZW52","key_id":"ops-2026","strategy":"rolling","nodes":[3,4]}`
	resp := post(t, srv, "/v1/plugins/releases", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got startResp
	decode(t, resp, &got)
	if got.Plugin != "acme" || got.Version != "1.0.0" {
		t.Errorf("response = %+v, want the release that started", got)
	}
	for _, c := range []struct{ name, got, want string }{
		{"plugin", svc.lastStart.Name, "acme"},
		{"version", svc.lastStart.Version, "1.0.0"},
		{"url", svc.lastStart.URL, "https://m/x.tar.gz"},
		{"sha256", svc.lastStart.SHA256, "abc"},
		{"signature", svc.lastStart.Signature, "ZW52"},
		{"key_id", svc.lastStart.KeyID, "ops-2026"},
		{"strategy", svc.lastStart.Strategy, "rolling"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if len(svc.lastStart.Nodes) != 2 || svc.lastStart.Nodes[0] != 3 {
		t.Errorf("nodes = %v, want the subset the console asked for", svc.lastStart.Nodes)
	}
}

func TestStartReportsTheWaveCount(t *testing.T) {
	// A progress display that only learns the total after the first poll
	// shows "wave 1 of 1" for a four-wave release.
	srv := newServer(t, okService(), "admin")
	defer srv.Close()
	var got startResp
	decode(t, post(t, srv, "/v1/plugins/releases", `{"plugin":"a","version":"1","url":"u","strategy":"rolling"}`), &got)
	if got.Waves != 3 {
		t.Errorf("waves = %d, want the plan's wave count", got.Waves)
	}
}

func TestAdvanceSaysWhetherAWaveWentOut(t *testing.T) {
	// "The wave is not accounted for" is not an error — it is the gate
	// doing its job. A console that saw only a 200 would show progress
	// that has not happened.
	svc := okService()
	svc.advance = func(string) (bool, release.Status, error) {
		return false, release.Status{Plugin: "acme", Wave: 1, Waves: 3, Pending: []uint64{7}}, nil
	}
	srv := newServer(t, svc, "admin")
	defer srv.Close()

	var got advanceResp
	decode(t, post(t, srv, "/v1/plugins/releases/acme/advance", ""), &got)
	if got.Moved {
		t.Error("moved = true for a wave that was not accounted for")
	}
	if len(got.Pending) != 1 || got.Pending[0] != 7 {
		t.Errorf("pending = %v, want the node holding the release up", got.Pending)
	}
}

func TestHaltCarriesTheOperatorsReason(t *testing.T) {
	// The reason is what the next person reads at 3am. A halt that
	// recorded only "halted" would leave them guessing who stopped it and
	// why.
	svc := okService()
	srv := newServer(t, svc, "admin")
	defer srv.Close()

	resp := post(t, srv, "/v1/plugins/releases/acme/halt", `{"reason":"the canary is alerting on p99"}`)
	resp.Body.Close()
	if svc.lastHalt != "acme" {
		t.Errorf("halt name = %q, want acme", svc.lastHalt)
	}
	if svc.lastHaltWhy != "the canary is alerting on p99" {
		t.Errorf("halt reason = %q, want the operator's sentence", svc.lastHaltWhy)
	}
}

func TestHaltWithoutABodyStillStopsTheRelease(t *testing.T) {
	// The common call is a button press. Demanding a reason would make the
	// urgent case harder than the considered one.
	svc := okService()
	srv := newServer(t, svc, "admin")
	defer srv.Close()

	resp := post(t, srv, "/v1/plugins/releases/acme/halt", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a bodyless halt", resp.StatusCode)
	}
	resp.Body.Close()
	if svc.lastHaltWhy == "" {
		t.Error("the halt reached the service with no reason at all")
	}
}

func TestListReturnsAnEmptyArrayRatherThanNull(t *testing.T) {
	// A console that receives null for "nothing running" renders whatever
	// its language does with null — which is not the same as an empty
	// table.
	srv := newServer(t, &fakeService{}, "admin")
	defer srv.Close()

	resp := get(t, srv, "/v1/plugins/releases")
	var got struct {
		Items []release.Status `json:"items"`
		Total int              `json:"total"`
	}
	decode(t, resp, &got)
	if got.Items == nil {
		t.Error("items decoded to nil, want an empty array")
	}
	if got.Total != 0 {
		t.Errorf("total = %d, want 0", got.Total)
	}
}

// ---------------------------------------------------------------------
// failures a console has to branch on
// ---------------------------------------------------------------------

func TestASecondReleaseOfOnePackageIsAConflict(t *testing.T) {
	// 409 rather than 400: nothing is wrong with the request, the package
	// already has a release in flight.
	svc := okService()
	svc.start = func() (release.Status, error) { return release.Status{}, release.ErrReleaseRunning }
	srv := newServer(t, svc, "admin")
	defer srv.Close()

	resp := post(t, srv, "/v1/plugins/releases", `{"plugin":"a","version":"1","url":"u","strategy":"rolling"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decode(t, resp, &body)
	if body.Error.Code != "release_running" {
		t.Errorf("code = %q, want release_running", body.Error.Code)
	}
}

func TestAStatusForAnUnknownReleaseIsNotFound(t *testing.T) {
	svc := okService()
	svc.status = func(string) (release.Status, error) { return release.Status{}, release.ErrNoRelease }
	srv := newServer(t, svc, "admin")
	defer srv.Close()

	resp := get(t, srv, "/v1/plugins/releases/ghost")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAPartialRollbackReturnsTheStatusAlongsideTheError(t *testing.T) {
	// The nodes that still hold the package are the whole point of the
	// answer. A 500 with only a message would make the console start again
	// from a blank table while the fleet is half-rolled-back.
	svc := okService()
	svc.rollback = func(string) (release.Status, error) {
		return release.Status{Plugin: "acme", Version: "1.0.0", Pending: []uint64{5, 6}},
			errors.New("rollback did not complete on 2 node(s)")
	}
	srv := newServer(t, svc, "admin")
	defer srv.Close()

	resp := post(t, srv, "/v1/plugins/releases/acme/rollback", "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — the rollback reached the fleet and did not finish", resp.StatusCode)
	}
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Status release.Status `json:"status"`
	}
	decode(t, resp, &got)
	if got.Error.Code != "rollback_incomplete" {
		t.Errorf("code = %q, want rollback_incomplete", got.Error.Code)
	}
	if len(got.Status.Pending) != 2 {
		t.Errorf("pending = %v, want the nodes that still hold the package", got.Status.Pending)
	}
}

func TestAnUnwiredManagerSaysNotConfiguredRatherThanInternal(t *testing.T) {
	// A manager without the tunnel is not malfunctioning, it is not
	// configured. 503 tells an operator to finish the rollout deployment;
	// 500 sends them to the logs.
	r := chi.NewRouter()
	r.Use(asRole("admin"))
	NewHandler(nil).Register(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp := get(t, srv, "/v1/plugins/releases")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestSetServiceBackFillsTheHandler(t *testing.T) {
	// The release manager needs the tunnel client, which main builds later
	// than the HTTP handler. If the routes could not be back-filled they
	// would 503 forever on a fully wired manager.
	h := NewHandler(nil)
	h.SetService(okService())
	r := chi.NewRouter()
	r.Use(asRole("admin"))
	h.Register(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp := get(t, srv, "/v1/plugins/releases")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after the service was wired", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------
//
// A release is the widest-blast-radius action the console can take: it
// puts new code, including L2 tools that restart services, onto hosts.
// These tests run the *production* audit middleware with a recording
// repo rather than hand-installing a context slot, because the slot key
// is unexported in that package — and because the property worth
// protecting is the end-to-end one: a handler that calls SetAuditEvent
// produces a row.

// auditRepo records what the middleware emitted. It is mutex-guarded
// because the write happens on the server goroutine and the read on the
// test goroutine, with only a TCP connection between them — a
// happens-before edge the race detector does not accept.
type auditRepo struct {
	mu   sync.Mutex
	rows []*auditmodel.Log
}

func (r *auditRepo) Insert(_ context.Context, l *auditmodel.Log) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, l)
	return nil
}

func (r *auditRepo) List(_ context.Context, _ bizaudit.ListFilters) ([]auditmodel.Log, int64, error) {
	return nil, 0, nil
}

func (r *auditRepo) DeleteOlderThan(_ context.Context, _ time.Time) (int64, error) { return 0, nil }

func (r *auditRepo) emitted() []*auditmodel.Log {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*auditmodel.Log(nil), r.rows...)
}

// newAuditServer wires the real AuditMiddleware in front of the routes.
func newAuditServer(t *testing.T, svc Service, role string) (*httptest.Server, *auditRepo) {
	t.Helper()
	repo := &auditRepo{}
	uc := bizaudit.New(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Use(auditmw.AuditMiddleware(uc))
	r.Use(asRole(role))
	NewHandler(svc).Register(r)
	return httptest.NewServer(r), repo
}

// only asserts exactly one row came out and returns it. A second row
// means the handler audited the same action twice, which would double
// count an operator's action in every report built on this table.
func only(t *testing.T, repo *auditRepo) *auditmodel.Log {
	t.Helper()
	rows := repo.emitted()
	if len(rows) != 1 {
		t.Fatalf("emitted %d audit rows, want exactly 1: %+v", len(rows), rows)
	}
	return rows[0]
}

func TestEveryRouteThatTouchesTheFleetRecordsAnAuditRow(t *testing.T) {
	// start, advance, halt and rollback all change what runs on hosts.
	// A trail that skipped any of them would answer "who shipped this"
	// for the code but not for the stop, and the stop is the action an
	// incident review actually asks about.
	for _, tc := range []struct {
		name, method, path, body, action string
	}{
		{"start", "POST", "/v1/plugins/releases", `{"plugin":"acme","version":"1.0.0","strategy":"rolling"}`, auditmodel.ActionPluginReleaseStart},
		{"advance", "POST", "/v1/plugins/releases/acme/advance", "", auditmodel.ActionPluginReleaseAdvance},
		{"halt", "POST", "/v1/plugins/releases/acme/halt", `{"reason":"cpu spike"}`, auditmodel.ActionPluginReleaseHalt},
		{"rollback", "POST", "/v1/plugins/releases/acme/rollback", "", auditmodel.ActionPluginReleaseRollback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, repo := newAuditServer(t, okService(), "admin")
			defer srv.Close()

			var resp *http.Response
			if tc.method == "GET" {
				resp = get(t, srv, tc.path)
			} else {
				resp = post(t, srv, tc.path, tc.body)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			row := only(t, repo)
			if row.Action != tc.action {
				t.Errorf("action = %q, want %q", row.Action, tc.action)
			}
			if row.ResourceType != auditmodel.ResourcePlugin {
				t.Errorf("resource_type = %q, want %q", row.ResourceType, auditmodel.ResourcePlugin)
			}
			if row.ResourceID != "acme" {
				t.Errorf("resource_id = %q, want acme — the package the request was about", row.ResourceID)
			}
			if row.Status != auditmodel.StatusSuccess {
				t.Errorf("status = %q, want success", row.Status)
			}
			if row.Role != "admin" {
				t.Errorf("role = %q, want admin — the middleware fills this from the tenant slot", row.Role)
			}
			if row.PayloadJSON == "" {
				t.Error("payload is empty; the release detail is the part a reviewer reads")
			}
		})
	}
}

func TestReadingReleasesDoesNotWriteAuditRows(t *testing.T) {
	// audit_logs is a curated trail of actions, not an access log. A
	// console that polls status every two seconds while a canary runs
	// must not bury the release in its own progress polls.
	srv, repo := newAuditServer(t, okService(), "admin")
	defer srv.Close()

	for _, path := range []string{"/v1/plugins/releases", "/v1/plugins/releases/acme"} {
		resp := get(t, srv, path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
	if rows := repo.emitted(); len(rows) != 0 {
		t.Errorf("emitted %d rows for two reads, want 0", len(rows))
	}
}

func TestARefusedReleaseIsAuditedWithTheReason(t *testing.T) {
	// The interesting case is the one that did not go out. When a
	// package reaches the fleet without a decision on record, an audit
	// trail that only holds successes cannot say who tried or why it
	// bounced.
	svc := okService()
	svc.start = func() (release.Status, error) {
		return release.Status{}, release.ErrReleaseRunning
	}
	srv, repo := newAuditServer(t, svc, "admin")
	defer srv.Close()

	resp := post(t, srv, "/v1/plugins/releases", `{"plugin":"acme","version":"2.0.0","strategy":"rolling"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	row := only(t, repo)
	if row.Action != auditmodel.ActionPluginReleaseStart {
		t.Errorf("action = %q, want %q", row.Action, auditmodel.ActionPluginReleaseStart)
	}
	if row.Status != auditmodel.StatusFailure {
		t.Errorf("status = %q, want failure — the attempt is what happened", row.Status)
	}
	if row.ErrorCode != "release_running" {
		t.Errorf("error_code = %q, want release_running — the same code the response carried", row.ErrorCode)
	}
	if row.ResourceID != "acme" {
		t.Errorf("resource_id = %q, want acme — the request did name a package", row.ResourceID)
	}
}

func TestAWaveThatDidNotMoveIsAuditedButNotAsAnError(t *testing.T) {
	// A wave that did not move is the normal state while a canary is
	// still being graded. It must be visible as "nothing happened"
	// rather than as a malfunction, or the trail teaches an operator to
	// page someone for a release that is simply waiting.
	svc := okService()
	svc.advance = func(string) (bool, release.Status, error) {
		return false, release.Status{Plugin: "acme", Wave: 1, Waves: 3, Pending: []uint64{7}}, nil
	}
	srv, repo := newAuditServer(t, svc, "admin")
	defer srv.Close()

	resp := post(t, srv, "/v1/plugins/releases/acme/advance", "")
	resp.Body.Close()
	row := only(t, repo)
	if row.Status != auditmodel.StatusFailure {
		t.Errorf("status = %q, want failure — nothing advanced", row.Status)
	}
	if row.ErrorCode != "" {
		t.Errorf("error_code = %q, want empty; a pending wave is not an error code", row.ErrorCode)
	}
	if !strings.Contains(row.PayloadJSON, "blocked") {
		t.Errorf("payload = %s, want a blocked reason", row.PayloadJSON)
	}
	if !strings.Contains(row.PayloadJSON, `"moved":false`) {
		t.Errorf("payload = %s, want moved=false so the row can be filtered", row.PayloadJSON)
	}
}

func TestTheOperatorsHaltReasonIsWhatTheAuditRowSays(t *testing.T) {
	// The reason is the only field in the row that explains *why*, and
	// the person reading it is usually not the person who typed it.
	srv, repo := newAuditServer(t, okService(), "admin")
	defer srv.Close()

	resp := post(t, srv, "/v1/plugins/releases/acme/halt", `{"reason":"canary p99 doubled"}`)
	resp.Body.Close()
	row := only(t, repo)
	if row.Action != auditmodel.ActionPluginReleaseHalt {
		t.Fatalf("action = %q", row.Action)
	}
	if !strings.Contains(row.PayloadJSON, "canary p99 doubled") {
		t.Errorf("payload = %s, want the operator's sentence", row.PayloadJSON)
	}
}

func TestAHaltWithNoReasonSaysSoRatherThanRecordingAnEmptyField(t *testing.T) {
	// A blank reason in the payload reads as "the field was dropped".
	// Naming the default makes it clear the operator stopped it from the
	// console without saying why, which is a different fact.
	srv, repo := newAuditServer(t, okService(), "admin")
	defer srv.Close()

	resp := post(t, srv, "/v1/plugins/releases/acme/halt", "")
	resp.Body.Close()
	row := only(t, repo)
	if !strings.Contains(row.PayloadJSON, "halted from the console") {
		t.Errorf("payload = %s, want the default reason recorded", row.PayloadJSON)
	}
}

// fakeInventory stands in for the node adapter on the read side.
type fakeInventory struct {
	installed func(edgeID uint64) ([]ports.PluginInfo, error)
	lastEdge  uint64
}

func (f *fakeInventory) Installed(_ context.Context, edgeID uint64) ([]ports.PluginInfo, error) {
	f.lastEdge = edgeID
	return f.installed(edgeID)
}

// newInventoryServer is newServer plus the inventory. The existing helper
// takes only a Service because the release routes never needed one; adding
// a second constructor rather than a variadic keeps every existing call
// site reading the way it did.
func newInventoryServer(t *testing.T, svc Service, inv Inventory, role string) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Use(asRole(role))
	h := NewHandler(svc)
	h.SetInventory(inv)
	h.Register(r)
	return httptest.NewServer(r)
}

// The endpoint this whole exercise was for: a node's package set, readable.
// Before it, the only window onto what a host was running was a release in
// flight, which is exactly when nobody is asking.
func TestInstalled_ReportsWhatTheNodeIsRunning(t *testing.T) {
	inv := &fakeInventory{installed: func(edgeID uint64) ([]ports.PluginInfo, error) {
		return []ports.PluginInfo{
			{Name: "opskeeper-sre-readonly", Version: "1.2.0", Digest: "sha256:aa"},
			{Name: "opskeeper-sre-repair", Version: "0.4.1"},
		}, nil
	}}
	srv := newInventoryServer(t, &fakeService{}, inv, "admin")
	defer srv.Close()

	resp := get(t, srv, "/v1/plugins/nodes/7/installed")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		EdgeID   uint64             `json:"edge_id"`
		Packages []ports.PluginInfo `json:"packages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.EdgeID != 7 {
		t.Errorf("edge_id = %d, want the node that was asked about", got.EdgeID)
	}
	if inv.lastEdge != 7 {
		t.Errorf("adapter saw node %d, want 7 — the path parameter is the node", inv.lastEdge)
	}
	if len(got.Packages) != 2 || got.Packages[0].Name != "opskeeper-sre-readonly" {
		t.Errorf("packages = %+v, want the node's own list in the node's order", got.Packages)
	}
}

// A node that runs nothing is a fact, and it has to arrive as a fact.
//
// The adapter returns a nil slice for an empty set, and Go marshals that as
// `null`. A console reading `packages` would then have to tell "this node
// runs nothing" from "the field is missing" by inspecting a null, and the
// natural thing to do with a null in JavaScript is treat it as absent — at
// which point a node running nothing and a node the manager never reached
// render identically. The handler normalises to an empty slice so the two
// are different HTTP answers: this is 200, the other is 502.
func TestInstalled_AnEmptyNodeIsAnEmptyListAndNeverNull(t *testing.T) {
	inv := &fakeInventory{installed: func(uint64) ([]ports.PluginInfo, error) { return nil, nil }}
	srv := newInventoryServer(t, &fakeService{}, inv, "admin")
	defer srv.Close()

	resp := get(t, srv, "/v1/plugins/nodes/7/installed")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a node running nothing is not a failure", resp.StatusCode)
	}
	// Decoded as a map rather than the handler's own struct: this asserts
	// the bytes on the wire, and `null` versus `[]` is invisible to a
	// struct decode.
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	pkgs, ok := raw["packages"].([]any)
	if !ok {
		t.Fatalf("packages = %#v, want an empty array", raw["packages"])
	}
	if len(pkgs) != 0 {
		t.Errorf("packages = %#v, want empty", pkgs)
	}
}

// 502 and not 500 is the whole point of this endpoint's error handling.
//
// The request was well formed and the control plane is fine, so the fault
// is on the far side of the tunnel. A console has to be able to say "this
// node did not answer" rather than "the platform is broken", because the
// first is one host to look at and the second is somebody to page. Collapsing
// it into a 500 sends an operator to the manager's logs for what is a node
// that is busy or restarting.
func TestInstalled_ANodeThatDidNotAnswerIsBadGatewayNotInternal(t *testing.T) {
	inv := &fakeInventory{installed: func(uint64) ([]ports.PluginInfo, error) {
		return nil, errors.New("plugin.list: node 7 did not answer: i/o timeout")
	}}
	srv := newInventoryServer(t, &fakeService{}, inv, "admin")
	defer srv.Close()

	resp := get(t, srv, "/v1/plugins/nodes/7/installed")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 so the console can blame the node", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "node_unreachable" {
		t.Errorf("code = %q, want node_unreachable", body.Error.Code)
	}
}

// A manager that was never given a tunnel must say so, and must not say it
// as 404.
//
// 404 means the route does not exist, which sends an operator looking for a
// version that has the feature. 503 says the route is here and this
// deployment cannot use it — the same shape the compatibility matrix uses
// for a missing version snapshot, and for the same reason: the control
// plane cannot tell, and "cannot tell" is not "the answer is no".
func TestInstalled_AnUnwiredInventoryIsServiceUnavailableNotNotFound(t *testing.T) {
	srv := newInventoryServer(t, &fakeService{}, nil, "admin")
	defer srv.Close()

	resp := get(t, srv, "/v1/plugins/nodes/7/installed")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — the route exists, this deployment cannot use it", resp.StatusCode)
	}
}

// The same three-way distinction, one level down: a manager that HAS a tunnel
// but whose adapter was built without one is still "cannot tell", not a
// node failure. ErrNoTunnel is the sentinel that keeps those apart.
func TestInstalled_AManagerWithNoTunnelIsServiceUnavailableNotBadGateway(t *testing.T) {
	inv := &fakeInventory{installed: func(uint64) ([]ports.PluginInfo, error) {
		return nil, fmt.Errorf("reading node 7: %w", release.ErrNoTunnel)
	}}
	srv := newInventoryServer(t, &fakeService{}, inv, "admin")
	defer srv.Close()

	resp := get(t, srv, "/v1/plugins/nodes/7/installed")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — nothing is known about any node", resp.StatusCode)
	}
}

// A node id that is not one. Zero is refused rather than broadcast to
// whichever node happens to answer, which is what a `0` that reached the
// tunnel would mean.
func TestInstalled_RefusesAnEdgeIDThatIsNotANode(t *testing.T) {
	inv := &fakeInventory{installed: func(uint64) ([]ports.PluginInfo, error) {
		t.Error("the adapter was called for a node id that is not one")
		return nil, nil
	}}
	srv := newInventoryServer(t, &fakeService{}, inv, "admin")
	defer srv.Close()

	for _, path := range []string{
		"/v1/plugins/nodes/0/installed",
		"/v1/plugins/nodes/abc/installed",
		"/v1/plugins/nodes/-1/installed",
	} {
		resp := get(t, srv, path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", path, resp.StatusCode)
		}
	}
}

// What a node runs is governance information — it names the L2 tools a host
// can reach — so it sits behind the same admin wall as the routes that put
// them there. Reading is not the dangerous half; reading is who decides to
// read it.
func TestInstalled_ANSmallerRoleIsRefused(t *testing.T) {
	inv := &fakeInventory{installed: func(uint64) ([]ports.PluginInfo, error) {
		t.Error("a non-admin reached the node inventory")
		return nil, nil
	}}
	srv := newInventoryServer(t, &fakeService{}, inv, "user")
	defer srv.Close()

	resp := get(t, srv, "/v1/plugins/nodes/7/installed")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}
