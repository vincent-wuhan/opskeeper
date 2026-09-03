#!/usr/bin/env python3
"""recovery_verify 剧本专用 fake backend（HTTP 18445）。

模拟 opskeeper backend：
  - /v1/mcp tools/call recovery.verify → 返回 VerifiedDelta
  - /v1/state/<id> PUT → 记录 postmortem phase
"""
from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse

STATE: dict[str, dict] = {}


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
        p = urlparse(self.path)
        if p.path == "/healthz":
            self._json(200, {"status": "ok"})
            return
        if p.path.startswith("/v1/state/"):
            tid = p.path[len("/v1/state/"):]
            self._json(200, {"state": STATE.get(tid, {})})
            return
        self._json(404, {"error": "not found"})

    def do_PUT(self):
        p = urlparse(self.path)
        if p.path.startswith("/v1/state/"):
            tid = p.path[len("/v1/state/"):]
            length = int(self.headers.get("Content-Length", "0"))
            STATE[tid] = json.loads(self.rfile.read(length)) if length else {}
            self._json(200, {"ok": True})
            return
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
            if tool == "recovery.verify":
                self._json(200, {
                    "jsonrpc": "2.0", "id": data.get("id"),
                    "result": {
                        "pass": True,
                        "baseline": {"pg.connection.utilization": 0.55},
                        "current": {"pg.connection.utilization": 0.42},
                        "delta": {"pg.connection.utilization": -0.13},
                        "tolerance": 0.05,
                    },
                })
                return
            if tool == "knowledge.query":
                self._json(200, {
                    "jsonrpc": "2.0", "id": data.get("id"),
                    "result": {"hits": [{"title": "Runbook: pg conn pool", "score": 0.95}]},
                })
                return
            self._json(200, {"jsonrpc": "2.0", "id": data.get("id"), "result": {"echo": tool}})
            return
        self._json(404, {"error": "not found"})


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 18445), Handler).serve_forever()