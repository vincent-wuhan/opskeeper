// chatdiagnose/loop/investigator_toolset_adapter.go — InvestigatorToolset
// narrow adapter（pragmatic Day 5+）。
//
// 设计：
//   - Investigate：返回 1 条最小 evidence（resource_alert + alertID），让 LLM 有最小输入
//     真实 evidence 链路（query_metrics / query_logs / link_runtime_to_commit）
//     Day 5+ 接 internal/manager/biz/aiops/tools 三件套
//   - ListRemediations：5 个 resource_type 的静态映射（pg / redis / k8s / host / mq）
//     Day 5+ 替换为 internal/manager/biz/aiops/remediations/ 真策略
//
// 失败语义：slog warn + 返回空（KB 风格：不阻塞 investigated worker）

package loop

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// InvestigatorToolsetAdapter 是 loop.InvestigatorToolset 的 pragmatic 实现。
type InvestigatorToolsetAdapter struct {
	log *slog.Logger
}

// NewInvestigatorToolsetAdapter 构造。
func NewInvestigatorToolsetAdapter(log *slog.Logger) *InvestigatorToolsetAdapter {
	if log == nil {
		log = slog.Default()
	}
	return &InvestigatorToolsetAdapter{log: log.With(slog.String("comp", "loop.investigator_toolset"))}
}

// Compile-time interface satisfaction check.
var _ InvestigatorToolset = (*InvestigatorToolsetAdapter)(nil)

// Investigate 返回 1 条最小 evidence。
//
// 真实 evidence 链路（query_metrics / query_logs / link_runtime_to_commit）Day 5+ 接入。
func (i *InvestigatorToolsetAdapter) Investigate(ctx context.Context, resourceType, alertID string, timeWindow TimeWindow) ([]EvidenceItem, error) {
	if resourceType == "" {
		return nil, fmt.Errorf("loop: Investigate requires resourceType")
	}
	if alertID == "" {
		return nil, fmt.Errorf("loop: Investigate requires alertID")
	}
	return []EvidenceItem{
		{
			Tool:      "resource_alert",
			Query:     fmt.Sprintf("resource=%s alert_id=%s window=[%s, %s]", resourceType, alertID, timeWindow.Start.Format(time.RFC3339), timeWindow.End.Format(time.RFC3339)),
			Value:     alertID,
			Count:     1,
			Timestamp: timeWindow.End,
		},
	}, nil
}

// ListRemediations 静态映射 5 个 resource_type → 默认 remediation options。
//
// Day 5+：从 internal/manager/biz/aiops/remediations/ 真策略替换。
func (i *InvestigatorToolsetAdapter) ListRemediations(ctx context.Context, resourceType string) ([]RemediationOption, error) {
	if resourceType == "" {
		return nil, fmt.Errorf("loop: ListRemediations requires resourceType")
	}
	opts, ok := staticRemediations[resourceType]
	if !ok {
		i.log.Warn("investigator_toolset: unknown resource_type, returning empty",
			slog.String("resource_type", resourceType))
		return []RemediationOption{}, nil
	}
	return opts, nil
}

// staticRemediations 静态映射。Day 5+ 接真策略。
//
// 设计原则：
//   - 每个 resource_type 给 ≥1 个 safe 选项 + ≥1 个 mutating 选项（让 approved worker 有路径选择）
//   - Action 命名遵循 `<resource>.<verb>` 约定（与 aiops/tools/toolset.go 对齐）
//   - Target 留空（执行时由 loop orchestrator 注入 incident context）
//   - AutoApprove 仅 safe 选项为 true（spec §"AutoApprove 策略"）
var staticRemediations = map[string][]RemediationOption{
	"pg": {
		{Action: "pg.terminate_long_tx", Risk: "mutating", AutoApprove: false},
		{Action: "pg.kill_backend", Risk: "mutating", AutoApprove: false},
		{Action: "pg.vacuum_analyze", Risk: "safe", AutoApprove: true},
		{Action: "pg.connection_pause", Risk: "mutating", AutoApprove: false},
	},
	"redis": {
		{Action: "redis.failover", Risk: "mutating", AutoApprove: false},
		{Action: "redis.client_kill", Risk: "mutating", AutoApprove: false},
		{Action: "redis.memory_purge", Risk: "safe", AutoApprove: true},
	},
	"k8s": {
		{Action: "k8s.rolling_restart", Risk: "mutating", AutoApprove: false},
		{Action: "k8s.scale_up", Risk: "mutating", AutoApprove: false},
		{Action: "k8s.evict_pod", Risk: "mutating", AutoApprove: false},
		{Action: "k8s.cordon_node", Risk: "mutating", AutoApprove: false},
	},
	"host": {
		{Action: "host.restart_service", Risk: "mutating", AutoApprove: false},
		{Action: "host.garbage_collect", Risk: "safe", AutoApprove: true},
		{Action: "host.disk_cleanup", Risk: "safe", AutoApprove: true},
	},
	"mq": {
		{Action: "mq.drain_queue", Risk: "mutating", AutoApprove: false},
		{Action: "mq.replay_messages", Risk: "mutating", AutoApprove: false},
		{Action: "mq.pause_consumer", Risk: "safe", AutoApprove: true},
	},
}
