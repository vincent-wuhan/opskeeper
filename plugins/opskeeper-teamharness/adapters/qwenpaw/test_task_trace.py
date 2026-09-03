"""task_trace.py 集成测试。

启动 fake opskeeper，验证 on_task_start / on_task_end：
  1. 实际打到 backend（不是 /v1/audit/events 老路径）
  2. 包含 Bearer + signature + trace context 头
  3. body 是合法 state.json，audit 数组追加 event
"""
import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

# Ensure plugin paths importable
HERE = os.path.dirname(os.path.abspath(__file__))
PLUGIN_MCP = os.path.join(HERE, "plugins", "opskeeper-teamharness", "mcp")
sys.path.insert(0, PLUGIN_MCP)

# Make task_trace importable
ADAPTER = os.path.join(HERE, "plugins", "opskeeper-teamharness", "adapters", "qwenpaw")
sys.path.insert(0, ADAPTER)


class FakeBackend(BaseHTTPRequestHandler):
    """记录所有收到的请求 path + headers + body。"""

    events = []

    def do_GET(self):
        self.__record("GET", b"")
        if self.path.startswith("/v1/state/"):
            # 返回已有 state (空 audit)
            self._send({"task_id": self.path.split("/")[-1], "audit": [], "phase": "rca"})

    def do_PUT(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n)
        self.__record("PUT", body)
        self._send({"ok": True, "audit_count": len(json.loads(body).get("audit", []))})

    def __record(self, method, body):
        FakeBackend.events.append({
            "method": method,
            "path": self.path,
            "headers": dict(self.headers),
            "body": body,
        })

    def _send(self, obj):
        out = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *a, **k):
        pass


class TaskTraceTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.backend = HTTPServer(("127.0.0.1", 0), FakeBackend)
        cls.port = cls.backend.server_address[1]
        threading.Thread(target=cls.backend.serve_forever, daemon=True).start()
        cls.env = {
            "OPSKEEPER_BACKEND_URL": f"http://127.0.0.1:{cls.port}",
            "OPSKEEPER_GATEWAY_KEY": "test-key-abc123",
            "OPSKEEPER_TENANT_ID": "t1",
            "TRACEPARENT": "00-aaaa1111aaaa1111aaaa1111aaaa1111-bbbb2222bbbb2222-01",
        }
        for k, v in cls.env.items():
            os.environ[k] = v

    @classmethod
    def tearDownClass(cls):
        cls.backend.shutdown()

    def setUp(self):
        FakeBackend.events = []
        # 重新 import task_trace 模块（确保每次拿到新 env）
        if "task_trace" in sys.modules:
            del sys.modules["task_trace"]
        from task_trace import on_task_start, on_task_end
        self.on_task_start = on_task_start
        self.on_task_end = on_task_end

    def test_task_start_calls_state_put(self):
        self.on_task_start("incident-001", "opskeeper-investigator", "agent-007")
        # 应有 1 GET + 1 PUT
        methods = [e["method"] for e in FakeBackend.events]
        self.assertEqual(methods, ["GET", "PUT"], f"got {methods}")
        put = next(e for e in FakeBackend.events if e["method"] == "PUT")
        self.assertEqual(put["path"], "/v1/state/incident-001")
        # body 应是 state.json 含 audit event
        body = json.loads(put["body"])
        self.assertEqual(body["task_id"], "incident-001")
        self.assertEqual(len(body["audit"]), 1)
        self.assertEqual(body["audit"][0]["event"], "task_start")
        self.assertEqual(body["audit"][0]["actor"], "agent-007")
        self.assertEqual(body["audit"][0]["role"], "opskeeper-investigator")

    def test_task_end_calls_state_put(self):
        self.on_task_end("incident-002", "opskeeper-verifier", "completed", 12.34)
        put = next(e for e in FakeBackend.events if e["method"] == "PUT")
        body = json.loads(put["body"])
        self.assertEqual(body["task_id"], "incident-002")
        self.assertEqual(body["audit"][0]["event"], "task_end")
        self.assertIn("duration_s=12.34", body["audit"][0]["reason"])

    def test_task_trace_propagates_trace_context(self):
        """trace context 必须经 sign_request 注入 header。"""
        self.on_task_start("incident-003", "opskeeper-alerter", "agent-008")
        put = next(e for e in FakeBackend.events if e["method"] == "PUT")
        # urllib 把 header key normalize（首字母大写）
        traceparent = put["headers"].get("traceparent") or put["headers"].get("Traceparent")
        self.assertEqual(
            traceparent,
            "00-aaaa1111aaaa1111aaaa1111aaaa1111-bbbb2222bbbb2222-01",
        )

    def test_task_trace_includes_bearer(self):
        """Bearer GatewayKey + HMAC 签名仍生效。"""
        self.on_task_start("incident-004", "opskeeper-critic", "agent-009")
        put = next(e for e in FakeBackend.events if e["method"] == "PUT")
        auth = put["headers"].get("Authorization", "")
        self.assertTrue(auth.startswith("Bearer "))
        self.assertIn("test-key-abc123", auth)
        # 签名头
        self.assertIn("X-Opskeeper-Signature", put["headers"])

    def test_no_gateway_key_silently_returns_false(self):
        """无 OPSKEEPER_GATEWAY_KEY 不阻塞 Worker。"""
        saved = os.environ.pop("OPSKEEPER_GATEWAY_KEY", None)
        try:
            result = self.on_task_start("incident-005", "opskeeper-x", "agent-x")
            self.assertFalse(result)
            # 没有打到 backend
            self.assertEqual(len(FakeBackend.events), 0)
        finally:
            if saved:
                os.environ["OPSKEEPER_GATEWAY_KEY"] = saved


if __name__ == "__main__":
    unittest.main(verbosity=2)
