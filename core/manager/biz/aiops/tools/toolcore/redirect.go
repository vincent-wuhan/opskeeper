package toolcore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

// RedirectStub is a sentinel BaseTool that occupies a "deep-dive" tool
// name in the coordinator's toolbag without actually executing the
// query. It exists to catch the LLM's habit of hallucinating
// training-time tool names (e.g. it "remembers" host_bash / get_host_load
// even when those aren't presented in the schema) and turn the crash
// into a useful redirect: a one-sentence tool result that tells the
// model the real tool lives on a specialist, so it should re-invoke
// via AgentTool.
//
// Without these stubs eino's graph runtime aborts with
// "[NodeRunError] tool X not found in toolsNode indexes" the moment
// the LLM picks a hallucinated name, and the whole turn is wasted.
// With stubs in place the model sees a normal tool message, learns
// from it, and tries again with AgentTool on the next iteration.
//
// Each stub is registered ONLY in the coordinator's toolbag. Workers
// (specialists) get the real tools through their own whitelist.
type RedirectStub struct {
	// ToolName is the wire-level name the LLM will try to call. Must
	// match a real tool name that the LLM is known to hallucinate.
	ToolName string

	// Specialist is the subagent_type to redirect the LLM toward
	// (e.g. "specialist-compute").
	Specialist string

	// Reason is the one-line "why this tool isn't here" blurb.
	// Surfaced in the redirect message so the LLM understands the
	// architecture, not just the workaround.
	Reason string
}

// Info satisfies basetool.BaseTool. The schema is a permissive
// object — we accept whatever the LLM throws at us because we never
// read the args; the body is always the same redirect.
func (s *RedirectStub) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        s.ToolName,
		Description: fmt.Sprintf("[路由提示] 当前 coordinator 没有直接持有这个工具；复杂诊断请通过 AgentTool 派给 %s。", s.Specialist),
		WhenToUse: fmt.Sprintf(
			"这是路由提示，不是真实业务工具结果。如果用户的问题需要 %s 类能力，用 AgentTool(subagent_type=\"%s\", ...) 派活；不要把这个提示解释成业务失败。",
			s.Reason, s.Specialist,
		),
		Parameters: json.RawMessage(`{"type":"object","additionalProperties":true}`),
		Class:      "read",
	}, nil
}

// InvokableRun returns a fixed redirect message. We intentionally do
// NOT inspect argsJSON — the goal is to get the LLM unstuck, not to
// emulate the tool. The message includes the suggested AgentTool
// invocation form so the next iteration has a concrete template.
func (s *RedirectStub) InvokableRun(_ context.Context, _ string, _ ...basetool.InvokeOption) (string, error) {
	payload := map[string]any{
		"status":         "routing_hint",
		"hint":           fmt.Sprintf("This is an internal routing hint, not a business failure. If the user needs this deep-dive capability, call AgentTool to dispatch to %s.", s.Specialist),
		"reason":         s.Reason,
		"suggested_call": fmt.Sprintf(`AgentTool(description="…", subagent_type=%q, prompt="<self-contained task>")`, s.Specialist),
	}
	out, _ := json.Marshal(payload)
	return string(out), nil
}

// Compile-time guard.
var _ basetool.BaseTool = (*RedirectStub)(nil)

// DispatchDisabledStub occupies the AgentTool name when the coordinator
// cannot dispatch to a specialist, so the model is told why instead of
// hitting a bare "Tool AgentTool not found".
//
// It exists because the base prompt's routing rules are unconditional —
// "DELEGATE 第一工具必须是 AgentTool" — while dispatch is conditional. The
// write-action gate (agent/write_enabled) defaults to off, so an admin who
// never touched it runs a coordinator whose most-advertised instruction is
// impossible to follow. Without a stub the model calls the name it was told
// to call, gets a runtime error that reads like a missing feature, and
// concludes the architecture has no specialists — which is both wrong and
// unrecoverable within the turn.
//
// Class is read on purpose: the stub has to survive the very filter that
// stripped the real tool.
type DispatchDisabledStub struct {
	// Reason is the one-line explanation surfaced to the model.
	Reason string
}

// Info satisfies basetool.BaseTool. The schema stays permissive; the body is
// always the same explanation.
func (s *DispatchDisabledStub) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name: "AgentTool",
		Description: "[策略提示] 当前不能派发专家子代理：" + s.Reason +
			" 只读工具仍然可用，请基于它们回答，并说明深度诊断当前不可用。",
		WhenToUse: "这是策略提示，不是业务失败。" + s.Reason +
			" 不要反复调用，也不要把它当作工具缺失或系统故障。",
		Parameters: json.RawMessage(`{"type":"object","additionalProperties":true}`),
		Class:      "read",
	}, nil
}

// InvokableRun returns the fixed explanation. Args are not inspected — the
// goal is to unstick the model, not to emulate dispatch.
func (s *DispatchDisabledStub) InvokableRun(_ context.Context, _ string, _ ...basetool.InvokeOption) (string, error) {
	payload := map[string]any{
		"status": "policy_hint",
		"hint": "Subagent dispatch is disabled by policy. " + s.Reason +
			" Answer with the read-only tools you have, and tell the user that " +
			"a deeper diagnosis needs an admin to enable agent write actions.",
		"reason": s.Reason,
	}
	out, _ := json.Marshal(payload)
	return string(out), nil
}

// Compile-time guard.
var _ basetool.BaseTool = (*DispatchDisabledStub)(nil)

// CoordinatorRedirectStubs is the canonical set of stubs to install in
// the coordinator toolbag. Each entry maps a hallucination-prone tool
// name to the specialist that actually owns it.
//
// The list is conservative — only tools the LLM has been observed to
// hallucinate in evaluations land here. We don't shadow the entire
// specialist bag, because:
//   - Each stub takes up a slot in the LLM-presented schema list.
//   - If we shadow too many, the LLM's prompt budget bloats and it may
//     start treating stubs as "valid options to consider".
//
// Pure data; safe to append over time as new hallucinations show up.
// dispatchDisabledStub builds the AgentTool placeholder. The reason names the
// gate rather than guessing: an admin reading the console transcript should
// see the switch they have to flip.
func dispatchDisabledStub() *DispatchDisabledStub {
	return &DispatchDisabledStub{
		Reason: "管理员已关闭 AI 助理的写操作权限（设置 → 助理 → 写操作权限，对应 agent/write_enabled），AgentTool 属于写类工具，已被安全过滤。",
	}
}

func CoordinatorRedirectStubs() []basetool.BaseTool {
	defs := []basetool.BaseTool{
		// compute domain
		&RedirectStub{ToolName: "get_host_load", Specialist: "specialist-compute", Reason: "host CPU / memory / load 实时快照"},
		&RedirectStub{ToolName: "get_host_processes", Specialist: "specialist-compute", Reason: "host top 进程"},
		&RedirectStub{ToolName: "get_edge_summary", Specialist: "specialist-compute", Reason: "single-host 综合快照（CPU / mem / disk / load）"},

		// disk domain
		&RedirectStub{ToolName: "host_du_summary", Specialist: "specialist-disk", Reason: "目录占用分析"},
		&RedirectStub{ToolName: "host_find_large_files", Specialist: "specialist-disk", Reason: "大文件定位"},
		&RedirectStub{ToolName: "host_stat_file", Specialist: "specialist-disk", Reason: "单文件 stat"},

		// network domain
		&RedirectStub{ToolName: "host_probe_http", Specialist: "specialist-network", Reason: "HTTP 探测"},
		&RedirectStub{ToolName: "host_probe_dns", Specialist: "specialist-network", Reason: "DNS 探测"},
		&RedirectStub{ToolName: "host_probe_tcp", Specialist: "specialist-network", Reason: "TCP 端口探测"},
		&RedirectStub{ToolName: "host_netns_inspect", Specialist: "specialist-network", Reason: "netns 内部状态"},

		// ops / mutating
		&RedirectStub{ToolName: "host_bash", Specialist: "specialist-ops", Reason: "host shell（读类命令一般走 ops，写类要走 reviewer）"},
		&RedirectStub{ToolName: "host_restart_service", Specialist: "specialist-ops", Reason: "systemd 重启（会走 reviewer 二审）"},

		// SRE / incident
		&RedirectStub{ToolName: "correlate_incident", Specialist: "incident-investigator", Reason: "incident 多信号关联"},
		&RedirectStub{ToolName: "get_incident_detail", Specialist: "incident-investigator", Reason: "incident 详情"},
		&RedirectStub{ToolName: "rank_edges", Specialist: "specialist-sre", Reason: "集群按指标排序"},
		&RedirectStub{ToolName: "find_outlier_edges", Specialist: "specialist-sre", Reason: "集群异常机检测"},

		// PromQL / LogQL — these belong to whichever specialist is best for
		// the question; default to incident-investigator (it has the
		// widest read scope and can re-dispatch if needed).
		&RedirectStub{ToolName: "query_promql", Specialist: "incident-investigator", Reason: "PromQL 查询；如果只关心健康度可派 specialist-sre"},
		&RedirectStub{ToolName: "query_logql", Specialist: "incident-investigator", Reason: "LogQL 查询"},
		&RedirectStub{ToolName: "query_traceql", Specialist: "incident-investigator", Reason: "TraceQL 查询"},
	}
	// AgentTool is the one name the coordinator is told to call directly
	// rather than be redirected away from, so it needs a stub that explains
	// a policy decision instead of naming a specialist. A real AgentTool in
	// the bag always wins over this one (see the de-dupe in
	// chatruntime.Runtime), so the stub is inert whenever dispatch is on.
	defs = append(defs, dispatchDisabledStub())
	return defs
}
