#!/usr/bin/env python3
"""alert_storm 剧本专用 fake backend（HTTP 18443）。

模拟 opskeeper backend 暴露的最小 endpoints：
  - /healthz
  - /v1/alerts/webhook (POST, 接收 Prometheus/AM 告警)
  - /v1/state/<id> (PUT, GET)
  - /v1/mcp (POST, JSON-RPC for plugin stdio MCP proxy)
"""
from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse

INCIDENTS: dict[str, dict] = {}
STATE: dict[str, dict] = {}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args, **kwargs):  # silence stderr noise
        pass

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
        if p.path == "/v1/alerts/webhook":
            # 简单 dedup by host
            for alert in data.get("alerts", []):
                host = alert.get("labels", {}).get("host", "unknown")
                INCIDENTS.setdefault(host, []).append(alert)
            self._json(200, {"received": len(data.get("alerts", []))})
            return
        if p.path == "/v1/mcp":
            self._json(200, {"jsonrpc": "2.0", "id": data.get("id"), "result": {"echo": True}})
            return
        self._json(404, {"error": "not found"})


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 18443), Handler).serve_forever()