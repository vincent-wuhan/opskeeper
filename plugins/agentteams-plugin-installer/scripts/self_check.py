#!/usr/bin/env python3
"""Self-check for agentteams-plugin-installer Dashboard plugin."""
from __future__ import annotations
import os
import sys
import json
import zipfile
import io
from pathlib import Path

PLUGIN_DIR = Path(__file__).resolve().parent.parent
DASHBOARD_DIR = PLUGIN_DIR / "dashboard"
errors: list[str] = []


def check(cond: bool, msg: str) -> None:
    status = "OK" if cond else "FAIL"
    print(f"  [{status}] {msg}")
    if not cond:
        errors.append(msg)


def main() -> int:
    print("agentteams-plugin-installer self-check")
    print("=" * 60)

    pf = DASHBOARD_DIR / "public" / "plugin.json"
    check(pf.is_file(), f"plugin.json exists: {pf}")
    if pf.is_file():
        try:
            data = json.loads(pf.read_text())
        except json.JSONDecodeError as e:
            errors.append(f"plugin.json invalid JSON: {e}")
            data = {}
        check(data.get("apiVersion") == "dashboard.agentteams/v1",
              "plugin.json apiVersion=dashboard.agentteams/v1")
        check(data.get("kind") == "DashboardPlugin",
              "plugin.json kind=DashboardPlugin")
        check(data.get("id") == "agentteams-plugin-installer",
              "plugin.json id=agentteams-plugin-installer")
        check(bool(data.get("version", "").startswith("1.")),
              f"plugin.json version=1.x.x (got {data.get('version')})")
        entry = data.get("entry") or {}
        check(bool(entry.get("dashboard")), "plugin.json entry.dashboard declared")
        eps = set(data.get("extensionPoints") or [])
        check(eps == {"sidebar-menu", "route", "dashboard-widget", "detail-panel", "toolbar"},
              f"plugin.json covers all 5 extension points (got {sorted(eps)})")

    main_jsx = DASHBOARD_DIR / "src" / "main.jsx"
    check(main_jsx.is_file(), f"main.jsx exists: {main_jsx}")
    if main_jsx.is_file():
        text = main_jsx.read_text()
        check("export function activate" in text, "main.jsx exports activate function")
        check("export default" in text, "main.jsx has default export")
        for ep in ("registerMenuItem", "registerRoute", "registerWidget",
                   "registerDetailBlock", "registerToolbarButton"):
            check(f"api.{ep}" in text, f"main.jsx registers {ep}")

    ext_dir = DASHBOARD_DIR / "src" / "extensions"
    for f in ("sidebar-menu.jsx", "route.jsx", "dashboard-widget.jsx",
              "detail-panel.jsx", "toolbar.jsx", "api.js"):
        check((ext_dir / f).is_file(), f"extensions/{f} exists")

    check((DASHBOARD_DIR / "vite.config.mjs").is_file(), "vite.config.mjs exists")
    check((DASHBOARD_DIR / "vite-plugin-host-react.mjs").is_file(),
          "vite-plugin-host-react.mjs exists")
    check((DASHBOARD_DIR / "package.json").is_file(), "package.json exists")

    standalone = os.environ.get("OPSKEEPER_STANDALONE") == "1"
    backend_env = None if standalone else os.environ.get("OPSKEEPER_BACKEND_HANDLER_PATH")
    if standalone:
        print("  [i] backend handler check skipped in standalone release mode")
    else:
        if backend_env:
            backend = Path(backend_env)
        else:
            backend = Path(__file__).resolve().parents[3] / "internal" / "manager" / "server" / "agentteams" / "plugin_http.go"
        check(backend.is_file(), f"backend handler: {backend} (set OPSKEEPER_BACKEND_HANDLER_PATH to override)")
        if backend.is_file():
            text = backend.read_text()
            for route in ("/v1/plugins", "/v1/plugins/{id}",
                          "/v1/plugins/install", "/v1/plugins/{id}/enable",
                          "/v1/plugins/{id}/disable", "/v1/plugins/{id}/sync",
                          "/v1/plugins/{id}/push"):
                check(route in text, f"backend exposes route {route}")

    dist = DASHBOARD_DIR / "dist" / "main.js"
    if dist.is_file():
        check(dist.stat().st_size > 0, f"dist/main.js built ({dist.stat().st_size} bytes)")
    else:
        print(f"  [i] dist/main.js not yet built (run npm run build)")

    print()
    print("  [i] zip packaging smoke test")
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as zf:
        zf.writestr("plugin.json", pf.read_text() if pf.is_file() else "{}")
        if dist.is_file():
            zf.writestr("main.js", dist.read_bytes())
    size = buf.tell()
    check(size > 100, f"zip buffer non-trivial ({size} bytes)")

    test_env = None if standalone else os.environ.get("OPSKEEPER_BACKEND_TEST_PATH")
    if standalone:
        print("  [i] backend test check skipped in standalone release mode")
    else:
        if test_env:
            test_path = Path(test_env)
        else:
            test_path = Path(__file__).resolve().parents[3] / "internal" / "manager" / "server" / "agentteams" / "plugin_http_test.go"
        check(test_path.is_file(), f"backend test exists: {test_path} (set OPSKEEPER_BACKEND_TEST_PATH to override)")

    print("=" * 60)
    if errors:
        print(f"FAIL: {len(errors)} checks failed")
        for e in errors:
            print(f"  - {e}")
        return 1
    print(f"OK: all checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
