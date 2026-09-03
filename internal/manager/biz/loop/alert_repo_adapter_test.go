package loop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	alertbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/alert"
	alertmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
)

// stubAlertRepo 是 alertbiz.Repo 的最小内存实现：
//   - ListIncidents 由调用方注入行为
//   - 其他方法返回零值（AlertRepoAdapter 不调用，不会触达）
//
// 用途：让 AlertRepoAdapter 的测试可以只关心 ListIncidents 的入参/出参映射。
type stubAlertRepo struct {
	alertbiz.Repo // 嵌入以继承其他方法（panic-on-call 行为由 alert.Repo nil 检查兜底）
	listIncidents func(ctx context.Context, filter alertbiz.IncidentFilter) ([]*alertmodel.Incident, error)
}

func (s *stubAlertRepo) ListIncidents(ctx context.Context, filter alertbiz.IncidentFilter) ([]*alertmodel.Incident, error) {
	if s.listIncidents == nil {
		return nil, nil
	}
	return s.listIncidents(ctx, filter)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// 1. 空 key + since 兜底：返回最近 24h 内 incident → 全部映成 DetectionEvent
func TestAlertRepoAdapter_FindByLabelsetkey_DefaultSince(t *testing.T) {
	now := time.Now().UTC()
	repo := &stubAlertRepo{
		listIncidents: func(_ context.Context, _ alertbiz.IncidentFilter) ([]*alertmodel.Incident, error) {
			return []*alertmodel.Incident{
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
		listIncidents: func(_ context.Context, _ alertbiz.IncidentFilter) ([]*alertmodel.Incident, error) {
			return []*alertmodel.Incident{
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
	var captured alertbiz.IncidentFilter
	repo := &stubAlertRepo{
		listIncidents: func(_ context.Context, f alertbiz.IncidentFilter) ([]*alertmodel.Incident, error) {
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
		listIncidents: func(_ context.Context, _ alertbiz.IncidentFilter) ([]*alertmodel.Incident, error) {
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
		inc := &alertmodel.Incident{Scope: tc.scope}
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
