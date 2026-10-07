package edge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	devicebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/device"
	biz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	devicemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/device"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
)

// fakeDeviceRepo is the in-memory devicebiz.Repo used by handler tests.
// Only Get / GetMany are exercised — register-side calls are out of
// scope for the HTTP layer.
type fakeDeviceRepo struct {
	byID map[uint64]*devicemodel.Device
}

func newFakeDeviceRepo(rows ...*devicemodel.Device) *fakeDeviceRepo {
	m := map[uint64]*devicemodel.Device{}
	for _, d := range rows {
		m[d.ID] = d
	}
	return &fakeDeviceRepo{byID: m}
}

func (d *fakeDeviceRepo) FindOrCreateByFingerprint(context.Context, *devicemodel.Device) (*devicemodel.Device, error) {
	return nil, nil
}
func (d *fakeDeviceRepo) RebindFingerprint(context.Context, string, string) error { return nil }
func (d *fakeDeviceRepo) UpdateHostFacts(context.Context, uint64, devicebiz.HostFacts) error {
	return nil
}
func (d *fakeDeviceRepo) MarkOnline(context.Context, uint64) error  { return nil }
func (d *fakeDeviceRepo) MarkOffline(context.Context, uint64) error { return nil }

func (d *fakeDeviceRepo) ReconcileOfflineOrphans(context.Context) (int64, error) { return 0, nil }
func (d *fakeDeviceRepo) Get(_ context.Context, id uint64) (*devicemodel.Device, error) {
	if v, ok := d.byID[id]; ok {
		return v, nil
	}
	return nil, errs.ErrNotFound
}
func (d *fakeDeviceRepo) GetMany(_ context.Context, ids []uint64) (map[uint64]*devicemodel.Device, error) {
	out := map[uint64]*devicemodel.Device{}
	for _, id := range ids {
		if v, ok := d.byID[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}
func (d *fakeDeviceRepo) UpdateUsage(context.Context, uint64, devicebiz.Usage) error { return nil }
func (d *fakeDeviceRepo) UpdateRoles(context.Context, uint64, uint8) error           { return nil }
func (d *fakeDeviceRepo) UpdateNameDescription(context.Context, uint64, string, string) error {
	return nil
}
func (d *fakeDeviceRepo) SetNodeID(context.Context, uint64, uint64) error { return nil }
func (d *fakeDeviceRepo) List(context.Context, devicebiz.ListFilter) ([]*devicemodel.Device, error) {
	out := make([]*devicemodel.Device, 0, len(d.byID))
	for _, v := range d.byID {
		out = append(out, v)
	}
	return out, nil
}
func (d *fakeDeviceRepo) Count(context.Context) (int64, error) { return int64(len(d.byID)), nil }
func (d *fakeDeviceRepo) Delete(_ context.Context, id uint64) error {
	if _, ok := d.byID[id]; !ok {
		return errs.ErrNotFound
	}
	delete(d.byID, id)
	return nil
}
func (d *fakeDeviceRepo) DeleteOfflineWithLinkedEdges(ctx context.Context, id uint64) error {
	return d.Delete(ctx, id)
}

// fakeSvc is an in-memory EdgeService for handler tests. Matches the real
// Service's method signatures exactly; any drift will fail compile.
type fakeSvc struct {
	createResp *biz.CreateResult
	createErr  error

	listResp []*model.Edge
	listErr  error

	getResp *model.Edge
	getErr  error

	deleteErr error

	rotateResp string
	rotateErr  error

	updateRolesErr error

	lastCreatedBy   *uint64
	lastListFlt     biz.ListFilter
	lastGetID       uint64
	lastDeleteID    uint64
	lastRotateID    uint64
	lastRolesEdgeID uint64
	lastRolesNames  []string

	// batch-test instrumentation. mu guards the slices/maps because the
	// batch runner invokes these methods from concurrent goroutines.
	mu            sync.Mutex
	deleteIDs     []uint64        // every id passed to Delete (batch-aware)
	upgradeIDs    []uint64        // every id passed to UpgradeAgent
	fetchIDs      []uint64        // every id passed to FetchPackage
	deleteFailIDs map[uint64]bool // ids for which Delete returns ErrNotFound
	fetchFailIDs  map[uint64]bool // ids for which FetchPackage fails (nothing staged)
	applyFailIDs  map[uint64]bool // ids for which ApplyPackage fails (staged, not applied)
	applyAccepted bool            // ApplyPackage.Accepted to report
	fetchManifest int             // FetchPackage.ManifestFiles to report
}

func (f *fakeSvc) Create(_ context.Context, _ string, createdBy *uint64) (*biz.CreateResult, error) {
	f.lastCreatedBy = createdBy
	return f.createResp, f.createErr
}
func (f *fakeSvc) List(_ context.Context, flt biz.ListFilter) ([]*model.Edge, error) {
	f.lastListFlt = flt
	return f.listResp, f.listErr
}
func (f *fakeSvc) Get(_ context.Context, id uint64) (*model.Edge, error) {
	f.lastGetID = id
	return f.getResp, f.getErr
}
func (f *fakeSvc) Delete(_ context.Context, id uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastDeleteID = id
	f.deleteIDs = append(f.deleteIDs, id)
	if f.deleteFailIDs[id] {
		return errs.ErrNotFound
	}
	return f.deleteErr
}
func (f *fakeSvc) RotateSecret(_ context.Context, id uint64) (string, error) {
	f.lastRotateID = id
	return f.rotateResp, f.rotateErr
}
func (f *fakeSvc) UpdateRoles(_ context.Context, id uint64, names []string) error {
	f.lastRolesEdgeID = id
	f.lastRolesNames = names
	return f.updateRolesErr
}
func (f *fakeSvc) UpgradeAgent(_ context.Context, id uint64, _ string, _ string) (tunnel.AgentUpgradeResponse, error) {
	f.mu.Lock()
	f.upgradeIDs = append(f.upgradeIDs, id)
	f.mu.Unlock()
	return tunnel.AgentUpgradeResponse{}, nil
}
func (f *fakeSvc) FetchPackage(_ context.Context, id uint64, _ string, _ string, _ string) (tunnel.FetchPackageResponse, error) {
	f.mu.Lock()
	f.fetchIDs = append(f.fetchIDs, id)
	mf := f.fetchManifest
	f.mu.Unlock()
	if f.fetchFailIDs[id] {
		return tunnel.FetchPackageResponse{}, errs.ErrEdgeOffline
	}
	return tunnel.FetchPackageResponse{ManifestFiles: mf}, nil
}
func (f *fakeSvc) ApplyPackage(_ context.Context, id uint64) (tunnel.ApplyPackageResponse, error) {
	f.mu.Lock()
	accepted := f.applyAccepted
	fail := f.applyFailIDs[id]
	f.mu.Unlock()
	if fail {
		return tunnel.ApplyPackageResponse{}, errs.ErrEdgeOffline
	}
	return tunnel.ApplyPackageResponse{Accepted: accepted}, nil
}
func (f *fakeSvc) GetProcessList(_ context.Context, _ uint64, _ uint32, _ string) (tunnel.GetProcessListResponse, error) {
	return tunnel.GetProcessListResponse{}, nil
}
func (f *fakeSvc) PluginHealth(_ uint64) []biz.PluginHealth { return nil }

// buildRouter wraps h.Register on a chi router with a middleware that
// injects the given tenant (simulating auth).
func buildRouter(h *Handler, t tenantctx.Tenant) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(tenantctx.With(req.Context(), t)))
		})
	})
	h.Register(r)
	return r
}

func TestKnownArch(t *testing.T) {
	for _, arch := range []string{"linux-amd64", "linux-arm64"} {
		if !knownArch(arch) {
			t.Fatalf("knownArch(%q) = false, want true", arch)
		}
	}
	for _, arch := range []string{"darwin-amd64", "darwin-arm64", "linux-arm"} {
		if knownArch(arch) {
			t.Fatalf("knownArch(%q) = true, want false", arch)
		}
	}
}

func TestCreate_AdminHappyPath(t *testing.T) {
	created := time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC)
	svc := &fakeSvc{
		createResp: &biz.CreateResult{
			Edge:      &model.Edge{ID: 5, Name: "n", CreatedAt: created},
			AccessKey: "ak-plain",
			SecretKey: "sk-plain",
		},
	}
	devices := newFakeDeviceRepo()
	h := NewHandler(svc, devices, nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 42, Role: "admin"})

	req := httptest.NewRequest(http.MethodPost, "/v1/edges", strings.NewReader(`{"name":"n"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body createResp
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if body.ID != 5 || body.SecretKey != "sk-plain" || body.AccessKeyID != "ak-plain" {
		t.Errorf("body = %+v", body)
	}
	if svc.lastCreatedBy == nil || *svc.lastCreatedBy != 42 {
		t.Errorf("createdBy = %v, want 42", svc.lastCreatedBy)
	}
}

func TestCreate_NonAdminForbidden(t *testing.T) {
	svc := &fakeSvc{}
	devices := newFakeDeviceRepo()
	h := NewHandler(svc, devices, nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 7, Role: "user"})

	req := httptest.NewRequest(http.MethodPost, "/v1/edges", strings.NewReader(`{"name":"n"}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Code != "forbidden" {
		t.Errorf("code = %q, want forbidden", body.Code)
	}
}

func TestListAsUser(t *testing.T) {
	devID := uint64(101)
	svc := &fakeSvc{
		listResp: []*model.Edge{
			{ID: 1, Name: "a", Status: "online", AccessKeyID: "ak-1", DeviceID: &devID},
			{ID: 2, Name: "b", Status: "offline", AccessKeyID: "ak-2"},
		},
	}
	devices := newFakeDeviceRepo(&devicemodel.Device{ID: devID, Hostname: "srv-1", OS: "linux"})
	h := NewHandler(svc, devices, nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 7, Role: "user"})

	req := httptest.NewRequest(http.MethodGet, "/v1/edges?status=online&limit=5", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body listResp
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 2 || len(body.Items) != 2 {
		t.Errorf("body = %+v", body)
	}
	if body.Items[0].HostInfo == nil || body.Items[0].HostInfo.Hostname != "srv-1" {
		t.Errorf("items[0].host_info = %#v, want hostname=srv-1", body.Items[0].HostInfo)
	}
	if svc.lastListFlt.Status != "online" || svc.lastListFlt.Limit != 5 {
		t.Errorf("filter = %+v, want status=online limit=5", svc.lastListFlt)
	}
}

func TestGet(t *testing.T) {
	devID := uint64(909)
	svc := &fakeSvc{
		getResp: &model.Edge{ID: 9, Name: "edge-9", Status: "offline", AccessKeyID: "ak-9", DeviceID: &devID},
	}
	devices := newFakeDeviceRepo(&devicemodel.Device{ID: devID, Hostname: "edge-9-host", CPUCount: 8})
	h := NewHandler(svc, devices, nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "user"})

	req := httptest.NewRequest(http.MethodGet, "/v1/edges/9", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var body getResp
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID != 9 || body.Name != "edge-9" {
		t.Errorf("body = %+v", body)
	}
	if body.HostInfo == nil || body.HostInfo.Hostname != "edge-9-host" {
		t.Errorf("host_info = %#v, want hostname=edge-9-host", body.HostInfo)
	}
	if svc.lastGetID != 9 {
		t.Errorf("lastGetID = %d, want 9", svc.lastGetID)
	}
}

func TestGetNotFound(t *testing.T) {
	svc := &fakeSvc{getErr: errs.ErrNotFound}
	devices := newFakeDeviceRepo()
	h := NewHandler(svc, devices, nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "user"})

	req := httptest.NewRequest(http.MethodGet, "/v1/edges/999", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Code != "not-found" {
		t.Errorf("code = %q, want not-found", body.Code)
	}
}

func TestDelete_AdminHappyPath(t *testing.T) {
	svc := &fakeSvc{}
	devices := newFakeDeviceRepo()
	h := NewHandler(svc, devices, nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	req := httptest.NewRequest(http.MethodDelete, "/v1/edges/7", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if svc.lastDeleteID != 7 {
		t.Errorf("lastDeleteID = %d, want 7", svc.lastDeleteID)
	}
}

func TestDelete_NonAdminForbidden(t *testing.T) {
	svc := &fakeSvc{}
	devices := newFakeDeviceRepo()
	h := NewHandler(svc, devices, nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "user"})

	req := httptest.NewRequest(http.MethodDelete, "/v1/edges/7", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestRotateSecret_AdminHappyPath(t *testing.T) {
	svc := &fakeSvc{rotateResp: "new-sk"}
	devices := newFakeDeviceRepo()
	h := NewHandler(svc, devices, nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	req := httptest.NewRequest(http.MethodPost, "/v1/edges/3/rotate-secret", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var body rotateResp
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.SecretKey != "new-sk" {
		t.Errorf("secret_key = %q, want new-sk", body.SecretKey)
	}
	if svc.lastRotateID != 3 {
		t.Errorf("lastRotateID = %d, want 3", svc.lastRotateID)
	}
}

func TestRotateSecret_NonAdminForbidden(t *testing.T) {
	svc := &fakeSvc{}
	devices := newFakeDeviceRepo()
	h := NewHandler(svc, devices, nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "user"})

	req := httptest.NewRequest(http.MethodPost, "/v1/edges/3/rotate-secret", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// fakePkgResolver returns a fixed bundle triple so batch upgrade-package
// tests don't need a real edge-bundles dir.
type fakePkgResolver struct{}

func (fakePkgResolver) ResolveBundle(_ string, _ string) (string, string, string, error) {
	return "https://example/opskeeper-edge", strings.Repeat("a", 64), "v9.9.9", nil
}

func postJSON(router http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestBatchDelete_AdminHappyPath(t *testing.T) {
	svc := &fakeSvc{}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	w := postJSON(router, "/v1/edges/batch/delete", `{"ids":[1,2,3]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body batchResp
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 3 || body.Succeeded != 3 || body.Failed != 0 {
		t.Errorf("summary = %+v, want total=3 succeeded=3", body)
	}
	sort.Slice(svc.deleteIDs, func(i, j int) bool { return svc.deleteIDs[i] < svc.deleteIDs[j] })
	if len(svc.deleteIDs) != 3 || svc.deleteIDs[0] != 1 || svc.deleteIDs[2] != 3 {
		t.Errorf("deleteIDs = %v, want [1 2 3]", svc.deleteIDs)
	}
}

func TestBatchDelete_PartialFailure(t *testing.T) {
	svc := &fakeSvc{deleteFailIDs: map[uint64]bool{2: true}}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	w := postJSON(router, "/v1/edges/batch/delete", `{"ids":[1,2,3]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body batchResp
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Succeeded != 2 || body.Failed != 1 {
		t.Errorf("summary = %+v, want succeeded=2 failed=1", body)
	}
	for _, r := range body.Results {
		if r.ID == 2 {
			if r.OK || r.Code != "not-found" {
				t.Errorf("id=2 result = %+v, want ok=false code=not-found", r)
			}
		}
	}
}

func TestBatchDelete_Dedupes(t *testing.T) {
	svc := &fakeSvc{}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	w := postJSON(router, "/v1/edges/batch/delete", `{"ids":[5,5,5]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var body batchResp
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Total != 1 || len(svc.deleteIDs) != 1 {
		t.Errorf("total=%d deleteIDs=%v, want a single deduped delete", body.Total, svc.deleteIDs)
	}
}

func TestBatchDelete_EmptyIDsInvalid(t *testing.T) {
	svc := &fakeSvc{}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	w := postJSON(router, "/v1/edges/batch/delete", `{"ids":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestBatchDelete_NonAdminForbidden(t *testing.T) {
	svc := &fakeSvc{}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "user"})

	w := postJSON(router, "/v1/edges/batch/delete", `{"ids":[1,2]}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if len(svc.deleteIDs) != 0 {
		t.Errorf("delete should not run for non-admin; got %v", svc.deleteIDs)
	}
}

func TestBatchUpgradeAgent_HappyPath(t *testing.T) {
	svc := &fakeSvc{}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	body := `{"ids":[1,2],"url":"https://example/edge","sha256":"` + strings.Repeat("a", 64) + `"}`
	w := postJSON(router, "/v1/edges/batch/upgrade", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp batchResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Succeeded != 2 {
		t.Errorf("succeeded = %d, want 2", resp.Succeeded)
	}
	if len(svc.upgradeIDs) != 2 {
		t.Errorf("upgradeIDs = %v, want 2 calls", svc.upgradeIDs)
	}
}

func TestBatchUpgradeAgent_MissingURLInvalid(t *testing.T) {
	svc := &fakeSvc{}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	w := postJSON(router, "/v1/edges/batch/upgrade", `{"ids":[1],"url":"","sha256":""}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestBatchUpgradePackage_HappyPath(t *testing.T) {
	svc := &fakeSvc{applyAccepted: true, fetchManifest: 7}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	h.SetPackageResolver(fakePkgResolver{})
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	w := postJSON(router, "/v1/edges/batch/upgrade-package", `{"ids":[1,2,3]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp batchResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Succeeded != 3 || resp.Failed != 0 {
		t.Errorf("summary = %+v, want succeeded=3", resp)
	}
	for _, r := range resp.Results {
		if !r.Applied || r.Version != "v9.9.9" || r.ManifestFiles != 7 {
			t.Errorf("result = %+v, want applied=true version=v9.9.9 manifest=7", r)
		}
	}
}

func TestBatchUpgradePackage_NotWired(t *testing.T) {
	svc := &fakeSvc{}
	h := NewHandler(svc, newFakeDeviceRepo(), nil) // no resolver
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	w := postJSON(router, "/v1/edges/batch/upgrade-package", `{"ids":[1]}`)
	if w.Code == http.StatusOK {
		t.Fatalf("expected non-200 when resolver missing; body=%s", w.Body.String())
	}
	if len(svc.fetchIDs) != 0 {
		t.Errorf("fetch should not run when resolver missing; got %v", svc.fetchIDs)
	}
}

// --- 决策 332：两条最高后果的节点写路由，以及它们「不写什么」 --------------------
//
// 331 把 62 条此前不可见的路由摆到台面上，并按后果排了序，第一条是
// 「谁换了节点的凭据」、第二条是「这台机器上现在跑的是哪个插件」。这两条
// 此处补的是审计行，而用例真正要钉住的是**行里没有的东西**：
//
//	轮换密钥的那一行绝不能包含新密钥；
//	开关插件的那一行绝不能包含 spec。
//
// 前者是常识，后者不是：spec 里可能有凭据（database_metrics 会顺手往密钥库
// 写条目），而一个只存当前值的字段在 append-only 的链里是负资产——读者会
// 以为自己读到的是事实，而它可能三个月前就被改过一次了。**一个假装完整的
// 快照，比一个诚实的「决定」危险。**

type fakePluginCfg struct {
	gotEdge   uint64
	gotPlugin string
	gotInput  biz.SetInput
}

func (f *fakePluginCfg) ListForUI(context.Context, uint64) ([]biz.PluginRow, error) {
	return nil, nil
}
func (f *fakePluginCfg) Set(_ context.Context, edgeID uint64, plugin string, in biz.SetInput) (*biz.PluginRow, error) {
	f.gotEdge, f.gotPlugin, f.gotInput = edgeID, plugin, in
	return &biz.PluginRow{PluginName: plugin, Enabled: in.Enabled}, nil
}
func (f *fakePluginCfg) CountByPlugin(context.Context) (map[string]int64, error) {
	return map[string]int64{}, nil
}

// callWithSlot issues one request through a slot-bearing context and returns
// whatever the handler handed to the audit port. Everything else about the
// request is the same as production; the slot is what a real process installs
// (决策 321: without it every SetAuditEvent is a no-op).
func callWithSlot(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	ev, ok := auditport.GetAuditEvent(req.Context())
	return rec, ev, ok
}

func TestRotateSecretWritesARowAndNotTheSecret(t *testing.T) {
	const minted = "sk-live-DO-NOT-LOG-ME"
	h := NewHandler(&fakeSvc{rotateResp: minted}, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	rec, ev, ok := callWithSlot(t, router, http.MethodPost, "/v1/edges/3/rotate-secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !ok {
		t.Fatal("no audit event: a credential rotation that leaves no row is a rotation nobody can answer for")
	}
	if ev.Action != auditport.ActionEdgeRotateSecret || ev.Status != auditport.StatusSuccess {
		t.Errorf("event = %+v, want a successful edge_rotate_secret", ev)
	}
	if ev.ResourceType != auditport.ResourceEdge || ev.ResourceID != "3" {
		t.Errorf("resource = %s/%s, want edge/3 — the node is the thing whose credential changed", ev.ResourceType, ev.ResourceID)
	}
	// The whole point. The secret is in the HTTP response, which is where the
	// caller needs it; it must not be anywhere the chain will keep it.
	for _, bucket := range []any{ev.Payload, ev.ResourceName, ev.ErrorMessage} {
		if strings.Contains(dumpForAssertion(bucket), minted) {
			t.Errorf("the minted secret reached the audit event (%+v): every chain reader becomes a credential holder", bucket)
		}
	}
}

func TestSetPluginWritesTheDecisionAndNotTheSpec(t *testing.T) {
	cfg := &fakePluginCfg{}
	h := NewHandler(&fakeSvc{}, newFakeDeviceRepo(), cfg)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	const spec = `{"dsn":"postgres://ops:hunter2@10.0.0.7:5432/prod"}`
	rec, ev, ok := callWithSlot(t, router, http.MethodPut, "/v1/edges/9/plugins/database_metrics",
		`{"enabled":true,"spec":`+spec+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !ok {
		t.Fatal("no audit event: which plugin runs on which host has to be written down somewhere")
	}
	if ev.Action != auditport.ActionEdgePluginSet || ev.Status != auditport.StatusSuccess {
		t.Errorf("event = %+v, want a successful edge_plugin_set", ev)
	}
	if ev.ResourceID != "9" || ev.ResourceName != "database_metrics" {
		t.Errorf("resource = %s/%s, want 9/database_metrics", ev.ResourceID, ev.ResourceName)
	}
	payload, _ := ev.Payload.(map[string]any)
	if got, _ := payload["enabled"].(bool); !got {
		t.Errorf("payload = %v, want the decision (enabled=true) on it", ev.Payload)
	}
	if strings.Contains(dumpForAssertion(ev.Payload), "hunter2") {
		t.Errorf("the connection string reached the chain: %v", ev.Payload)
	}
	// And the config really did carry the secret through the handler — if this
	// stops being true the assertion above has stopped proving anything.
	if cfg.gotInput.Spec["dsn"] == nil {
		t.Fatal("the fake never received the spec, so the payload assertions above are vacuous")
	}
}

// dumpForAssertion renders any event field for a substring search. A helper
// rather than three fmt calls because the assertion has to be written once and
// used for all three fields — a check applied to two of three is the kind of
// check that reads as thorough.
func dumpForAssertion(v any) string {
	if v == nil {
		return ""
	}
	if str, ok := v.(string); ok {
		return str
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// --- 决策 333：供应链面，以及「一次点按不等于一次事件」 --------------------------
//
// 批量升级一次最多动 500 台机器。这一批用例要钉住的是四件事：
//
//  1. **每台机器一行**，不是一个请求一行 —— 否则「哪台没升上去」只能去翻日志；
//  2. **URL 的 query 被剥掉** —— 里面常常是预签名参数，而**预签名令牌落进
//     append-only 的链，就是一个寿命很长、且无法撤销的持有者凭证**；
//  3. **「字节到了但没生效」单独可辨** —— 批量升级里最危险的不是失败，是这种；
//  4. 成功与失败各是各的状态。

func TestBatchAgentUpgradeLandsOneRowPerNodeAndStripsTheQuery(t *testing.T) {
	svc := &fakeSvc{}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	const signed = "https://mirror.example.com/edge/agent.tar.gz?X-Amz-Signature=DEADBEEF&X-Amz-Expires=900"
	body := `{"ids":[3,4,5],"url":"` + signed + `","sha256":"aabbcc"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/edges/batch/upgrade", strings.NewReader(body))
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}

	extra := auditport.ExtraAuditEvents(req.Context())
	if len(extra) != 3 {
		t.Fatalf("landed %d rows for 3 nodes, want one each: %+v", len(extra), extra)
	}
	for i, want := range []string{"3", "4", "5"} {
		row := extra[i]
		if row.Action != auditport.ActionEdgeAgentUpgrade || row.ResourceID != want {
			t.Errorf("row %d = %s/%s, want edge_agent_upgrade/%s", i, row.Action, row.ResourceID, want)
		}
		if row.Status != auditport.StatusSuccess {
			t.Errorf("row %d status = %q, want success", i, row.Status)
		}
		blob := dumpForAssertion(row.Payload)
		if strings.Contains(blob, "X-Amz-Signature") || strings.Contains(blob, "DEADBEEF") {
			t.Errorf("row %d kept the presigned token: %s", i, blob)
		}
		if !strings.Contains(blob, "agent.tar.gz") {
			t.Errorf("row %d lost the artifact path, which is the evidence worth keeping: %s", i, blob)
		}
		if !strings.Contains(blob, "aabbcc") {
			t.Errorf("row %d lost the digest: %s", i, blob)
		}
	}
}

func TestBatchPackageUpgradeDistinguishesStagedFromFailed(t *testing.T) {
	// 5 fails to download (nothing on disk), 6 downloads but refuses to apply
	// (bytes on disk, still running the old bundle), 7 is clean.
	svc := &fakeSvc{
		fetchManifest: 12,
		fetchFailIDs:  map[uint64]bool{5: true},
		applyFailIDs:  map[uint64]bool{6: true},
		applyAccepted: true,
	}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	h.SetPackageResolver(fakePkgResolver{})
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	req := httptest.NewRequest(http.MethodPost, "/v1/edges/batch/upgrade-package", strings.NewReader(`{"ids":[5,6,7]}`))
	req = req.WithContext(auditport.WithSlot(req.Context()))
	router.ServeHTTP(httptest.NewRecorder(), req)

	extra := auditport.ExtraAuditEvents(req.Context())
	if len(extra) != 3 {
		t.Fatalf("landed %d rows for 3 nodes: %+v", len(extra), extra)
	}
	type want struct {
		id     string
		status string
		staged bool
	}
	for i, w := range []want{
		{"5", auditport.StatusFailure, false}, // never arrived
		{"6", auditport.StatusFailure, true},  // arrived, did not take effect
		{"7", auditport.StatusSuccess, true},
	} {
		row := extra[i]
		if row.ResourceID != w.id || row.Status != w.status {
			t.Errorf("row %d = %s/%s, want %s/%s", i, row.ResourceID, row.Status, w.id, w.status)
		}
		payload, _ := row.Payload.(map[string]any)
		if got, _ := payload["staged"].(bool); got != w.staged {
			t.Errorf("row %d staged = %v, want %v — \"failed\" and \"staged but not applied\" are different states", i, got, w.staged)
		}
	}
}

func TestSinglePackageUpgradeRecordsStagedButNotApplied(t *testing.T) {
	svc := &fakeSvc{applyFailIDs: map[uint64]bool{8: true}, fetchManifest: 3}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	h.SetPackageResolver(fakePkgResolver{})
	router := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "admin"})

	req := httptest.NewRequest(http.MethodPost, "/v1/edges/8/upgrade-package", strings.NewReader(""))
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (staged, not applied); body=%s", rec.Code, rec.Body.String())
	}
	ev, ok := auditport.GetAuditEvent(req.Context())
	if !ok {
		t.Fatal("no row: \"the bytes are on the node but the node did not change\" is exactly the state an investigation needs")
	}
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	payload, _ := ev.Payload.(map[string]any)
	if staged, _ := payload["staged"].(bool); !staged {
		t.Errorf("payload = %v, want staged=true", ev.Payload)
	}
}

// --- 决策 334：节点生命周期两端 --------------------------------------------------
//
// 332/333 写的是弧线中段（哪段字节、哪个插件、谁的钥匙）；334 补两端——
// **谁把这台机器放进来的、谁把它摘出去的**。中间那一段只有在两端可答的时候
// 才答得完整：一条只有「节点 417」而没有「谁放进来」的记录，等于没有。
//
// 注册是全平台唯一一个「创建即发凭据」的路由，所以它也是最容易顺手把密钥写进
// 审计行的地方——而链恰恰是最多人能读的地方。

func TestRegisterWritesWhoAdmittedTheNodeAndNotItsKey(t *testing.T) {
	const minted = "sk-live-REGISTER-DO-NOT-LOG"
	svc := &fakeSvc{createResp: &biz.CreateResult{
		Edge:      &model.Edge{ID: 21, Name: "prod-web-03"},
		AccessKey: "AKIAEXAMPLE", SecretKey: minted,
	}}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 5, Role: "admin"})

	req := httptest.NewRequest(http.MethodPost, "/v1/edges", strings.NewReader(`{"name":"prod-web-03"}`))
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}

	ev, ok := auditport.GetAuditEvent(req.Context())
	if !ok {
		t.Fatal("no row: \"who admitted this host\" is the first question asked of any node")
	}
	if ev.Action != auditport.ActionEdgeRegister || ev.ResourceID != "21" || ev.ResourceName != "prod-web-03" {
		t.Errorf("event = %+v, want edge_register on edge/21 named prod-web-03", ev)
	}
	blob := dumpForAssertion(ev.Payload)
	if strings.Contains(blob, minted) || strings.Contains(blob, "sk-live") {
		t.Errorf("the minted secret reached the chain: %s", blob)
	}
	if !strings.Contains(blob, "AKIAEXAMPLE") {
		t.Errorf("the access key id was dropped: %s — it is an identifier, not a credential, and it is how a rotation names its target", blob)
	}
}

func TestDeleteWritesWhoRemovedTheNodeByName(t *testing.T) {
	svc := &fakeSvc{getResp: &model.Edge{ID: 7, Name: "prod-db-01"}}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 5, Role: "admin"})

	req := httptest.NewRequest(http.MethodDelete, "/v1/edges/7", nil)
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	ev, ok := auditport.GetAuditEvent(req.Context())
	if !ok {
		t.Fatal("no row: a node removed without a row is a node that comes back unexplained")
	}
	if ev.Action != auditport.ActionEdgeDelete || ev.ResourceName != "prod-db-01" {
		t.Errorf("event = %+v, want edge_delete carrying the node's name", ev)
	}
}

// 批量删除：每台一行。理由与批量升级同一条，但在这里更硬——
// **一台没被摘掉的机器会继续心跳、继续拿着旧凭据。**
func TestBatchDeleteLandsOneRowPerNode(t *testing.T) {
	svc := &fakeSvc{deleteFailIDs: map[uint64]bool{5: true}}
	h := NewHandler(svc, newFakeDeviceRepo(), nil)
	router := buildRouter(h, tenantctx.Tenant{UserID: 5, Role: "admin"})

	req := httptest.NewRequest(http.MethodPost, "/v1/edges/batch/delete", strings.NewReader(`{"ids":[5,6]}`))
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	extra := auditport.ExtraAuditEvents(req.Context())
	if len(extra) != 2 {
		t.Fatalf("landed %d rows for 2 nodes: %+v", len(extra), extra)
	}
	for i, want := range []struct {
		id     string
		status string
	}{{"5", auditport.StatusFailure}, {"6", auditport.StatusSuccess}} {
		if extra[i].ResourceID != want.id || extra[i].Status != want.status {
			t.Errorf("row %d = %s/%s, want %s/%s — a node that failed to be removed is still in the fleet",
				i, extra[i].ResourceID, extra[i].Status, want.id, want.status)
		}
		if extra[i].Action != auditport.ActionEdgeDelete {
			t.Errorf("row %d action = %q", i, extra[i].Action)
		}
	}
}
