package federation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"

	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// HTTP tests.
//
// The service's own behaviour is covered in biz/federation. What is under test
// here is the shape of the answer: who may ask, what a refusal looks like on
// the wire, and — the one that is easy to get wrong — that the console can
// never be handed a credential by accident.

func asRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if role == "" {
				next.ServeHTTP(w, r)
				return
			}
			tn := tenantctx.Tenant{UserID: 1, Role: role}
			next.ServeHTTP(w, r.WithContext(tenantctx.With(r.Context(), tn)))
		})
	}
}

func newServer(t *testing.T, svc Service, push fedbiz.Pusher, role string) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Use(asRole(role))
	h := NewHandler(svc)
	h.SetPusher(push)
	h.Register(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		out = nil
	}
	return resp.StatusCode, out
}

// realService wires the actual registry and publisher over a signed tree, so
// these tests exercise the same code path an operator's click takes.
func realService(t *testing.T) (Service, *pluginmanifest.Signer, string) {
	t.Helper()
	signer, _, err := pluginmanifest.GenerateSigner("release-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	reg := fedbiz.NewRegistry(nil)
	pub, err := fedbiz.NewPublisher(reg, signer)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	svc, err := fedbiz.NewService(reg, pub)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, signer, signedTreeOnDisk(t)
}

func signedTreeOnDisk(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "opskeeper-sre-readonly")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: opskeeper-sre-readonly
  version: 1.0.0
  vendor: acme
  homepage: https://example.invalid/readonly
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  tools:
    - {name: host_probe_tcp, class: read}
  required_scopes:
    - host.read
  audit: {emits: true, mutates: false}
  approval: {required: false}
  install: {strategy: rolling, min_edge_version: 0.1.0}
`
	for path, body := range map[string]string{
		filepath.Join(root, pluginmanifest.ManifestFile):      manifest,
		filepath.Join(root, "extensions", "tool", "tools.go"): "package tool\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return root
}

// fakePusher scripts the tunnel side.
type fakePusher struct {
	state    tunnel.ClusterStateResponse
	stateErr error

	pushes   int
	lastPush tunnel.ClusterPolicyRequest
	verdict  tunnel.ClusterPolicyResponse
	pushErr  error
}

func (p *fakePusher) PushPolicy(_ context.Context, _ floorfed.ClusterID, req tunnel.ClusterPolicyRequest) (tunnel.ClusterPolicyResponse, error) {
	p.pushes++
	p.lastPush = req
	return p.verdict, p.pushErr
}

func (p *fakePusher) AskState(context.Context, floorfed.ClusterID) (tunnel.ClusterStateResponse, error) {
	return p.state, p.stateErr
}

// TestEveryRouteIsAdmin covers the whole surface at once, because the property
// is "there is no non-admin route" and enumerating them by hand in one place
// is the only way that stays true as routes are added.
func TestEveryRouteIsAdmin(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, nil, "viewer")

	calls := []struct{ method, path, body string }{
		{"POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`},
		{"GET", "/v1/federation/clusters", ""},
		{"GET", "/v1/federation/clusters/prod-cn-north", ""},
		{"POST", "/v1/federation/clusters/prod-cn-north/policy", `{"staged_root":"/tmp/x"}`},
		{"POST", "/v1/federation/clusters/prod-cn-north/policy/ack", `{"outcome":{"version":1}}`},
		{"GET", "/v1/federation/clusters/prod-cn-north/state", ""},
	}
	for _, c := range calls {
		code, _ := do(t, srv, c.method, c.path, c.body)
		if code != http.StatusForbidden {
			t.Errorf("%s %s = %d as a viewer, want 403", c.method, c.path, code)
		}
	}
}

func TestNoSessionIsUnauthorizedNotInternal(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, nil, "")

	// A missing session is a missing session, not a crash. Reporting it as
	// 500 sends an operator to the manager's logs for a missing cookie.
	code, _ := do(t, srv, "GET", "/v1/federation/clusters", "")
	if code != http.StatusUnauthorized {
		t.Errorf("GET without a session = %d, want 401", code)
	}
}

func TestEnrollReturnsTheTokenExactlyOnce(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, nil, "admin")

	code, body := do(t, srv, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north","name":"华东生产一区"}`)
	if code != http.StatusCreated {
		t.Fatalf("enroll = %d, want 201 (%v)", code, body)
	}
	token, _ := body["provisioning_token"].(string)
	if len(token) < 40 {
		t.Fatalf("provisioning token is %d characters", len(token))
	}

	// And it never comes back on a read. A read endpoint is the one place a
	// credential has no business appearing, and the only place a console
	// would be tempted to cache it.
	_, list := do(t, srv, "GET", "/v1/federation/clusters", "")
	if strings.Contains(mustJSON(t, list), token) {
		t.Errorf("the list response carried the provisioning token")
	}
	if strings.Contains(mustJSON(t, list), "token_hash") {
		t.Errorf("the list response carried the stored credential hash")
	}
	_, one := do(t, srv, "GET", "/v1/federation/clusters/prod-cn-north", "")
	if strings.Contains(mustJSON(t, one), token) {
		t.Errorf("the detail response carried the provisioning token")
	}
}

func TestEnrollRefusesAnIdentityItCannotStore(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, nil, "admin")

	for _, id := range []string{"", "Prod/North", "-leading", "UPPER"} {
		code, _ := do(t, srv, "POST", "/v1/federation/clusters", `{"id":"`+id+`"}`)
		if code != http.StatusBadRequest {
			t.Errorf("enroll %q = %d, want 400", id, code)
		}
	}
}

func TestAnEmptyListIsAListAndNotNull(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, nil, "admin")

	_, body := do(t, srv, "GET", "/v1/federation/clusters", "")
	raw, ok := body["clusters"]
	if !ok {
		t.Fatalf("response has no clusters key: %v", body)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("clusters = %T, want an array — a null here is indistinguishable from a failed parse", raw)
	}
	if len(list) != 0 {
		t.Errorf("clusters = %d entries, want 0", len(list))
	}
}

func TestAnUnknownClusterIsNotFound(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, nil, "admin")

	if code, _ := do(t, srv, "GET", "/v1/federation/clusters/prod-cn-south", ""); code != http.StatusNotFound {
		t.Errorf("GET an unenrolled cluster = %d, want 404", code)
	}
	// A path that is not even a cluster identity answers the same way, so
	// the route is not an existence oracle for differently-spelled ids.
	if code, _ := do(t, srv, "GET", "/v1/federation/clusters/Prod%2FNorth", ""); code != http.StatusNotFound {
		t.Errorf("GET a malformed id = %d, want 404", code)
	}
}

func TestPublishSignsTheTreeAndReportsWhatWentOut(t *testing.T) {
	svc, signer, tree := realService(t)
	srv := newServer(t, svc, nil, "admin")
	do(t, srv, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`)

	code, body := do(t, srv, "POST", "/v1/federation/clusters/prod-cn-north/policy",
		`{"staged_root":`+mustJSONString(t, tree)+`,"reason":"widen read scope"}`)
	if code != http.StatusOK {
		t.Fatalf("publish = %d, want 200 (%v)", code, body)
	}
	b, _ := body["bundle"].(map[string]any)
	if b == nil {
		t.Fatalf("response has no bundle: %v", body)
	}
	if b["version"] != float64(1) {
		t.Errorf("version = %v, want 1", b["version"])
	}
	if b["package_name"] != "opskeeper-sre-readonly" {
		t.Errorf("package_name = %v", b["package_name"])
	}
	if b["reason"] != "widen read scope" {
		t.Errorf("reason = %v", b["reason"])
	}
	// The envelope on the wire must be the root's own signature, not
	// something a caller supplied — the endpoint takes a tree, never a
	// signature.
	env, _ := b["envelope"].(string)
	if env == "" {
		t.Fatalf("no envelope in the bundle")
	}
	decoded, err := decodeTransport(env)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if decoded.KeyID != signer.KeyID() {
		t.Errorf("envelope key = %q, want the root's %q", decoded.KeyID, signer.KeyID())
	}
	// The tree on disk stays unsigned: the root never writes a signature
	// into a directory the operator may still be editing.
	if _, err := pluginmanifest.ReadEnvelope(tree); err == nil {
		t.Errorf("publishing wrote %s into the operator's tree", pluginmanifest.SignatureFile)
	}
	if body["highest_issued"] != float64(1) {
		t.Errorf("highest_issued = %v, want 1 — the response must report post-publish state", body["highest_issued"])
	}
}

func TestPublishRefusesATreeItCannotSign(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, nil, "admin")
	do(t, srv, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`)

	// A directory that is not a package. This is the operator pointing at
	// the wrong path, and it is a 400 with a code that says so — not a 500
	// and, critically, not a consumed version number.
	code, body := do(t, srv, "POST", "/v1/federation/clusters/prod-cn-north/policy",
		`{"staged_root":"/definitely/not/a/package"}`)
	if code != http.StatusBadRequest {
		t.Errorf("publish of an unsigned directory = %d, want 400 (%v)", code, body)
	}
	if errCode(body) != "nothing_to_publish" {
		t.Errorf("code = %q, want nothing_to_publish", errCode(body))
	}

	_, one := do(t, srv, "GET", "/v1/federation/clusters/prod-cn-north", "")
	if behind, _ := one["highest_issued"].(float64); behind != 0 {
		t.Errorf("highest_issued = %v after a failed publish, want 0 — a version must not be spent on a failure", one["highest_issued"])
	}
}

func TestPublishRequiresAStagedRoot(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, nil, "admin")
	do(t, srv, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`)

	if code, _ := do(t, srv, "POST", "/v1/federation/clusters/prod-cn-north/policy", `{}`); code != http.StatusBadRequest {
		t.Errorf("publish with no staged_root = %d, want 400", code)
	}
}

// TestAKeylessRootKeepsEverythingButTheSigning covers the shape of a
// deployment that was never given a release key. It is a control plane with
// one feature off, not a control plane that answers 503 to everything — so
// the enrolment, the listing and the state read all keep working, and the
// publish answers with a code that says "this root cannot sign" rather than
// one that sends the operator looking for a different directory.
func TestAKeylessRootKeepsEverythingButTheSigning(t *testing.T) {
	svc, err := fedbiz.NewService(fedbiz.NewRegistry(nil), nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv := newServer(t, svc, nil, "admin")

	if code, body := do(t, srv, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north","name":"north"}`); code != http.StatusCreated {
		t.Fatalf("enrol on a keyless root = %d, want 201 (%v)", code, body)
	}
	if code, body := do(t, srv, "GET", "/v1/federation/clusters", ""); code != http.StatusOK {
		t.Errorf("list on a keyless root = %d, want 200 (%v)", code, body)
	}

	code, body := do(t, srv, "POST", "/v1/federation/clusters/prod-cn-north/policy",
		`{"staged_root":"/tmp/somewhere"}`)
	if code != http.StatusServiceUnavailable {
		t.Errorf("publish on a keyless root = %d, want 503 (%v)", code, body)
	}
	if errCode(body) != "no_release_key" {
		t.Errorf("code = %q, want no_release_key", errCode(body))
	}

	// The one that would be a data-integrity bug rather than a missing
	// feature: a version spent on a decision this root could never make.
	_, one := do(t, srv, "GET", "/v1/federation/clusters/prod-cn-north", "")
	if issued, _ := one["highest_issued"].(float64); issued != 0 {
		t.Errorf("highest_issued = %v after a keyless publish, want 0 — a version must not be spent on a refusal", one["highest_issued"])
	}
}

// TestARefusedVersionReadsAsBehind is the console-facing half of a property
// that lives in the service: a cluster that answered and declined is not
// caught up, and must not render as a green tick.
func TestARefusedVersionReadsAsBehind(t *testing.T) {
	svc, _, tree := realService(t)
	srv := newServer(t, svc, nil, "admin")
	do(t, srv, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`)
	do(t, srv, "POST", "/v1/federation/clusters/prod-cn-north/policy", `{"staged_root":`+mustJSONString(t, tree)+`}`)

	code, body := do(t, srv, "POST", "/v1/federation/clusters/prod-cn-north/policy/ack",
		`{"outcome":{"version":1,"accepted":false,"live":0,"reason":"admission: exceeds this cluster's limits"}}`)
	if code != http.StatusOK {
		t.Fatalf("ack = %d, want 200 (%v)", code, body)
	}
	if body["behind"] != true {
		t.Errorf("behind = %v after a refusal, want true", body["behind"])
	}
	last, _ := body["last_ack"].(map[string]any)
	if last == nil || last["accepted"] != false {
		t.Errorf("last_ack = %v, want the refusal to be visible", body["last_ack"])
	}
}

func TestAcknowledgingAVersionTheRootNeverIssuedIsAConflict(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, nil, "admin")
	do(t, srv, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`)

	code, body := do(t, srv, "POST", "/v1/federation/clusters/prod-cn-north/policy/ack",
		`{"outcome":{"version":99,"accepted":true}}`)
	if code != http.StatusConflict {
		t.Errorf("ack of an unissued version = %d, want 409 (%v)", code, body)
	}
	if errCode(body) != "unknown_version" {
		t.Errorf("code = %q, want unknown_version", errCode(body))
	}
}

func TestAckWithoutAVersionIsRejected(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, nil, "admin")
	do(t, srv, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`)

	if code, _ := do(t, srv, "POST", "/v1/federation/clusters/prod-cn-north/policy/ack", `{"outcome":{}}`); code != http.StatusBadRequest {
		t.Errorf("ack with no version = %d, want 400", code)
	}
}

// TestStateAsksTheChild is the endpoint that makes the root's answer about the
// child rather than about itself. It has to stay up even when the pusher is
// missing, and it has to say "unreachable" rather than "enforcing nothing".
func TestStateAsksTheChild(t *testing.T) {
	svc, _, _ := realService(t)
	push := &fakePusher{state: tunnel.ClusterStateResponse{
		Policy:          7,
		Bundle:          floorfed.Bundle{ClusterID: "prod-cn-north", Version: 7},
		LastRootContact: time.Now().Add(-72 * time.Hour),
		Enforcing:       true,
		NodeCount:       40,
	}}
	srv := newServer(t, svc, push, "admin")
	do(t, srv, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`)

	code, body := do(t, srv, "GET", "/v1/federation/clusters/prod-cn-north/state", "")
	if code != http.StatusOK {
		t.Fatalf("state = %d, want 200 (%v)", code, body)
	}
	if body["policy"] != float64(7) {
		t.Errorf("policy = %v, want the child's own answer of 7", body["policy"])
	}
	if body["enforcing"] != true {
		t.Errorf("enforcing = %v", body["enforcing"])
	}
	if body["node_count"] != float64(40) {
		t.Errorf("node_count = %v", body["node_count"])
	}
}

func TestStateOnASilentChildIsBadGatewayNotNothing(t *testing.T) {
	svc, _, _ := realService(t)
	srv := newServer(t, svc, &fakePusher{stateErr: os.ErrDeadlineExceeded}, "admin")

	code, body := do(t, srv, "GET", "/v1/federation/clusters/prod-cn-north/state", "")
	if code != http.StatusBadGateway {
		t.Errorf("state from a silent child = %d, want 502 (%v)", code, body)
	}
	if errCode(body) != "child_unreachable" {
		t.Errorf("code = %q, want child_unreachable", errCode(body))
	}
	// The load-bearing part: it must not answer as though the cluster is
	// enforcing nothing, because a cluster that has not answered is quite
	// possibly enforcing something perfectly well.
	if strings.Contains(mustJSON(t, body), `"policy":0`) {
		t.Errorf("a silent child was reported as enforcing version 0")
	}
}

func TestEveryRouteAnswersServiceUnavailableBeforeWiring(t *testing.T) {
	srv := newServer(t, nil, nil, "admin")

	calls := []struct{ method, path, body string }{
		{"POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`},
		{"GET", "/v1/federation/clusters", ""},
		{"GET", "/v1/federation/clusters/prod-cn-north", ""},
		{"POST", "/v1/federation/clusters/prod-cn-north/policy", `{"staged_root":"/tmp"}`},
		{"POST", "/v1/federation/clusters/prod-cn-north/policy/ack", `{"outcome":{"version":1}}`},
		{"GET", "/v1/federation/clusters/prod-cn-north/state", ""},
	}
	for _, c := range calls {
		code, body := do(t, srv, c.method, c.path, c.body)
		// 503 rather than 404: "this manager cannot federate" and "this
		// route does not exist" send an operator to different pages.
		if code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", c.method, c.path, code)
		}
		if errCode(body) != "not_wired" {
			t.Errorf("%s %s code = %q, want not_wired", c.method, c.path, errCode(body))
		}
	}
}

func TestStateIsServiceUnavailableWithNoTunnel(t *testing.T) {
	svc, _, _ := realService(t)
	// Enrolled over a real server so the membership exists, then read state
	// with no pusher wired.
	srv := newServer(t, svc, nil, "admin")
	do(t, srv, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`)

	if code, _ := do(t, srv, "GET", "/v1/federation/clusters/prod-cn-north/state", ""); code != http.StatusServiceUnavailable {
		t.Errorf("state with no pusher = %d, want 503", code)
	}
	// And the reads that do not need a tunnel still work, because a root
	// whose link is down must still be able to say who it has enrolled.
	if code, _ := do(t, srv, "GET", "/v1/federation/clusters", ""); code != http.StatusOK {
		t.Errorf("list with no pusher = %d, want 200", code)
	}
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	if e == nil {
		return ""
	}
	s, _ := e["code"].(string)
	return s
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// decodeTransport turns the base64 envelope on the wire back into a struct —
// the exact bytes a child would receive.
func decodeTransport(env string) (pluginmanifest.Envelope, error) {
	var out pluginmanifest.Envelope
	raw, err := base64.StdEncoding.DecodeString(env)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(raw, &out)
}
