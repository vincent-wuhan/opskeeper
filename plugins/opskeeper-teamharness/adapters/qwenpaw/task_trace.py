"""opskeeper-teamharness task lifecycle tracking.

Worker qwenpaw 在 task 开始 / 结束时调用 `on_task_start` / `on_task_end`。
本模块把 lifecycle 事件以 audit event 形式写到 opskeeper state.json，
backend agentteams/http.go 的 putState handler 把它合到 state.audit 列表里。
这样 Worker 调度 → state.json → LoongSuite trace 三者能按 trace_id 关联。

REST 路由复用 plugin 既有 `/v1/state/{task_id}` 端点（Bearer GatewayKey +
HMAC-SHA256 + trace context 透传；与 stdio MCP server 共享 auth.py）。
不走 `/v1/audit/events`（backend 暂无该 REST）。
"""
from __future__ import annotations

import json
import os
import sys
import time
import urllib.error
import urllib.request

# 把 plugin mcp/ 加入 sys.path 以复用 sign_request
_HERE = os.path.dirname(os.path.abspath(__file__))
# task_trace.py 与 plugin.py 同级放在 install root;asset_dir = install_root/<plugin-name>
_MCP_DIR = os.path.normpath(os.path.join(_HERE, "opskeeper-teamharness", "mcp"))
if _MCP_DIR not in sys.path:
    sys.path.insert(0, _MCP_DIR)

from auth import sign_request  # noqa: E402


def _post_state(task_id: str, audit_event: dict) -> bool:
    """把 audit event 合到 state.json audit 列表。

    1. GET /v1/state/{task_id} 读现有 state
    2. 把 audit_event append 到 state.audit[]
    3. PUT /v1/state/{task_id} 回写

    返回 True 表示成功。失败（state 不存在 / 网络错误）静默 — task_trace
    是 best-effort，不能阻塞 Worker 业务逻辑。
    """
    backend = os.environ.get("OPSKEEPER_BACKEND_URL", "http://opskeeper:8443")
    key = os.environ.get("OPSKEEPER_GATEWAY_KEY", "")
    if not key:
        return False

    headers_get = sign_request(b"", key=key)
    # GET 不需要 body，但 sign_request 已经包含 Authorization + 签名（body=""）

    def _do(method: str, url: str, body: bytes) -> bytes:
        h = sign_request(body, key=key)
        req = urllib.request.Request(url, data=body if body else None, headers=h, method=method)
        try:
            with urllib.request.urlopen(req, timeout=2) as resp:
                return resp.read()
        except urllib.error.HTTPError as e:
            err = e.read().decode("utf-8", errors="replace")[:200]
            raise RuntimeError(f"HTTP {e.code}: {err}") from e

    try:
        # 1. 读现有 state
        existing_raw = _do("GET", f"{backend}/v1/state/{task_id}", b"")
        try:
            state = json.loads(existing_raw)
        except json.JSONDecodeError:
            state = {}
        audit = list(state.get("audit") or [])
        audit.append(audit_event)
        state["audit"] = audit
        if "task_id" not in state:
            state["task_id"] = task_id

        # 2. 回写
        body = json.dumps(state, ensure_ascii=False).encode("utf-8")
        _do("PUT", f"{backend}/v1/state/{task_id}", body)
        return True
    except Exception:
        # best-effort：失败不影响 Worker
        return False


def on_task_start(task_id: str, role: str, agent_id: str) -> None:
    """Worker task 开始 — 写 audit event。"""
    _post_state(task_id, {
        "event": "task_start",
        "actor": agent_id,
        "role": role,
        "reason": f"qwenpaw worker {role} started task",
        "at": time.time(),
    })


def on_task_end(task_id: str, role: str, status: str, duration_s: float) -> None:
    """Worker task 结束 — 写 audit event。"""
    _post_state(task_id, {
        "event": "task_end",
        "actor": role,
        "reason": f"status={status} duration_s={duration_s:.2f}",
        "at": time.time(),
    })
