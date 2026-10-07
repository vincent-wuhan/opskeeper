package flow

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
	bizflow "github.com/vincent-wuhan/opskeeper/core/domains/biz/flow"
	flowmodel "github.com/vincent-wuhan/opskeeper/core/domains/model/flow"
)

// The orchestration surface is the closest thing this platform has to a loaded
// weapon: one click sets off an ordered set of tool calls through the same
// executor the approval inbox guards. These tests are about the two rows that
// matter — somebody set it off, and somebody ran one node in isolation — and,
// as always on this chain, about what those rows must **not** carry.

const testGraph = `{"nodes":[{"id":"t","type":"trigger.manual"},{"id":"a","type":"set","config":{"name":"x","value":"1"}}],"edges":[{"id":"e1","source":"t","sourcePort":"next","target":"a"}]}`

type fakeFlowRepo struct {
	flow    *flowmodel.Flow
	deleted bool
	created *flowmodel.Flow
}

func (r *fakeFlowRepo) Create(_ context.Context, f *flowmodel.Flow) error {
	f.ID = 12
	r.created, r.flow = f, f
	return nil
}
func (r *fakeFlowRepo) Update(_ context.Context, f *flowmodel.Flow) error { r.flow = f; return nil }
func (r *fakeFlowRepo) Get(_ context.Context, id uint64) (*flowmodel.Flow, error) {
	if r.flow == nil || r.flow.ID != id || r.deleted {
		return nil, errs.ErrNotFound
	}
	return r.flow, nil
}
func (r *fakeFlowRepo) List(context.Context, int, int) ([]*flowmodel.Flow, int64, error) {
	return nil, 0, nil
}
func (r *fakeFlowRepo) ListEnabled(context.Context) ([]*flowmodel.Flow, error) { return nil, nil }
func (r *fakeFlowRepo) Delete(_ context.Context, id uint64) error {
	if r.flow == nil || r.flow.ID != id {
		return errs.ErrNotFound
	}
	r.deleted = true
	return nil
}

type fakeRunRepo struct{ runs []*flowmodel.FlowRun }

func (r *fakeRunRepo) CreateRun(_ context.Context, run *flowmodel.FlowRun) error {
	r.runs = append(r.runs, run)
	return nil
}
func (r *fakeRunRepo) UpdateRun(context.Context, *flowmodel.FlowRun) error { return nil }
func (r *fakeRunRepo) GetRun(context.Context, string) (*flowmodel.FlowRun, error) {
	return nil, errs.ErrNotFound
}
func (r *fakeRunRepo) ListRuns(context.Context, uint64, int) ([]*flowmodel.FlowRun, error) {
	return nil, nil
}
func (r *fakeRunRepo) CreateNode(context.Context, *flowmodel.FlowRunNode) error { return nil }
func (r *fakeRunRepo) UpdateNode(context.Context, *flowmodel.FlowRunNode) error { return nil }
func (r *fakeRunRepo) ListNodes(context.Context, string) ([]*flowmodel.FlowRunNode, error) {
	return nil, nil
}
func (r *fakeRunRepo) SweepStaleRunning(context.Context, string) (int64, error) { return 0, nil }
func (r *fakeRunRepo) PruneRuns(context.Context, time.Time) (int64, error)      { return 0, nil }

func newFlowStack(t *testing.T, repo *fakeFlowRepo, runs *fakeRunRepo, engine *bizflow.Engine) http.Handler {
	t.Helper()
	uc := bizflow.NewUsecase(repo, runs, engine, nil)
	h := NewHandler(uc)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(tenantctx.With(req.Context(), tenantctx.Tenant{UserID: 3, Role: "admin"})))
		})
	})
	h.Register(r)
	return r
}

func call(t *testing.T, stack http.Handler, method, path, body string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, req)
	ev, ok := auditport.GetAuditEvent(req.Context())
	return rec, ev, ok
}

// 决策 335：按下执行的那一行。input 不进链——它已经作为 TriggerJSON 落库，
// 而且可能含凭据；链上要回答的是「谁在什么时候让哪份编排跑了起来」。
func TestRunNamesWhoSetItOffWithoutCopyingTheInput(t *testing.T) {
	repo := &fakeFlowRepo{flow: &flowmodel.Flow{ID: 5, Name: "nightly-disk-check", Enabled: true, GraphJSON: testGraph}}
	runs := &fakeRunRepo{}
	engine := bizflow.NewEngine(bizflow.Executors{}, runs, nil)
	stack := newFlowStack(t, repo, runs, engine)

	const secret = "postgres://ops:hunter2@db/prod"
	rec, ev, ok := call(t, stack, http.MethodPost, "/v1/flows/5/run", `{"input":{"dsn":"`+secret+`"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !ok {
		t.Fatal("no row: \"who set this off\" is the first question of any incident on this surface")
	}
	if ev.Action != auditport.ActionFlowRun || ev.ResourceType != auditport.ResourceFlow || ev.ResourceID != "5" {
		t.Errorf("event = %+v, want flow_run on flow/5", ev)
	}
	blob := mustJSON(t, ev.Payload)
	if strings.Contains(blob, "hunter2") {
		t.Errorf("the run input reached the chain: %s — it is already persisted on the run", blob)
	}
	payload, _ := ev.Payload.(map[string]any)
	if payload["run_id"] == nil || payload["run_id"] == "" {
		t.Errorf("payload = %v, want the run id so the execution can be looked up", ev.Payload)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// test-node 的失败是**带内**的：HTTP 200，错误在响应体里。任何靠状态码判断成败的
// 东西都会把它当成成功——而它其实真的动了手，会触到工具。
// 这条用例守的就是那个「显式写一行 failure」。
func TestTestNodeFailureIsInBandAndStillGetsAFailureRow(t *testing.T) {
	repo := &fakeFlowRepo{flow: &flowmodel.Flow{ID: 5, Name: "nightly", Enabled: true, GraphJSON: testGraph}}
	runs := &fakeRunRepo{}
	// No engine wired: TestNode returns ErrNotWiredYet, which the handler
	// surfaces as 200 + {"error": ...}.
	stack := newFlowStack(t, repo, runs, nil)

	rec, ev, ok := call(t, stack, http.MethodPost, "/v1/flows/5/test-node",
		`{"node_type":"tool","config":{"tool":"shell","cmd":"rm -rf /"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("body = %s, want the in-band error the editor reads", rec.Body.String())
	}
	if !ok {
		t.Fatal("no row: a failed test run reached the tools and left no trace at all")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure — a 200 response here does not mean it worked", ev.Status)
	}
	if ev.Action != auditport.ActionFlowTestNode {
		t.Errorf("action = %q", ev.Action)
	}
	payload, _ := ev.Payload.(map[string]any)
	if payload["node_type"] != "tool" {
		t.Errorf("payload = %v, want the node type that was tried", ev.Payload)
	}
}

// config 进链吗？不进——它就是那条命令本身。
func TestTestNodeRowCarriesTheNodeTypeAndNotItsConfig(t *testing.T) {
	repo := &fakeFlowRepo{flow: &flowmodel.Flow{ID: 5, Name: "nightly", Enabled: true, GraphJSON: testGraph}}
	stack := newFlowStack(t, repo, &fakeRunRepo{}, nil)

	_, ev, ok := call(t, stack, http.MethodPost, "/v1/flows/5/test-node",
		`{"node_type":"http_request","config":{"url":"https://x","headers":{"Authorization":"Bearer TOPSEKRET"}}}`)
	if !ok {
		t.Fatal("no row")
	}
	if strings.Contains(mustJSON(t, ev.Payload), "TOPSEKRET") {
		t.Errorf("the node config reached the chain: %s", mustJSON(t, ev.Payload))
	}
}

func TestDefinitionRowsCarryNamesBecauseThatIsTheQuestion(t *testing.T) {
	repo := &fakeFlowRepo{}
	stack := newFlowStack(t, repo, &fakeRunRepo{}, nil)

	_, ev, ok := call(t, stack, http.MethodPost, "/v1/flows", `{"name":"nightly-disk-check","graph":{}}`)
	if !ok || ev.Action != auditport.ActionFlowCreate {
		t.Fatalf("create row = %+v (ok=%v)", ev, ok)
	}
	if ev.ResourceName != "nightly-disk-check" {
		t.Errorf("create row has no name: %+v — \"which automation was created\" is unanswerable", ev)
	}

	// Delete reads the name **before** deleting, so the row survives the row.
	repo.flow = &flowmodel.Flow{ID: 12, Name: "nightly-disk-check", Enabled: true, GraphJSON: "{}"}
	_, ev, ok = call(t, stack, http.MethodDelete, "/v1/flows/12", "")
	if !ok || ev.Action != auditport.ActionFlowDelete {
		t.Fatalf("delete row = %+v (ok=%v)", ev, ok)
	}
	if ev.ResourceName != "nightly-disk-check" {
		t.Errorf("delete row = %+v, want it to name what was deleted", ev)
	}
}

func TestToggleRowSaysWhichWayItWent(t *testing.T) {
	repo := &fakeFlowRepo{flow: &flowmodel.Flow{ID: 5, Name: "nightly", Enabled: true, GraphJSON: testGraph}}
	stack := newFlowStack(t, repo, &fakeRunRepo{}, nil)

	for _, want := range []bool{false, true} {
		_, ev, ok := call(t, stack, http.MethodPost, "/v1/flows/5/toggle", `{"enabled":`+boolStr(want)+`}`)
		if !ok || ev.Action != auditport.ActionFlowToggle {
			t.Fatalf("toggle row = %+v (ok=%v)", ev, ok)
		}
		payload, _ := ev.Payload.(map[string]any)
		if got, _ := payload["enabled"].(bool); got != want {
			t.Errorf("payload = %v, want enabled=%v — \"this automation is now armed\" is the question", ev.Payload, want)
		}
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
