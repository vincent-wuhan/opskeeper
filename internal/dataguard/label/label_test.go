package label

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
	"github.com/vincent-wuhan/opskeeper/internal/dataguard/heuristic"
	"github.com/vincent-wuhan/opskeeper/internal/dataguard/store"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// fakeRepo 是 label.Repo 的 in-memory 实现。
type fakeRepo struct {
	labels map[string]*store.DataSensitivityLabel
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{labels: map[string]*store.DataSensitivityLabel{}}
}

func key(rt, rid string) string { return rt + "|" + rid }

func (r *fakeRepo) Create(_ context.Context, l *store.DataSensitivityLabel) error {
	r.labels[key(l.ResourceType, l.ResourceID)] = l
	return nil
}
func (r *fakeRepo) Get(_ context.Context, rt, rid string) (*store.DataSensitivityLabel, error) {
	l, ok := r.labels[key(rt, rid)]
	if !ok {
		return nil, errs.ErrNotFound
	}
	return l, nil
}
func (r *fakeRepo) List(_ context.Context, sens, src string, _, _ int) ([]*store.DataSensitivityLabel, int64, error) {
	out := make([]*store.DataSensitivityLabel, 0)
	for _, l := range r.labels {
		if sens != "" && l.Sensitivity != sens {
			continue
		}
		if src != "" && l.LabelSource != src {
			continue
		}
		out = append(out, l)
	}
	return out, int64(len(out)), nil
}
func (r *fakeRepo) Delete(_ context.Context, rt, rid string) error {
	if _, ok := r.labels[key(rt, rid)]; !ok {
		return errs.ErrNotFound
	}
	delete(r.labels, key(rt, rid))
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

// fakeResolver 按 type 规则返回父资源。
type fakeResolver struct{ parents map[string][]ParentRef }

func (f *fakeResolver) Parents(_ context.Context, rt, rid string) ([]ParentRef, error) {
	k := rt + "|" + rid
	if p, ok := f.parents[k]; ok {
		return p, nil
	}
	return nil, errors.New("no parent")
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ───────────────────────── Tests ─────────────────────────

func TestApplyHeuristic_AutoLabelHighConfidence(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())

	res := heuristic.Resource{Type: heuristic.ResourcePostgres, ID: "tbl_pii", Name: "user_pii"}
	l, err := m.ApplyHeuristic(context.Background(), res)
	if err != nil {
		t.Fatal(err)
	}
	if l == nil {
		t.Fatal("expected auto-label for high confidence")
	}
	if l.Sensitivity != string(dataguard.Confidential) {
		t.Errorf("sensitivity = %s, want Confidential", l.Sensitivity)
	}
	if l.LabelSource != string(store.SourceHeuristic) {
		t.Errorf("source = %s, want heuristic", l.LabelSource)
	}
	if l.Confidence != 0.85 {
		t.Errorf("confidence = %f, want 0.85", l.Confidence)
	}
}

func TestApplyHeuristic_TopSecretK8sSecret(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())

	res := heuristic.Resource{
		Type: heuristic.ResourceK8s, ID: "kube-system-tls",
		Name:  "tls-secret",
		Extra: map[string]string{"kind": "Secret"},
	}
	l, err := m.ApplyHeuristic(context.Background(), res)
	if err != nil || l == nil {
		t.Fatalf("expected K8s Secret to be auto-labeled: l=%v err=%v", l, err)
	}
	if l.Sensitivity != string(dataguard.TopSecret) {
		t.Errorf("sensitivity = %s, want TopSecret", l.Sensitivity)
	}
}

func TestApplyHeuristic_LowConfidenceSkips(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())

	// 没有对应规则时不应自动打标（无匹配 + 兜底 0.50 < 阈值）
	res := heuristic.Resource{Type: heuristic.ResourceRedis, ID: "generic", Name: "nohints"}
	l, err := m.ApplyHeuristic(context.Background(), res)
	if err != nil {
		t.Fatal(err)
	}
	if l != nil {
		t.Errorf("expected nil for no-match, got %+v", l)
	}
}

func TestApplyHeuristic_DoesNotOverwriteManualLabel(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())

	// 1) 人工先打 public
	if err := m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "orders", Sensitivity: "Public",
	}, "alice"); err != nil {
		t.Fatal(err)
	}

	// 2) 启发式判定 orders 应该是 confidential — 但不应覆盖人工
	res := heuristic.Resource{Type: heuristic.ResourcePostgres, ID: "orders", Name: "orders"}
	l, err := m.ApplyHeuristic(context.Background(), res)
	if err != nil {
		t.Fatal(err)
	}
	_ = l
	got, _ := m.Get(context.Background(), "pg", "orders")
	if got.LabelSource != string(store.SourceManual) {
		t.Errorf("source = %s, want manual", got.LabelSource)
	}
	if got.Sensitivity != "Public" {
		t.Errorf("sensitivity = %s, want Public", got.Sensitivity)
	}
}

func TestCreateManual_InvalidSensitivity(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())
	err := m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "x", Sensitivity: "TopSecret", // 合法
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	err = m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "y", Sensitivity: "bogus",
	}, "admin")
	if err == nil {
		t.Error("invalid sensitivity should error")
	}
}

func TestUpdateOverride_RecordsPrevious(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())

	if err := m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "orders", Sensitivity: "Public",
	}, "alice"); err != nil {
		t.Fatal(err)
	}
	l, err := m.UpdateOverride(context.Background(), "pg", "orders", dataguard.Restricted, "bob", "compliance review")
	if err != nil {
		t.Fatal(err)
	}
	if l.LabelSource != string(store.SourceOverride) {
		t.Errorf("source = %s, want override", l.LabelSource)
	}
	if !contains(l.Notes, "override_of=Public") || !contains(l.Notes, "compliance review") {
		t.Errorf("notes should record previous + reason, got: %q", l.Notes)
	}
}

func TestResolveEffective_PreferredOrder(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())

	// 自身 manual → 用自身
	_ = m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "t", Sensitivity: "Confidential",
	}, "alice")
	s, conf, via, err := m.ResolveEffective(context.Background(), "pg", "t")
	if err != nil || s != dataguard.Confidential || conf != 1.0 || via {
		t.Errorf("manual not preferred: s=%s conf=%f via=%v err=%v", s, conf, via, err)
	}

	// 自身 heuristic 0.95 → 用自身
	if _, err := m.ApplyHeuristic(context.Background(), heuristic.Resource{
		Type: heuristic.ResourcePostgres, ID: "col_id_card",
		Name: "users", Extra: map[string]string{"column": "id_card"},
	}); err != nil {
		t.Fatal(err)
	}
	s, conf, via, _ = m.ResolveEffective(context.Background(), "pg", "col_id_card")
	if s != dataguard.Restricted || conf != 0.95 || via {
		t.Errorf("heuristic high confidence not used: s=%s conf=%f via=%v", s, conf, via)
	}

	// 自身 heuristic 0.80 < 0.85 → 不应直接用 → 应默认 Internal
	if _, err := m.ApplyHeuristic(context.Background(), heuristic.Resource{
		Type: heuristic.ResourceRedis, ID: "low_conf_key", Name: "random:key",
	}); err != nil {
		t.Fatal(err)
	}
	s, conf, via, _ = m.ResolveEffective(context.Background(), "redis", "low_conf_key")
	if s != dataguard.Internal || conf != 0.50 || via {
		t.Errorf("low confidence should fallback to Internal default: s=%s conf=%f via=%v", s, conf, via)
	}
}

func TestResolveEffective_InheritanceFromParent(t *testing.T) {
	repo := newFakeRepo()
	resolver := &fakeResolver{parents: map[string][]ParentRef{
		"pg|tbl_users": {{Type: "pg", ID: "db_main"}},
	}}
	m := NewLabelManager(repo, resolver, heuristic.NewCompositeEngine(), silentLogger())

	// 父资源 confidential (manual)
	_ = m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "db_main", Sensitivity: "Confidential",
	}, "alice")

	// 子资源没有显式标签
	s, conf, via, err := m.ResolveEffective(context.Background(), "pg", "tbl_users")
	if err != nil {
		t.Fatal(err)
	}
	if s != dataguard.Confidential || conf != 1.0 || !via {
		t.Errorf("inherit failed: s=%s conf=%f via=%v", s, conf, via)
	}
}

func TestResolveEffective_OverrideBeatsParent(t *testing.T) {
	repo := newFakeRepo()
	resolver := &fakeResolver{parents: map[string][]ParentRef{
		"pg|col_ssn": {{Type: "pg", ID: "db_main"}},
	}}
	m := NewLabelManager(repo, resolver, heuristic.NewCompositeEngine(), silentLogger())

	// 父资源 confidential
	_ = m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "db_main", Sensitivity: "Confidential",
	}, "alice")
	// 子资源 override 为 restricted（更敏感）
	if _, err := m.UpdateOverride(context.Background(), "pg", "col_ssn", dataguard.Restricted, "bob", "PII"); err != nil {
		t.Fatal(err)
	}

	// override 应该胜出（不是从父继承）
	s, conf, via, _ := m.ResolveEffective(context.Background(), "pg", "col_ssn")
	if s != datadog_Restricted || conf != 1.0 || via {
		t.Errorf("override should win: s=%s conf=%f via=%v", s, conf, via)
	}
}

func TestResolveEffective_NoLabelsDefaultsInternal(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())
	s, _, via, err := m.ResolveEffective(context.Background(), "pg", "untracked")
	if err != nil {
		t.Fatal(err)
	}
	if s != dataguard.Internal || via {
		t.Errorf("expected default Internal non-inherited, got s=%s via=%v", s, via)
	}
}

func TestInheritFromParent_DoesNotOverwriteManual(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())

	// 父 confidential
	_ = m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "db_main", Sensitivity: "Confidential",
	}, "alice")

	// 子 1：人工 public — 不应被继承覆盖
	_ = m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "tbl_users", Sensitivity: "Public",
	}, "alice")

	count, err := m.InheritFromParent(context.Background(), "pg", "db_main", "pg", []string{"tbl_users", "tbl_orders"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 inherited (tbl_orders only), got %d", count)
	}
	tbl_users, _ := m.Get(context.Background(), "pg", "tbl_users")
	if tbl_users.Sensitivity != "Public" {
		t.Errorf("tbl_users should remain Public, got %s", tbl_users.Sensitivity)
	}
	tbl_orders, _ := m.Get(context.Background(), "pg", "tbl_orders")
	if tbl_orders.Sensitivity != "Confidential" || tbl_orders.LabelSource != string(store.SourceInherited) {
		t.Errorf("tbl_orders inherit failed: %+v", tbl_orders)
	}
}

func TestList_FilterBySensitivityAndSource(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())

	_ = m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "a", Sensitivity: "Public",
	}, "alice")
	_ = m.CreateManual(context.Background(), &store.DataSensitivityLabel{
		ResourceType: "pg", ResourceID: "b", Sensitivity: "Confidential",
	}, "alice")

	items, total, err := m.List(context.Background(), "Public", "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 {
		t.Errorf("expected 1 Public, got %d", total)
	}
	items, total, _ = m.List(context.Background(), "", string(store.SourceManual), 100, 0)
	if total != 2 || len(items) != 2 {
		t.Errorf("expected 2 manual, got %d", total)
	}
}

func TestParseJSONTags(t *testing.T) {
	tags, err := ParseJSONTags(`["PCI-DSS","GDPR"]`)
	if err != nil || len(tags) != 2 || tags[0] != "PCI-DSS" {
		t.Errorf("ParseJSONTags err=%v tags=%v", err, tags)
	}
	if out, _ := EncodeJSONTags([]string{"PCI-DSS", "GDPR"}); out != `["PCI-DSS","GDPR"]` {
		t.Errorf("EncodeJSONTags = %s", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 修复 typo: datadog_Restricted → dataguard.Restricted
const datadog_Restricted = dataguard.Restricted
