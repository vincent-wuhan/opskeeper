package llmgw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// The plan's security suite requires "a node's token must not carry another
// node's inference" to run in CI, and the probe method it borrows from
// 2607.14166 is why this file is shaped the way it is.
//
// The obvious version of that test is one line — a wrong credential is
// refused — and it already exists. The question a line cannot answer is
// whether anything a node *controls* can be turned into an identity, because a
// gateway that read a node id out of a header or a body field would pass every
// test that presents a well-formed credential. The whole argument for this
// package rests on the identity coming from the credential and from nowhere
// else, so the tests below present a hostile request alongside a valid one and
// assert that the hostile part is inert.

const (
	nodeACredential = "ak-node-a:sk-node-a"
	nodeBCredential = "ak-node-b:sk-node-b"
	nodeAID         = uint64(11)
	nodeBID         = uint64(22)
)

// recordingLimiter is the per-node rate gate reduced to the list of nodes it
// was asked about. It exists because the edge id is the only identity this
// package forwards anywhere: the rate bucket is keyed on it, the log line
// carries it, and the ledger is charged under it. Which nodes the gate was
// consulted about is the shortest honest witness to "who the gateway thought
// was calling".
type recordingLimiter struct {
	mu   sync.Mutex
	seen []uint64
}

func (l *recordingLimiter) Allow(_ context.Context, edgeID uint64) (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, edgeID)
	return true, ""
}

func (l *recordingLimiter) nodes() []uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]uint64(nil), l.seen...)
}

// countingCompleter is the provider stand-in, reduced to a call count. The
// assertion that matters for a refused request is that the model was never
// reached at all, and a reply value cannot show that.
type countingCompleter struct {
	mu    sync.Mutex
	calls int
}

func (c *countingCompleter) Complete(_ context.Context, _ pigmodel.Request) (*pigai.AssistantMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	reply := pigai.AssistantMessage{}
	reply.Content = append(reply.Content, pigai.TextContent{Text: "ok"})
	return &reply, nil
}

func (c *countingCompleter) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func twoNodeAuth() *stubAuth {
	return &stubAuth{edges: map[string]uint64{
		nodeACredential: nodeAID,
		nodeBCredential: nodeBID,
	}}
}

const identityBody = `{"model":"m","messages":[{"role":"user","content":"why is the disk full"}]}`

// send posts a completion with whatever credential and headers the test wants
// to be hostile.
func send(t *testing.T, gateway *Handler, credential, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	gateway.Register(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// A request carries more than its credential: headers, a query string, and a
// body PiG fills from the node's own configuration. Every one of those is
// attacker-controlled the moment a node is compromised, which is exactly the
// case the centralized credential was introduced for. So the node's own id is
// asserted in all four places at once, and the answer has to stay the node
// that authenticated — with the request served rather than refused, or the
// test would pass for the wrong reason.
func TestNothingANodeSaysCanNameTheNodeThatIsCalling(t *testing.T) {
	limiter := &recordingLimiter{}
	gateway := gatedHandler(t, twoNodeAuth(), &stubCompleter{reply: &pigai.AssistantMessage{}}, nil, limiter)

	body := `{"model":"m","edge_id":22,"node_id":22,"session_id":"node-b",` +
		`"messages":[{"role":"user","content":"why is the disk full"}]}`
	rec := send(t, gateway, nodeACredential, body, map[string]string{
		"X-Opskeeper-Edge-Id": "22",
		"X-Edge-Id":           "22",
		"X-Opskeeper-Node-Id": "22",
		"X-Forwarded-Edge-Id": "22",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a request naming the wrong node was refused rather than "+
			"served under the credential's own identity, so this test is not measuring what it "+
			"claims: %s", rec.Code, rec.Body.String())
	}
	got := limiter.nodes()
	if len(got) != 1 || got[0] != nodeAID {
		t.Errorf("the gate was consulted about %v, want [%d] — the identity came from "+
			"something in the request rather than from the credential", got, nodeAID)
	}
}

// The same claim one layer down: a node's allowance belongs to that node. The
// burst is one, so a single completion spends node A's whole bucket and
// everything after that must be refused for A and served for B. A gateway
// that keyed the bucket on a claimed id would instead let A keep calling under
// B's name until B's bucket ran out too — one node spending the fleet's
// budget, and a 429 that teaches the operator nothing.
func TestOneNodesAllowanceCannotBeSpentOnAnothers(t *testing.T) {
	gateway := gatedHandler(t, twoNodeAuth(), &stubCompleter{reply: &pigai.AssistantMessage{}},
		nil, NewLimiter(1))

	if rec := send(t, gateway, nodeACredential, identityBody, nil); rec.Code != http.StatusOK {
		t.Fatalf("node A's first request: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Node A is out of allowance and claims to be node B. It is still node A.
	rec := send(t, gateway, nodeACredential, identityBody,
		map[string]string{"X-Opskeeper-Edge-Id": "22"})
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("node A past its allowance: status = %d, want 429 — a forged identity in a "+
			"header moved the request onto another node's bucket: %s", rec.Code, rec.Body.String())
	}

	// And node B, which has spent nothing, is still served.
	if rec := send(t, gateway, nodeBCredential, identityBody, nil); rec.Code != http.StatusOK {
		t.Errorf("node B after node A exhausted itself: status = %d, want 200 — one node's spend "+
			"leaked into another's allowance: %s", rec.Code, rec.Body.String())
	}
}

// The refusal has to be total. One node's access key with another node's
// secret is not "close enough" — it is a forgery, and if it reached the model
// the fleet's audit trail would name a node that never asked anything.
// Asserted on the provider call count rather than the status code, because a
// 401 raised after the model answered would still bill the operator and still
// attribute the spend to a node that did not ask.
func TestAMixedPairIsRefusedBeforeTheModelIsReached(t *testing.T) {
	completer := &countingCompleter{}
	gateway := gatedHandler(t, twoNodeAuth(), completer, nil, nil)

	rec := send(t, gateway, "ak-node-a:sk-node-b", identityBody, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("node A's access key with node B's secret: status = %d, want 401: %s",
			rec.Code, rec.Body.String())
	}
	if calls := completer.callCount(); calls != 0 {
		t.Errorf("the model was called %d times for a forged credential; it must be called zero "+
			"times, because a refusal after the provider answered still spends money and still "+
			"attributes the spend to a node that did not ask", calls)
	}
	if !strings.Contains(rec.Body.String(), "authentication_error") {
		t.Errorf("body = %s, want the authentication_error shape so a provider client does not "+
			"retry a forgery as a transient failure", rec.Body.String())
	}
}

// A missing or malformed credential is the same refusal, reached without the
// authenticator being consulted at all. It is a separate case because the
// header parsing is the one piece of this chain that runs before anything can
// vouch for it, and a gateway that answered an anonymous request here would be
// a key dispenser with a URL.
func TestAnAnonymousRequestNeverReachesTheModel(t *testing.T) {
	completer := &countingCompleter{}
	gateway := gatedHandler(t, twoNodeAuth(), completer, nil, nil)

	for _, credential := range []string{
		"",               // no header at all
		"ak-node-a",      // no secret half
		":sk-node-a",     // no access key
		"ak-node-a:",     // empty secret
		"Basic YWtzOmNr", // the wrong scheme entirely
	} {
		rec := send(t, gateway, credential, identityBody, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("credential %q: status = %d, want 401", credential, rec.Code)
		}
	}
	if calls := completer.callCount(); calls != 0 {
		t.Errorf("the model was called %d times for anonymous requests; it must be zero", calls)
	}
}
