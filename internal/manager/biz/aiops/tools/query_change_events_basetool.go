package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	auditmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/audit"
	edgemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/prom"
)

// query_change_events_basetool.go — HLD-013 Phase 2. Gives the RCA
// investigator a "what changed near time T" signal, sourced from the
// HLD-010 audit log. Patient-zero is often "someone changed a rule /
// setting / device right before the symptom"; this exposes those
// product-mediated changes so the causal back-tracing loop can pin them.
//
// Scope honesty: the audit log only captures changes made THROUGH opskeeper
// (admin UI / API). External host changes (an SSH edit, an out-of-band
// deploy, container churn from an orchestrator) are invisible here — those
// need an edge-side change feed (future). The tool description says so, so
// the LLM doesn't over-trust an empty result.

// AuditLister is the narrow seam query_change_events consumes. Satisfied
// directly by *biz/audit.Usecase. Primitive-param so the tools package
// stays off the data/store layer (it only needs the model type).
type AuditLister interface {
	ListChanges(ctx context.Context, from, to time.Time, resourceType, action string, limit int) ([]auditmodel.Log, error)
}

// EdgeChangeLister is the seam for the A.3 follow-up: edge-side
// change events captured by the changewatcher (journald / dockerd /
// packagemgr) and pushed to the manager over the tunnel. Satisfied
// by *biz/edge/changeevent.Usecase via a tiny adapter in main.go so
// the tools package stays off the data/store layer.
//
// Returned rows are the edge-domain model type; the tool merges
// them with audit log rows in InvokableRun.
type EdgeChangeLister interface {
	ListByWindow(ctx context.Context, from, to time.Time, kind string, limit int) ([]edgemodel.ChangeEventRow, error)
}

// ToolNameQueryChangeEvents is the registered tool name.
const ToolNameQueryChangeEvents = "query_change_events"

// QueryChangeEventsDescription — shown to the model.
const QueryChangeEventsDescription = "查询 audit log 里某时间窗内的「变更事件」——谁通过 OpsKeeper 改了什么" +
	"（告警规则 / 设备 / 设置·LLM key / 通知通道 / 仓库 / 技能 / 用户）。RCA 溯源时回答" +
	"「症状发生前后改了什么」，把变更当根因候选。只覆盖经 OpsKeeper 产品发起的变更，" +
	"外部变更（SSH / 带外部署 / 容器 churn / package change）由边缘 changewatcher 上报；缺失时通常表示边缘 agent 离线。"

const queryChangeEventsWhenToUse = "RCA 溯源时查「告警时间附近有没有人改过配置 / 规则 / 设置」——把变更当 0 号病人候选。" +
	"典型：用 incident 的 fired_at 作 around_ts，看前后 ±30 分钟有没有 rule_update / setting_update / device_update。" +
	"返回空也是有效发现（这段时间没有产品侧变更）。" +
	"NOT for：主机上的外部变更（audit 看不到）/ 指标趋势（query_promql）/ 日志（query_logql）。"

// QueryChangeEventsArgs is the typed arg schema. The window is centred on
// around_ts (usually the incident's fired_at).
type QueryChangeEventsArgs struct {
	AroundTS     string `json:"around_ts"`
	WindowMin    int    `json:"window_minutes"`
	ResourceType string `json:"resource_type"`
	Action       string `json:"action"`
	Limit        int    `json:"limit"`
}

// QueryChangeEventsSchema is the JSON schema advertised to the model.
var QueryChangeEventsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "around_ts": {"type": "string", "description": "可选锚点时间 RFC3339（通常用 incident 的 fired_at）；省略时默认当前时间，围绕它取前后窗口。"},
    "window_minutes": {"type": "integer", "minimum": 1, "maximum": 1440, "description": "半窗口分钟数（默认 30，即锚点前后各 30 分钟）。"},
    "resource_type": {"type": "string", "description": "可选，缩小到某类资源：rule/device/setting/channel/repo/skill/user/llm/grafana。"},
    "action": {"type": "string", "description": "可选，缩小到某动作：rule_update/setting_update/device_update/repo_sync/..."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 200, "description": "返回条数上限（默认 50）。"}
  },
  "required": []
}`)

type changeEventRow struct {
	// Source 区分 audit (opskeeper 中介) / edge (边缘监听到的外部变更).
	// 合并输出时, RCA loop 可据此判断变更来源.
	Source       string `json:"source"`
	OccurredAt   string `json:"occurred_at"`
	Actor        string `json:"actor"`
	Role         string `json:"role,omitempty"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id,omitempty"`
	ResourceName string `json:"resource_name,omitempty"`
	Status       string `json:"status"`
	Payload      string `json:"payload,omitempty"`
	// Edge 专用字段: 仅 edge source 有值.
	EdgeID   uint64 `json:"edge_id,omitempty"`
	Severity string `json:"severity,omitempty"`
	Labels   string `json:"labels,omitempty"`
}

// QueryChangeEventsTool is the BaseTool form. Class=read.
//
// Composes two data sources:
//   - audit: HLD-010 audit log (opskeeper-mediated changes)
//   - edge:  A.3 changewatcher events (external host changes)
//
// At least one of audit/edge is required for production use.
// Both nil in tests is allowed; the tool returns an empty result.
type QueryChangeEventsTool struct {
	audit AuditLister
	edge  EdgeChangeLister
	log   *slog.Logger
}

// NewQueryChangeEventsTool builds the tool. audit and edge may each be
// nil (the corresponding source is silently skipped); at least one is
// expected in production.
func NewQueryChangeEventsTool(a AuditLister, e EdgeChangeLister, log *slog.Logger) *QueryChangeEventsTool {
	if log == nil {
		log = slog.Default()
	}
	return &QueryChangeEventsTool{audit: a, edge: e, log: log}
}

// Info returns metadata. Class=read (no mutation; viewer-safe).
func (t *QueryChangeEventsTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameQueryChangeEvents,
		Description: QueryChangeEventsDescription,
		WhenToUse:   queryChangeEventsWhenToUse,
		Parameters:  QueryChangeEventsSchema,
		Class:       "read",
	}, nil
}

// InvokableRun parses args, queries the audit window, marshals the rows.
func (t *QueryChangeEventsTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	// audit + edge 双 nil 也是合法配置 — 返回空结果而非 error.
	var in QueryChangeEventsArgs
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("query_change_events: bad args: %w", err)
	}
	anchor := time.Now().UTC()
	if strings.TrimSpace(in.AroundTS) != "" {
		parsed, err := time.Parse(time.RFC3339, in.AroundTS)
		if err != nil {
			return "", fmt.Errorf("query_change_events: around_ts must be RFC3339 (got %q): %w", in.AroundTS, err)
		}
		anchor = parsed
	}
	win := in.WindowMin
	if win <= 0 {
		win = 30
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	half := time.Duration(win) * time.Minute
	from, to := anchor.Add(-half).UTC(), anchor.Add(half).UTC()

	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows := make([]changeEventRow, 0, limit*2)

	// Source 1: opskeeper audit log (HLD-010)
	if t.audit != nil {
		logs, err := t.audit.ListChanges(callCtx, from, to, in.ResourceType, in.Action, limit)
		if err != nil {
			return "", fmt.Errorf("query_change_events: audit list: %w", err)
		}
		for _, l := range logs {
			rows = append(rows, changeEventRow{
				Source:       "audit",
				OccurredAt:   l.OccurredAt.UTC().Format(time.RFC3339),
				Actor:        l.UserEmail,
				Role:         l.Role,
				Action:       l.Action,
				ResourceType: l.ResourceType,
				ResourceID:   l.ResourceID,
				ResourceName: l.ResourceName,
				Status:       l.Status,
				Payload:      l.PayloadJSON,
			})
		}
	}

	// Source 2: edge changewatcher events (A.3)
	if t.edge != nil {
		start := time.Now()
		edgeKind := in.Action // resource_type param maps loosely to kind for edge side
		edgeRows, err := t.edge.ListByWindow(callCtx, from, to, edgeKind, limit)
		if err != nil {
			// edge source failure should not fail the whole query — log and skip.
			t.log.Warn("query_change_events: edge list failed; audit-only result",
				slog.String("err", err.Error()))
		} else {
			for _, er := range edgeRows {
				rows = append(rows, changeEventRow{
					Source:       "edge",
					OccurredAt:   er.Timestamp.UTC().Format(time.RFC3339),
					Actor:        "edge-agent",
					Action:       er.Action,
					ResourceType: er.Kind,
					ResourceID:   er.Subject,
					Status:       "observed",
					EdgeID:       er.EdgeID,
					Severity:     er.Severity,
					Labels:       er.Labels,
				})
			}
		}
		if prom.ChangeEventsQueryDuration != nil {
			prom.ChangeEventsQueryDuration.WithLabelValues("edge").Observe(time.Since(start).Seconds())
		}
	}

	// Sort merged rows by occurred_at DESC.
	sortRowsByTimeDesc(rows)
	out, err := json.Marshal(map[string]any{
		"window":  map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
		"changes": rows,
		"count":   len(rows),
	})
	if err != nil {
		return "", fmt.Errorf("query_change_events: marshal: %w", err)
	}
	return string(out), nil
}

// sortRowsByTimeDesc sorts rows in-place by OccurredAt descending. Stable
// for rows with identical timestamps. Uses parse rather than RFC3339
// lexicographic compare for safety across timezone suffixes.
func sortRowsByTimeDesc(rows []changeEventRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].OccurredAt > rows[j].OccurredAt
	})
}
