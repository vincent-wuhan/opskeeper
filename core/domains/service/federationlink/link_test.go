package federationlink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Link tests.
//
// Everything here is about the two questions the root has to answer before
// it is allowed to push anything at a cluster: is this caller the one that
// cluster enrolled, and did the answer that came back belong to the question
// that was asked. The dangerous mistakes are enumerable — binding on a
// refusal, pushing to a caller nobody vouched for, and believing an answer
// about a different version.

type recordedCall struct {
	edgeID uint64
	method string
	body   []byte
}

// fakeCaller records what the link tried to do and answers from a script.
type fakeCaller struct {
	mu     sync.Mutex
	calls  []recordedCall
	answer func(edgeID uint64, method string, body []byte) ([]byte, error)
}

func (c *fakeCaller) Call(_ context.Context, edgeID uint64, method string, body []byte) ([]byte, error) {
	c.mu.Lock()
	c.calls = append(c.calls, recordedCall{edgeID: edgeID, method: method, body: append([]byte(nil), body...)})
	answer := c.answer
	c.mu.Unlock()
	if answer == nil {
		return nil, errors.New("fakeCaller: no answer scripted")
	}
	return answer(edgeID, method, body)
}

func (c *fakeCaller) seen() []recordedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedCall(nil), c.calls...)
}

func (c *fakeCaller) count() int { return len(c.seen()) }

// scriptedRegistrar is the registry seen from the link's side of the wire.
type scriptedRegistrar struct {
	mu       sync.Mutex
	tokens   map[floorfed.ClusterID]string
	members  map[floorfed.ClusterID]fedbiz.Member
	attempts int
}

func newScriptedRegistrar() *scriptedRegistrar {
	return &scriptedRegistrar{
		tokens:  map[floorfed.ClusterID]string{},
		members: map[floorfed.ClusterID]fedbiz.Member{},
	}
}

// enrol mirrors what the real registry does at enrolment, so the tests
// authenticate the way production does rather than the way a mock would
// prefer.
func (r *scriptedRegistrar) enrol(id floorfed.ClusterID, token string, m fedbiz.Member) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[id] = token
	m.Cluster.ID = id
	r.members[id] = m
}

func (r *scriptedRegistrar) setMember(m fedbiz.Member) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.members[m.Cluster.ID] = m
}

// Known mirrors the real registry: a member exists or it does not, and
// knowing that says nothing about the token.
func (r *scriptedRegistrar) Known(id floorfed.ClusterID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.members[id]
	return ok
}

// It answers in Enrolled, the port's own shape, while still keeping enrolled
// members as fedbiz.Member — because a fake that stored the projection would
// stop being able to model a registry that knows more than the link asks for,
// and the point of this type is to be the registry as the link sees it.
func (r *scriptedRegistrar) Authenticate(id floorfed.ClusterID, token string, claimed floorfed.Cluster) (Enrolled, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	want, ok := r.tokens[id]
	// One error for both, because the real registry does the same and a
	// fake that told them apart would let an enumeration bug in the link
	// pass its own tests.
	if !ok || token != want || token == "" {
		return Enrolled{}, fedbiz.ErrRefused
	}
	m := r.members[id]
	m.Cluster.Name = claimed.Name
	r.members[id] = m
	return Enrolled{Acknowledged: m.Acknowledged}, nil
}

func clusterID(t *testing.T, raw string) floorfed.ClusterID {
	t.Helper()
	id, err := floorfed.NewClusterID(raw)
	if err != nil {
		t.Fatalf("NewClusterID(%q): %v", raw, err)
	}
	return id
}

const childEdgeID = uint64(4242)

func helloBody(t *testing.T, id floorfed.ClusterID, token string) []byte {
	t.Helper()
	body, err := json.Marshal(tunnel.ClusterHelloRequest{
		Cluster:           floorfed.Cluster{ID: id, Name: "north production"},
		ProvisioningToken: token,
	})
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	return body
}

func sayHello(t *testing.T, l *Links, edgeID uint64, id floorfed.ClusterID, token string) tunnel.ClusterHelloResponse {
	t.Helper()
	raw, err := l.HandleHello(context.Background(), edgeID, helloBody(t, id, token))
	if err != nil {
		t.Fatalf("HandleHello: %v", err)
	}
	var resp tunnel.ClusterHelloResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal hello response: %v", err)
	}
	return resp
}

func newTestLink(t *testing.T, reg *scriptedRegistrar) (*Links, *fakeCaller) {
	t.Helper()
	caller := &fakeCaller{}
	l, err := NewLink(caller, reg,
		WithHeartbeat(45*time.Second),
		// The link logs every bind and every refusal. That is right in
		// production and unreadable in a test that makes a dozen of
		// them, so the tests ask for a quiet logger and the log lines
		// themselves are not what these tests are about.
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	return l, caller
}

func boundLink(t *testing.T, id floorfed.ClusterID, token string) (*Links, *fakeCaller) {
	t.Helper()
	reg := newScriptedRegistrar()
	reg.enrol(id, token, fedbiz.Member{})
	l, caller := newTestLink(t, reg)
	if resp := sayHello(t, l, childEdgeID, id, token); !resp.Accepted {
		t.Fatalf("hello was refused: %s", resp.Reason)
	}
	return l, caller
}

func bundleFor(id floorfed.ClusterID, version uint64) tunnel.ClusterPolicyRequest {
	return tunnel.ClusterPolicyRequest{
		Bundle: floorfed.Bundle{
			ClusterID: id,
			Version:   version,
			// Envelope is not filled in: these tests are about the
			// channel, and a bundle that fails the child's own
			// validation is covered on the child's side.
		},
		StagedPath: "/var/lib/opskeeper/staging/v1",
	}
}

func TestHelloBindsTheCallerToTheCluster(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	l, caller := boundLink(t, id, "tok-north")

	edgeID, ok := l.Bound(id)
	if !ok || edgeID != childEdgeID {
		t.Fatalf("Bound = (%d, %v), want (%d, true)", edgeID, ok, childEdgeID)
	}
	if caller.count() != 0 {
		t.Errorf("a hello made %d outbound calls; it should make none", caller.count())
	}
}

func TestHelloReportsWhatTheRootBelievesRatherThanWhatItPublished(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	reg := newScriptedRegistrar()
	reg.enrol(id, "tok-north", fedbiz.Member{Acknowledged: 7, HighestIssued: 9})
	l, _ := newTestLink(t, reg)

	resp := sayHello(t, l, childEdgeID, id, "tok-north")
	if !resp.Accepted {
		t.Fatalf("hello was refused: %s", resp.Reason)
	}
	// 7, not 9. The root's newest publish is a fact about the root's
	// ledger; what the child is enforcing is a fact about the child, and
	// a root that reported 9 here would tell a child that is already
	// ahead of the ledger that it is behind it.
	if resp.PolicyVersion != 7 {
		t.Errorf("policy_version = %d, want the acknowledged 7 rather than the published 9", resp.PolicyVersion)
	}
	if resp.HeartbeatSeconds != 45 {
		t.Errorf("heartbeat_seconds = %d, want 45", resp.HeartbeatSeconds)
	}
}

// TestARefusedHelloDoesNotBind is the property the whole binding table rests
// on: a token that does not match leaves no trace, and the same caller may
// still say hello correctly a moment later.
func TestARefusedHelloDoesNotBind(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	reg := newScriptedRegistrar()
	reg.enrol(id, "tok-north", fedbiz.Member{})
	l, _ := newTestLink(t, reg)

	if resp := sayHello(t, l, childEdgeID, id, "tok-wrong"); resp.Accepted {
		t.Fatalf("a wrong token was accepted: %+v", resp)
	}
	if _, ok := l.Bound(id); ok {
		t.Fatal("a refused hello left a binding behind")
	}
	if resp := sayHello(t, l, childEdgeID, id, "tok-north"); !resp.Accepted {
		t.Fatalf("the correct hello after a refusal was also refused: %s", resp.Reason)
	}
}

// TestEveryRegistryRefusalSoundsTheSame: a caller that can tell "no such
// cluster" from "wrong token" can enumerate which clusters exist, and the
// registry goes to real lengths not to. The link must not undo that.
func TestEveryRegistryRefusalSoundsTheSame(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	reg := newScriptedRegistrar()
	reg.enrol(id, "tok-north", fedbiz.Member{})
	l, _ := newTestLink(t, reg)

	unknown := sayHello(t, l, childEdgeID, clusterID(t, "prod-cn-south"), "tok-north")
	wrongToken := sayHello(t, l, childEdgeID, id, "tok-wrong")
	emptyToken := sayHello(t, l, childEdgeID, id, "")

	if unknown.Accepted || wrongToken.Accepted || emptyToken.Accepted {
		t.Fatalf("a refusal was accepted: unknown=%+v wrong=%+v empty=%+v", unknown, wrongToken, emptyToken)
	}
	if unknown.Reason != wrongToken.Reason {
		t.Errorf("unknown-cluster refusal says %q but a wrong token says %q; those two must be the same sentence",
			unknown.Reason, wrongToken.Reason)
	}
	if strings.Contains(unknown.Reason, "prod-cn") || strings.Contains(wrongToken.Reason, "tok-") {
		t.Errorf("a refusal echoed the caller's own input back: %q / %q", unknown.Reason, wrongToken.Reason)
	}
}

// TestAHelloWithNoSessionIsRefused covers the case the tunnel should make
// impossible. If it is ever reachable, binding on it would hand a cluster to
// anyone who can open a TCP connection.
func TestAHelloWithNoSessionIsRefused(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	reg := newScriptedRegistrar()
	reg.enrol(id, "tok-north", fedbiz.Member{})
	l, _ := newTestLink(t, reg)

	resp := sayHello(t, l, 0, id, "tok-north")
	if resp.Accepted {
		t.Fatalf("a hello with no authenticated session was accepted: %+v", resp)
	}
	if _, ok := l.Bound(id); ok {
		t.Fatal("a hello with no session left a binding behind")
	}
}

// TestAnUnreadableHelloIsAnErrorNotARefusal keeps a version-skewed child
// retrying instead of concluding it was turned away.
func TestAnUnreadableHelloIsAnErrorNotARefusal(t *testing.T) {
	l, _ := newTestLink(t, newScriptedRegistrar())
	if _, err := l.HandleHello(context.Background(), childEdgeID, []byte("{not json")); err == nil {
		t.Fatal("a hello this root cannot parse was answered rather than refused")
	}
}

func TestPushGoesToTheBoundCallerCarryingTheBundle(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	l, caller := boundLink(t, id, "tok-north")

	sent := bundleFor(id, 3)
	var got tunnel.ClusterPolicyRequest
	caller.answer = func(_ uint64, _ string, body []byte) ([]byte, error) {
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal push: %v", err)
		}
		return json.Marshal(tunnel.ClusterPolicyResponse{
			Outcome: floorfed.Outcome{Version: 3, Accepted: true, Live: 3},
		})
	}

	resp, err := l.PushPolicy(context.Background(), id, sent)
	if err != nil {
		t.Fatalf("PushPolicy: %v", err)
	}
	if !resp.Outcome.Accepted || resp.Outcome.Version != 3 {
		t.Errorf("outcome = %+v, want version 3 accepted", resp.Outcome)
	}
	calls := caller.seen()
	if len(calls) != 1 {
		t.Fatalf("made %d calls, want 1", len(calls))
	}
	if calls[0].edgeID != childEdgeID {
		t.Errorf("pushed to edge %d, want the bound %d", calls[0].edgeID, childEdgeID)
	}
	if calls[0].method != tunnel.MethodClusterPolicy {
		t.Errorf("called %q, want %q", calls[0].method, tunnel.MethodClusterPolicy)
	}
	if got.Bundle.Version != 3 || got.StagedPath != sent.StagedPath {
		t.Errorf("the child received %+v, want version 3 at %q", got.Bundle, sent.StagedPath)
	}
}

// TestPushToAnUnboundClusterTouchesNothing: a root that has not seen a child
// since enrolling it must not reach out to a caller id it once remembered,
// because the broker will have handed that number to somebody else.
func TestPushToAnUnboundClusterTouchesNothing(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	l, caller := boundLink(t, id, "tok-north")
	l.Forget(childEdgeID)

	_, err := l.PushPolicy(context.Background(), id, bundleFor(id, 1))
	if !errors.Is(err, ErrUnbound) {
		t.Fatalf("push after a disconnect = %v, want ErrUnbound", err)
	}
	if caller.count() != 0 {
		t.Errorf("a push to an unbound cluster made %d calls", caller.count())
	}
	if _, err := l.AskState(context.Background(), id); !errors.Is(err, ErrUnbound) {
		t.Errorf("ask after a disconnect = %v, want ErrUnbound", err)
	}
}

// TestAnAnswerAboutAnotherVersionIsRefused: recording it would put a version
// in the ledger that this root never published, and the console would render
// it as a version the cluster adopted.
func TestAnAnswerAboutAnotherVersionIsRefused(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	l, caller := boundLink(t, id, "tok-north")
	caller.answer = func(uint64, string, []byte) ([]byte, error) {
		return json.Marshal(tunnel.ClusterPolicyResponse{
			Outcome: floorfed.Outcome{Version: 2, Accepted: true},
		})
	}

	resp, err := l.PushPolicy(context.Background(), id, bundleFor(id, 3))
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("an answer about version 2 to a push of 3 = %v, want ErrProtocol", err)
	}
	if resp.Outcome.Version != 0 {
		t.Errorf("a protocol failure still returned a usable outcome %+v; a caller that checked the error first would still be tempted to record it", resp.Outcome)
	}
}

// TestARetryableAnswerIsNotJudgedByItsVersion is why Retryable exists: the
// child could not have decided, so it owes the root no version number and
// the root must not treat the missing one as a mismatch.
func TestARetryableAnswerIsNotJudgedByItsVersion(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	l, caller := boundLink(t, id, "tok-north")
	caller.answer = func(uint64, string, []byte) ([]byte, error) {
		return json.Marshal(tunnel.ClusterPolicyResponse{
			Outcome:   floorfed.Outcome{Version: 0},
			Retryable: true,
			Error:     "staged policy not present yet",
		})
	}

	resp, err := l.PushPolicy(context.Background(), id, bundleFor(id, 3))
	if err != nil {
		t.Fatalf("a retryable answer was treated as a failure: %v", err)
	}
	if !resp.Retryable {
		t.Errorf("retryable = false after a transport that preserved it")
	}
}

// TestATransportFailureIsNotARefusal: the version the push was for is still
// worth sending again, and the only way a caller can know that is if this
// comes back as a transport error rather than a verdict.
func TestATransportFailureIsNotARefusal(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	l, caller := boundLink(t, id, "tok-north")
	caller.answer = func(uint64, string, []byte) ([]byte, error) {
		return nil, errors.New("broker: no route to that caller")
	}

	_, err := l.PushPolicy(context.Background(), id, bundleFor(id, 5))
	if err == nil {
		t.Fatal("a transport failure came back as a verdict")
	}
	if errors.Is(err, ErrProtocol) || errors.Is(err, ErrUnbound) {
		t.Errorf("a transport failure was classified as %v", err)
	}
	if !strings.Contains(err.Error(), "version 5") {
		t.Errorf("refusal = %v, want it to name the version so a retry knows what was lost", err)
	}
}

func TestAskStateCarriesTheClusterAndChecksWhoseAnswerItIs(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	l, caller := boundLink(t, id, "tok-north")

	var got tunnel.ClusterStateRequest
	caller.answer = func(_ uint64, _ string, body []byte) ([]byte, error) {
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal state request: %v", err)
		}
		return json.Marshal(tunnel.ClusterStateResponse{
			Policy:          4,
			Bundle:          floorfed.Bundle{ClusterID: id, Version: 4},
			LastRootContact: time.Now().Add(-90 * time.Minute),
			Enforcing:       true,
			NodeCount:       12,
		})
	}

	st, err := l.AskState(context.Background(), id)
	if err != nil {
		t.Fatalf("AskState: %v", err)
	}
	if got.Cluster != id {
		t.Errorf("the child was asked about %q, want %q", got.Cluster, id)
	}
	if st.Policy != 4 || st.NodeCount != 12 {
		t.Errorf("state = %+v, want version 4 across 12 nodes", st)
	}
	if st.LastRootContact.IsZero() {
		t.Error("the child's own last contact with a root was dropped")
	}
}

// TestAnAnswerAboutAnotherClusterIsRefused: reporting a bundle addressed to
// somebody else as this cluster's state would put the wrong policy in front
// of an operator deciding whether a rollout landed.
func TestAnAnswerAboutAnotherClusterIsRefused(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	l, caller := boundLink(t, id, "tok-north")
	caller.answer = func(uint64, string, []byte) ([]byte, error) {
		return json.Marshal(tunnel.ClusterStateResponse{
			Policy: 4,
			Bundle: floorfed.Bundle{ClusterID: clusterID(t, "prod-cn-south"), Version: 4},
		})
	}

	if _, err := l.AskState(context.Background(), id); !errors.Is(err, ErrProtocol) {
		t.Fatalf("an answer carrying another cluster's bundle = %v, want ErrProtocol", err)
	}
}

// TestAChildThatHasNeverBeenToldAnythingIsNotAMismatch: its bundle is
// legitimately empty, and treating that as somebody else's answer would make
// a fresh cluster permanently unreadable.
func TestAChildThatHasNeverBeenToldAnythingIsNotAMismatch(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	l, caller := boundLink(t, id, "tok-north")
	caller.answer = func(uint64, string, []byte) ([]byte, error) {
		return json.Marshal(tunnel.ClusterStateResponse{Policy: 0, Enforcing: false})
	}

	st, err := l.AskState(context.Background(), id)
	if err != nil {
		t.Fatalf("AskState on a cluster that has heard nothing: %v", err)
	}
	if st.Enforcing {
		t.Error("a cluster enforcing version 0 reported itself as enforcing")
	}
}

// TestForgetDropsEveryClusterThatCallerSpokeFor: the caller id is opaque and
// the broker recycles it, so a stale binding eventually points a policy push
// at a process that never said hello.
func TestForgetDropsEveryClusterThatCallerSpokeFor(t *testing.T) {
	north, south := clusterID(t, "prod-cn-north"), clusterID(t, "prod-cn-south")
	reg := newScriptedRegistrar()
	reg.enrol(north, "tok-north", fedbiz.Member{})
	reg.enrol(south, "tok-south", fedbiz.Member{})
	l, _ := newTestLink(t, reg)
	sayHello(t, l, childEdgeID, north, "tok-north")
	sayHello(t, l, childEdgeID, south, "tok-south")
	sayHello(t, l, 99, north, "tok-north") // the other caller wins north

	// Two clusters claimed this caller, but north had already moved to
	// caller 99, so only one of them loses anything here.
	if got := l.Forget(childEdgeID); got != 1 {
		t.Errorf("Forget released %d bindings, want the 1 that still pointed at it", got)
	}
	if _, ok := l.Bound(south); ok {
		t.Error("a cluster whose only caller went away is still bound")
	}
	if edgeID, ok := l.Bound(north); !ok || edgeID != 99 {
		t.Errorf("north = (%d, %v), want the surviving caller 99", edgeID, ok)
	}
}

func TestAReconnectingChildIsBoundToItsNewCaller(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	l, caller := boundLink(t, id, "tok-north")
	sayHello(t, l, 77, id, "tok-north")

	edgeID, ok := l.Bound(id)
	if !ok || edgeID != 77 {
		t.Fatalf("Bound = (%d, %v), want the caller that said hello last", edgeID, ok)
	}
	caller.answer = func(uint64, string, []byte) ([]byte, error) {
		return json.Marshal(tunnel.ClusterPolicyResponse{Outcome: floorfed.Outcome{Version: 1, Accepted: true}})
	}
	if _, err := l.PushPolicy(context.Background(), id, bundleFor(id, 1)); err != nil {
		t.Fatalf("PushPolicy after a reconnect: %v", err)
	}
	if got := caller.seen(); len(got) != 1 || got[0].edgeID != 77 {
		t.Errorf("the push went to %+v, want caller 77", got)
	}
}
