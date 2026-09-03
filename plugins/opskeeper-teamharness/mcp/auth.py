#!/usr/bin/env python3
"""Bearer GatewayKey 注入 + 请求签名 + LoongSuite trace context 透传。

opskeeper-teamharness stdio MCP server 调用 opskeeper HTTP /v1/mcp 时：
1. 必须注入 Authorization 头（Worker qwenpaw 容器启动时由
   agentteams-controller credentials.go 注入 OPSKEEPER_GATEWAY_KEY）
2. 签名策略（v1 启用）：HMAC-SHA256(secret, ts + body) → X-Opskeeper-Signature
3. trace context 透传：把 Worker qwenpaw 启动时注入的 LOONG_TRACE_ID /
   LOONG_SPAN_ID（或 W3C traceparent）透传到 backend，backend 把它写入
   state.json.TraceID + audit log；这样 Worker → backend → audit 全链路
   可在 LoongSuite / Tempo 关联。
"""
from __future__ import annotations

import hashlib
import hmac
import json
import os
import time
from typing import Any


def get_gateway_key() -> str:
    """获取 Worker qwenpaw 注入的 GatewayKey。"""
    key = os.environ.get("OPSKEEPER_GATEWAY_KEY", "")
    if not key:
        raise RuntimeError(
            "OPSKEEPER_GATEWAY_KEY env var is empty. "
            "agentteams-controller should inject this on Worker provisioning. "
            "If running outside AgentTeams, set OPSKEEPER_GATEWAY_KEY manually."
        )
    return key


def get_backend_url() -> str:
    """opskeeper backend base URL."""
    return os.environ.get("OPSKEEPER_BACKEND_URL", "http://opskeeper.opskeeper-system.svc.cluster.local:8443")


def get_tenant_id() -> str:
    """tenant_id from env (multi-tenant isolation)."""
    return os.environ.get("OPSKEEPER_TENANT_ID", "default")


def get_trace_context() -> dict[str, str]:
    """从 Worker 容器 env 提取 LoongSuite trace context。

    支持两种注入方式（按优先级）：
      1. W3C `traceparent` header 内容（agentteams-controller 标准做法）
      2. 直接 env `LOONG_TRACE_ID` / `LOONG_SPAN_ID`（agentteams v2.0+ 旧协议）

    返回非空 dict：{'traceparent': '00-...-...-01'} 或
                   {'X-Trace-Id': '...', 'X-Span-Id': '...'}。
    backend mcp middleware 优先识别 W3C traceparent，回退 X-Trace-Id/X-Span-Id。

    Worker qwenpaw 启动时由 agentteams-controller podspec env 注入：
      - name: TRACEPARENT
        valueFrom:
          fieldRef: { fieldPath: metadata.annotations['trace.agentteams.io/traceparent'] }
    """
    headers: dict[str, str] = {}
    tp = os.environ.get("TRACEPARENT", "").strip()
    if tp:
        # W3C trace context: 00-<trace_id 32 hex>-<span_id 16 hex>-<flags 2 hex>
        parts = tp.split("-")
        if len(parts) == 4 and len(parts[1]) == 32 and len(parts[2]) == 16:
            headers["traceparent"] = tp
        ts = os.environ.get("TRACESTATE", "").strip()
        if ts:
            headers["tracestate"] = ts
    trace_id = os.environ.get("LOONG_TRACE_ID", "").strip()
    span_id = os.environ.get("LOONG_SPAN_ID", "").strip()
    if trace_id and "traceparent" not in headers:
        # 退回直接 ID 注入：backend 仍可关联
        headers["X-Trace-Id"] = trace_id
        if span_id:
            headers["X-Span-Id"] = span_id
    return headers


def sign_request(body: bytes, key: str | None = None) -> dict[str, str]:
    """构造带签名 + trace context 的请求头。

    Returns:
        headers dict，包含：
          - Authorization / X-Opskeeper-Version / X-Opskeeper-Signature /
            X-Opskeeper-Timestamp / X-Opskeeper-Tenant / X-Opskeeper-Body-SHA256
          - traceparent / tracestate 或 X-Trace-Id / X-Span-Id（如 Worker 注入）

    v2 签名（2026-08-27 起强制）：backend 用 HMAC-SHA256(secret=key, msg=ts + "." + body_sha256)
    校验 X-Opskeeper-Signature，避免把完整 body 留在中间件 ctx 里拖慢请求。
    """
    if key is None:
        key = get_gateway_key()
    ts = str(int(time.time()))
    body_sha256 = hashlib.sha256(body).hexdigest()
    msg = ts.encode() + b"." + body_sha256.encode()
    sig = hmac.new(key.encode(), msg, hashlib.sha256).hexdigest()
    headers: dict[str, Any] = {
        "Authorization": f"Bearer {key}",
        "Content-Type": "application/json",
        "X-Opskeeper-Version": "v1",
        "X-Opskeeper-Timestamp": ts,
        "X-Opskeeper-Signature": sig,
        "X-Opskeeper-Body-SHA256": body_sha256,
        "X-Opskeeper-Tenant": get_tenant_id(),
    }
    headers.update(get_trace_context())
    return headers


def verify_response(body: bytes, signature: str, timestamp: str, key: str) -> bool:
    """校验 opskeeper 响应签名（防止中间人篡改）。"""
    msg = timestamp.encode() + b"." + body
    expected = hmac.new(key.encode(), msg, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)


if __name__ == "__main__":
    import sys
    sample = json.dumps({"jsonrpc": "2.0", "method": "tools/list", "id": 1}).encode()
    headers = sign_request(sample, key="test-key-1234567890")
    print(json.dumps(headers, indent=2))
    # 自检：trace context
    sys.exit(0 if all(k in headers for k in ("Authorization", "X-Opskeeper-Version")) else 1)
