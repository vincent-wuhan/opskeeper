package alert

// aggregation.go — 告警聚合降噪（BENY-915）。
//
// 缺口：dedupe 只按 规则×设备 去重，同根因不同规则的告警风暴
// （CPU/延迟/错误率各红一遍）各自成 incident，值班被淹。
//
// 方案（规则层聚合，LLM 语义归并由 investigator 多假设承担）：
//   - 新 incident 创建时，若同设备存在 aggWindow（默认 10min）内
//     的既有聚合组，加入该组；否则自成一个新组（组 ID = 自身 ID）。
//   - 组是"同一件事"的呈现单位：通知/驾驶舱/上行契约按组折叠，
//     组内成员作为多源证据喂给多假设调查。

import (
	"context"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
)

// AggregationRepo 聚合所需的最窄持久化接口（测试可轻量实现）。
type AggregationRepo interface {
	ListIncidents(ctx context.Context, filter IncidentFilter) ([]*model.Incident, error)
	UpdateIncidentAggGroup(ctx context.Context, id, groupID uint64) error
}

const aggWindow = 10 * time.Minute

// AssignAggregation 把新创建的 incident 归入聚合组并返回组 ID。
// 幂等：已有组直接返回。
func AssignAggregation(ctx context.Context, repo AggregationRepo, inc *model.Incident, now time.Time) (uint64, error) {
	if inc == nil || inc.DeviceID == nil {
		return inc.ID, nil
	}
	if inc.AggGroupID != nil {
		return *inc.AggGroupID, nil
	}
	// 找同设备 open 且已归组、窗口内活跃的其他 incident
	others, err := repo.ListIncidents(ctx, IncidentFilter{
		Status:   model.IncidentStatusOpen,
		DeviceID: inc.DeviceID,
		Limit:    100,
	})
	if err != nil {
		return inc.ID, err
	}
	var groupID uint64
	for _, o := range others {
		if o.ID == inc.ID || o.AggGroupID == nil {
			continue
		}
		if now.Sub(o.LastFiredAt) > aggWindow {
			continue
		}
		if groupID == 0 || *o.AggGroupID < groupID {
			groupID = *o.AggGroupID // 取最早组
		}
	}
	if groupID == 0 {
		groupID = inc.ID // 自成新组
	}
	g := groupID
	inc.AggGroupID = &g
	if err := repo.UpdateIncidentAggGroup(ctx, inc.ID, g); err != nil {
		return g, err
	}
	return g, nil
}

// AggregationGroup 一个聚合组的折叠视图。
type AggregationGroup struct {
	GroupID      uint64
	LeadTitle    string
	Severity     string // 组内最高
	Members      []AggregationMember
	FirstFiredAt time.Time
	LastFiredAt  time.Time
}

type AggregationMember struct {
	IncidentID uint64
	Rule       string
	RuleName   string
	Severity   string
	EventCount uint64
	Summary    string
}

// BuildAggregationGroup 从同设备 incident 列表折叠一个组（组内成员 =
// 同 AggGroupID）。列表为空返回 nil。
func BuildAggregationGroup(incidents []*model.Incident) *AggregationGroup {
	var g *AggregationGroup
	sevRank := map[string]int{"critical": 3, "warning": 2, "info": 1}
	for _, inc := range incidents {
		if inc.AggGroupID == nil {
			continue
		}
		if g == nil {
			g = &AggregationGroup{GroupID: *inc.AggGroupID, Severity: inc.Severity, FirstFiredAt: inc.FirstFiredAt, LeadTitle: inc.Title}
		}
		g.Members = append(g.Members, AggregationMember{
			IncidentID: inc.ID, Rule: inc.Rule, RuleName: inc.RuleName,
			Severity: inc.Severity, EventCount: inc.EventCount, Summary: inc.Summary,
		})
		if inc.FirstFiredAt.Before(g.FirstFiredAt) {
			g.FirstFiredAt = inc.FirstFiredAt
			g.LeadTitle = inc.Title // 最早者为 lead
		}
		if inc.LastFiredAt.After(g.LastFiredAt) {
			g.LastFiredAt = inc.LastFiredAt
		}
		if sevRank[inc.Severity] > sevRank[g.Severity] {
			g.Severity = inc.Severity
		}
	}
	return g
}
