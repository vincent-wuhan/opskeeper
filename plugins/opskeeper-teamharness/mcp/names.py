#!/usr/bin/env python3
"""opskeeper-teamharness MCP name routing.

plugin 端工具名（Worker LLM 看到）⇄ backend opskeeper /v1/mcp 实际工具名 的映射。
backend 实际工具名（按 `internal/manager/biz/aiops/tools/*_basetool.go` 的 ToolNameXxx 常量）：

  - loop.investigate      (internal/manager/biz/loop/mcp_adapter.go: ToolNameInvestigate)
  - loop.correlate        (internal/manager/biz/loop/mcp_adapter.go: ToolNameCorrelate)
  - recovery.verify       (internal/manager/biz/loop/mcp_adapter.go: ToolNameVerify)
  - query_promql          (internal/manager/biz/aiops/tools/query_promql.go)
  - query_incidents       (internal/manager/biz/aiops/tools/query_incidents.go)
  - get_incident_detail   (internal/manager/biz/aiops/tools/get_incident_detail.go)
  - analyze_database_status (internal/manager/biz/aiops/tools/analyze_database_status.go)
  - get_host_load         (internal/manager/biz/aiops/tools/host_load.go)
  - get_host_processes    (internal/manager/biz/aiops/tools/host_processes.go)
  - host_restart_service  (internal/manager/biz/aiops/tools/restart_service_basetool.go)
  - host_find_large_files (internal/manager/biz/aiops/tools/host_files_basetool.go)
  - host_du_summary       (internal/manager/biz/aiops/tools/host_files_basetool.go)
  - host_stat_file        (internal/manager/biz/aiops/tools/host_files_basetool.go)
  - query_knowledge       (internal/manager/biz/aiops/tools/query_knowledge_basetool.go)
  - list_repo_sources     (internal/manager/biz/aiops/tools/code_source_basetool.go)
  - read_source           (internal/manager/biz/aiops/tools/code_source_basetool.go)
  - grep_source           (internal/manager/biz/aiops/tools/code_source_basetool.go)
  - query_logql           (internal/manager/biz/aiops/tools/query_logql.go)
  - query_traceql         (internal/manager/biz/aiops/tools/query_traceql.go)
  - list_metric_catalog   (internal/manager/biz/aiops/tools/metric_catalog_tool.go)

plugin 自实现的工具（不走 /v1/mcp，走 plugin HTTP handler 在
internal/manager/server/agentteams/http.go，路由通过 cmd/opskeeper/main.go
的 api.Group(...) 注册，所以 path 全部带 /api 前缀；server.py:263 把
get_backend_url() 与 path 直接拼接，因此 path_template 必须自带 /api）。
注意：/healthz 公开端点在 backend mux 根，不在 api.Group 内，不要加 /api。

  - hitl.decide      ->  POST  /api/v1/hitl/decide
  - state.put        ->  PUT   /api/v1/state/{task_id}
  - state.get        ->  GET   /api/v1/state/{task_id}
  - knowledge.write  ->  POST  /api/v1/knowledge/docs   (v1.0.2: postmortem 落盘知识库)
  - incident.record  ->  POST  /api/v1/incidents/events (角色阶段事件审计)
  - skills.get       ->  GET   /api/v1/skills/{name}    (用于 Manager 把 Worker 需要的 SKILL.md 拉给 runtime)

不做 name remap 的工具直接透传（plugin name == backend name）。

P1 TODO（plugin 不暴露，Worker 走不到）：
  - incident.update_status   (backend 没有该 tool；走 incident webhook 替代)
  - postgres.query           (backend 没有 pg SQL 直查工具；走 analyze_database_status)
  - postgres.long_running_tx (harness case 内部用 Go injector 直查 pg_stat_activity；
                              不通过 plugin MCP 暴露，避免越权)
  - k8s.get_pod_status       (backend 无 K8s 工具；harness case 内部 Go 注入)
  - k8s.list_events          (同上)
  - knowledge.upsert         (KB 写走 opskeeper 既有 /v1/knowledge/docs，已以 plugin-native `knowledge.write` 暴露，v1.0.2)
  - audit.list / audit.search (backend 暴露在 /v1/audit/* REST，不通过 MCP 暴露)
  - metric.query_range       (backend query_promql 只支持 instant；range 走 plugin 端
                              Prometheus HTTP range_query 端点或 worker-side 缓存)
"""

from __future__ import annotations

from typing import Any, Optional

# plugin 端友好名 -> backend 端实际工具名（透传到 /v1/mcp）
NAME_REMAP: dict[str, str] = {
    # metric
    "metric.query": "query_promql",
    # incident
    "incident.list": "query_incidents",
    "incident.get": "get_incident_detail",
    # postgres
    "postgres.analyze_status": "analyze_database_status",
    # host
    "host.get_load": "get_host_load",
    "host.get_processes": "get_host_processes",
    # knowledge
    "knowledge.query": "query_knowledge",
    # 其余名字（loop.* / recovery.verify / host.restart_service）已在 backend 直接对齐
}

# plugin 自实现工具：name -> HTTP 路由信息
#   kind: "hitl" | "state_get" | "state_put" | "skill"
#   method: HTTP method
#   path_template: 用 {param} 占位符；resolved path 由 server.py 用 arguments 替换
PLUGIN_NATIVE: dict[str, dict[str, Any]] = {
    "hitl.decide": {
        "kind": "hitl",
        "method": "POST",
        "path_template": "/api/v1/hitl/decide",
        # body 来源: 整个 arguments（已是 JSON）
        "body_from_args": True,
    },
    "state.get": {
        "kind": "state_get",
        "method": "GET",
        "path_template": "/api/v1/state/{task_id}",
        # path 参数从 arguments.task_id 取
        "path_param": "task_id",
    },
    "state.put": {
        "kind": "state_put",
        "method": "PUT",
        "path_template": "/api/v1/state/{task_id}",
        "path_param": "task_id",
        # body: arguments.state (去掉 task_id)
        "body_field": "state",
    },
    "knowledge.write": {
        "kind": "knowledge_upsert",
        "method": "POST",
        "path_template": "/api/v1/knowledge/docs",
        # body: 整个 arguments（已是 JSON；含 doc_id/title/content/tags/source/fingerprint）
        "body_from_args": True,
    },
    "incident.record": {
        "kind": "incident_record",
        "method": "POST",
        "path_template": "/api/v1/incidents/events",
        "body_from_args": True,
    },
}


def resolve_backend_name(plugin_name: str) -> Optional[str]:
    """返回 backend 实际工具名。

    透传场景：plugin_name 已在 NAME_REMAP 中 -> 返回 backend 名；
    否则返回 plugin_name（plugin 与 backend 同名）。
    返回 None 表示该工具不是 backend MCP tool，应走 plugin native 路由。
    """
    if plugin_name in PLUGIN_NATIVE:
        return None
    return NAME_REMAP.get(plugin_name, plugin_name)


def is_plugin_native(plugin_name: str) -> bool:
    return plugin_name in PLUGIN_NATIVE


def native_route(plugin_name: str) -> Optional[dict[str, Any]]:
    return PLUGIN_NATIVE.get(plugin_name)
