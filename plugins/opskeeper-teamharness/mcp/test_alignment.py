#!/usr/bin/env python3
"""opskeeper-teamharness MCP name alignment tests.

无需网络，跑得快。覆盖：

  - tools.py 列出的每个工具名都能在 backend 或 plugin native 解析（无悬挂名字）
  - NAME_REMAP 中 plugin 端名 → backend 名 的映射正确
  - PLUGIN_NATIVE 路由配置正确（method/path_template/path_param）
  - 端到端 stdio MCP subprocess：fake backend 收真实 backend 工具名

运行：
  python3 plugins/opskeeper-teamharness/mcp/test_alignment.py
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Thread

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from names import (  # noqa: E402
    NAME_REMAP,
    PLUGIN_NATIVE,
    is_plugin_native,
    native_route,
    resolve_backend_name,
)
from tools import get_tools  # noqa: E402


class NameResolutionTests(unittest.TestCase):
    def test_resolve_backend_name_known_remaps(self):
        """Known plugin→backend remaps resolve to backend name."""
        cases = {
            "metric.query": "query_promql",
            "incident.list": "query_incidents",
            "incident.get": "get_incident_detail",
            "postgres.analyze_status": "analyze_database_status",
            "host.get_load": "get_host_load",
            "host.get_processes": "get_host_processes",
            "knowledge.query": "query_knowledge",
        }
        for plugin_name, backend_name in cases.items():
            self.assertEqual(
                resolve_backend_name(plugin_name), backend_name,
                f"plugin {plugin_name!r} should remap to {backend_name!r}",
            )

    def test_resolve_backend_name_passthrough(self):
        """plugin name == backend name 直接透传。"""
        for n in ("loop.investigate", "loop.correlate", "recovery.verify",
                  "recovery.execute", "host.restart_service"):
            self.assertEqual(resolve_backend_name(n), n)

    def test_resolve_backend_name_plugin_native_returns_none(self):
        """plugin native tool → None (表示不走 /v1/mcp)。"""
        for n in ("hitl.decide", "state.put", "state.get", "incident.record"):
            self.assertIsNone(resolve_backend_name(n))

    def test_is_plugin_native(self):
        for n in ("hitl.decide", "state.put", "state.get", "incident.record"):
            self.assertTrue(is_plugin_native(n))
        for n in ("loop.investigate", "metric.query"):
            self.assertFalse(is_plugin_native(n))

    def test_native_route_required_keys(self):
        for name, route in PLUGIN_NATIVE.items():
            self.assertIn("method", route, f"{name} missing method")
            self.assertIn("path_template", route, f"{name} missing path_template")
            self.assertIn("kind", route, f"{name} missing kind")
            # PUT/POST 必须有 body 字段定义
            self.assertTrue(
                route["method"] == "GET" or route.get("body_from_args") or route.get("body_field"),
                f"{name} PUT/POST 工具未声明 body 来源",
            )

    def test_incident_record_native_route(self):
        route = native_route("incident.record")
        self.assertIsNotNone(route)
        self.assertEqual(route["method"], "POST")
        self.assertEqual(route["path_template"], "/api/v1/incidents/events")
        self.assertTrue(route.get("body_from_args"))

    def test_no_overlap_between_native_and_remap(self):
        """plugin native 不应在 NAME_REMAP 中（避免歧义）。"""
        for name in PLUGIN_NATIVE:
            self.assertNotIn(name, NAME_REMAP, f"{name} 同时在 NAME_REMAP 和 PLUGIN_NATIVE")


class ToolCatalogConsistencyTests(unittest.TestCase):
    """tools.py 列出的每个工具都能 resolve 到 backend 或 plugin native。"""

    def setUp(self):
        self.tool_names = {t["name"] for t in get_tools()}

    def test_every_tool_resolvable(self):
        """tools.py 里的每个 name 要么有 backend remap，要么是 plugin native，要么同名透传。"""
        for name in self.tool_names:
            is_native = is_plugin_native(name)
            backend = resolve_backend_name(name)
            self.assertTrue(
                is_native or backend is not None,
                f"tool {name!r} 既非 plugin native 又无法 resolve 到 backend",
            )

    def test_no_dangling_remap_keys(self):
        """NAME_REMAP 中所有 plugin 端 name 都必须在 tools.py 出现（避免改名字孤立）。"""
        for plugin_name in NAME_REMAP:
            self.assertIn(
                plugin_name, self.tool_names,
                f"NAME_REMAP key {plugin_name!r} 不在 tools.py（孤立 remap）",
            )

    def test_no_dangling_native_keys(self):
        for name in PLUGIN_NATIVE:
            self.assertIn(
                name, self.tool_names,
                f"PLUGIN_NATIVE key {name!r} 不在 tools.py",
            )

    def test_input_schema_required_keys(self):
        """每个 tool 的 inputSchema 必须有 type=object。"""
        for t in get_tools():
            schema = t.get("inputSchema", {})
            self.assertEqual(
                schema.get("type"), "object",
                f"tool {t['name']!r} inputSchema.type != object",
            )


class FakeBackend(BaseHTTPRequestHandler):
    """记录收到的请求路径 + body + headers（用于 trace 透传验证）。

    路由前缀约定：真实 opskeeper backend 把 MCP / hitl / state / knowledge
    挂在 api.Group(...) 下，所以 plugin 走 server.py 转发时必须带 /api
    前缀（plugin mcp/PLUGIN_NATIVE 与 server.py:336 的 /api/v1/mcp 都一致）。
    本 FakeBackend 因此只接受 /api 前缀的路径。
    """

    received: list[tuple[str, str, bytes]] = []
    received_json: list[dict[str, Any]] = []
    received_headers: list[dict[str, str]] = []

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n)
        FakeBackend.received_headers.append(dict(self.headers))
        if self.path == "/api/v1/mcp":
            req = json.loads(body)
            FakeBackend.received_json.append(req)
            tool = req.get("params", {}).get("name", "?")
            FakeBackend.received.append(("POST", self.path, tool))
            r = {"jsonrpc": "2.0", "id": 1, "result": {
                "content": [{"type": "text", "text": json.dumps({"echoed": tool})}]}}
        elif self.path == "/api/v1/hitl/decide":
            FakeBackend.received.append(("POST", self.path, body[:30]))
            r = {"ok": True}
        else:
            r = {"error": "unknown POST", "path": self.path}
        self._send(r)

    def do_PUT(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n)
        FakeBackend.received_headers.append(dict(self.headers))
        FakeBackend.received.append(("PUT", self.path, body[:60]))
        self._send({"ok": True, "path": self.path})

    def do_GET(self):
        if self.path == "/_received":
            self._send(FakeBackend.received)
            return
        FakeBackend.received_headers.append(dict(self.headers))
        FakeBackend.received.append(("GET", self.path, b""))
        if self.path.startswith("/api/v1/state/"):
            self._send({"task_id": self.path.split("/")[-1], "phase": "ok"})
        else:
            self._send({"error": "unknown GET"})

    def _send(self, obj):
        out = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *a, **k):
        pass


class StdioMCPEndToEndTests(unittest.TestCase):
    """Spawn plugin stdio MCP server, send real JSON-RPC, verify fake backend sees correct names."""

    @classmethod
    def setUpClass(cls):
        cls.backend = HTTPServer(("127.0.0.1", 0), FakeBackend)
        cls.port = cls.backend.server_address[1]
        cls.thread = Thread(target=cls.backend.serve_forever, daemon=True)
        cls.thread.start()
        cls.env = {
            **os.environ,
            "OPSKEEPER_BACKEND_URL": f"http://127.0.0.1:{cls.port}",
            "OPSKEEPER_GATEWAY_KEY": "test-key",
            "OPSKEEPER_TENANT_ID": "t1",
            "OPSKEEPER_TIMEOUT": "5",
        }

    @classmethod
    def tearDownClass(cls):
        cls.backend.shutdown()
        cls.backend.server_close()

    def setUp(self):
        FakeBackend.received = []
        FakeBackend.received_json = []
        FakeBackend.received_headers = []

    def _send(self, requests: list[dict]) -> list[dict]:
        proc = subprocess.run(
            [sys.executable, os.path.join(HERE, "server.py")],
            input=("\n".join(json.dumps(r) for r in requests) + "\n").encode(),
            capture_output=True,
            env=self.env,
            timeout=10,
        )
        self.assertEqual(
            proc.returncode, 0,
            f"server.py crashed: stderr={proc.stderr.decode()!r}",
        )
        # 每行一个 JSON 响应
        return [json.loads(line) for line in proc.stdout.decode().splitlines() if line.strip()]

    def test_metric_query_remaps_to_query_promql(self):
        resp = self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "metric.query", "arguments": {"query": "up"}},
        }])[0]
        self.assertNotIn("error", resp, f"unexpected error: {resp}")
        # FakeBackend 收到的 tool 名必须是 query_promql，不是 metric.query
        backend_calls = [(m, p, body) for m, p, body in FakeBackend.received if m == "POST"]
        self.assertEqual(len(backend_calls), 1)
        method, path, tool_name = backend_calls[0]
        self.assertEqual(path, "/api/v1/mcp")
        self.assertEqual(tool_name, "query_promql",
                         f"plugin should remap metric.query → query_promql, got {tool_name!r}")
        self.assertEqual(FakeBackend.received_json[0]["params"]["arguments"], {"expr": "up"})

    def test_incident_list_remaps_to_query_incidents(self):
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "incident.list", "arguments": {"limit": 5}},
        }])
        tool_name = [body for m, p, body in FakeBackend.received if m == "POST"][0]
        self.assertEqual(tool_name, "query_incidents")

    def test_postgres_analyze_status_uses_backend_array_contract(self):
        arguments = {
            "db_types": ["postgresql"],
            "lookback_seconds": 900,
        }
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "postgres.analyze_status", "arguments": arguments},
        }])
        calls = [body for method, path, body in FakeBackend.received if method == "POST"]
        self.assertEqual(calls, ["analyze_database_status"])
        self.assertEqual(FakeBackend.received_json[0]["params"]["arguments"], arguments)

    def test_knowledge_query_remaps_to_query_knowledge(self):
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "knowledge.query", "arguments": {"query": "x", "top_k": 7}},
        }])
        request_body = FakeBackend.received_json[0]
        tool_name = request_body["params"]["name"]
        self.assertEqual(tool_name, "query_knowledge")
        self.assertEqual(request_body["params"]["arguments"], {"query": "x", "max_results": 7})

    def test_loop_investigate_passthrough(self):
        """loop.* 在 backend 和 plugin 同名，应透传不改写。"""
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {
                "name": "loop.investigate",
                "arguments": {
                    "incident_id": "i1",
                    "alert_group": ["a1"],
                    "correlation_hints": {"resource_type": "pg"},
                },
            },
        }])
        tool_name = [body for m, p, body in FakeBackend.received if m == "POST"][0]
        self.assertEqual(tool_name, "loop.investigate")

    def test_host_get_load_remaps(self):
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "host.get_load", "arguments": {"host": "1"}},
        }])
        tool_name = [body for m, p, body in FakeBackend.received if m == "POST"][0]
        self.assertEqual(tool_name, "get_host_load")
        self.assertEqual(
            FakeBackend.received_json[0]["params"]["arguments"],
            {"device_ids": [1]},
        )

    def test_incident_get_converts_numeric_id(self):
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "incident.get", "arguments": {"incident_id": "7"}},
        }])
        request_body = FakeBackend.received_json[0]
        self.assertEqual(request_body["params"]["name"], "get_incident_detail")
        self.assertEqual(request_body["params"]["arguments"], {"incident_ids": [7]})

    def test_state_get_routes_to_rest(self):
        """state.get 不走 /v1/mcp，应走 GET /v1/state/{task_id}。"""
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "state.get", "arguments": {"task_id": "inc-007"}},
        }])
        # 应该看到 GET /v1/state/inc-007，不应该有 POST /v1/mcp
        get_calls = [(m, p) for m, p, _ in FakeBackend.received
                     if m == "GET" and not p.startswith("/healthz")]
        post_calls = [(m, p) for m, p, _ in FakeBackend.received if m == "POST"]
        self.assertEqual(get_calls, [("GET", "/api/v1/state/inc-007")])
        self.assertEqual(post_calls, [])

    def test_state_put_routes_to_rest(self):
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {
                "name": "state.put",
                "arguments": {"task_id": "inc-008", "state": {"phase": "rca"}},
            },
        }])
        put_calls = [(m, p) for m, p, _ in FakeBackend.received if m == "PUT"]
        self.assertEqual(put_calls, [("PUT", "/api/v1/state/inc-008")])

    def test_hitl_decide_routes_to_rest(self):
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {
                "name": "hitl.decide",
                "arguments": {
                    "task_id": "inc-009", "decision": "approve",
                    "signers": ["alice"], "reason": "r",
                },
            },
        }])
        post_calls = [(m, p) for m, p, _ in FakeBackend.received if m == "POST"]
        self.assertEqual(post_calls, [("POST", "/api/v1/hitl/decide")])

    def test_unknown_tool_forwarded_to_backend(self):
        # plugin 不前置拒绝 unknown tool；透传到 backend，由 backend 决定。
        # 真实 backend 会返 tool_not_found 错误；fake backend 返 echo。
        resp = self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "totally.fake.tool", "arguments": {}},
        }])[0]
        self.assertIn("result", resp)
        # backend 收到的 tool 名应保持原样（plugin 不重写）
        post_calls = [(m, p, body) for m, p, body in FakeBackend.received if m == "POST" and p == "/api/v1/mcp"]
        self.assertEqual(len(post_calls), 1)
        self.assertEqual(post_calls[0][2], "totally.fake.tool")


class TraceContextTests(unittest.TestCase):
    """验证 plugin stdio MCP server 把 trace context 透传到 backend。"""

    @classmethod
    def setUpClass(cls):
        cls.backend = HTTPServer(("127.0.0.1", 0), FakeBackend)
        cls.port = cls.backend.server_address[1]
        cls.thread = Thread(target=cls.backend.serve_forever, daemon=True)
        cls.thread.start()
        cls.env = {
            **os.environ,
            "OPSKEEPER_BACKEND_URL": f"http://127.0.0.1:{cls.port}",
            "OPSKEEPER_GATEWAY_KEY": "test-key",
            "OPSKEEPER_TENANT_ID": "t1",
            "OPSKEEPER_TIMEOUT": "5",
        }

    @classmethod
    def tearDownClass(cls):
        cls.backend.shutdown()
        cls.backend.server_close()

    def setUp(self):
        FakeBackend.received = []
        FakeBackend.received_headers = []

    def _send(self, requests, extra_env=None):
        env = dict(self.env)
        if extra_env:
            env.update(extra_env)
        proc = subprocess.run(
            [sys.executable, os.path.join(HERE, "server.py")],
            input=("\n".join(json.dumps(r) for r in requests) + "\n").encode(),
            capture_output=True,
            env=env,
            timeout=10,
        )
        self.assertEqual(proc.returncode, 0,
                         f"server.py crashed: stderr={proc.stderr.decode()!r}")
        return [json.loads(line) for line in proc.stdout.decode().splitlines() if line.strip()]

    def _get_post_headers(self):
        """返回 fake backend 最近一次 POST 收到的 headers（从 received 列表抽取）.

        FakeBackend 是 BaseHTTPRequestHandler，需要扩展来记录 headers。"""
        # 简化：从 stderr 抽 log.info 输出不可行；改为检查 audit log（写到 stderr）
        # 改为让 FakeBackend 也存 headers.
        return getattr(self, "_last_headers", {})

    def test_w3c_traceparent_propagated_to_backend(self):
        """plugin 端注入 W3C traceparent，backend 收到的 header 应包含。"""
        # 清掉 setUpClass 继承的旧 env vars，确保 TRACEPARENT 不被
        # 父类测试残留污染。
        import os as _os
        for k in ("TRACEPARENT", "LOONG_TRACE_ID", "LOONG_SPAN_ID"):
            _os.environ.pop(k, None)
        trace_id = "0af7651916cd43dd8448eb211c80319c"
        span_id = "b7ad6b7169203331"
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "metric.query", "arguments": {"query": "up"}},
        }], extra_env={"TRACEPARENT": f"00-{trace_id}-{span_id}-01"})

        # FakeBackend.received_headers 记录了最近一次 POST 的 headers
        self.assertGreater(len(FakeBackend.received_headers), 0)
        last = FakeBackend.received_headers[-1]
        self.assertEqual(
            last.get("traceparent") or last.get("Traceparent"),
            f"00-{trace_id}-{span_id}-01",
            f"traceparent 未透传；headers={last}",
        )

    def test_loong_trace_id_fallback(self):
        """plugin 端无 W3C，只有 LOONG_TRACE_ID → X-Trace-Id header。"""
        import os as _os
        for k in ("TRACEPARENT", "LOONG_TRACE_ID", "LOONG_SPAN_ID"):
            _os.environ.pop(k, None)
        self._send([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "metric.query", "arguments": {"query": "up"}},
        }], extra_env={
            "LOONG_TRACE_ID": "abc123def456abc123def456abc123de",
            "LOONG_SPAN_ID": "1234567890abcdef",
        })
        last = FakeBackend.received_headers[-1]
        self.assertEqual(last.get("X-Trace-Id") or last.get("X-Trace-id"), "abc123def456abc123def456abc123de")
        self.assertEqual(last.get("X-Span-Id") or last.get("X-Span-id"), "1234567890abcdef")
        self.assertTrue(not any(k.lower()=="traceparent" for k in last))

    def test_no_trace_context_no_headers(self):
        """无 trace context → backend 不收到任何 trace headers。"""
        import os as _os
        for k in ("TRACEPARENT", "LOONG_TRACE_ID", "LOONG_SPAN_ID"):
            _os.environ.pop(k, None)
        proc = subprocess.run(
            [sys.executable, os.path.join(HERE, "server.py")],
            input=(json.dumps({
                "jsonrpc": "2.0", "method": "tools/call", "id": 1,
                "params": {"name": "metric.query", "arguments": {"query": "up"}},
            }) + "\n").encode(),
            capture_output=True, env=self.env, timeout=10,
        )
        self.assertEqual(proc.returncode, 0)
        last = FakeBackend.received_headers[-1]
        self.assertTrue(not any(k.lower()=="traceparent" for k in last))
        self.assertTrue(not any(k.lower()=="x-trace-id" for k in last))
        self.assertTrue(not any(k.lower()=="x-span-id" for k in last))

class HealthAndRetryTests(unittest.TestCase):
    """plugin mcp/server.py 启动 health check + tools/call 自动重试。"""

    def setUp(self):
        # 端口冲突避免：每个 test 启独立 backend
        self.backend = HTTPServer(("127.0.0.1", 0), FakeBackend)
        self.port = self.backend.server_address[1]
        self.thread = Thread(target=self.backend.serve_forever, daemon=True)
        self.thread.start()
        self.env = {
            **os.environ,
            "OPSKEEPER_BACKEND_URL": f"http://127.0.0.1:{self.port}",
            "OPSKEEPER_GATEWAY_KEY": "test-key",
            "OPSKEEPER_TENANT_ID": "t1",
            "OPSKEEPER_TIMEOUT": "5",
            "OPSKEEPER_SKIP_HEALTH_CHECK": "1",  # 默认跳过 health_check 自身
        }

    def tearDown(self):
        self.backend.shutdown()
        self.backend.server_close()

    def _spawn(self, requests, extra_env=None):
        env = dict(self.env)
        if extra_env:
            env.update(extra_env)
        return subprocess.run(
            [sys.executable, os.path.join(HERE, "server.py")],
            input=("\n".join(json.dumps(r) for r in requests) + "\n").encode(),
            capture_output=True,
            env=env,
            timeout=15,
        )

    def test_health_check_skipped_when_env_set(self):
        """OPSKEEPER_SKIP_HEALTH_CHECK=1 启动时不应阻塞或报错。"""
        proc = self._spawn([
            {"jsonrpc": "2.0", "method": "tools/list", "id": 1},
        ])
        self.assertEqual(proc.returncode, 0)
        # 应有 tools/list 输出
        resp = json.loads(proc.stdout.decode().splitlines()[0])
        self.assertIn("tools", resp["result"])

    def test_health_check_warns_when_backend_unreachable(self):
        """backend 不可达时启动仍能继续（只 log warning），不阻塞 tools/call。"""
        # 故意指向不可达端口
        env = dict(self.env)
        env["OPSKEEPER_BACKEND_URL"] = "http://127.0.0.1:1"  # port 1 几乎肯定无服务
        env["OPSKEEPER_SKIP_HEALTH_CHECK"] = ""  # 开启 health check
        env["OPSKEEPER_TIMEOUT"] = "2"
        proc = subprocess.run(
            [sys.executable, os.path.join(HERE, "server.py")],
            input=(json.dumps({"jsonrpc": "2.0", "method": "tools/call", "id": 1,
                              "params": {"name": "metric.query", "arguments": {"query": "up"}}})
                   + "\n").encode(),
            capture_output=True, env=env, timeout=15,
        )
        # server 仍能处理 tools/call（即使 backend 不可达，返回 error 给 Worker）
        self.assertEqual(proc.returncode, 0)
        resp = json.loads(proc.stdout.decode().splitlines()[0])
        self.assertIn("error", resp)
        # stderr 应有 health check warning
        self.assertIn("unreachable", proc.stderr.decode().lower())

    def test_tools_call_retries_on_5xx(self):
        """5xx 错误应自动重试 1 次；fake backend 配前 1 次 500 后 200。"""
        attempt = {"n": 0}
        original_do_POST = FakeBackend.do_POST

        def do_POST_with_flaky_500(self_fb):
            attempt["n"] += 1
            if attempt["n"] == 1 and self_fb.path == "/api/v1/mcp":
                # 第一次返 500，第二次以后正常
                err = b'{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"flaky"}}'
                self_fb.send_response(500)
                self_fb.send_header("Content-Type", "application/json")
                self_fb.send_header("Content-Length", str(len(err)))
                self_fb.end_headers()
                self_fb.wfile.write(err)
                return
            original_do_POST(self_fb)

        FakeBackend.do_POST = do_POST_with_flaky_500
        try:
            proc = self._spawn([{
                "jsonrpc": "2.0", "method": "tools/call", "id": 1,
                "params": {"name": "metric.query", "arguments": {"query": "up"}},
            }], extra_env={"OPSKEEPER_MAX_RETRIES": "1", "OPSKEEPER_SKIP_HEALTH_CHECK": "1"})
            self.assertEqual(proc.returncode, 0)
            resp = json.loads(proc.stdout.decode().splitlines()[0])
            # 第二次成功
            self.assertIn("result", resp, f"expected success after retry; got {resp}")
            self.assertGreaterEqual(attempt["n"], 2, "should have retried at least once")
        finally:
            FakeBackend.do_POST = original_do_POST

    def test_tools_call_no_retry_on_4xx(self):
        """4xx 客户端错误不重试（retry 无意义）。"""
        attempt = {"n": 0}
        original_do_POST = FakeBackend.do_POST

        def do_POST_4xx(self_fb):
            attempt["n"] += 1
            if self_fb.path == "/api/v1/mcp":
                err = b'{"error":"unauthorized"}'
                self_fb.send_response(401)
                self_fb.send_header("Content-Type", "application/json")
                self_fb.send_header("Content-Length", str(len(err)))
                self_fb.end_headers()
                self_fb.wfile.write(err)
                return
            original_do_POST(self_fb)

        FakeBackend.do_POST = do_POST_4xx
        try:
            proc = self._spawn([{
                "jsonrpc": "2.0", "method": "tools/call", "id": 1,
                "params": {"name": "metric.query", "arguments": {"query": "up"}},
            }], extra_env={"OPSKEEPER_MAX_RETRIES": "3", "OPSKEEPER_SKIP_HEALTH_CHECK": "1"})
            self.assertEqual(proc.returncode, 0)
            resp = json.loads(proc.stdout.decode().splitlines()[0])
            self.assertIn("error", resp)
            # 401 不重试
            self.assertEqual(attempt["n"], 1, "4xx should not be retried")
        finally:
            FakeBackend.do_POST = original_do_POST

class MetricsTests(unittest.TestCase):
    """plugin mcp/server.py Metrics counter：tools/list / tools/call / retry / 4xx / 5xx / network error。"""

    def setUp(self):
        self.backend = HTTPServer(("127.0.0.1", 0), FakeBackend)
        self.port = self.backend.server_address[1]
        self.thread = Thread(target=self.backend.serve_forever, daemon=True)
        self.thread.start()
        self.env = {
            **os.environ,
            "OPSKEEPER_BACKEND_URL": f"http://127.0.0.1:{self.port}",
            "OPSKEEPER_GATEWAY_KEY": "test-key",
            "OPSKEEPER_TENANT_ID": "t1",
            "OPSKEEPER_TIMEOUT": "5",
            "OPSKEEPER_SKIP_HEALTH_CHECK": "1",
        }

    def tearDown(self):
        self.backend.shutdown()
        self.backend.server_close()

    def _spawn(self, requests, extra_env=None):
        env = dict(self.env)
        if extra_env:
            env.update(extra_env)
        return subprocess.run(
            [sys.executable, os.path.join(HERE, "server.py")],
            input=("\n".join(json.dumps(r) for r in requests) + "\n").encode(),
            capture_output=True, env=env, timeout=15,
        )

    def _parse_metrics_stderr(self, stderr_text):
        """从 stderr 提取 opskeeper_mcp_metrics 事件。"""
        events = []
        for line in stderr_text.splitlines():
            line = line.strip()
            if line.startswith("{") and '"opskeeper_mcp_metrics"' in line:
                try:
                    events.append(json.loads(line))
                except json.JSONDecodeError:
                    continue
        return events

    def test_metrics_counted_per_tool(self):
        """每个 tool 的 success / failure 计数正确。"""
        proc = self._spawn([
            {"jsonrpc": "2.0", "method": "tools/list", "id": 1},
            {"jsonrpc": "2.0", "method": "tools/call", "id": 2,
             "params": {"name": "metric.query", "arguments": {"query": "up"}}},
            {"jsonrpc": "2.0", "method": "tools/call", "id": 3,
             "params": {"name": "metric.query", "arguments": {"query": "down"}}},
            {"jsonrpc": "2.0", "method": "tools/call", "id": 4,
             "params": {"name": "incident.list", "arguments": {}}},
        ])
        self.assertEqual(proc.returncode, 0)
        events = self._parse_metrics_stderr(proc.stderr.decode())
        # 至少有一次 metrics emit（每次操作递增时 emit）
        self.assertGreater(len(events), 0)
        # 取最后一个 snapshot
        last = events[-1]
        snap = last
        # metric.query 应该 2 次成功
        mq = snap.get("by_tool", {}).get("metric.query", {})
        self.assertEqual(mq.get("tools_call_success"), 2, f"got {mq}")
        # incident.list 应该 1 次成功
        il = snap.get("by_tool", {}).get("incident.list", {})
        self.assertEqual(il.get("tools_call_success"), 1, f"got {il}")
        # global counters
        g = snap.get("global", {})
        self.assertGreaterEqual(g.get("tools_list_total", 0), 1)
        self.assertGreaterEqual(g.get("tools_call_success", 0), 3)

    def test_metrics_count_4xx(self):
        """4xx 错误时 tools_call_4xx 计数 +1。"""
        attempt = {"n": 0}
        original = FakeBackend.do_POST

        def do_POST_4xx(self_fb):
            attempt["n"] += 1
            if self_fb.path == "/api/v1/mcp":
                err = b'{"error":"unauthorized"}'
                self_fb.send_response(401)
                self_fb.send_header("Content-Length", str(len(err)))
                self_fb.end_headers()
                self_fb.wfile.write(err)
                return
            original(self_fb)

        FakeBackend.do_POST = do_POST_4xx
        try:
            proc = self._spawn([{
                "jsonrpc": "2.0", "method": "tools/call", "id": 1,
                "params": {"name": "metric.query", "arguments": {"query": "up"}},
            }], extra_env={"OPSKEEPER_MAX_RETRIES": "0"})
            self.assertEqual(proc.returncode, 0)
            events = self._parse_metrics_stderr(proc.stderr.decode())
            last = events[-1] if events else {}
            self.assertGreaterEqual(
                last.get("global", {}).get("tools_call_4xx", 0), 1,
                f"expected tools_call_4xx >= 1; got {last}",
            )
        finally:
            FakeBackend.do_POST = original

    def test_metrics_count_retry(self):
        """retry 路径 tools_call_retry 计数 +1。"""
        attempt = {"n": 0}
        original = FakeBackend.do_POST

        def do_POST_flaky(self_fb):
            attempt["n"] += 1
            if self_fb.path == "/api/v1/mcp":
                if attempt["n"] == 1:
                    err = b'{"error":"flaky"}'
                    self_fb.send_response(500)
                    self_fb.send_header("Content-Length", str(len(err)))
                    self_fb.end_headers()
                    self_fb.wfile.write(err)
                    return
                original(self_fb)
                return
            original(self_fb)

        FakeBackend.do_POST = do_POST_flaky
        try:
            proc = self._spawn([{
                "jsonrpc": "2.0", "method": "tools/call", "id": 1,
                "params": {"name": "metric.query", "arguments": {"query": "up"}},
            }], extra_env={"OPSKEEPER_MAX_RETRIES": "2"})
            self.assertEqual(proc.returncode, 0)
            events = self._parse_metrics_stderr(proc.stderr.decode())
            last = events[-1] if events else {}
            # retry 至少一次（attempt 1 是 500，attempt 2 是 200）
            self.assertGreaterEqual(
                last.get("global", {}).get("tools_call_retry", 0), 1,
                f"expected tools_call_retry >= 1; got {last}",
            )
            self.assertGreaterEqual(
                last.get("global", {}).get("tools_call_5xx", 0), 1,
            )
        finally:
            FakeBackend.do_POST = original

    def test_metrics_count_plugin_native(self):
        """plugin native 路由（state.get）也计数。"""
        proc = self._spawn([{
            "jsonrpc": "2.0", "method": "tools/call", "id": 1,
            "params": {"name": "state.get", "arguments": {"task_id": "inc-1"}},
        }])
        self.assertEqual(proc.returncode, 0)
        events = self._parse_metrics_stderr(proc.stderr.decode())
        last = events[-1] if events else {}
        sg = last.get("by_tool", {}).get("state.get", {})
        self.assertEqual(sg.get("plugin_native_call_success"), 1, f"got {sg}")


if __name__ == "__main__":
    unittest.main(verbosity=2)
