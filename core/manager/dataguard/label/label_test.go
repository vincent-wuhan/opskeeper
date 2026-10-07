package label

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard/heuristic"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard/store"
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

// 审批行上带的是一个裸 id，而标签是按 (类型, id) 存的。这三个用例就是
// StrictestForResourceID 存在的理由：没有类型可用时，"最严格"是唯一安全的答案。
func TestStrictestForResourceID_TakesTheStrictestAcrossResourceTypes(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())
	ctx := context.Background()

	// 撞名：pod 与 service 用了同一个 id，跨类型撞上了。
	if err := repo.Create(ctx, &store.DataSensitivityLabel{
		ResourceType: "pod", ResourceID: "web-1", Sensitivity: "Public",
		LabelSource: string(store.SourceManual), Confidence: 1.0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, &store.DataSensitivityLabel{
		ResourceType: "service", ResourceID: "web-1", Sensitivity: "Restricted",
		LabelSource: string(store.SourceManual), Confidence: 1.0,
	}); err != nil {
		t.Fatal(err)
	}

	s, ok, err := m.StrictestForResourceID(ctx, "web-1")
	if err != nil || !ok {
		t.Fatalf("StrictestForResourceID: s=%q ok=%v err=%v", s, ok, err)
	}
	if s != dataguard.Restricted {
		t.Errorf("撞名时取了 %q，应取最严的 Restricted", s)
	}
}

// 一条不够自信的启发式标签不能让一次正常发布变成双签——这与 ResolveEffective
// 是同一套门槛，跨类型版本也不能松。
func TestStrictestForResourceID_AnUnconfidentHeuristicDoesNotCount(t *testing.T) {
	repo := newFakeRepo()
	m := NewLabelManager(repo, nil, heuristic.NewCompositeEngine(), silentLogger())
	ctx := context.Background()

	if err := repo.Create(ctx, &store.DataSensitivityLabel{
		ResourceType: "pod", ResourceID: "web-1", Sensitivity: "Public",
		LabelSource: string(store.SourceManual), Confidence: 1.0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, &store.DataSensitivityLabel{
		ResourceType: "secret", ResourceID: "web-1", Sensitivity: "TopSecret",
		LabelSource: string(store.SourceHeuristic), Confidence: 0.4,
	}); err != nil {
		t.Fatal(err)
	}

	s, ok, err := m.StrictestForResourceID(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || s != dataguard.Public {
		t.Errorf("0.4 的启发式 TopSecret 不该算数：s=%q ok=%v", s, ok)
	}
}

// 没找到就没找到。调用方知道自己的类别，不该由这一层替它猜一个。
func TestStrictestForResourceID_UnlabelledIsNotAGuess(t *testing.T) {
	m := NewLabelManager(newFakeRepo(), nil, heuristic.NewCompositeEngine(), silentLogger())

	for _, tc := range []struct{ name, id string }{
		{"完全没有标签", "never-seen"},
		{"空 id", "   "},
	} {
		s, ok, err := m.StrictestForResourceID(context.Background(), tc.id)
		if err != nil || ok || s != "" {
			t.Errorf("%s：应返回未找到，拿到 s=%q ok=%v err=%v", tc.name, s, ok, err)
		}
	}
}

// TestTheComplianceTagColumnRoundTrips 钉住写进这一列的形状与读它用的形状
// 是同一个。
//
// 这不是一条"测一下编解码能用"的用例。它的存在理由是一次真缺陷：写的是
// `[]string`（框架名），读的是 `[]ComplianceTag`（framework + controls +
// enforced），`json.Unmarshal` 每次都失败，而调用处写的是 `tags, _ :=`。
// 于是这列**永远读回空标签，而没有任何地方报错**——一个只测编解码各自能用的
// 测试会把那两半各自钉绿，正好放它过去。
//
// 所以断言的是往返，且断言的是**读回来的东西等于写进去的东西**，不是两个函数
// 各自的返回值。
func TestTheComplianceTagColumnRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []string
	}{
		{name: "nil", tags: nil},
		{name: "empty", tags: []string{}},
		{name: "one", tags: []string{"PCI-DSS"}},
		{name: "several", tags: []string{"PCI-DSS", "GDPR", "SOC2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := EncodeJSONTags(tc.tags)
			if err != nil {
				t.Fatalf("EncodeJSONTags(%v): %v", tc.tags, err)
			}
			decoded, err := DecodeJSONTags(encoded)
			if err != nil {
				t.Fatalf("DecodeJSONTags(%q): %v", encoded, err)
			}
			if len(decoded) != len(tc.tags) {
				t.Fatalf("round trip changed the length: wrote %d tags (%q), read %d (%q)",
					len(tc.tags), encoded, len(decoded), decoded)
			}
			for i := range tc.tags {
				if decoded[i] != tc.tags[i] {
					t.Errorf("round trip changed tag %d: wrote %q, read %q",
						i, tc.tags[i], decoded[i])
				}
			}
		})
	}
}

// TestDecodeJSONTagsReportsAMalformedColumn 是上面那条的另一半，也是当初缺的那一半。
//
// 那一列在损坏时读回空值，调用处又丢掉错误，于是"标签没了"和"这列没有标签"
// 从外面看一模一样。解析失败必须**说出来**：宁可让读标签的请求失败，也不能让它
// 安静地返回一份没有标签的标签。
func TestDecodeJSONTagsReportsAMalformedColumn(t *testing.T) {
	// 这就是当初写进去的形状——另一个包会写的、合法的 JSON，但它不是这一列的形状。
	// 它必须被当成损坏，而不是被当成空。
	const wrongShape = `[{"framework":"PCI-DSS","controls":["mfa-on-write"]}]`
	if _, err := DecodeJSONTags(wrongShape); err == nil {
		t.Fatalf("DecodeJSONTags(%s) returned no error; a column written by the other "+
			"encoder reads back as \"no tags\" instead of as a broken column", wrongShape)
	}
	// 只有"没写过东西"不算坏。"没写过东西"和"写了但坏了"必须能分开——
	// 这正是当初那个缺陷藏身的地方。
	for _, raw := range []string{"  ", "\t\n", ""} {
		t.Run("empty:"+raw, func(t *testing.T) {
			got, err := DecodeJSONTags(raw)
			if err != nil {
				t.Errorf("DecodeJSONTags(%q) = %v; an empty column is not a broken column", raw, err)
			}
			if len(got) != 0 {
				t.Errorf("DecodeJSONTags(%q) = %q; an empty column holds no tags", raw, got)
			}
		})
	}
	for _, raw := range []string{"not json", `["unterminated`, `{}`, `"PCI-DSS"`} {
		t.Run("broken:"+raw, func(t *testing.T) {
			if _, err := DecodeJSONTags(raw); err == nil {
				t.Errorf("DecodeJSONTags(%q) returned no error; broken and empty must not "+
					"look alike from the outside", raw)
			}
		})
	}
}
