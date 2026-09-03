// Package prom — agentteams_metrics.go
//
// AgentTeams / closed-loop orchestrator 专用 metrics，导出到同一个
// `/metrics` 端点（与 manager_metrics.go 共享 registry）。
//
// 为什么单独建文件（不混在 manager_metrics.go）：
//   - manager_metrics 是历史 alert/promwrite/device 业务；
//     agentteams_metrics 是 2026-08-26 新增的 AI-ops 业务。
//     关注点不同（一个管"我的告警怎么响"，一个管"我的
//     multi-agent 编排怎么跑"），分文件减少 reviewer 噪音。
//   - 共享 `internal/pkg/prom` 包避免新增 import 路径；调用方
//     `prom.ObserveAgentTeamsMCPCall(...)` 与 `prom.IncPromWrite(...)`
//     风格一致。
//
// 标签基数约束（与 manager_metrics.go 同样的纪律）：
//   - tool / role / phase / result 都是封闭枚举；
//     实际值域见各 var 声明旁的注释。
//   - 不允许带 tenant_id / incident_id / device_id 等高基数标签。
//
// 注册时机：与 RegisterManagerMetrics 并列，由 cmd/opskeeper/main.go 在
// 启动时一次性调用 RegisterAgentTeamsMetrics(reg, log)；之后所有 Inc /
// Observe 立即生效。未注册的 nil 检查确保 test 嵌入模式下不 panic。
package prom

import (
	"errors"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// AgentTeamsMCPCallTotal counts every JSON-RPC `tools/call` that the
	// opskeeper MCP server dispatches. One increment per call regardless
	// of success/denied; the result label distinguishes the path.
	//
	// Labels:
	//   tool = loop.correlate | loop.investigate | recovery.execute |
	//          recovery.verify | metric.query | incident.list |
	//          incident.get | postgres.analyze_status | host.* |
	//          knowledge.query | knowledge.write | hitl.decide |
	//          state.put | state.get | (other)
	//   role = alerter | investigator | critic | reviewer | repairer |
	//          verifier | reporter | unknown
	//   result = ok | denied | error | auth_failed
	AgentTeamsMCPCallTotal *prometheus.CounterVec

	// AgentTeamsMCPCallDuration observes per-tool wall-clock latency.
	// Buckets tuned for sub-second to multi-second LLM-driven calls
	// (loop.investigate historically 17s+ in our env).
	//
	// Labels:
	//   tool = same enumeration as AgentTeamsMCPCallTotal
	//   role = same enumeration as AgentTeamsMCPCallTotal
	AgentTeamsMCPCallDuration *prometheus.HistogramVec

	// AgentTeamsHigressResolveTotal counts consumer resolution calls.
	// High-resolution visibility into whether the Higress control plane
	// is reachable from this replica (any spike in result=fail indicates
	// upstream outage, not just per-request failures).
	//
	// Labels:
	//   result = ok | not_found | auth_failed | network_error | unknown
	AgentTeamsHigressResolveTotal *prometheus.CounterVec

	// AgentTeamsPluginSyncTotal counts plugin sync / install operations.
	// Three modes × two ops × two results = 12 buckets max.
	//
	// Labels:
	//   mode = worker-http | controller-discovery | stub
	//   operation = sync | install
	//   result = ok | fail
	AgentTeamsPluginSyncTotal *prometheus.CounterVec

	// LoopPhaseTotal counts phase transitions in the closed-loop
	// orchestrator (alerter → investigator → critic → reviewer →
	// repairer → verifier → reporter). One increment per terminal
	// phase outcome (ok | failed | rolled_back).
	//
	// Labels:
	//   phase = alerter | investigator | critic | reviewer | repairer |
	//           verifier | reporter
	//   result = ok | failed | rolled_back | severity_escalated
	LoopPhaseTotal *prometheus.CounterVec

	// LoopPhaseDuration observes per-phase wall-clock duration.
	// Same enumeration as LoopPhaseTotal.
	LoopPhaseDuration *prometheus.HistogramVec

	// LoopDBApprovedDecisionLookupTotal counts ApprovalDecision loader
	// outcomes from the recovered-phase Planner path (commit 1dcf0d8).
	// Specifically helps answer "is DB-backed loader being hit at all,
	// and is it returning real contracts vs defaults?"
	//
	// Labels:
	//   result = loaded | not_found | type_mismatch | payload_corrupted |
	//            db_error | skipped (contractID<=0)
	LoopDBApprovedDecisionLookupTotal *prometheus.CounterVec
)

// RegisterAgentTeamsMetrics registers agentteams / loop metrics into the
// shared registry. Must be called once at boot (cmd/opskeeper/main.go);
// subsequent calls no-op via AlreadyRegisteredError handling.
func RegisterAgentTeamsMetrics(reg prometheus.Registerer, log *slog.Logger) {
	if reg == nil {
		if log != nil {
			log.Warn("prom agentteams metrics: nil registry, skipping registration")
		}
		return
	}

	AgentTeamsMCPCallTotal = registerAgentTeamsCounter(reg,
		prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentteams_mcp_call_total",
			Help: "Total opskeeper MCP tools/call invocations.",
		}, []string{"tool", "role", "result"}),
		log, "agentteams_mcp_call_total")

	AgentTeamsMCPCallDuration = registerAgentTeamsHistogram(reg,
		prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "agentteams_mcp_call_duration_seconds",
			Help:    "Per-tool wall-clock latency for opskeeper MCP tools/call.",
			Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10, 20, 30, 60, 120},
		}, []string{"tool", "role"}),
		log, "agentteams_mcp_call_duration_seconds")

	AgentTeamsHigressResolveTotal = registerAgentTeamsCounter(reg,
		prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentteams_higress_resolve_total",
			Help: "Total Higress consumer resolution attempts.",
		}, []string{"result"}),
		log, "agentteams_higress_resolve_total")

	AgentTeamsPluginSyncTotal = registerAgentTeamsCounter(reg,
		prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentteams_plugin_sync_total",
			Help: "Total plugin sync/install operations.",
		}, []string{"mode", "operation", "result"}),
		log, "agentteams_plugin_sync_total")

	LoopPhaseTotal = registerAgentTeamsCounter(reg,
		prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loop_phase_total",
			Help: "Total closed-loop orchestrator phase terminal outcomes.",
		}, []string{"phase", "result"}),
		log, "loop_phase_total")

	LoopPhaseDuration = registerAgentTeamsHistogram(reg,
		prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "loop_phase_duration_seconds",
			Help:    "Per-phase wall-clock duration in the closed-loop orchestrator.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}, []string{"phase"}),
		log, "loop_phase_duration_seconds")

	LoopDBApprovedDecisionLookupTotal = registerAgentTeamsCounter(reg,
		prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loop_db_approved_decision_lookup_total",
			Help: "Outcomes of DBApprovedDecisionLoader.LoadApprovedDecision.",
		}, []string{"result"}),
		log, "loop_db_approved_decision_lookup_total")
}

// ---- Package-private helpers (call sites use these) ----

// ObserveAgentTeamsMCPCall records one MCP tools/call observation.
// start is captured by caller via time.Now(); result derives from err.
func ObserveAgentTeamsMCPCall(tool, role string, seconds float64, err error, authFailed bool, denied bool) {
	if AgentTeamsMCPCallTotal == nil {
		return
	}
	result := "ok"
	switch {
	case authFailed:
		result = "auth_failed"
	case denied:
		result = "denied"
	case err != nil:
		result = "error"
	}
	AgentTeamsMCPCallTotal.WithLabelValues(tool, role, result).Inc()
	if AgentTeamsMCPCallDuration != nil && !authFailed {
		AgentTeamsMCPCallDuration.WithLabelValues(tool, role).Observe(seconds)
	}
}

// IncAgentTeamsHigressResolve records one ResolveConsumer outcome.
func IncAgentTeamsHigressResolve(result string) {
	if AgentTeamsHigressResolveTotal == nil {
		return
	}
	AgentTeamsHigressResolveTotal.WithLabelValues(result).Inc()
}

// IncAgentTeamsPluginSync records one sync/install outcome.
func IncAgentTeamsPluginSync(mode, operation string, err error) {
	if AgentTeamsPluginSyncTotal == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "fail"
	}
	AgentTeamsPluginSyncTotal.WithLabelValues(mode, operation, result).Inc()
}

// ObserveLoopPhase records one phase terminal outcome + duration.
func ObserveLoopPhase(phase string, seconds float64, err error) {
	if LoopPhaseTotal == nil {
		return
	}
	result := "ok"
	switch {
	case err != nil && errors.Is(err, ErrSeverityEscalated):
		result = "severity_escalated"
	case err != nil && errors.Is(err, ErrRolledBack):
		result = "rolled_back"
	case err != nil:
		result = "failed"
	}
	LoopPhaseTotal.WithLabelValues(phase, result).Inc()
	if LoopPhaseDuration != nil {
		LoopPhaseDuration.WithLabelValues(phase).Observe(seconds)
	}
}

// ErrSeverityEscalated marks retry_count > MaxRetryCount escalation;
// see internal/manager/biz/loop/recovery.go. Exported so call sites in
// other packages can construct the sentinel without leaking private types.
var ErrSeverityEscalated = errSeverityEscalated{}

type errSeverityEscalated struct{}

func (errSeverityEscalated) Error() string { return "loop.recovery.severity_escalated" }

// IsSeverityEscalated lets call sites tag the error as severity_escalated
// without importing this package's private sentinel directly.
func IsSeverityEscalated(err error) bool {
	return errors.Is(err, ErrSeverityEscalated)
}

// ErrRolledBack marks a phase that completed via rollback path
// (verifier failed → recovered → approved).
var ErrRolledBack = errRolledBack{}

type errRolledBack struct{}

func (errRolledBack) Error() string { return "loop.rolled_back" }

// IsRolledBack lets call sites tag the error as rolled_back.
func IsRolledBack(err error) bool {
	return errors.Is(err, ErrRolledBack)
}

// IncLoopDBApprovedDecisionLookup records one DBApprovedDecisionLoader outcome.
func IncLoopDBApprovedDecisionLookup(result string) {
	if LoopDBApprovedDecisionLookupTotal == nil {
		return
	}
	LoopDBApprovedDecisionLookupTotal.WithLabelValues(result).Inc()
}

// ---- Internal registration helpers (mirror manager_metrics.go style) ----

func registerAgentTeamsCounter(reg prometheus.Registerer, c *prometheus.CounterVec, log *slog.Logger, name string) *prometheus.CounterVec {
	err := reg.Register(c)
	if err == nil {
		return c
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		if log != nil {
			log.Warn("prom agentteams metrics: counter already registered, reusing existing",
				slog.String("name", name))
		}
		if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
			return existing
		}
	}
	panic(err)
}

func registerAgentTeamsHistogram(reg prometheus.Registerer, h *prometheus.HistogramVec, log *slog.Logger, name string) *prometheus.HistogramVec {
	err := reg.Register(h)
	if err == nil {
		return h
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		if log != nil {
			log.Warn("prom agentteams metrics: histogram already registered, reusing existing",
				slog.String("name", name))
		}
		if existing, ok := are.ExistingCollector.(*prometheus.HistogramVec); ok {
			return existing
		}
	}
	panic(err)
}
