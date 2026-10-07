package loop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// stubAlertRepo 是 AlertReader 的最小内存实现：只有 ListIncidents 被注入
// 行为，另外两个方法返回零值（AlertRepoAdapter 不调用，不会触达）。
//
// 它实现的是 loop 自己的端口而不是 alert 的仓库接口——这是决策 279 之后
// 这些测试能留在本包的原因：端口在本包，替身就在本包，而替身与生产
// 转换（cmd/opskeeper/loop_alert_wiring.go）之间的距离由那边的测试负责。
type stubAlertRepo struct {
	listIncidents func(ctx context.Context, filter AlertIncidentFilter) ([]*AlertIncident, error)
}

func (s *stubAlertRepo) GetIncidentByID(context.Context, uint64) (*AlertIncident, error) {
	return nil, nil
}

func (s *stubAlertRepo) ListIncidents(ctx context.Context, filter AlertIncidentFilter) ([]*AlertIncident, error) {
	if s.listIncidents == nil {
		return nil, nil
	}
	return s.listIncidents(ctx, filter)
}

func (s *stubAlertRepo) GetRuleByID(context.Context, uint64) (*AlertRule, error) {
	return nil, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// 1. 空 key + since 兜底：返回最近 24h 内 incident → 全部映成 DetectionEvent
func TestAlertRepoAdapter_FindByLabelsetkey_DefaultSince(t *testing.T) {
	now := time.Now().UTC()
	repo := &stubAlertRepo{
		listIncidents: func(_ context.Context, _ AlertIncidentFilter) ([]*AlertIncident, error) {
			return []*AlertIncident{
				{ID: 1, Rule: "host.cpu", Scope: "host", Severity: "warning", FirstFiredAt: now, UpdatedAt: now},
				{ID: 2, Rule: "host.disk", Scope: "host", Severity: "critical", FirstFiredAt: now, UpdatedAt: now},
			}, nil
		},
	}
	a := NewAlertRepoAdapter(repo, discardLogger())
	got, err := a.FindByLabelsetkey(context.Background(), "", time.Time{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	if got[0].LabelSetKey != "host.cpu" {
		t.Errorf("event[0].LabelSetKey = %q, want host.cpu", got[0].LabelSetKey)
	}
	if got[0].Resource != "host" {
		t.Errorf("event[0].Resource = %q, want host", got[0].Resource)
	}
}

// 2. 显式 since 过滤掉 UpdatedAt < since 的 incident
func TestAlertRepoAdapter_FindByLabelsetkey_SinceFilter(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	repo := &stubAlertRepo{
		listIncidents: func(_ context.Context, _ AlertIncidentFilter) ([]*AlertIncident, error) {
			return []*AlertIncident{
				{ID: 1, Rule: "r1", Scope: "app", Severity: "warning", FirstFiredAt: old, UpdatedAt: old},
				{ID: 2, Rule: "r2", Scope: "app", Severity: "warning", FirstFiredAt: now, UpdatedAt: now},
			}, nil
		},
	}
	a := NewAlertRepoAdapter(repo, discardLogger())
	since := now.Add(-30 * time.Minute)
	got, err := a.FindByLabelsetkey(context.Background(), "", since)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 (since filtered), got %d", len(got))
	}
	if got[0].AlertID != "2" {
		t.Errorf("AlertID = %q, want 2", got[0].AlertID)
	}
}

// 3. labelsetkey 非空 → 透传给 filter.RuleKey
func TestAlertRepoAdapter_FindByLabelsetkey_KeyPropagated(t *testing.T) {
	var captured AlertIncidentFilter
	repo := &stubAlertRepo{
		listIncidents: func(_ context.Context, f AlertIncidentFilter) ([]*AlertIncident, error) {
			captured = f
			return nil, nil
		},
	}
	a := NewAlertRepoAdapter(repo, discardLogger())
	_, err := a.FindByLabelsetkey(context.Background(), "pg.long_tx", time.Now())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if captured.RuleKey != "pg.long_tx" {
		t.Errorf("filter.RuleKey = %q, want pg.long_tx", captured.RuleKey)
	}
	if captured.Limit != 100 {
		t.Errorf("filter.Limit = %d, want 100", captured.Limit)
	}
}

// 4. ListIncidents 返回 error → slog warn + 返回 nil, nil（KB 风格：不阻塞 correlated worker）
func TestAlertRepoAdapter_FindByLabelsetkey_ListError(t *testing.T) {
	repo := &stubAlertRepo{
		listIncidents: func(_ context.Context, _ AlertIncidentFilter) ([]*AlertIncident, error) {
			return nil, errors.New("synthetic db error")
		},
	}
	a := NewAlertRepoAdapter(repo, discardLogger())
	got, err := a.FindByLabelsetkey(context.Background(), "k", time.Time{})
	if err != nil {
		t.Fatalf("want non-fatal, got err: %v", err)
	}
	if got != nil {
		t.Errorf("want nil slice on error, got %+v", got)
	}
}

// 5. resourceFromIncident 边界 scope 全部覆盖
func TestAlertRepoAdapter_resourceFromIncident(t *testing.T) {
	cases := []struct {
		scope string
		want  string
	}{
		{"host", "host"},
		{"app", "app"},
		{"pg", "pg"},
		{"redis", "redis"},
		{"k8s", "k8s"},
		{"mq", "mq"},
		{"unknown-scope", "unknown"},
		{"", "unknown"},
	}
	for _, tc := range cases {
		inc := &AlertIncident{Scope: tc.scope}
		if got := resourceFromIncident(inc); got != tc.want {
			t.Errorf("scope=%q → %q, want %q", tc.scope, got, tc.want)
		}
	}
}

// 6. nil repo 构造 panic（接口契约：fail fast）
func TestAlertRepoAdapter_NilRepoPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("want panic on nil repo")
		}
	}()
	_ = NewAlertRepoAdapter(nil, discardLogger())
}
