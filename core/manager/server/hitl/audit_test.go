package hitl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizhitl "github.com/vincent-wuhan/opskeeper/core/manager/biz/hitl"
	hitlmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/hitl"
)

// --- 决策 314：AgentTeams HITL 提案的提出与决定上宿主链 -----------------------
//
// 这是本仓**第三个**审批形状的面（前两个是决策 309 的审批收件箱、
// 决策 312 的 agentteams/hitl/decide）。它此前不在闸门视野内，
// 直到决策 314 把 routeaudit 扩到每一个 HTTP 树才被看见。
//
// 载荷里最要紧的是 payload_hash：提案本身只是一句「有人请求做某事」，
// hash 钉住的是**批的是哪一份**。没有它，「谁批准了这条命令」在链上只能
// 回答成「某人批准了 3 号提案」。

// memRepo is an in-memory bizhitl.Repo, so these tests drive the real
// Service rather than a stand-in for it: the binding check inside
// TransitionAgentTeams (message_id + payload_hash must match the stored
// proposal) is exactly the thing being relied on, and a fake service would
// have made the assertions vacuous.
type memRepo struct {
	props map[string]*hitlmodel.Proposal
}

func newMemRepo() *memRepo { return &memRepo{props: map[string]*hitlmodel.Proposal{}} }

// assignID stands in for model.Proposal's BeforeCreate hook, which is what
// gives a real proposal its identity. Without it every snapshot came back
// with an empty ID and the decide call was rejected as ErrInvalid — and the
// assertions downstream compared "" to "" and passed without meaning anything.
func assignID(p *hitlmodel.Proposal) {
	if p.ID == "" {
		p.ID = "prop-" + p.MessageID
	}
}

func (m *memRepo) Create(_ context.Context, p *hitlmodel.Proposal) error {
	assignID(p)
	m.props[p.ID] = p
	return nil
}

func (m *memRepo) CreateAgentTeamsIdempotent(_ context.Context, p *hitlmodel.Proposal) error {
	assignID(p)
	m.props[p.ID] = p
	return nil
}

func (m *memRepo) Get(_ context.Context, id string) (*hitlmodel.Proposal, error) {
	p, ok := m.props[id]
	if !ok {
		return nil, errs.ErrNotFound
	}
	return p, nil
}

func (m *memRepo) List(_ context.Context, _ string, _, _ int) ([]*hitlmodel.Proposal, int64, error) {
	out := []*hitlmodel.Proposal{}
	for _, p := range m.props {
		out = append(out, p)
	}
	return out, int64(len(out)), nil
}

func (m *memRepo) CountByState(_ context.Context, _ string) (int64, error) { return 0, nil }
func (m *memRepo) CountByLegacyKind(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

func (m *memRepo) Transition(_ context.Context, id, expectedFrom string, fields hitlmodel.TransitionFields) error {
	p, ok := m.props[id]
	if !ok || p.State != expectedFrom {
		return errs.ErrConflict
	}
	p.State = fields.ToState
	if fields.Reason != nil {
		p.Reason = fields.Reason
	}
	if fields.ApprovedBy != nil {
		p.ApprovedBy = fields.ApprovedBy
	}
	if fields.RejectedBy != nil {
		p.RejectedBy = fields.RejectedBy
	}
	if fields.DecidedAt != nil {
		p.DecidedAt = fields.DecidedAt
	}
	return nil
}

func (m *memRepo) SetResult(_ context.Context, _, _, _ string, _ time.Time) error  { return nil }
func (m *memRepo) UpsertState(_ context.Context, _ *hitlmodel.ProposalState) error { return nil }
func (m *memRepo) LoadState(_ context.Context, _ string) (*hitlmodel.ProposalState, error) {
	return nil, errs.ErrNotFound
}
func (m *memRepo) DeleteState(_ context.Context, _ string) error { return nil }

func newAuditedRouter() (http.Handler, *memRepo) {
	repo := newMemRepo()
	svc := bizhitl.NewService(repo).WithClock(func() time.Time {
		return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	})
	router := chi.NewMux()
	NewHandler(svc).Register(router)
	return router, repo
}

func call(t *testing.T, router http.Handler, method, path, body string, tenant *tenantctx.Tenant) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Opskeeper-Version", "v1")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := auditport.WithSlot(req.Context())
	if tenant != nil {
		ctx = tenantctx.With(ctx, *tenant)
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	ev, set := auditport.GetAuditEvent(req.Context())
	return rec, ev, set
}

func adminTenant() *tenantctx.Tenant {
	tv := tenantctx.Tenant{UserID: 77, Role: tenantctx.RoleAdmin}
	tv.IsSuperuser = true
	return &tv
}

type envelope struct {
	Data bizhitl.ProposalSnapshot `json:"data"`
}

// proposeBody is a proposal that satisfies the real Service's envelope rules.
// Every field here is load-bearing, and that is the point: the four
// cross-field bindings (request_id==message_id, incident_id==session_id,
// payload.expires_at==top-level expires_at, command==action) are what make a
// proposal unambiguous, so a fixture that skipped them would be asserting
// against a shape the service never accepts. Timestamps are pinned to the
// stubbed clock (12:00:00Z) rather than to time.Now() so the payload_hash is
// reproducible across runs.
const proposeBody = `{
  "kind": "agentteams_hitl",
  "title": "restart api-7f",
  "summary": "api-7f P99 已超 12 分钟，按预案重启服务",
  "source": "agent",
  "session_id": "sess-1",
  "message_id": "msg-1",
  "severity": "dangerous",
  "sensitivity": "restricted",
  "im_thread_id": "room-1",
  "expires_at": "2026-10-06T13:00:00Z",
  "payload": {
    "fingerprint": "fp-1",
    "request_id": "msg-1",
    "incident_id": "sess-1",
    "action": "restart_service",
    "blast_radius": "single_device",
    "resource": "api-7f",
    "room_id": "room-1",
    "parameters": {
      "command": "restart_service",
      "device_id": 7,
      "service": "api-7f",
      "reason": "P99 已超 12 分钟"
    },
    "requested_at": "2026-10-06T12:00:00Z",
    "expires_at": "2026-10-06T13:00:00Z"
  }
}`

// propose raises a real proposal through the real service, so the snapshot's
// payload_hash is the genuine canonical hash rather than a fixture.
func propose(t *testing.T, router http.Handler) bizhitl.ProposalSnapshot {
	t.Helper()
	rec, ev, set := call(t, router, http.MethodPost, "/v1/hitl/proposals", proposeBody, adminTenant())
	if rec.Code != http.StatusCreated {
		t.Fatalf("propose: code = %d (%s)", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionHITLProposalCreate {
		t.Fatalf("propose: ev = %+v (set=%v)", ev, set)
	}
	var out envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// A snapshot without an identity would make every assertion below compare
	// "" to "" and pass, so refuse to continue in that case.
	if out.Data.ID == "" || out.Data.PayloadHash == "" {
		t.Fatalf("snapshot is not addressable: %+v", out.Data)
	}
	return out.Data
}

func decide(t *testing.T, router http.Handler, snap bizhitl.ProposalSnapshot, verb string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"message_id":      snap.MessageID,
		"payload_hash":    snap.PayloadHash,
		"matrix_event_id": "$evt-1",
		"reason":          "已确认是当前故障源",
	})
	if err != nil {
		t.Fatal(err)
	}
	return call(t, router, http.MethodPost, "/v1/hitl/proposals/"+snap.ID+"/"+verb, string(body), adminTenant())
}

// 批准这一下就是「让那条动作真的走下去」的那一下，所以载荷必须钉住
// **批的是哪一份**：proposal_id 加上 payload_hash。
func TestApproveIsAuditedWithThePayloadItLetLoose(t *testing.T) {
	router, _ := newAuditedRouter()
	snap := propose(t, router)

	rec, ev, set := decide(t, router, snap, "approve")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: code = %d (%s)", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionHITLProposalApprove || ev.Status != auditport.StatusSuccess {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if ev.ResourceType != auditport.ResourceHITLProposal || ev.ResourceID != snap.ID {
		t.Fatalf("resource = %q/%q", ev.ResourceType, ev.ResourceID)
	}
	p := ev.Payload.(map[string]any)
	if p["payload_hash"] != snap.PayloadHash {
		t.Fatalf("payload_hash = %v, want %v — 没有它就答不出「批的是哪一份」", p["payload_hash"], snap.PayloadHash)
	}
	if p["decided_by"] != uint64(77) {
		t.Fatalf("decided_by = %v", p["decided_by"])
	}
	if p["reason"] != "已确认是当前故障源" {
		t.Fatalf("reason = %v", p["reason"])
	}
	if p["state"] != hitlmodel.StateApproved {
		t.Fatalf("state = %v", p["state"])
	}
}

// 过期根本不是任何人的决定。把它折进 approve，
// 「谁批准过」的答案里就会掺进若干条谁都没批过的东西。
func TestExpireIsNotFiledAsAnApproval(t *testing.T) {
	router, _ := newAuditedRouter()
	snap := propose(t, router)

	rec, ev, set := decide(t, router, snap, "expire")
	if rec.Code != http.StatusOK {
		t.Fatalf("expire: code = %d (%s)", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionHITLProposalExpire {
		t.Fatalf("ev = %+v (set=%v), want hitl_proposal_expire", ev, set)
	}
}

func TestRejectIsItsOwnAction(t *testing.T) {
	router, _ := newAuditedRouter()
	snap := propose(t, router)

	_, ev, set := decide(t, router, snap, "reject")
	if !set || ev.Action != auditport.ActionHITLProposalReject {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
}

// 「谁批准过」必须答得出**，所以一个根本没被批准的提案也要留下失败行。
func TestDecisionAgainstAnUnknownProposalIsAudited(t *testing.T) {
	router, _ := newAuditedRouter()
	body := `{"message_id":"msg-x","payload_hash":"` + strings.Repeat("a", 64) +
		`","matrix_event_id":"$e","reason":"x"}`
	rec, ev, set := call(t, router, http.MethodPost, "/v1/hitl/proposals/no-such/approve", body, adminTenant())
	if rec.Code == http.StatusOK {
		t.Fatalf("approving an unknown proposal returned 200: %s", rec.Body.String())
	}
	if !set || ev.Status != auditport.StatusFailure || ev.ResourceID != "no-such" {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
}

// 「有人在一直试」与「没人试过」在链上必须长得不一样。
func TestNonAdminTransitionsAreAudited(t *testing.T) {
	user := tenantctx.Tenant{UserID: 42, Role: tenantctx.RoleUser}
	for _, tc := range []struct{ verb, wantAction string }{
		{"approve", auditport.ActionHITLProposalApprove},
		{"reject", auditport.ActionHITLProposalReject},
		{"expire", auditport.ActionHITLProposalExpire},
	} {
		router, _ := newAuditedRouter()
		snap := propose(t, router)
		body, _ := json.Marshal(map[string]string{
			"message_id": snap.MessageID, "payload_hash": snap.PayloadHash,
			"matrix_event_id": "$e", "reason": "x",
		})
		rec, ev, set := call(t, router, http.MethodPost, "/v1/hitl/proposals/"+snap.ID+"/"+tc.verb, string(body), &user)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: code = %d, want 403", tc.verb, rec.Code)
		}
		if !set || ev.Status != auditport.StatusFailure || ev.Action != tc.wantAction {
			t.Fatalf("%s: ev = %+v (set=%v)", tc.verb, ev, set)
		}
		if ev.Payload.(map[string]any)["decided_by"] != nil {
			t.Fatalf("%s: a rejected attempt claimed a decider", tc.verb)
		}
	}
}

// 提案那一行是决定那一行的另一半：没有它，链上的批准指向一个
// 没人看得见被提出过的提案。
func TestCreateRecordsWhoAskedAndForWhat(t *testing.T) {
	router, _ := newAuditedRouter()
	snap := propose(t, router)

	rec, ev, _ := call(t, router, http.MethodPost, "/v1/hitl/proposals", proposeBody, adminTenant())
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d", rec.Code)
	}
	if ev.ResourceID != snap.ID {
		t.Fatalf("resource id = %q, want the created proposal %q", ev.ResourceID, snap.ID)
	}
	p := ev.Payload.(map[string]any)
	if p["kind"] != "agentteams_hitl" || p["proposed_by"] != uint64(77) {
		t.Fatalf("payload = %v", p)
	}
	if p["payload_hash"] != snap.PayloadHash {
		t.Fatalf("create row lost the hash the decide row will be read against: %v", p)
	}
}
