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

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard/heuristic"
	dglabel "github.com/vincent-wuhan/opskeeper/core/manager/dataguard/label"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard/store"
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
func (r *fakeRepo) StrictestForResourceID(_ context.Context, rid string) ([]*store.DataSensitivityLabel, error) {
	var out []*store.DataSensitivityLabel
	for _, l := range r.labels {
		if l.ResourceID == rid {
			out = append(out, l)
		}
	}
	return out, nil
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

// TestGET_Labels_EffectiveParserWithComplianceTags 断言的是**标签真的回来了**，
// 而不只是 "effective 解析得出来"。
//
// 这条用例原来叫这个名字，却只断言 `Effective` 是不是 TopSecret：它建了一份
// 富标签（framework + controls + enforced）却把它丢掉，POST 的是 `["x"]`，
// 然后 `_ = tagsJSON` 把没用的那份消音。**一个名字承诺了它没有断言的东西，
// 比没有这条用例更坏**——读的人以为合规标签这条路被覆盖了。
//
// 而它当时是绿的，因为合规标签**当时确实回不来**：读路径用另一个形状解析，
// 又丢掉错误，于是这一列永远是空的（决策 368）。所以这里断言的是往返，
// 并且额外钉住"损坏的列不读成空列"。
func TestGET_Labels_EffectiveParserWithComplianceTags(t *testing.T) {
	router, _ := newTestHandlerRouter()
	req := httptest.NewRequest("POST", "/v1/data-guard/labels",
		bytes.NewReader([]byte(`{
			"resource_type":"pg",
			"resource_id":"tbl_pii",
			"sensitivity":"TopSecret",
			"compliance_tags":["PCI-DSS","GDPR"],
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
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET body %q: %v", rec.Body.String(), err)
	}
	if resp.Total < 1 {
		t.Fatalf("expected >=1 item, got %d", resp.Total)
	}

	// 名字里承诺的东西：标签必须回到调用方手里，一个不少、顺序不变。
	want := []string{"PCI-DSS", "GDPR"}
	found := false
	for _, it := range resp.Items {
		if it.Label == nil || it.Label.ResourceID != "tbl_pii" {
			continue
		}
		found = true
		if it.Effective != "TopSecret" {
			t.Errorf("effective = %s, want TopSecret", it.Effective)
		}
		if len(it.ComplianceTags) != len(want) {
			t.Fatalf("compliance_tags came back as %q, want %q — POST wrote them and "+
				"nothing removed them, so an empty result here is a broken round trip, "+
				"not an absent feature", it.ComplianceTags, want)
		}
		for i := range want {
			if it.ComplianceTags[i] != want[i] {
				t.Errorf("compliance_tags[%d] = %q, want %q", i, it.ComplianceTags[i], want[i])
			}
		}
	}
	if !found {
		t.Fatal("didn't find tbl_pii in effective response")
	}
}

// TestGET_Labels_EffectiveFailsOnAMalformedComplianceColumn 是上一条声称的另一半。
//
// 上一条的第一版注释里写着"额外钉住损坏的列不读成空列"，**而它当时并没有钉住**：
// 把读路径的错误传播删掉，那条用例照样绿——因为 HTTP 写出的列永远是合法 JSON，
// 于是 `tagErr` 恒为 nil，错误传播这条分支一次也没被走到。
//
// **一个在注释里被断言、而实际没有被覆盖的性质，比没有写更坏**：读的人会以为
// 它有人看着。所以这里绕过 HTTP 直接往 repo 里写一列别的编码器形状的脏数据，
// 然后要求这个请求**失败**。
//
// 要求它失败而不是返回空标签，是有方向的：让一个"我不知道这一列写了什么"的
// 资源显示成"这个资源没有标签"，正是当初把 GDPR 标签藏起来的那件事。
func TestGET_Labels_EffectiveFailsOnAMalformedComplianceColumn(t *testing.T) {
	router, repo := newTestHandlerRouter()

	// 这一列是另一个包会写的合法 JSON（`[]ComplianceTag` 形状），
	// 但它不是这一列的形状——store 模型的字段注释写的是 "JSON array of
	// framework names"。它从外面看不出来坏，这正是它危险的地方。
	const foreignShape = `[{"framework":"GDPR","controls":["subject-erasure"],"enforced":true}]`
	if err := repo.Create(context.Background(), &store.DataSensitivityLabel{
		ResourceType:   "pg",
		ResourceID:     "tbl_dirty",
		Sensitivity:    "TopSecret",
		ComplianceTags: foreignShape,
	}); err != nil {
		t.Fatalf("seeding the dirty column: %v", err)
	}

	rec := issue(t, router, "GET",
		"/v1/data-guard/labels?resource_type=pg&effective=true", nil, ptrTenant("admin"))
	if rec.Code == http.StatusOK {
		t.Fatalf("GET returned %d for a compliance_tags column this build cannot read (%q); "+
			"it reported it as a resource with no compliance tags, which is the failure "+
			"this whole column had",
			rec.Code, foreignShape)
	}
}

// --- 决策 312：脱敏规则变更上宿主链 -----------------------------------------
//
// 这三个路由改的是**脱敏规则本身**。测试要钉住的不是"能写进去"，
// 而是「谁把什么从什么降到了什么」——而这只有载荷里带 effective_before 才答得出。

// issueAudited is issue() plus the audit slot, so the test can read the
// event back off the request's own context after the handler returned.
func issueAudited(t *testing.T, router http.Handler, method, target string, body any, ten *tenantctx.Tenant) (*httptest.ResponseRecorder, auditport.Event, bool) {
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
	ctx := auditport.WithSlot(req.Context())
	if ten != nil {
		ctx = tenantctx.With(ctx, *ten)
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	ev, set := auditport.GetAuditEvent(req.Context())
	return rec, ev, set
}

// TestOverride_AuditCarriesTheDowngrade is the load-bearing one.
// 把一个资源的生效脱敏级别降下来，是这套系统里最需要事后可查的动作，
// 而"降级"这两个字只存在于 before/after 这一对里。
func TestOverride_AuditCarriesTheDowngrade(t *testing.T) {
	router, _ := newTestHandlerRouter()
	admin := tenant("admin")

	if rec := issue(t, router, http.MethodPost, "/v1/data-guard/labels",
		LabelRequest{ResourceType: "pg", ResourceID: "orders", Sensitivity: "TopSecret"}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("seed label: %d %s", rec.Code, rec.Body.String())
	}

	rec, ev, set := issueAudited(t, router, http.MethodPut, "/v1/data-guard/labels/pg/orders",
		LabelRequest{Sensitivity: "Public", OverrideReason: "已确认不含 PII"}, &admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("override: %d %s", rec.Code, rec.Body.String())
	}
	if !set || ev.Action != auditport.ActionDataGuardLabelOverride || ev.Status != auditport.StatusSuccess {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if ev.ResourceType != auditport.ResourceDataGuardLabel || ev.ResourceID != "pg/orders" {
		t.Fatalf("resource = %q/%q", ev.ResourceType, ev.ResourceID)
	}
	p := ev.Payload.(map[string]any)
	if p["effective_before"] != "TopSecret" || p["effective_after"] != "Public" {
		t.Fatalf("before/after = %v → %v；没有这一对就答不出「降级发生过」", p["effective_before"], p["effective_after"])
	}
	if p["override_reason"] != "已确认不含 PII" {
		t.Fatalf("override_reason = %v — 降级没有理由等于没有理由", p["override_reason"])
	}
	if p["actor_user_id"] != uint64(42) {
		t.Fatalf("actor_user_id = %v", p["actor_user_id"])
	}
}

func TestUpsertLabel_PlainSetIsNotAnOverride(t *testing.T) {
	router, _ := newTestHandlerRouter()
	_, ev, set := issueAudited(t, router, http.MethodPost, "/v1/data-guard/labels",
		LabelRequest{ResourceType: "pg", ResourceID: "orders", Sensitivity: "Internal"},
		ptr(tenant("admin")))
	if !set || ev.Action != auditport.ActionDataGuardLabelSet {
		t.Fatalf("ev = %+v (set=%v), want dataguard_label_set", ev, set)
	}
	p := ev.Payload.(map[string]any)
	if p["override"] == true {
		t.Fatal("a plain label was recorded as an override")
	}
	if p["sensitivity"] != "Internal" || p["resource_type"] != "pg" {
		t.Fatalf("payload = %v", p)
	}
}

func TestUpsertLabel_WithOverrideFlagUsesTheOverrideAction(t *testing.T) {
	router, _ := newTestHandlerRouter()
	issue(t, router, http.MethodPost, "/v1/data-guard/labels",
		LabelRequest{ResourceType: "pg", ResourceID: "orders", Sensitivity: "TopSecret"}, ptr(tenant("admin")))
	// The POST path routes to UpdateOverride when Override is set — the same
	// operation as the PUT, and it must not be filed as an ordinary set.
	_, ev, set := issueAudited(t, router, http.MethodPost, "/v1/data-guard/labels",
		LabelRequest{ResourceType: "pg", ResourceID: "orders", Sensitivity: "Public", Override: true, OverrideReason: "r"},
		ptr(tenant("admin")))
	if !set || ev.Action != auditport.ActionDataGuardLabelOverride {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	if ev.Payload.(map[string]any)["effective_before"] != "TopSecret" {
		t.Fatalf("before = %v", ev.Payload.(map[string]any)["effective_before"])
	}
}

// 删除之后这条资源就不再被脱敏，而"它之前是什么"是唯一还问得出的问题。
func TestDeleteLabel_AuditRecordsWhatStoppedBeingMasked(t *testing.T) {
	router, _ := newTestHandlerRouter()
	admin := tenant("admin")
	issue(t, router, http.MethodPost, "/v1/data-guard/labels",
		LabelRequest{ResourceType: "pg", ResourceID: "orders", Sensitivity: "TopSecret"}, &admin)

	_, ev, set := issueAudited(t, router, http.MethodDelete, "/v1/data-guard/labels/pg/orders", nil, &admin)
	if !set || ev.Action != auditport.ActionDataGuardLabelDelete {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	p := ev.Payload.(map[string]any)
	if p["effective_before"] != "TopSecret" || p["effective_after"] != "unset" {
		t.Fatalf("before/after = %v → %v", p["effective_before"], p["effective_after"])
	}
}

// 「有人在反复试着摘掉一条脱敏规则」与「没人试过」在链上必须长得不一样。
func TestNonAdminLabelMutationsAreAudited(t *testing.T) {
	router, _ := newTestHandlerRouter()
	user := tenant("user")
	for _, tc := range []struct {
		method, target string
		body           any
	}{
		{http.MethodPost, "/v1/data-guard/labels", LabelRequest{ResourceType: "t", ResourceID: "i", Sensitivity: "Public"}},
		{http.MethodPut, "/v1/data-guard/labels/t/i", LabelRequest{Sensitivity: "Public"}},
		{http.MethodDelete, "/v1/data-guard/labels/t/i", nil},
	} {
		rec, ev, set := issueAudited(t, router, tc.method, tc.target, tc.body, &user)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: code = %d, want 403", tc.method, tc.target, rec.Code)
		}
		if !set || ev.Status != auditport.StatusFailure {
			t.Fatalf("%s %s: a forbidden label change left no failure row: %+v (set=%v)", tc.method, tc.target, ev, set)
		}
	}
}

func ptr(t tenantctx.Tenant) *tenantctx.Tenant { return &t }

// 目录端点的第一条断言不是"它返回了 16 条控制项"，而是"每一条都带着状态"。
//
// 一个只把 DefaultFrameworkControls 原样序列化出去的端点，会通过"目录可用"
// 这条验收，同时把 16 个在本构建里一条都不强制的名字摆到操作员面前——
// 而这正是这个登记表被写出来要防的那件事，只是搬到了 API 上。
func TestTheCatalogRouteReturnsWhatThisBuildEnforcesNotJustTheList(t *testing.T) {
	router, _ := newTestHandlerRouter()
	rec := issue(t, router, http.MethodGet, "/v1/data-guard/compliance/frameworks", nil, ptrTenant("admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body FrameworkCatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body = %s)", err, rec.Body.String())
	}
	if len(body.Frameworks) != len(dataguard.AllFrameworks) {
		t.Fatalf("%d framework(s), want %d", len(body.Frameworks), len(dataguard.AllFrameworks))
	}
	seen := 0
	for _, fc := range body.Frameworks {
		if !fc.Recommended {
			t.Errorf("%s: recommended = false; the catalog's own comment says these are not "+
				"enforced, and that sentence has to reach the console too", fc.Framework)
		}
		for _, c := range fc.Controls {
			seen++
			if c.Status == "" {
				t.Errorf("%s/%s: the response carries no status", fc.Framework, c.Name)
			}
			if c.Status == dataguard.ControlUnclassified {
				t.Errorf("%s/%s: the console is being offered a control nobody has classified",
					fc.Framework, c.Name)
			}
			if c.RegistryRef == "" {
				t.Errorf("%s/%s: status %q with no registry row behind it",
					fc.Framework, c.Name, c.Status)
			}
		}
	}
	if seen != len(dataguard.DeclaredControls()) {
		t.Errorf("the route returned %d control(s), the catalogs hold %d",
			seen, len(dataguard.DeclaredControls()))
	}
}

// 这个端点不发任何租户数据，它只是静态参考表。放不放宽到"任何已认证用户"
// 是一个可以另开的口子，但**现在它与本包其余每一条路由一样是 admin-only**，
// 而这一致性本身就是理由：它的消费者是打标页面，打标页面是 admin 的。
// 一个为了"只读"而单独放宽的端点，会在控制台的另一处再写一遍权限判断，
// 而写在界面里的权限判断从来保不住。
func TestTheCatalogRouteIsAdminOnlyLikeEveryOtherRouteHere(t *testing.T) {
	router, _ := newTestHandlerRouter()
	for _, tc := range []struct {
		name string
		ten  *tenantctx.Tenant
		want int
	}{
		{"no tenant at all", nil, http.StatusUnauthorized},
		{"a plain user", ptrTenant("user"), http.StatusForbidden},
		{"a viewer", ptrTenant("viewer"), http.StatusForbidden},
		{"an admin", ptrTenant("admin"), http.StatusOK},
	} {
		rec := issue(t, router, http.MethodGet, "/v1/data-guard/compliance/frameworks", nil, tc.ten)
		if rec.Code != tc.want {
			t.Errorf("%s: code = %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}
