#!/usr/bin/env python3
"""opskeeper-teamharness MCP stdio server.

Proxies JSON-RPC calls (Worker qwenpaw stdin/stdout) to opskeeper backend
HTTP /v1/mcp (Bearer GatewayKey + HMAC-SHA256 signature).

Worker qwenpaw spawns this as a subprocess via plugin.yaml mcp.servers.args:
    python mcp/server.py

Protocol (JSON-RPC 2.0 on stdin/stdout):
    read: {"jsonrpc": "2.0", "method": "tools/list", "id": 1}
    write: {"jsonrpc": "2.0", "result": {"tools": [...]}, "id": 1}

    read: {"jsonrpc": "2.0", "method": "tools/call", "params": {"name": "loop.investigate", "arguments": {...}}, "id": 2}
    write: {"jsonrpc": "2.0", "result": {...RootCauseJSON...}, "id": 2}

Env vars (injected by qwenpaw on Worker start):
    OPSKEEPER_BACKEND_URL  - default http://opskeeper:8443
    OPSKEEPER_GATEWAY_KEY  - required, AgentTeams Controller provisioned
    OPSKEEPER_TENANT_ID    - default "default"
    OPSKEEPER_TIMEOUT      - HTTP timeout seconds, default 30
"""
from __future__ import annotations

import json
import logging
import os
import sys
import urllib.error
import urllib.request
from typing import Any

from auth import get_backend_url, get_gateway_key, get_tenant_id, sign_request
from tools import get_tools
from names import is_plugin_native, native_route, resolve_backend_name

logging.basicConfig(
    level=os.environ.get("OPSKEEPER_LOG_LEVEL", "INFO"),
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    stream=sys.stderr,  # MCP uses stdout for protocol; logs go to stderr
)
log = logging.getLogger("opskeeper-mcp")


class Metrics:
    """轻量级 in-process metrics counter，供 plugin stdio MCP server 自观测。

    设计原则：
    - 无外部依赖（不引 prometheus_client），避免 plugin 体积膨胀；
    - 进程内 thread-safe（lock 保护 dict）；
    - 输出到 stderr（JSON Lines），可被 opskeeper / Matrix 收集；
    - 主路径不因 metrics 失败抛错（try/except 包裹）。
    """

    def __init__(self) -> None:
        self._lock = __import__("threading").Lock()
        self._counters: dict[str, int] = {
            "tools_list_total": 0,
            "tools_call_success": 0,
            "tools_call_failure": 0,
            "tools_call_retry": 0,
            "tools_call_4xx": 0,
            "tools_call_5xx": 0,
            "tools_call_network_error": 0,
            "plugin_native_call_success": 0,
            "plugin_native_call_failure": 0,
        }
        self._by_tool: dict[str, dict[str, int]] = {}

    def incr(self, key: str, tool: str | None = None, n: int = 1) -> None:
        try:
            with self._lock:
                self._counters[key] = self._counters.get(key, 0) + n
                if tool is not None:
                    tb = self._counters_by_tool.setdefault(tool, {})
                    tb[key] = tb.get(key, 0) + n
            # 每次 increment emit 一行 JSON 到 stderr（Worker runtime 可收集）
            self.emit_to_stderr()
        except Exception:
            pass

    # alias for type checker
    @property
    def _counters_by_tool(self) -> dict[str, dict[str, int]]:
        return self._by_tool

    def snapshot(self) -> dict[str, Any]:
        try:
            with self._lock:
                return {
                    "global": dict(self._counters),
                    "by_tool": {k: dict(v) for k, v in self._by_tool.items()},
                }
        except Exception:
            return {"global": {}, "by_tool": {}}

    def emit_to_stderr(self) -> None:
        """把 snapshot 写到 stderr（JSON Lines）；Worker runtime 可收集。"""
        try:
            import json as _json
            payload = {"event": "opskeeper_mcp_metrics", **self.snapshot()}
            sys.stderr.write(_json.dumps(payload, ensure_ascii=False) + "\n")
            sys.stderr.flush()
        except Exception:
            pass


METRICS = Metrics()


def http_post(url: str, body: bytes, headers: dict[str, str], timeout: int = 30) -> bytes:
    """POST body to url with given headers. Returns response body bytes."""
    req = urllib.request.Request(url, data=body, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.read()
    except urllib.error.HTTPError as e:
        # opskeeper 返回的 4xx/5xx 也带 JSON-RPC 错误体，原样透传
        err_body = e.read()
        log.warning("HTTP %d from opskeeper: %s", e.code, err_body[:200])
        raise


def http_request_with_retry(
    method: str,
    url: str,
    body: bytes | None,
    headers: dict[str, str],
    timeout: int,
    max_retries: int = 1,
    backoff_seconds: float = 0.5,
) -> bytes:
    """带 retry + backoff 的 HTTP 请求 helper。

    - 4xx 客户端错误立即抛（不重试，retry 无意义）
    - 5xx / 网络错误 / timeout 最多重试 max_retries 次（默认 1）
    - backoff_seconds 是指数 backoff 基础（attempt 1: backoff * 1, attempt 2: backoff * 2）

    用于 tools/call 与 plugin native 路由的稳定调用。
    """
    import time as _time
    last_exc: Exception | None = None
    for attempt in range(max_retries + 1):
        try:
            return http_request(method, url, body, headers, timeout=timeout)
        except urllib.error.HTTPError as e:
            # 4xx 不重试（retry 也不会成功）
            if 400 <= e.code < 500:
                METRICS.incr("tools_call_4xx")
                raise
            last_exc = e
            METRICS.incr("tools_call_5xx")
            log.warning(
                "HTTP %d from opskeeper (attempt %d/%d): %s",
                e.code, attempt + 1, max_retries + 1, e.read()[:200].decode("utf-8", errors="replace"),
            )
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            last_exc = e
            METRICS.incr("tools_call_network_error")
            log.warning(
                "transport error from opskeeper (attempt %d/%d): %s: %s",
                attempt + 1, max_retries + 1, type(e).__name__, e,
            )
        if attempt < max_retries:
            METRICS.incr("tools_call_retry")
            sleep_for = backoff_seconds * (2 ** attempt)
            _time.sleep(sleep_for)
    assert last_exc is not None
    raise last_exc


def health_check(timeout: int = 5) -> bool:
    """启动时检查 opskeeper backend 是否可达。

    返回 True 表示 healthy；False 表示不可达（仍可继续启动，由 MCP caller 决定）。
    通过 OPSKEEPER_SKIP_HEALTH_CHECK=1 跳过（CI / 单元测试场景）。
    """
    if os.environ.get("OPSKEEPER_SKIP_HEALTH_CHECK", "").lower() in ("1", "true", "yes"):
        log.info("health check skipped (OPSKEEPER_SKIP_HEALTH_CHECK=1)")
        return True
    url = get_backend_url().rstrip("/") + "/healthz"
    try:
        # /healthz 公开端点，不需要 Bearer；但 opskeeper 标准鉴权是 Bearer，
        # 这里用空 Bearer 让 backend 返回 401 也算"backend reachable"。
        # 直接看是否连接成功。
        req = urllib.request.Request(url, method="GET")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            healthy = 200 <= resp.status < 300
            log.info("health check %s: %d", url, resp.status)
            return healthy
    except urllib.error.HTTPError as e:
        # 401/403 表示 backend 起来了但要鉴权 — 算 reachable
        if e.code in (401, 403):
            log.info("health check %s reachable (auth required): %d", url, e.code)
            return True
        log.warning("health check %s failed: HTTP %d", url, e.code)
        return False
    except Exception as e:
        log.warning("health check %s failed: %s: %s", url, type(e).__name__, e)
        return False


def http_request(method: str, url: str, body: bytes | None, headers: dict[str, str], timeout: int = 30) -> bytes:
    """Generic HTTP request helper (GET/PUT/POST/PATCH)。返回 response body bytes。"""
    method_upper = method.upper()
    req = urllib.request.Request(url, data=body, headers=headers, method=method_upper)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.read()
    except urllib.error.HTTPError as e:
        err_body = e.read()
        log.warning("HTTP %d from opskeeper: %s", e.code, err_body[:200])
        raise


def handle_initialize(req: dict[str, Any]) -> dict[str, Any]:
    """MCP initialize — 返回协议版本和能力声明。"""
    return {
        "jsonrpc": "2.0",
        "id": req.get("id"),
        "result": {
            "protocolVersion": "2024-11-05",
            "serverInfo": {"name": "opskeeper-teamharness", "version": "1.0.0"},
            "capabilities": {"tools": {"listChanged": False}},
        },
    }


def handle_tools_list(req: dict[str, Any]) -> dict[str, Any]:
    """tools/list — 返回工具目录。"""
    METRICS.incr("tools_list_total")
    return {
        "jsonrpc": "2.0",
        "id": req.get("id"),
        "result": {"tools": get_tools()},
    }


def _resolve_native_path(template: str, arguments: dict[str, Any], path_param: str | None) -> str:
    """Replace {path_param} in template with arguments[path_param]."""
    if not path_param:
        return template
    val = arguments.get(path_param)
    if not val or not isinstance(val, str):
        raise ValueError(f"plugin native tool requires string argument {path_param!r}, got {val!r}")
    return template.replace("{" + path_param + "}", val)


def handle_plugin_native_call(tool_name: str, arguments: dict[str, Any], req_id: Any) -> dict[str, Any]:
    """Dispatch hitl.decide / state.put / state.get to plugin HTTP routes."""
    route = native_route(tool_name)
    if route is None:
        return _jsonrpc_error(req_id, -32601, f"unknown plugin native tool: {tool_name}")

    method = route["method"]
    path_template = route["path_template"]
    path_param = route.get("path_param")
    try:
        path = _resolve_native_path(path_template, arguments, path_param)
    except ValueError as e:
        return _jsonrpc_error(req_id, -32602, str(e))

    backend_url = get_backend_url() + path

    # body 构造
    if route.get("body_from_args"):
        body_bytes = json.dumps(arguments, ensure_ascii=False).encode("utf-8")
    elif route.get("body_field"):
        body_field = route["body_field"]
        body_obj = {k: v for k, v in arguments.items() if k != path_param}
        body_bytes = json.dumps(body_obj, ensure_ascii=False).encode("utf-8")
    else:
        body_bytes = b""

    headers = sign_request(body_bytes)
    timeout = int(os.environ.get("OPSKEEPER_TIMEOUT", "30"))

    log.info("plugin-native: tool=%s method=%s path=%s", tool_name, method, path)

    try:
        resp_body = http_request_with_retry(
            method, backend_url, body_bytes if body_bytes else None, headers, timeout=timeout,
            max_retries=int(os.environ.get("OPSKEEPER_MAX_RETRIES", "1")),
        )
    except urllib.error.HTTPError as e:
        err_body = e.read().decode("utf-8", errors="replace")
        METRICS.incr("plugin_native_call_failure", tool=tool_name)
        return _jsonrpc_error(req_id, -32000, f"opskeeper HTTP {e.code}", err_body[:500])
    except Exception as e:
        METRICS.incr("plugin_native_call_failure", tool=tool_name)
        log.exception("plugin-native call failed: tool=%s", tool_name)
        return _jsonrpc_error(req_id, -32603, f"opskeeper unreachable: {type(e).__name__}: {e}")

    # 透传 opskeeper REST 响应（一般是 JSON 包装 {"state": {...}} 或 {"ok": true}）
    try:
        parsed = json.loads(resp_body)
    except json.JSONDecodeError:
        METRICS.incr("plugin_native_call_failure", tool=tool_name)
        return _jsonrpc_error(req_id, -32603, f"opskeeper returned non-JSON: {resp_body[:200]!r}")
    METRICS.incr("plugin_native_call_success", tool=tool_name)
    return {"jsonrpc": "2.0", "id": req_id, "result": parsed}


def _jsonrpc_error(req_id: Any, code: int, message: str, data: Any = None) -> dict[str, Any]:
    err: dict[str, Any] = {"code": code, "message": message}
    if data is not None:
        err["data"] = data
    return {"jsonrpc": "2.0", "id": req_id, "error": err}


def handle_tools_call(req: dict[str, Any]) -> dict[str, Any]:
    """tools/call — 分发到 plugin native 路由 或 opskeeper /v1/mcp。"""
    params = req.get("params") or {}
    tool_name = params.get("name", "")
    arguments = params.get("arguments") or {}
    req_id = req.get("id")

    # 1) plugin native 路由（hitl.decide / state.put / state.get）
    if is_plugin_native(tool_name):
        return handle_plugin_native_call(tool_name, arguments, req_id)

    # 2) backend MCP tool — 改写 plugin 端友好名 → backend 实际名
    backend_name = resolve_backend_name(tool_name)
    if backend_name is None or backend_name == "":
        return _jsonrpc_error(req_id, -32601, f"unknown tool: {tool_name}")

    arguments = normalize_backend_arguments(backend_name, arguments)

    # 构造内部 JSON-RPC 请求体（opskeeper 期望的 MCP 格式）
    inner = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {"name": backend_name, "arguments": arguments},
    }
    body = json.dumps(inner, ensure_ascii=False).encode("utf-8")

    backend_url = get_backend_url() + "/api/v1/mcp"
    headers = sign_request(body)
    timeout = int(os.environ.get("OPSKEEPER_TIMEOUT", "30"))

    log.info("calling opskeeper: tool=%s tenant=%s", tool_name, get_tenant_id())

    try:
        resp_body = http_request_with_retry(
            "POST", backend_url, body, headers, timeout=timeout,
            max_retries=int(os.environ.get("OPSKEEPER_MAX_RETRIES", "1")),
        )
    except urllib.error.HTTPError as e:
        err_body = e.read().decode("utf-8", errors="replace")
        if 400 <= e.code < 500:
            METRICS.incr("tools_call_4xx", tool=tool_name)
        elif e.code >= 500:
            METRICS.incr("tools_call_5xx", tool=tool_name)
        METRICS.incr("tools_call_failure", tool=tool_name)
        return {
            "jsonrpc": "2.0",
            "id": req.get("id"),
            "error": {
                "code": -32000,
                "message": f"opskeeper HTTP {e.code}",
                "data": err_body[:500],
            },
        }
    except Exception as e:
        METRICS.incr("tools_call_network_error", tool=tool_name)
        METRICS.incr("tools_call_failure", tool=tool_name)
        log.exception("opskeeper call failed")
        return {
            "jsonrpc": "2.0",
            "id": req.get("id"),
            "error": {
                "code": -32603,
                "message": f"opskeeper unreachable: {type(e).__name__}: {e}",
            },
        }

    # 透传 opskeeper 的 JSON-RPC 响应
    try:
        inner_resp = json.loads(resp_body)
    except json.JSONDecodeError:
        METRICS.incr("tools_call_failure", tool=tool_name)
        return {
            "jsonrpc": "2.0",
            "id": req.get("id"),
            "error": {"code": -32603, "message": "opskeeper returned non-JSON"},
        }

    # 重组为外层 JSON-RPC 响应
    if "error" in inner_resp:
        METRICS.incr("tools_call_failure", tool=tool_name)
        return {
            "jsonrpc": "2.0",
            "id": req.get("id"),
            "error": inner_resp["error"],
        }
    METRICS.incr("tools_call_success", tool=tool_name)
    return {
        "jsonrpc": "2.0",
        "id": req.get("id"),
        "result": inner_resp.get("result"),
    }


def normalize_backend_arguments(backend_name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    """Convert deprecated plugin argument names to backend schema names."""
    if backend_name == "query_knowledge" and "top_k" in arguments:
        arguments = dict(arguments)
        arguments.setdefault("max_results", arguments.pop("top_k"))
    if backend_name == "query_promql" and "query" in arguments:
        arguments = dict(arguments)
        arguments.setdefault("expr", arguments.pop("query"))
    if backend_name in {"get_host_load", "get_host_processes"} and "host" in arguments:
        arguments = dict(arguments)
        host = arguments.pop("host")
        try:
            device_id = int(host)
        except (TypeError, ValueError) as exc:
            raise ValueError("host must be a numeric device id") from exc
        arguments.setdefault("device_ids", [device_id])
    if backend_name == "get_incident_detail" and "incident_id" in arguments:
        arguments = dict(arguments)
        incident_id = arguments.pop("incident_id")
        if isinstance(incident_id, list):
            incident_ids = incident_id
        else:
            try:
                incident_ids = [int(incident_id)]
            except (TypeError, ValueError) as exc:
                raise ValueError("incident_id must be numeric") from exc
        arguments.setdefault("incident_ids", incident_ids)
    return arguments


HANDLERS = {
    "initialize": handle_initialize,
    "tools/list": handle_tools_list,
    "tools/call": handle_tools_call,
}


def main() -> int:
    """stdio MCP server 主循环：read JSON line → dispatch → write JSON line。"""
    log.info(
        "opskeeper-teamharness MCP stdio server starting; backend=%s",
        get_backend_url(),
    )

    # 启动健康检查：best-effort，失败不阻塞启动（Worker 业务决定要不要继续）
    if not health_check():
        log.warning(
            "opskeeper backend unreachable at startup; tools/call may fail. "
            "Set OPSKEEPER_SKIP_HEALTH_CHECK=1 to silence."
        )

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError as e:
            err = {
                "jsonrpc": "2.0",
                "id": None,
                "error": {"code": -32700, "message": f"parse error: {e}"},
            }
            sys.stdout.write(json.dumps(err, ensure_ascii=False) + "\n")
            sys.stdout.flush()
            continue

        method = req.get("method", "")
        handler = HANDLERS.get(method)
        if handler is None:
            resp = {
                "jsonrpc": "2.0",
                "id": req.get("id"),
                "error": {"code": -32601, "message": f"method not found: {method}"},
            }
        else:
            try:
                resp = handler(req)
            except Exception as e:
                log.exception("handler failed for method=%s", method)
                resp = {
                    "jsonrpc": "2.0",
                    "id": req.get("id"),
                    "error": {
                        "code": -32603,
                        "message": f"internal error: {type(e).__name__}: {e}",
                    },
                }

        sys.stdout.write(json.dumps(resp, ensure_ascii=False) + "\n")
        sys.stdout.flush()

    return 0


if __name__ == "__main__":
    sys.exit(main())
