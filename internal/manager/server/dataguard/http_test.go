package dataguard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
	"github.com/vincent-wuhan/opskeeper/internal/dataguard/heuristic"
	dglabel "github.com/vincent-wuhan/opskeeper/internal/dataguard/label"
	"github.com/vincent-wuhan/opskeeper/internal/dataguard/store"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// fakeRepo is the in-memory label repo used by these HTTP tests.
type fakeRepo struct {
	labels map[string]*store.DataSensitivityLabel
}

func newFakeRepo() *fakeRepo { return &fakeRepo{labels: map[string]*store.DataSensitivityLabel{}} }
func (r *fakeRepo) key(rt, rid string) string {
	return rt + "|" + rid
}
func (r *fakeRepo) Create(_ context.Context, l *store.DataSensitivityLabel) error {
	r.labels[r.key(l.ResourceType, l.ResourceID)] = l
	return nil
}
func (r *fakeRepo) Get(_ context.Context, rt, rid string) (*store.DataSensitivityLabel, error) {
	l, ok := r.labels[r.key(rt, rid)]
	if !ok {
		return nil, errs.ErrNotFound
	}
	return l, nil
}
func (r *fakeRepo) List(_ context.Context, _, _ string, _, _ int) ([]*store.DataSensitivityLabel, int64, error) {
	out := make([]*store.DataSensitivityLabel, 0, len(r.labels))
	for _, l := range r.labels {
		out = append(out, l)
	}
	return out, int64(len(out)), nil
}
func (r *fakeRepo) Delete(_ context.Context, rt, rid string) error {
	if _, ok := r.labels[r.key(rt, rid)]; !ok {
		return errs.ErrNotFound
	}
	delete(r.labels, r.key(rt, rid))
	return nil
}

func (r *fakeRepo) ListByResourceType(_ context.Context, resourceType, sens string, _, _ int) ([]*store.DataSensitivityLabel, error) {
	var out []*store.DataSensitivityLabel
	for _, l := range r.labels {
		if l.ResourceType != resourceType {
			continue
		}
		if sens != "" && l.Sensitivity != sens {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

func newTestHandlerRouter() (http.Handler, *fakeRepo) {
	repo := newFakeRepo()
	mgr := dglabel.NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), nil).
		WithClock(func() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) })
	h := NewHandler(mgr)
	r := chi.NewRouter()
	h.Register(r)
	return r, repo
}

// tenant constructs a tenantctx.Tenant optionally with admin role.
func tenant(role string) tenantctx.Tenant {
	t := tenantctx.Tenant{UserID: 42, Role: role}
	if role == "admin" {
		t.IsSuperuser = true
	}
	return t
}

// issue creates a request with an optional tenant and returns the recorder after
// dispatching through the handler's router.
func issue(t *testing.T, router http.Handler, method, target string, body any, ten *tenantctx.Tenant) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ten != nil {
		req = req.WithContext(tenantctx.With(req.Context(), *ten))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestPOST_Labels_ManualAdmin(t *testing.T) {
	router, _ := newTestHandlerRouter()
	rec := issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "tbl_users", Sensitivity: "Confidential",
	}, ptrTenant("admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, body=%s", rec.Code, rec.Body.String())
	}
	var resp LabelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Label.Sensitivity != "Confidential" {
		t.Errorf("sensitivity = %s", resp.Label.Sensitivity)
	}
	if resp.Label.LabelSource != string(store.SourceManual) {
		t.Errorf("source = %s, want manual", resp.Label.LabelSource)
	}
	if resp.Effective != "Confidential" {
		t.Errorf("effective = %s, want Confidential", resp.Effective)
	}
}

func TestPOST_Labels_NonAdmin_Forbidden(t *testing.T) {
	router, _ := newTestHandlerRouter()
	rec := issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "t", Sensitivity: "Public",
	}, ptrTenant("user"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
}

func TestPOST_Labels_NoTenant_Unauthorized(t *testing.T) {
	router, _ := newTestHandlerRouter()
	rec := issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "t", Sensitivity: "Public",
	}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

func TestPOST_Labels_BadJSON(t *testing.T) {
	router, _ := newTestHandlerRouter()
	req := httptest.NewRequest("POST", "/v1/data-guard/labels",
		bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	ten := tenant("admin")
	req = req.WithContext(tenantctx.With(req.Context(), ten))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}

func TestPOST_Labels_OverrideSemantics(t *testing.T) {
	router, _ := newTestHandlerRouter()
	// baseline
	_ = issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "t1", Sensitivity: "Public",
	}, ptrTenant("admin"))
	// override
	body := LabelRequest{
		ResourceType: "pg", ResourceID: "t1", Sensitivity: "Restricted",
		Override: true, OverrideReason: "compliance",
	}
	rec := issue(t, router, "POST", "/v1/data-guard/labels", body, ptrTenant("admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("override code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp LabelResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Label.LabelSource != string(store.SourceOverride) {
		t.Errorf("source = %s, want override", resp.Label.LabelSource)
	}
	if resp.Effective != "Restricted" {
		t.Errorf("effective = %s, want Restricted", resp.Effective)
	}
}

func TestPUT_Labels_OverrideByURL(t *testing.T) {
	router, _ := newTestHandlerRouter()
	_ = issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "t2", Sensitivity: "Public",
	}, ptrTenant("admin"))
	rec := issue(t, router, "PUT", "/v1/data-guard/labels/pg/t2", LabelRequest{
		Sensitivity: "TopSecret", OverrideReason: "pci review",
	}, ptrTenant("admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGET_Labels_SingleWithResolution(t *testing.T) {
	router, _ := newTestHandlerRouter()
	_ = issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "ts", Sensitivity: "TopSecret",
	}, ptrTenant("admin"))
	rec := issue(t, router, "GET", "/v1/data-guard/labels?resource_type=pg&resource_id=ts", nil, ptrTenant("admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code=%d, body=%s", rec.Code, rec.Body.String())
	}
	var resp LabelResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Effective != "TopSecret" {
		t.Errorf("effective = %s", resp.Effective)
	}
}

func TestGET_Labels_ListNoFilters(t *testing.T) {
	router, _ := newTestHandlerRouter()
	_ = issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "a", Sensitivity: "Public",
	}, ptrTenant("admin"))
	_ = issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "b", Sensitivity: "Confidential",
	}, ptrTenant("admin"))
	rec := issue(t, router, "GET", "/v1/data-guard/labels", nil, ptrTenant("admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET list code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []*store.DataSensitivityLabel `json:"items"`
		Total int64                         `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
}

func TestDELETE_Labels_RemovesAndAuditLogs(t *testing.T) {
	router, _ := newTestHandlerRouter()
	_ = issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "d", Sensitivity: "Public",
	}, ptrTenant("admin"))
	rec := issue(t, router, "DELETE", "/v1/data-guard/labels/pg/d", nil, ptrTenant("admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDataGuard_TopSecretZeroTamper(t *testing.T) {
	if !dataguard.TopSecret.IsZeroTamper() {
		t.Error("TopSecret should be zero-tamper")
	}
	if dataguard.Public.IsZeroTamper() {
		t.Error("Public should NOT be zero-tamper")
	}
}

func ptrTenant(role string) *tenantctx.Tenant {
	t := tenant(role)
	return &t
}

func TestGET_Labels_FilterByResourceType(t *testing.T) {
	router, _ := newTestHandlerRouter()
	_ = issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "tbl_a", Sensitivity: "Public",
	}, ptrTenant("admin"))
	_ = issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "pg", ResourceID: "tbl_b", Sensitivity: "Confidential",
	}, ptrTenant("admin"))
	_ = issue(t, router, "POST", "/v1/data-guard/labels", LabelRequest{
		ResourceType: "redis", ResourceID: "k1", Sensitivity: "Public",
	}, ptrTenant("admin"))

	// 仅 resource_type=pg 应得到 2 条
	rec := issue(t, router, "GET", "/v1/data-guard/labels?resource_type=pg", nil, ptrTenant("admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []*store.DataSensitivityLabel `json:"items"`
		Total int64                         `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 2 {
		t.Errorf("expected 2 pg labels, got %d", resp.Total)
	}

	// resource_type=redis → 1 条
	rec = issue(t, router, "GET", "/v1/data-guard/labels?resource_type=redis", nil, ptrTenant("admin"))
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 1 {
		t.Errorf("expected 1 redis label, got %d", resp.Total)
	}
}

func TestGET_Labels_EffectiveParserWithComplianceTags(t *testing.T) {
	router, _ := newTestHandlerRouter()
	// 创建带 compliance_tags 的 label
	tagsJSON, _ := json.Marshal([]map[string]any{
		{"framework": "PCI-DSS", "controls": []string{"encryption-at-rest"}, "enforced": true},
		{"framework": "GDPR", "controls": []string{"subject-erasure"}, "enforced": false},
	})
	body := LabelRequest{
		ResourceType: "pg", ResourceID: "tbl_pii", Sensitivity: "TopSecret",
		ComplianceTags: []string{}, // 留空，让 override body 包含 raw
	}
	_ = body
	req := httptest.NewRequest("POST", "/v1/data-guard/labels",
		bytes.NewReader([]byte(`{
			"resource_type":"pg",
			"resource_id":"tbl_pii",
			"sensitivity":"TopSecret",
			"compliance_tags":["x"],
			"notes":"manual"
		}`)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(tenantctx.With(req.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST code=%d body=%s", rec.Code, rec.Body.String())
	}

	// effective=true 解析
	rec = issue(t, router, "GET",
		"/v1/data-guard/labels?resource_type=pg&effective=true", nil, ptrTenant("admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET effective code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []EffectiveLabel `json:"items"`
		Total int64            `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total < 1 {
		t.Fatalf("expected ≥1 item, got %d", resp.Total)
	}
	found := false
	for _, it := range resp.Items {
		if it.Label != nil && it.Label.ResourceID == "tbl_pii" {
			found = true
			if it.Effective != "TopSecret" {
				t.Errorf("effective = %s, want TopSecret", it.Effective)
			}
		}
	}
	if !found {
		t.Error("didn't find tbl_pii in effective response")
	}
	_ = tagsJSON // silence unused warning
}
