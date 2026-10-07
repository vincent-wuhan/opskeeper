// crystallized_test.go — tests for the crystallisation review surface.
//
// The point of these tests is the wiring the mechanism was missing: a
// promoted pattern and the draft it would emit have to be reachable by an
// operator. They drive a real crystallize.Ledger, not a stub, because the
// bug class this surface can have is exactly "the listing and the document
// disagree about what would be installed" — and a stub cannot disagree with
// itself.
package aiops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/crystallize"
)

var crystallizedBase = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func crystallizedTrial(minutes int) crystallize.Trial {
	return crystallize.Trial{
		At: crystallizedBase.Add(time.Duration(minutes) * time.Minute),
		Pattern: crystallize.Pattern{
			Fault: crystallize.Fault{Kind: "host.disk_full", Family: "host"},
			Action: crystallize.Action{
				Tool:        "host.restart_service",
				Class:       domain.ClassWrite,
				Argv:        []string{"systemctl", "restart", "orders-api"},
				Target:      "host:i-0abc123",
				Trigger:     domain.AutonomyTrigger{Kind: domain.TriggerMetricAbove, Metric: "node_disk_used_ratio", Threshold: 0.92},
				BlastRadius: domain.RadiusPod,
				TTL:         15 * time.Minute,
			},
		},
		Outcome:  crystallize.OutcomeVerified,
		Evidence: "incident-" + string(rune('a'+minutes)),
	}
}

func promotedLedger(t *testing.T) *crystallize.Ledger {
	t.Helper()
	l := crystallize.NewLedger(crystallize.Policy{})
	for i := 1; i <= crystallize.DefaultMinCleanStreak; i++ {
		if _, err := l.Record(crystallizedTrial(i)); err != nil {
			t.Fatalf("record trial %d: %v", i, err)
		}
	}
	if got := len(l.Promoted()); got != 1 {
		t.Fatalf("promoted = %d, want 1", got)
	}
	return l
}

func adminTenant() tenantctx.Tenant {
	return tenantctx.Tenant{UserID: 42, Email: "admin@example.com", Role: "admin", IsSuperuser: true}
}

func TestCrystallized_NotWiredAnswers503(t *testing.T) {
	t.Parallel()
	r := buildRouter(NewHandler(&fakeService{}), adminTenant())
	req := httptest.NewRequest(http.MethodGet, "/v1/loops/crystallized", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 body=%s", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "not-wired" {
		t.Fatalf("code = %q, want not-wired", body.Code)
	}
}

func TestCrystallized_RequiresAdmin(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeService{})
	h.SetPatterns(promotedLedger(t))
	r := buildRouter(h, tenantctx.Tenant{UserID: 7, Role: "user"})
	req := httptest.NewRequest(http.MethodGet, "/v1/loops/crystallized", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestCrystallized_ListsPromotedPatternWithEvidence(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeService{})
	h.SetPatterns(promotedLedger(t))
	r := buildRouter(h, adminTenant())
	req := httptest.NewRequest(http.MethodGet, "/v1/loops/crystallized", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body CrystallizedListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("total = %d, items = %d, want 1", body.Total, len(body.Items))
	}
	p := body.Items[0]
	if !strings.HasPrefix(p.Name, "opskeeper-crystallized-host-disk-full-") {
		t.Fatalf("name = %q, want the crystallized package prefix", p.Name)
	}
	if p.Tool != "host.restart_service" || p.Class != "write" {
		t.Fatalf("tool/class = %q/%q", p.Tool, p.Class)
	}
	if len(p.Argv) != 3 || p.Argv[0] != "systemctl" {
		t.Fatalf("argv = %v, want the exact vector that ran", p.Argv)
	}
	if p.Target != "host:i-0abc123" {
		t.Fatalf("target = %q", p.Target)
	}
	if p.Trigger.Metric != "node_disk_used_ratio" || p.Trigger.Threshold != 0.92 {
		t.Fatalf("trigger = %+v", p.Trigger)
	}
	if p.SafetyLevel != string(domain.SafetyL2) {
		t.Fatalf("safety = %q, want L2 for a write", p.SafetyLevel)
	}
	if p.Streak < crystallize.DefaultMinCleanStreak {
		t.Fatalf("streak = %d", p.Streak)
	}
	if len(p.Evidence) != crystallize.DefaultMinCleanStreak {
		t.Fatalf("evidence = %v, want the runs the promotion rests on", p.Evidence)
	}
	if body.Policy.MinCleanStreak != crystallize.DefaultMinCleanStreak {
		t.Fatalf("policy streak = %d", body.Policy.MinCleanStreak)
	}
}

func TestCrystallized_EmptyLedgerIsTotalZero(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeService{})
	h.SetPatterns(crystallize.NewLedger(crystallize.Policy{}))
	r := buildRouter(h, adminTenant())
	req := httptest.NewRequest(http.MethodGet, "/v1/loops/crystallized", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body CrystallizedListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 0 || len(body.Items) != 0 {
		t.Fatalf("items = %v, want none", body.Items)
	}
}

func TestCrystallizedOne_RendersTheReviewableDocument(t *testing.T) {
	t.Parallel()
	ledger := promotedLedger(t)
	h := NewHandler(&fakeService{})
	h.SetPatterns(ledger)
	draft, err := ledger.DraftFor(ledger.Promoted()[0])
	if err != nil {
		t.Fatalf("DraftFor: %v", err)
	}
	r := buildRouter(h, adminTenant())
	req := httptest.NewRequest(http.MethodGet, "/v1/loops/crystallized/"+draft.Name(), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body CrystallizedDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Pattern.Name != draft.Name() {
		t.Fatalf("name = %q, want %q", body.Pattern.Name, draft.Name())
	}
	if body.Pattern.SafetyLevel != string(domain.SafetyL2) {
		t.Fatalf("safety = %q, want L2", body.Pattern.SafetyLevel)
	}
	if !strings.Contains(body.YAML, "systemctl") {
		t.Fatalf("yaml does not show the argv:\n%s", body.YAML)
	}
	if !strings.Contains(body.YAML, "Draft, not a release") {
		t.Fatalf("yaml does not mark itself a draft:\n%s", body.YAML)
	}
}

func TestCrystallizedOne_UnknownNameIsNotFound(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeService{})
	h.SetPatterns(promotedLedger(t))
	r := buildRouter(h, adminTenant())
	req := httptest.NewRequest(http.MethodGet, "/v1/loops/crystallized/nope", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 body=%s", w.Code, w.Body.String())
	}
}

func TestCrystallizedPromote_WritesDraftForReview(t *testing.T) {
	t.Parallel()
	ledger := promotedLedger(t)
	h := NewHandler(&fakeService{})
	h.SetPatterns(ledger)
	root := t.TempDir()
	h.SetDraftRoot(root)
	draft, err := ledger.DraftFor(ledger.Promoted()[0])
	if err != nil {
		t.Fatalf("DraftFor: %v", err)
	}
	r := buildRouter(h, adminTenant())
	req := httptest.NewRequest(http.MethodPost, "/v1/loops/crystallized/"+draft.Name()+"/promote", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body CrystallizedPromoteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Name != draft.Name() {
		t.Fatalf("name = %q, want %q", body.Name, draft.Name())
	}
	doc := body.Dir + string(os.PathSeparator) + "pig-ops.yaml"
	if _, err := os.Stat(doc); err != nil {
		t.Fatalf("draft not written to %s: %v", doc, err)
	}
}

func TestCrystallizedPromote_RefusesToOverwriteAnEditedDraft(t *testing.T) {
	t.Parallel()
	ledger := promotedLedger(t)
	h := NewHandler(&fakeService{})
	h.SetPatterns(ledger)
	h.SetDraftRoot(t.TempDir())
	draft, err := ledger.DraftFor(ledger.Promoted()[0])
	if err != nil {
		t.Fatalf("DraftFor: %v", err)
	}
	r := buildRouter(h, adminTenant())
	path := "/v1/loops/crystallized/" + draft.Name() + "/promote"
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := do(); w.Code != http.StatusOK {
		t.Fatalf("first promote = %d, body=%s", w.Code, w.Body.String())
	}
	if w := do(); w.Code != http.StatusConflict {
		t.Fatalf("second promote = %d, want 409: a re-promotion must not overwrite a review", w.Code)
	}
}

func TestCrystallizedPromote_NoRootAnswers503(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeService{})
	h.SetPatterns(promotedLedger(t))
	r := buildRouter(h, adminTenant())
	req := httptest.NewRequest(http.MethodPost, "/v1/loops/crystallized/x/promote", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// An empty pattern list is ambiguous, and the ambiguity is the whole point
// of this test. The ledger is in-memory by design, so a manager that
// restarted has a perfectly valid, perfectly empty list whose true reading
// is "nothing since boot" — not "nothing has ever been promoted". An
// operator who deployed a crystallised pattern and came back to an empty
// list has to be able to tell those apart, because the two call for
// opposite responses: one is a fleet with nothing to promote, the other is
// evidence that did not survive a restart.
//
// So the response has to carry the window it counted.
func TestCrystallized_EmptyListSaysWhichWindowItCounted(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeService{})
	h.SetPatterns(crystallize.NewLedger(crystallize.Policy{}))
	r := buildRouter(h, adminTenant())
	req := httptest.NewRequest(http.MethodGet, "/v1/loops/crystallized", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body CrystallizedListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 0 || len(body.Items) != 0 {
		t.Fatalf("items = %v, want none", body.Items)
	}
	if body.ObservingSince == "" {
		t.Fatal("an empty pattern list carries no observing_since, so it reads as " +
			"\"nothing has ever been promoted\" — which is a claim this manager cannot make")
	}
	since, err := time.Parse(time.RFC3339, body.ObservingSince)
	if err != nil {
		t.Fatalf("observing_since %q is not RFC3339: %v", body.ObservingSince, err)
	}
	if since.After(time.Now()) {
		t.Errorf("observing_since = %s, which is in the future", since)
	}
	// A zero time formatted rather than blanked is the failure this guards:
	// it looks like data, and a client sorting or diffing it cannot tell it
	// apart from a real observation start.
	if strings.HasPrefix(body.ObservingSince, "0001-") {
		t.Errorf("observing_since = %s, which is a zero time wearing a timestamp's clothes", body.ObservingSince)
	}
}

// A list that is NOT empty carries the same baseline, because a reader
// comparing two snapshots needs the window to be stated identically in
// both. A field that appears only when the answer is uncomfortable is a
// field that gets ignored.
func TestCrystallized_APopulatedListCarriesTheSameWindow(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeService{})
	h.SetPatterns(promotedLedger(t))
	r := buildRouter(h, adminTenant())
	req := httptest.NewRequest(http.MethodGet, "/v1/loops/crystallized", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var body CrystallizedListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) == 0 {
		t.Fatal("fixture produced no patterns, so this test would pass vacuously")
	}
	if body.ObservingSince == "" {
		t.Error("a populated list omits observing_since; the field must not appear only when empty")
	}
}

// Unwiring must clear the window as well as the reader. A stale timestamp
// left behind would be a second way for the response to describe a ledger
// this handler no longer has — and it would survive until the next boot.
func TestCrystallized_UnwiringClearsTheObservationWindow(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeService{})
	h.SetPatterns(promotedLedger(t))
	if h.patternsSince.IsZero() {
		t.Fatal("wiring a ledger recorded no observation start")
	}
	h.SetPatterns(nil)
	if !h.patternsSince.IsZero() {
		t.Errorf("unwiring left an observation start of %s behind", h.patternsSince)
	}
	// And the route still answers 503 rather than serving the list it held
	// a moment ago.
	r := buildRouter(h, adminTenant())
	req := httptest.NewRequest(http.MethodGet, "/v1/loops/crystallized", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status after unwiring = %d, want 503", w.Code)
	}
}

// promoteWithAudit runs the promote route with an audit slot installed and
// returns the event the handler chose to record, if any.
//
// 装 slot 是必要的：这个 handler 通过 SetAuditEvent 往请求上下文里写，
// 而 slot 由中间件安装。一条**没有**经过中间件的请求里 SetAuditEvent
// 会静默无操作——所以"没有事件"必须能被测试与"事件是空的"区分开，
// 下面那条 noEvent 就是为此存在的对照。
func promoteWithAudit(t *testing.T, r http.Handler, path string) (int, *auditport.Event) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil).
		WithContext(auditport.WithSlot(reqCtx()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	ev, ok := auditport.GetAuditEvent(req.Context())
	if !ok {
		return w.Code, nil
	}
	return w.Code, &ev
}

func reqCtx() context.Context { return context.Background() }

// 晋升是"以后由平台按这份文档执行、不再经过模型"这个决定的落点。
// 它此前不留任何审计，而同一个仓库里推进一个发布波次会留——
// 那条不对称就是本轮要关的洞。
func TestAPromotionIsAuditedWithTheArgvItWillRun(t *testing.T) {
	t.Parallel()
	ledger := promotedLedger(t)
	h := NewHandler(&fakeService{})
	h.SetPatterns(ledger)
	h.SetDraftRoot(t.TempDir())
	draft, err := ledger.DraftFor(ledger.Promoted()[0])
	if err != nil {
		t.Fatalf("DraftFor: %v", err)
	}
	want := ledger.Promoted()[0].Pattern.Action

	code, ev := promoteWithAudit(t, buildRouter(h, adminTenant()),
		"/v1/loops/crystallized/"+draft.Name()+"/promote")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if ev == nil {
		t.Fatal("the promotion left no audit event; a promotion is the one action on " +
			"this surface that changes what the platform will do, and it has to be on the chain")
	}
	if ev.Action != auditport.ActionCrystallizePromote {
		t.Errorf("action = %q, want %q", ev.Action, auditport.ActionCrystallizePromote)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q, want %q", ev.Status, auditport.StatusSuccess)
	}
	if ev.ResourceID != draft.Name() {
		t.Errorf("resource id = %q, want the promoted package name %q", ev.ResourceID, draft.Name())
	}

	// argv 是这一行存在的理由：它就是节点将要逐词执行的那份文档。
	payload, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want a map", ev.Payload)
	}
	argv, ok := payload["argv"].([]string)
	if !ok {
		t.Fatalf("payload has no argv: %v", payload)
	}
	if strings.Join(argv, "\x00") != strings.Join(want.Argv, "\x00") {
		t.Errorf("audited argv = %q, want the declaration's own %q", argv, want.Argv)
	}
	if payload["tool"] != want.Tool {
		t.Errorf("audited tool = %v, want %q", payload["tool"], want.Tool)
	}
}

// 一次失败的尝试也是事件。不入账的话，链上只剩"什么都没发生"，
// 而真实发生过一次请求——而且是那种最需要被看见的请求。
func TestAFailedPromotionIsAuditedToo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		path     string
		wantCode int
		wantErr  string
	}{
		{"unknown name", "/v1/loops/crystallized/not-a-real-pattern/promote",
			http.StatusNotFound, "not_found"},
		{"refuses to overwrite an edited draft", "", http.StatusConflict, "conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := promotedLedger(t)
			h := NewHandler(&fakeService{})
			h.SetPatterns(ledger)
			h.SetDraftRoot(t.TempDir())
			draft, err := ledger.DraftFor(ledger.Promoted()[0])
			if err != nil {
				t.Fatalf("DraftFor: %v", err)
			}
			path := tc.path
			if path == "" {
				path = "/v1/loops/crystallized/" + draft.Name() + "/promote"
				// 先成功一次，第二次才是冲突。
				if code, _ := promoteWithAudit(t, buildRouter(h, adminTenant()), path); code != http.StatusOK {
					t.Fatalf("first promote = %d, want 200", code)
				}
			}
			code, ev := promoteWithAudit(t, buildRouter(h, adminTenant()), path)
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d body=%s", code, tc.wantCode, http.StatusText(code))
			}
			if ev == nil {
				t.Fatalf("a failed promotion left no audit event; the attempt is the event")
			}
			if ev.Status != auditport.StatusFailure {
				t.Errorf("status = %q, want %q", ev.Status, auditport.StatusFailure)
			}
			if ev.ErrorCode != tc.wantErr {
				t.Errorf("error code = %q, want %q", ev.ErrorCode, tc.wantErr)
			}
		})
	}
}

// 对照：一次根本没有晋升发生（没装草稿目录）时，路由 503 且**不应该**
// 伪造一条事件。这条测试同时证明上面的断言真的能区分"有事件"与"没事件"，
// 而不是 SetAuditEvent 在任何情况下都写点什么。
func TestAPromoteThatNeverHappenedWritesNoEvent(t *testing.T) {
	t.Parallel()
	ledger := promotedLedger(t)
	h := NewHandler(&fakeService{})
	h.SetPatterns(ledger)
	// 没有 SetDraftRoot：路由在动手之前就 503。
	draft, err := ledger.DraftFor(ledger.Promoted()[0])
	if err != nil {
		t.Fatalf("DraftFor: %v", err)
	}
	code, ev := promoteWithAudit(t, buildRouter(h, adminTenant()),
		"/v1/loops/crystallized/"+draft.Name()+"/promote")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if ev != nil {
		t.Errorf("a promotion that never happened wrote an audit event: %+v", ev)
	}
}
