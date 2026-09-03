#!/usr/bin/env python3
"""opskeeper-teamharness MCP tools catalog.

每个 tool 是 JSON-RPC `tools/list` 返回项：
  { name, description, inputSchema }

name 取自 plugin 端的「Worker-friendly」命名空间（namespace.method）。
通过 `names.NAME_REMAP` 把 Worker 调用改写到 backend 实际工具名。

plugin 自实现的工具（hitl.decide / state.put / state.get）在 names.PLUGIN_NATIVE，
tools/call 时路由到 /v1/{hitl,state}/* 不走 /v1/mcp。

backend 实际不存在的工具（P1 TODO）已删除：
  - metric.query_range (query_promql 只 instant；range 由 worker 端缓存)
  - incident.update_status (backend 无)
  - postgres.query (backend 无 pg SQL 直查；走 analyze_database_status)
  - postgres.long_running_tx (harness case 内部 Go 注入；非 MCP)
  - k8s.get_pod_status / k8s.list_events (backend 无 K8s 工具；harness 内部 Go 注入)
  - knowledge.upsert (走 /v1/knowledge/docs REST) — v1.0.2 已以 plugin-native `knowledge.write` 形式暴露
  - audit.list / audit.search (走 /v1/audit/* REST)

backend 实际存在但 plugin tools.py 未列的（Worker 需要可加）：
  - query_logql / query_traceql (日志 / 链路追踪)
  - list_metric_catalog (Prometheus metric 发现)
  - host_find_large_files / host_du_summary / host_stat_file (主机文件系统)
  - list_repo_sources / read_source / grep_source (代码仓库 RAG)

如果 Worker 实际用得上，把它们加入 TOOLS 即可，names.py 不需要改（plugin 与
backend 同名，透传）。
"""
from __future__ import annotations

from typing import Any

TOOLS: list[dict[str, Any]] = [
    # ── loop / recovery（plugin name == backend name，透传）──────────────
    {
        "name": "loop.investigate",
        "description": "一次性返回 RootCauseJSON：根因 / 因果链 / 现象 / 置信度。复用 opskeeper 7 阶段 orchestrator.Run。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "incident_id": {"type": "string", "description": "事故 ID"},
                "alert_group": {"type": "array", "items": {"type": "string"}, "description": "相关告警 ID 列表"},
                "correlation_hints": {"type": "object", "description": "alerter 提供的关联提示"},
            },
            "required": ["incident_id", "alert_group", "correlation_hints"],
        },
    },
    {
        "name": "loop.correlate",
        "description": "关联时间窗口内的多个告警，输出 alert_group。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "raw_alerts": {"type": "array", "description": "原始告警列表"},
                "window": {"type": "string", "description": "关联窗口，如 5m / 1h"},
            },
            "required": ["raw_alerts"],
        },
    },
    {
        "name": "recovery.verify",
        "description": "对比修复后指标与自适应 baseline，输出 VerifiedDelta。复用 D4 自适应 baseline。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "incident_id": {"type": "string"},
                "baseline_window": {"type": "string"},
                "compare_window": {"type": "string"},
                "tolerance": {"type": "number"},
                "metrics": {"type": "array", "items": {"type": "string"}},
            },
            "required": ["incident_id"],
        },
    },
    {
        "name": "recovery.execute",
        "description": "执行已获人工批准的修复动作（对应 backend recovery.execute）。必须携带精确 proposal_id，禁止 skip_audit。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "incident_id": {"type": "string", "minLength": 1},
                "proposal_id": {"type": "string", "pattern": "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"},
                "skill_id": {"type": "string", "minLength": 1},
                "target": {"type": "string", "minLength": 1},
                "resource_type": {"type": "string", "enum": ["host", "pg", "redis", "k8s", "app"]},
                "baseline_window": {"type": "string", "pattern": "^[1-9][0-9]*(m|h|s)$"},
                "compare_window": {"type": "string", "pattern": "^[1-9][0-9]*(m|h|s)$"},
                "tolerance": {"type": "number", "minimum": 0, "maximum": 1},
                "parameters": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {
                        "command": {"type": "string", "enum": ["restart_service", "kill_process", "resize_pool", "noop"]},
                        "device_id": {"type": "integer", "minimum": 1},
                        "service": {"type": "string", "minLength": 1, "maxLength": 255},
                        "incident_id": {"type": "string", "minLength": 1, "maxLength": 64},
                        "fixture_manifest_id": {"type": "string", "minLength": 8, "maxLength": 128},
                        "pool_manifest_id": {"type": "string", "minLength": 8, "maxLength": 128},
                        "reason": {"type": "string", "minLength": 1, "maxLength": 512},
                        "skip_audit": {"type": "boolean", "default": False},
                    },
                    "required": ["command", "reason"],
                },
            },
            "required": ["incident_id", "proposal_id", "skill_id", "target", "resource_type", "parameters"],
            "additionalProperties": False,
        },
    },
    # ── metric（plugin metric.query → backend query_promql）──────────────
    {
        "name": "metric.query",
        "description": "PromQL 即时查询（对应 backend query_promql）。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "expr": {"type": "string", "description": "PromQL expression"},
                "lookback_seconds": {"type": "integer", "minimum": 60, "maximum": 604800},
            },
            "required": ["expr"],
        },
    },
    # ── incident（plugin incident.list/get → backend query_incidents/get_incident_detail）──
    {
        "name": "incident.list",
        "description": "列事故，支持过滤（对应 backend query_incidents）。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "status": {"type": "string"},
                "severity": {"type": "string"},
                "since_minutes": {"type": "integer", "minimum": 1},
                "edge_id": {"type": "integer"},
                "rule_key": {"type": "string"},
                "limit": {"type": "integer"},
            },
        },
    },
    {
        "name": "incident.get",
        "description": "获取事故详情（对应 backend get_incident_detail）。",
        "inputSchema": {
            "type": "object",
            "properties": {"incident_id": {"type": "integer", "minimum": 1}},
            "required": ["incident_id"],
        },
    },
    # ── postgres（plugin postgres.analyze_status → backend analyze_database_status）──
    {
        "name": "postgres.analyze_status",
        "description": "分析数据库健康状态（pg_stat_activity / connections / locks，对应 backend analyze_database_status）。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "device_ids": {"type": "array", "items": {"type": "integer"}, "minItems": 1, "maxItems": 16},
                "db_types": {"type": "array", "items": {"type": "string", "enum": ["mysql", "postgresql", "postgres", "pg", "redis", "mongodb", "mongo"]}},
                "source_ids": {"type": "array", "items": {"type": "string"}},
                "lookback_seconds": {"type": "integer", "minimum": 300, "maximum": 86400},
                "include_custommetrics": {"type": "boolean"},
                "include_disabled": {"type": "boolean"},
            },
        },
    },
    # ── host（plugin name == backend name，透传）─────────────────────────
    {
        "name": "host.get_load",
        "description": "获取主机 load / mem / disk。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "device_ids": {
                    "type": "array",
                    "items": {"type": "integer"},
                    "minItems": 1,
                    "maxItems": 16,
                },
            },
            "required": ["device_ids"],
        },
    },
    {
        "name": "host.get_processes",
        "description": "获取主机进程列表。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "device_ids": {
                    "type": "array",
                    "items": {"type": "integer"},
                    "minItems": 1,
                    "maxItems": 16,
                },
                "top_n": {"type": "integer", "minimum": 1, "maximum": 100},
                "sort_by": {"type": "string", "enum": ["cpu", "mem"]},
            },
            "required": ["device_ids"],
        },
    },
    {
        "name": "host.restart_service",
        "description": "重启主机服务（mutating：需 reviewer.approve + HITL 双签）。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "host": {"type": "string"},
                "service": {"type": "string"},
                "reason": {"type": "string"},
            },
            "required": ["host", "service", "reason"],
        },
    },
    # ── knowledge（plugin knowledge.query → backend query_knowledge）──────
    {
        "name": "knowledge.query",
        "description": "知识库 RAG 查询（pgvector + BM25 双索引，对应 backend query_knowledge）。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {"type": "string"},
                "max_results": {"type": "integer", "default": 5, "minimum": 1, "maximum": 20},
                "incident_id": {"type": "string", "minLength": 1, "maxLength": 128},
            },
            "required": ["query", "incident_id"],
        },
    },
    # ── plugin native（不走 /v1/mcp，路由到 /v1/{hitl,state}/*）──────────
    {
        "name": "hitl.decide",
        "description": "上报 HITL 决策（worker 把 blast_radius=cluster 的决策上报给 opskeeper）。safety_level 必填：L0/L1 不需要 HITL 但保留字段以便审计；L2 走双签；L3 不应到达此工具。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "task_id": {"type": "string"},
                "decision": {"type": "string", "enum": ["approve", "reject"]},
                "signers": {"type": "array", "items": {"type": "string"}},
                "reason": {"type": "string"},
                "safety_level": {"type": "string", "enum": ["L0", "L1", "L2", "L3"], "description": "由 safety/levels.py.resolve_safety_level() 解析；缺失时 backend 拒绝（L2 必填）。"},
                "blast_radius": {"type": "string", "enum": ["host", "service", "cluster", "tenant_wide", "region", "account"]},
                "confidence": {"type": "number", "minimum": 0, "maximum": 1},
            },
            "required": ["task_id", "decision", "safety_level"],
        },
    },
    {
        "name": "state.put",
        "description": "写 MinIO state.json（AgentTeams Manager 调用）。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "task_id": {"type": "string"},
                "state": {"type": "object"},
            },
            "required": ["task_id", "state"],
        },
    },
    {
        "name": "state.get",
        "description": "读 MinIO state.json。",
        "inputSchema": {
            "type": "object",
            "properties": {"task_id": {"type": "string"}},
            "required": ["task_id"],
        },
    },
    {
        "name": "knowledge.write",
        "description": "落盘知识库文档（postmortem.md / runbook / RCA pattern）。走 AgentTeams Bearer 专用 POST /v1/knowledge/docs 并写入 Qdrant。source 与 fingerprint 会沉淀为 tags；文档 ID 由 tenant + title 确定性生成。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "title": {"type": "string"},
                "title_en": {"type": "string"},
                "content": {"type": "string"},
                "url": {"type": "string"},
                "path": {"type": "string", "description": "如 'incident/postmortem'"},
                "tags": {"type": "array", "items": {"type": "string"}, "description": "如 ['postmortem', 'pg-conn-pool', 'sre-team']"},
                "source": {"type": "string", "description": "如 'postmortem-worker:incident-2026-08-26-001'"},
                "fingerprint": {"type": "string", "description": "RCA 模式指纹，写入 fingerprint:<value> tag"},
            },
            "required": ["title", "content"],
        },
    },
    {
        "name": "incident.record",
        "description": "记录当前角色的 OPSKEEPER-113 控制审计事件。事件类型由服务端按 Bearer 角色推导：alerter=alert.received，investigator=root_cause.confirmed，reviewer=recommendation.approved，repairer=action.executed，verifier=recovery_signal.observed，reporter=incident.closed。tenant/actor/trace 不可由请求伪造。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "incident_id": {"type": "string", "minLength": 1, "maxLength": 128},
                "occurred_at": {"type": "string", "format": "date-time"},
                "evidence_ref": {"type": "string", "minLength": 1, "maxLength": 512, "description": "必须指向服务端可回放证据，如 plugin JSON-RPC audit / HITL proposal / 指标查询结果。"},
                "action_fingerprint": {"type": "string", "minLength": 1, "maxLength": 256, "description": "repairer 记录 action.executed 时必填，用于重复动作指标。"},
                "recovery_signal": {"type": "boolean", "description": "verifier 记录恢复信号时填写；closure 只有在 true 恢复信号之后才被接受。"},
            },
            "required": ["incident_id", "evidence_ref"],
        },
    },
]


def get_tools() -> list[dict[str, Any]]:
    return TOOLS


if __name__ == "__main__":
    import json
    print(json.dumps({"tools": get_tools()}, indent=2, ensure_ascii=False))
