package federation

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"

	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// svcAndTree builds the concrete service (not the handler's narrow interface)
// so a test can install a delivery outcome before the first publish.
func svcAndTree(t *testing.T) (*fedbiz.Service, string) {
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
	return svc, signedTreeOnDisk(t)
}

// enrollOne puts a cluster on the service through the real route, so the audit
// rows under test are produced by the same path an operator takes.
func enrollOne(t *testing.T, h *Handler) {
	t.Helper()
	if rec, _, _ := auditable(t, h, "POST", "/v1/federation/clusters",
		`{"id":"prod-cn-north","name":"华东生产一区"}`, "admin"); rec.Code != http.StatusCreated {
		t.Fatalf("enroll = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// auditable runs one request in-process with an audit slot installed, so the
// test can read the row the handler declared. 审计槽由中间件装上；这里装在身份
// 之前，顺序与 port.go 记的那个坑一致。
func auditable(t *testing.T, h *Handler, method, path, body, role string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	ctx := auditport.WithSlot(r.Context())
	ctx = tenantctx.With(ctx, tenantctx.Tenant{UserID: 1, Role: role})
	r = r.WithContext(ctx)
	router := chi.NewRouter()
	h.Register(router)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	ev, set := auditport.GetAuditEvent(r.Context())
	return rec, ev, set
}

func pload(t *testing.T, ev auditport.Event) map[string]any {
	t.Helper()
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map", ev.Payload)
	}
	return p
}

// delivering wires a service whose delivery outcome the test scripts.
func delivering(t *testing.T, verdict tunnel.ClusterPolicyResponse, pushErr error) (*Handler, string) {
	t.Helper()
	svc, tree := svcAndTree(t)
	dist, err := fedbiz.NewFileDistributor(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFileDistributor: %v", err)
	}
	p := &fakePusher{verdict: verdict, pushErr: pushErr}
	svc.SetDelivery(p, dist, dist)
	if _, err := floorfed.NewClusterID("prod-cn-north"); err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	return NewHandler(svc), tree
}

// TestEnrollAuditRowDoesNotCarryTheProvisioningToken is the load-bearing one
// for 决策 341, and it is decision 337's rule arriving on a second surface.
//
// Enroll mints a credential that lets a remote process act for a cluster, and
// it is shown exactly once. The chain cannot revoke it — re-enrolling rotates
// the credential, but the row already written stays readable — so writing it
// down would trade a revocable secret for an eternal one, exactly as
// `shareReport`'s token did.
func TestEnrollAuditRowDoesNotCarryTheProvisioningToken(t *testing.T) {
	svc, _ := svcAndTree(t)
	h := NewHandler(svc)

	rec, ev, set := auditable(t, h, "POST", "/v1/federation/clusters",
		`{"id":"prod-cn-north","name":"华东生产一区"}`, "admin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("enroll = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out enrollResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ProvisioningToken == "" {
		t.Fatal("no token returned — nothing to keep out of the chain")
	}
	if !set {
		t.Fatal("enroll left no audit event")
	}
	if ev.Action != auditport.ActionFederationClusterEnroll {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionFederationClusterEnroll)
	}
	if ev.ResourceType != auditport.ResourceFederationCluster || ev.ResourceID != "prod-cn-north" {
		t.Fatalf("resource = %q/%q, want federation_cluster/prod-cn-north", ev.ResourceType, ev.ResourceID)
	}
	if ev.ResourceName != "华东生产一区" {
		t.Errorf("name = %q, want the cluster name", ev.ResourceName)
	}

	whole, _ := json.Marshal(ev)
	if strings.Contains(string(whole), out.ProvisioningToken) {
		t.Fatalf("audit row carries the provisioning token: %s", whole)
	}
	p := pload(t, ev)
	if raw, ok := p["token_shown_once"]; !ok {
		t.Error("row has no token_shown_once key")
	} else if got, isBool := raw.(bool); !isBool || !got {
		t.Errorf("token_shown_once = %v, want true", raw)
	}
}

// TestPublishRecordsThatNobodyWasTold is the state `Delivery`'s own comment
// singles out: a publish that issued a version and delivered it to nobody is
// otherwise indistinguishable from one that worked.
//
// Note the status stays success — the version **was** minted, and booking it
// as a failure would lose a real issuance from "who signed version N". The
// separate fact is carried by delivery_status.
func TestPublishRecordsThatNobodyWasTold(t *testing.T) {
	// No SetDelivery on the service: Attempted is false.
	svc, tree := svcAndTree(t)
	h := NewHandler(svc)
	enrollOne(t, h)

	rec2, ev, set := auditable(t, h, "POST", "/v1/federation/clusters/prod-cn-north/policy",
		`{"staged_root":`+mustJSONString(t, tree)+`,"reason":"widen read scope"}`, "admin")
	if rec2.Code != http.StatusOK {
		t.Fatalf("publish = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	if !set {
		t.Fatal("publish left no audit event")
	}
	if ev.Action != auditport.ActionFederationPolicyPublish {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionFederationPolicyPublish)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Fatalf("status = %q, want success —— 版本确实签发出去了", ev.Status)
	}
	p := pload(t, ev)
	if p["delivery_status"] != "not_attempted" {
		t.Fatalf("delivery_status = %v, want not_attempted —— 「发出去了」与「没人被告知」必须分得开", p["delivery_status"])
	}
	if got, _ := p["attempted"].(bool); got {
		t.Error("attempted = true, want false")
	}
	if got, _ := p["delivered"].(bool); got {
		t.Error("delivered = true, want false")
	}
	if v, _ := p["version"].(uint64); v != 1 {
		t.Errorf("version = %v, want 1", p["version"])
	}
	if p["reason"] != "widen read scope" {
		t.Errorf("reason = %v, want the operator's own words", p["reason"])
	}
}

// TestPublishRecordsAChildThatRefused: accepted=false is a decision the child
// made, and it is the answer an operator most needs. `retryable` staying false
// is what tells them not to send it again.
func TestPublishRecordsAChildThatRefused(t *testing.T) {
	h, tree := delivering(t, tunnel.ClusterPolicyResponse{
		Outcome: floorfed.Outcome{Version: 1, Accepted: false, Live: 7, Reason: "manifest version below min_edge_version"},
	}, nil)
	enrollOne(t, h)

	rec, ev, set := auditable(t, h, "POST", "/v1/federation/clusters/prod-cn-north/policy",
		`{"staged_root":`+mustJSONString(t, tree)+`}`, "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("publish left no audit event")
	}
	p := pload(t, ev)
	if p["delivery_status"] != "declined" {
		t.Fatalf("delivery_status = %v, want declined", p["delivery_status"])
	}
	if got, _ := p["child_accepted"].(bool); got {
		t.Error("child_accepted = true, want absent/false on a refusal")
	}
	if p["child_reason"] != "manifest version below min_edge_version" {
		t.Errorf("child_reason = %v, want the child's own words", p["child_reason"])
	}
	if got, _ := p["retryable"].(bool); got {
		t.Error("retryable = true, want false —— 一个决定不是一次故障")
	}
}

// TestPublishRecordsATransportFailure is a different world from a refusal even
// though both render as `delivered: false` on the console.
func TestPublishRecordsATransportFailure(t *testing.T) {
	h, tree := delivering(t, tunnel.ClusterPolicyResponse{}, errBoom{})
	enrollOne(t, h)

	rec, ev, set := auditable(t, h, "POST", "/v1/federation/clusters/prod-cn-north/policy",
		`{"staged_root":`+mustJSONString(t, tree)+`}`, "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("publish left no audit event")
	}
	p := pload(t, ev)
	if p["delivery_status"] != "transport_failed" {
		t.Fatalf("delivery_status = %v, want transport_failed", p["delivery_status"])
	}
	if p["delivery_error"] == nil {
		t.Error("no delivery_error on a transport failure")
	}
}

// TestPublishRecordsThatItLanded is the other end, and it must not be the only
// case the test can see — a rule that always says "declined" passes a
// declined-only assertion.
func TestPublishRecordsThatItLanded(t *testing.T) {
	h, tree := delivering(t, tunnel.ClusterPolicyResponse{
		Outcome: floorfed.Outcome{Version: 1, Accepted: true, Live: 1},
	}, nil)
	enrollOne(t, h)

	rec, ev, set := auditable(t, h, "POST", "/v1/federation/clusters/prod-cn-north/policy",
		`{"staged_root":`+mustJSONString(t, tree)+`}`, "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("publish left no audit event")
	}
	p := pload(t, ev)
	if p["delivery_status"] != "delivered" {
		t.Fatalf("delivery_status = %v, want delivered", p["delivery_status"])
	}
	if got, _ := p["child_accepted"].(bool); !got {
		t.Error("child_accepted = false/absent, want true")
	}
	if got, _ := p["live"].(uint64); got != 1 {
		t.Errorf("live = %v, want 1", p["live"])
	}
}

// TestRedeliverAuditRowCarriesTheVersion: redelivery reuses the bytes rather
// than re-packaging, so "how many times did version N go out" is only
// answerable if every redelivery row carries its version.
func TestRedeliverAuditRowCarriesTheVersion(t *testing.T) {
	h, tree := delivering(t, tunnel.ClusterPolicyResponse{
		Outcome: floorfed.Outcome{Version: 1, Accepted: true, Live: 1},
	}, nil)
	enrollOne(t, h)
	auditable(t, h, "POST", "/v1/federation/clusters/prod-cn-north/policy",
		`{"staged_root":`+mustJSONString(t, tree)+`}`, "admin")

	rec, ev, set := auditable(t, h, "POST", "/v1/federation/clusters/prod-cn-north/policy/redeliver", "", "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("redeliver = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("redeliver left no audit event")
	}
	if ev.Action != auditport.ActionFederationPolicyRedeliver {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionFederationPolicyRedeliver)
	}
	p := pload(t, ev)
	if v, _ := p["version"].(uint64); v != 1 {
		t.Errorf("version = %v, want 1 —— 重发的是同一个版本，不是新铸的一个", p["version"])
	}
	if _, ok := p["delivery_status"]; !ok {
		t.Error("redeliver row has no delivery_status key")
	}
}

// TestAckAuditRowKeepsTheRefusal is the other end of a link that can drop: the
// answer arrives later or not at all, and a rollout that cannot record its own
// outcome shows green forever.
func TestAckAuditRowKeepsTheRefusal(t *testing.T) {
	// The root refuses to record an outcome for a version it never issued —
	// so the cluster has to have taken version 2 first. That refusal is the
	// real shape: the bytes arrived, the policy engine said no, and the
	// cluster is still enforcing version 1.
	h, tree := delivering(t, tunnel.ClusterPolicyResponse{
		Outcome: floorfed.Outcome{Version: 1, Accepted: true, Live: 1},
	}, nil)
	enrollOne(t, h)
	for range 2 {
		if rec, _, _ := auditable(t, h, "POST", "/v1/federation/clusters/prod-cn-north/policy",
			`{"staged_root":`+mustJSONString(t, tree)+`}`, "admin"); rec.Code != http.StatusOK {
			t.Fatalf("publish = %d, body=%s", rec.Code, rec.Body.String())
		}
	}

	body := `{"outcome":{"version":2,"accepted":false,"live":1,"reason":"policy engine refused: blast_radius exceeds cluster"}}`
	rec, ev, set := auditable(t, h, "POST", "/v1/federation/clusters/prod-cn-north/policy/ack", body, "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("ack = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("ack left no audit event")
	}
	if ev.Action != auditport.ActionFederationPolicyAck {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionFederationPolicyAck)
	}
	p := pload(t, ev)
	// Key presence: a refusal and an absent field are not the same claim.
	raw, ok := p["accepted"]
	if !ok {
		t.Fatal("ack row has no accepted key —— 链上答不出「对方接受了没有」")
	}
	if got, isBool := raw.(bool); !isBool || got {
		t.Errorf("accepted = %v (bool=%v), want false", raw, isBool)
	}
	if v, _ := p["version"].(uint64); v != 2 {
		t.Errorf("version = %v, want 2", p["version"])
	}
	// Live on a refusal is the *previous* version — "still enforcing something,
	// just not this" is the answer an operator needs.
	if v, _ := p["live"].(uint64); v != 1 {
		t.Errorf("live = %v, want 1 —— 拒绝时它仍是上一个版本，这正是运营者要的答案", p["live"])
	}
	if p["reason"] != "policy engine refused: blast_radius exceeds cluster" {
		t.Errorf("reason = %v, want the child's own words", p["reason"])
	}
	if _, ok := p["behind_after_ack"]; !ok {
		t.Error("ack row has no behind_after_ack key")
	}
}

// TestDeniedFederationRouteClaimsNothing: a 403 must not look like an audited
// enrolment or publish.
func TestDeniedFederationRouteClaimsNothing(t *testing.T) {
	svc, _ := svcAndTree(t)
	h := NewHandler(svc)
	rec, ev, set := auditable(t, h, "POST", "/v1/federation/clusters", `{"id":"prod-cn-north"}`, "viewer")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer enroll = %d, want 403", rec.Code)
	}
	if set {
		t.Fatalf("denied write claimed an audit row: %+v", ev)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "tunnel: connection reset by peer" }

// TestPublishTellsAReplayApartFromARefusal is the fourth state, and it is the
// one that reads most like a refusal on the console.
//
// A replay is not a failure: the child already made this decision and it was
// delivered twice. But it is also not a no-op the caller should ignore,
// because the answer to "is this cluster on version 6?" is no — and without
// its own state a replayed outcome reads as a cheerful yes.
func TestPublishTellsAReplayApartFromARefusal(t *testing.T) {
	h, tree := delivering(t, tunnel.ClusterPolicyResponse{
		Outcome: floorfed.Outcome{Version: 3, Accepted: false, Superseded: true, Live: 5},
	}, nil)
	enrollOne(t, h)

	rec, ev, set := auditable(t, h, "POST", "/v1/federation/clusters/prod-cn-north/policy",
		`{"staged_root":`+mustJSONString(t, tree)+`}`, "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("publish left no audit event")
	}
	p := pload(t, ev)
	if p["delivery_status"] != "superseded" {
		t.Fatalf("delivery_status = %v, want superseded —— 「重发」与「拒绝」必须分得开", p["delivery_status"])
	}
	// A replay carries no refusal reason: the child did not refuse anything.
	if p["child_reason"] != nil {
		t.Errorf("child_reason = %v, want absent on a replay", p["child_reason"])
	}
	if got, _ := p["child_accepted"].(bool); got {
		t.Error("child_accepted = true, want absent/false on a replay")
	}
}
