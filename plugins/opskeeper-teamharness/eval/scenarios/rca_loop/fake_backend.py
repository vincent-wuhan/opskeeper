#!/usr/bin/env python3
"""rca_loop 剧本专用 fake backend（HTTP 18444）。

模拟 opskeeper backend：
  - /v1/mcp tools/call loop.investigate → 返回 confidence=0.4（触发 critic）
  - 第二次调用返回 confidence=0.88（critic 通过）
"""
from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse

INVESTIGATE_CALL_COUNT: dict[str, int] = {}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a, **k): pass

    def _json(self, code: int, payload):
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if urlparse(self.path).path == "/healthz":
            self._json(200, {"status": "ok"})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        p = urlparse(self.path)
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length) if length else b""
        try:
            data = json.loads(body) if body else {}
        except json.JSONDecodeError:
            data = {}
        if p.path == "/v1/mcp":
            req = data.get("params") or {}
            tool = req.get("name", "")
            args = req.get("arguments") or {}
            if tool == "loop.investigate":
                inc = args.get("incident_id", "inc-001")
                INVESTIGATE_CALL_COUNT[inc] = INVESTIGATE_CALL_COUNT.get(inc, 0) + 1
                # 第1次: confidence=0.4 → 触发 critic
                # 第2次: confidence=0.88 → critic 通过
                conf = 0.4 if INVESTIGATE_CALL_COUNT[inc] == 1 else 0.88
                self._json(200, {
                    "jsonrpc": "2.0", "id": data.get("id"),
                    "result": {
                        "root_cause": "pg-conn-pool-saturation",
                        "causal_chain": [{"from": "pool-exhaust", "to": "query-timeout"}],
                        "symptom": "503 from API",
                        "confidence": conf,
                        "attempt": INVESTIGATE_CALL_COUNT[inc],
                    },
                })
                return
            self._json(200, {"jsonrpc": "2.0", "id": data.get("id"), "result": {"echo": tool}})
            return
        self._json(404, {"error": "not found"})


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 18444), Handler).serve_forever()