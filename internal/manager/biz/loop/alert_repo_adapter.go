// chatdiagnose/loop/alert_repo_adapter.go — AlertRepository narrow adapter
// 把 managerbizalert.Repo 适配到 loop.AlertRepository 接口。
//
// 设计：
//   - 复用现有 alert.Repo（main.go:947 构造）
//   - Pragmatic Day 5+：用 time-based fallback（FindByLabelsetkey 走 ListIncidents{Status="open", Since=since}）
//   - 失败 slog warn + 继续（与 chatruntime-kb 同模式）
//
// 已知 limitation：
//   - alert incident 表无 labelsetkey 列；用 Rule 字段作为 labelsetkey proxy
//   - Day 5+ 增量 migration 加 labelsetkey 列 + 索引，改为精确匹配

package loop

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	manberalbizalert "github.com/vincent-wuhan/opskeeper/internal/manager/biz/alert"
	alertmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
)

// AlertRepoAdapter 把 manberalbizalert.Repo 适配到 loop.AlertRepository。
type AlertRepoAdapter struct {
	repo manberalbizalert.Repo
	log  *slog.Logger
}

// NewAlertRepoAdapter 构造。repo 不得为 nil。
func NewAlertRepoAdapter(repo manberalbizalert.Repo, log *slog.Logger) *AlertRepoAdapter {
	if repo == nil {
		panic("loop: NewAlertRepoAdapter: repo is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &AlertRepoAdapter{repo: repo, log: log.With(slog.String("comp", "loop.alert_repo_adapter"))}
}

// Compile-time interface satisfaction check.
var _ AlertRepository = (*AlertRepoAdapter)(nil)

// FindByLabelsetkey 实现 AlertRepository 接口。
//
// Pragmatic 实现：
//   - key 不为空时按 RuleKey 过滤（alert incident.Rule 字段作为 labelsetkey proxy）
//   - key 为空时拉最近 firing/open incidents
//   - 拉 Limit=100，按 UpdatedAt DESC（依赖 ListIncidents 默认排序）
//
// 失败语义：slog warn + 返回 nil（KB 风格：不阻塞 correlated worker）
func (a *AlertRepoAdapter) FindByLabelsetkey(ctx context.Context, key string, since time.Time) ([]DetectionEvent, error) {
	if since.IsZero() {
		since = time.Now().UTC().Add(-24 * time.Hour)
	}

	filter := manberalbizalert.IncidentFilter{
		Limit: 100,
	}
	if key != "" {
		filter.RuleKey = key
	}
	// Status 留空 → ListIncidents 默认不过滤；Day 5+ 加 "firing" 状态精确匹配。

	incidents, err := a.repo.ListIncidents(ctx, filter)
	if err != nil {
		a.log.Warn("alert_repo_adapter: ListIncidents failed (non-fatal)",
			slog.String("labelsetkey", key),
			slog.Any("err", err))
		return nil, nil
	}

	events := make([]DetectionEvent, 0, len(incidents))
	for _, inc := range incidents {
		if inc == nil {
			continue
		}
		if inc.UpdatedAt.Before(since) {
			continue
		}
		events = append(events, incidentToDetectionEvent(inc))
	}
	return events, nil
}

// incidentToDetectionEvent 把 alert.Incident 转 loop.DetectionEvent。
// 简化映射：Rule 作为 labelsetkey、DeviceID 推断 resource_type、Severity 直接透传。
func incidentToDetectionEvent(inc *alertmodel.Incident) DetectionEvent {
	return DetectionEvent{
		AlertID:     strconv.FormatInt(int64(inc.ID), 10),
		Severity:    inc.Severity,
		Resource:    resourceFromIncident(inc),
		LabelSetKey: inc.Rule,
		DetectedAt:  inc.FirstFiredAt,
	}
}

// resourceFromIncident 简化映射：
//   - Scope=="host" → "host"
//   - Scope=="app" → "app"
//   - Scope=="pg/redis/k8s/mq" → Scope
//   - 默认 → "unknown"
func resourceFromIncident(inc *alertmodel.Incident) string {
	switch inc.Scope {
	case "host", "app", "pg", "redis", "k8s", "mq":
		return inc.Scope
	default:
		return "unknown"
	}
}
