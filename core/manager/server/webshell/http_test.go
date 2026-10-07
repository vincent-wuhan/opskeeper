package webshell

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	bizwebshell "github.com/vincent-wuhan/opskeeper/core/manager/biz/webshell"
	wsmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/webshell"
)

// fleetBeyondTheOldPage is the number this whole file is about.
//
// The implementation this replaced asked the edge store for the newest
// thousand edges and looked for the one belonging to the device in Go. A
// fleet of a thousand and one therefore produced "device offline or unknown"
// about a host that was answering heartbeats, and the operator's next move
// was to go and reboot a machine that was fine.
//
// A regression test for that has to put the target past the old page, or it
// passes against the old code and proves nothing. Everything below is
// therefore anchored to a device whose edge id is beyond it.
const fleetBeyondTheOldPage = 1000

type fakeLinks struct {
	edgeID uint64
	err    error
	calls  int
	seen   []uint64
}

// The relation argument this fake used to check is gone, and its removal is
// the point: it asserted that the value arriving was Host, which is a test
// of a fact the caller had no way to state differently. Asserting a constant
// looks like coverage and measures nothing — but it is also the only reason
// anyone ever noticed the parameter was constant.
func (f *fakeLinks) LookupEdgeForDevice(_ context.Context, deviceID uint64) (uint64, error) {
	f.calls++
	f.seen = append(f.seen, deviceID)
	return f.edgeID, f.err
}

// fakeEdgeStatus answers the presence question and nothing else, which is
// what the port now permits.
//
// It used to hold a `*edgemodel.Edge` and check that the id it was asked for
// matched the id in the row. That check was the only place the fake could
// disagree with a real store, and it existed because the port handed out a
// row: a port that returns one string cannot be asked about a row it did not
// ask for, so the id check went with the row (decision 251).
type fakeEdgeStatus struct {
	status string
	err    error
	calls  int
}

func (f *fakeEdgeStatus) PresenceStatus(_ context.Context, _ uint64) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.status, nil
}

func newHandlerUnderTest(links DeviceLinks, edges domain.EdgeStatusQuery) *Handler {
	return &Handler{links: links, edges: edges}
}

// TestADeviceBeyondTheOldPageStillResolves is the regression this file exists
// for. The old code could not pass it: it had no way to ask about edge 4321
// without listing a page, and the page stopped at 1000.
func TestADeviceBeyondTheOldPageStillResolves(t *testing.T) {
	const (
		deviceID = uint64(77)
		edgeID   = uint64(fleetBeyondTheOldPage + 341)
	)
	links := &fakeLinks{edgeID: edgeID}
	edges := &fakeEdgeStatus{status: domain.EdgeStatusOnline}

	got, err := newHandlerUnderTest(links, edges).resolveEdge(context.Background(), deviceID)
	if err != nil {
		t.Fatalf("resolveEdge: %v", err)
	}
	if got != edgeID {
		t.Fatalf("resolved edge %d, want %d", got, edgeID)
	}
}

// TestResolvingADeviceCostsTwoPointLookups pins the property that makes the
// bug impossible rather than merely absent: the answer no longer depends on
// how many edges exist, because nothing lists them.
//
// Counting the calls is the assertion. A test that only checks the returned
// id would still be green against an implementation that lists the whole
// table and filters it correctly, which is the shape that was wrong.
func TestResolvingADeviceCostsTwoPointLookups(t *testing.T) {
	links := &fakeLinks{edgeID: 9}
	edges := &fakeEdgeStatus{status: domain.EdgeStatusOnline}

	if _, err := newHandlerUnderTest(links, edges).resolveEdge(context.Background(), 3); err != nil {
		t.Fatalf("resolveEdge: %v", err)
	}
	if links.calls != 1 {
		t.Errorf("junction looked up %d times, want 1: the device-to-edge relation is a single row", links.calls)
	}
	if edges.calls != 1 {
		t.Errorf("edge status read %d times, want 1: it is a primary key read", edges.calls)
	}
}

// TestTheEdgePortStaysOneMethodWide stops the fix from being undone by
// widening. Nothing stops somebody handing this handler the full edge
// repository again, and the full repository has a List on it — at which
// point the page comes back, and with it the fleet-size cliff.
//
// A method count is an odd thing to assert, and it is asserted here because
// the alternative is a comment, and a comment is what the next person
// optimises away.
func TestTheEdgePortStaysOneMethodWide(t *testing.T) {
	got := reflect.TypeOf((*domain.EdgeStatusQuery)(nil)).Elem()
	if got.NumMethod() != 1 {
		t.Fatalf("domain.EdgeStatusQuery has %d methods (%v); a second one is how List comes back, "+
			"and List is what made a fleet of a thousand and one look like a dead host",
			got.NumMethod(), methodNames(got))
	}
	if name := got.Method(0).Name; name != "PresenceStatus" {
		t.Errorf("domain.EdgeStatusQuery's one method is %s, want PresenceStatus", name)
	}
}

// TestTheEdgePortAnswersWithAValueAndNotARow is the second half of the same
// guard, and it is the half that would have caught the previous shape.
//
// A method count alone is satisfied by a one-method port that returns
// `*edgemodel.Edge`, which is exactly what this was. What that shape cost is
// not the method count: it is that a row can be absent, so the holder has to
// carry a branch for `(nil, nil)` — a state no repository in this tree
// produces, and therefore a branch no test can reach and no reviewer can
// justify. The return type is therefore measured, on the type, rather than
// inferred from the body.
func TestTheEdgePortAnswersWithAValueAndNotARow(t *testing.T) {
	method, ok := reflect.TypeOf((*domain.EdgeStatusQuery)(nil)).Elem().MethodByName("PresenceStatus")
	if !ok {
		t.Fatal("domain.EdgeStatusQuery has no PresenceStatus method")
	}
	outs := method.Type.NumOut()
	if outs != 2 {
		t.Fatalf("PresenceStatus returns %d values (%v); the port answers one question, "+
			"so a third return value would be a second question nobody asked",
			outs, method.Type)
	}
	// The value is the FIRST return and the error is the second. The first
	// draft of this guard read slot 1, and it failed green-on-arrival for
	// the wrong reason: slot 1 is `error`, so it reported "interface" for a
	// method whose answer is a plain string. A guard that measures the wrong
	// slot is worse than no guard, because it passes for a reason nobody
	// chose — this comment is here so the next reader does not "fix" it by
	// relaxing the assertion instead of reading the signature.
	if got := method.Type.Out(0).Kind(); got != reflect.String {
		t.Errorf("PresenceStatus's first return is %s, want string; a pointer or a struct "+
			"lets an absent row back in, and the absent-row branch is what this port exists "+
			"to delete", got)
	}
	if got := method.Type.Out(1); got != reflect.TypeOf((*error)(nil)).Elem() {
		t.Errorf("PresenceStatus's second return is %v, want error", got)
	}
}

func methodNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		out = append(out, t.Method(i).Name)
	}
	return out
}

// TestTheThreeWaysAShellCannotOpenSayDifferentThings is about the log, not
// the wire. The browser still gets one 503 — that contract is unchanged and
// a client that only knows "not now" is easier to serve than one that has to
// understand our reasons. But the three reasons are genuinely different
// incidents: a host nobody registered, a host whose agent died, and a row
// that was deleted out from under a live session. An operator reading the
// log needs to be able to tell them apart, and "device offline or unknown"
// told them nothing.
func TestTheThreeWaysAShellCannotOpenSayDifferentThings(t *testing.T) {
	cases := []struct {
		name  string
		links *fakeLinks
		edges *fakeEdgeStatus
		wants []string
	}{
		{
			name:  "no edge was ever registered for the device",
			links: &fakeLinks{err: errors.New("edge_devices: not found")},
			edges: &fakeEdgeStatus{status: domain.EdgeStatusOnline},
			wants: []string{"no edge registered", "not found"},
		},
		{
			name:  "the junction row points at nothing",
			links: &fakeLinks{edgeID: 0},
			edges: &fakeEdgeStatus{status: domain.EdgeStatusOnline},
			wants: []string{"no edge registered"},
		},
		{
			name:  "the agent is registered but not answering",
			links: &fakeLinks{edgeID: 12},
			edges: &fakeEdgeStatus{status: domain.EdgeStatusOffline},
			wants: []string{"is offline"},
		},
		{
			name:  "the edge row disappeared",
			links: &fakeLinks{edgeID: 12},
			edges: &fakeEdgeStatus{err: errors.New("record not found")},
			wants: []string{"read edge 12"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := newHandlerUnderTest(tc.links, tc.edges).resolveEdge(context.Background(), 5)
			if err == nil {
				t.Fatalf("resolveEdge returned edge %d, want an error", id)
			}
			if id != 0 {
				t.Errorf("resolveEdge returned edge id %d alongside an error; a caller that "+
					"checks the id first would open a stream to an edge it could not verify", id)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q; the three failures are different "+
						"incidents and the log is the only place they can be told apart", err, want)
				}
			}
		})
	}
}

// TestAnUnwiredLookupRefusesRatherThanGuessing covers the boot-time mistake.
// A nil dependency used to sit in a struct field that nothing read, so a
// miswiring was invisible until something else broke. Now the field is on
// the path, and the only safe answer to "nobody told me how to find the
// edge" is to refuse.
func TestAnUnwiredLookupRefusesRatherThanGuessing(t *testing.T) {
	if _, err := (&Handler{}).resolveEdge(context.Background(), 1); err == nil {
		t.Fatal("a handler with no lookups wired resolved a device; it must refuse instead")
	}
}

// --- 决策 333：谁掐掉了这个会话 --------------------------------------------------
//
// webshell 是通向生产机器的一条交互线路，线路里坐着某个人的凭据。关掉它不是
// 状态变化，是一个动作——调查时问的正是这个动作。所以这一行必须存在，且必须
// 带上「是哪台机器、是谁、转发给了哪个副本」。
//
// 跨副本那条路径尤其要记：转发之后，事后无法区分「这里杀掉的」与「那里杀掉的」。

type killableSink struct{ killedWith string }

func (k *killableSink) OnOutput([]byte) error { return nil }
func (k *killableSink) OnExit(int, string)    {}
func (k *killableSink) Kill(reason string)    { k.killedWith = reason }

func TestKillSessionWritesWhoCutWhoseSession(t *testing.T) {
	router := bizwebshell.NewRouter()
	sink := &killableSink{}
	router.Register("sess-42", sink, bizwebshell.ActiveSession{
		SessionID: "sess-42", OpskeeperUserID: 9, SSHUser: "ops", EdgeID: 4,
	})
	h := NewHandler(nil, router, nil, nil, nil, nil)
	// Through the real router, not straight into the handler: the handler
	// reads the session id out of the chi route context, and calling it
	// directly would hand it an empty id — which would then 404 for a reason
	// that has nothing to do with the thing under test.
	r := chi.NewRouter()
	h.Register(r)

	req := httptest.NewRequest(http.MethodDelete, "/v1/webshell/sessions/sess-42", nil)
	req = req.WithContext(tenantctx.With(req.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	ev, ok := auditport.GetAuditEvent(req.Context())
	if !ok {
		t.Fatal("no row: \"who cut this person off\" is the question an investigation asks first")
	}
	if ev.Action != auditport.ActionWebshellSessionKill {
		t.Errorf("action = %q, want webshell_session_kill", ev.Action)
	}
	if ev.ResourceType != auditport.ResourceWebshellSession || ev.ResourceID != "sess-42" {
		t.Errorf("resource = %q/%q, want webshell_session/sess-42", ev.ResourceType, ev.ResourceID)
	}
	if sink.killedWith != wsmodel.TerminatedByAdminKill {
		t.Errorf("the session was closed with %q; the row and the act must agree", sink.killedWith)
	}
}

// 杀不掉就不该有成功行——这条测的是「没发生的事不会被记成发生了」。
func TestKillSessionThatFindsNothingLandsNoRow(t *testing.T) {
	router := bizwebshell.NewRouter()
	h := NewHandler(nil, router, nil, nil, nil, nil)
	r := chi.NewRouter()
	h.Register(r)

	req := httptest.NewRequest(http.MethodDelete, "/v1/webshell/sessions/nope", nil)
	req = req.WithContext(tenantctx.With(req.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ev, ok := auditport.GetAuditEvent(req.Context()); ok {
		t.Errorf("a kill that found nothing still wrote %s/%s", ev.Action, ev.ResourceID)
	}
}
