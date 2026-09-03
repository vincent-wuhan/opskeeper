"""worker-entrypoint.py — mock qwenpaw runtime for Docker 演示

目的: 在 Docker compose 中提供一个**真实**暴露 opskeeper-teamharness 注册的 4 个
HTTP 端点的 worker 容器,使得 opskeeper-manager 通过 plugin sync → worker HTTP →
qwenpaw subprocess install 的完整链路可在单台机器上端到端验证。

实现要点:
- 复用真实 opskeeper-teamharness plugin.py 的 OpskeeperTeamHarnessPlugin + build_install_plugin_router
- 构造一个 mock api 对象,提供 register_prompt_section / register_skill_provider /
  register_middleware / register_runtime_hook / register_http_router 等 stub
- 调 plugin.register(mock_api) → 真实触发 _register_http → FastAPI router 挂载
- install-plugin 端点的处理逻辑完全来自 plugin.py 的 build_install_plugin_router
  (与生产 qwenpaw worker 上的代码相同)

注意: 这是演示用 mock,不是真实 qwenpaw runtime。
生产部署 worker 应使用含 qwenpaw + opskeeper-teamharness plugin 的真实镜像。
"""
from __future__ import annotations

import importlib.util
import logging
import os
import sys
import time
from pathlib import Path
from typing import Any

import uvicorn
import yaml as yamllib  # for reading plugin.yaml metadata.name
from fastapi import FastAPI

# ----- 日志 -----
_LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()
logging.basicConfig(
    level=_LOG_LEVEL,
    format="%(asctime)s %(levelname)s %(name)s | %(message)s",
)
log = logging.getLogger("worker-entrypoint")


PLUGIN_BASE = Path(os.getenv("PLUGIN_BASE", "/opt/agentteams/plugins/opskeeper-teamharness"))
PLUGIN_PATH = PLUGIN_BASE / "adapters/qwenpaw/plugin.py"
PLUGIN_YAML = PLUGIN_BASE / "plugin.yaml"


def _resolve_plugin_id() -> str:
    """读取 plugin.yaml metadata.name 作为 qwenpaw 真实行为下的 plugin id.

    qwenpaw register_http_router 框架会**忽略** plugin 调用方传入的 prefix,
    自动用 plugin.yaml 的 metadata.name 作为 /api/<name>/ 前缀。
    """
    try:
        with open(PLUGIN_YAML, "r", encoding="utf-8") as fh:
            data = yamllib.safe_load(fh)
        return str(data["metadata"]["name"])
    except Exception as exc:  # noqa: BLE001
        log.warning("failed to read plugin.yaml metadata.name, falling back to 'opskeeper-teamharness': %s", exc)
        return "opskeeper-teamharness"


PLUGIN_ID = _resolve_plugin_id()


def load_opskeeper_teamharness_plugin():
    """Import opskeeper-teamharness plugin.py 并取 singleton"""
    spec = importlib.util.spec_from_file_location("opskeeper_teamharness_plugin", PLUGIN_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"failed to load spec: {PLUGIN_PATH}")
    module = importlib.util.module_from_spec(spec)
    sys.modules["opskeeper_teamharness_plugin"] = module
    spec.loader.exec_module(module)
    return module.plugin


class MockQwenPawAPI:
    """满足 opskeeper-teamharness register(api) 调用的最小 mock api

    register_http_router 会把 FastAPI router 接到 self.routers,
    我们在 main 里把所有 routers 挂到 app。
    """

    def __init__(self) -> None:
        self.routers: list[tuple[Any, str | None, list[str] | None]] = []
        self.prompt_sections: list[dict[str, Any]] = []
        self.skill_providers: list[dict[str, Any]] = []
        self.middlewares: list[Any] = []
        self.runtime_hooks: list[Any] = []

    # qwenpaw register_* API surface
    def register_prompt_section(self, name: str, *, after: str | None = None,
                                provider: Any = None, priority: int = 0, **_: Any) -> None:
        self.prompt_sections.append({"name": name, "after": after, "priority": priority})
        log.info("mock-api: register_prompt_section name=%s after=%s priority=%d", name, after, priority)

    def register_skill_provider(self, path: Any, **_: Any) -> None:
        self.skill_providers.append({"path": str(path)})
        log.info("mock-api: register_skill_provider path=%s", path)

    def register_middleware(self, factory: Any, *, priority: int = 0, **_: Any) -> None:
        self.middlewares.append({"factory": factory, "priority": priority})
        log.info("mock-api: register_middleware priority=%d", priority)

    def register_runtime_hook(self, hook: Any) -> None:
        self.runtime_hooks.append(hook)
        log.info("mock-api: register_runtime_hook")

    def register_http_router(self, router: Any, *, prefix: str | None = None,
                             tags: list[str] | None = None, **_: Any) -> None:
        # **关键**:模拟真实 qwenpaw 框架行为 — **忽略** 调用方传入的 prefix,
        # 自动以 PLUGIN_ID(plugin.yaml metadata.name)作为 /api/<name>/ 前缀。
        # 这保证 worker 端实际路径与 AgentTeams upstream 一致:
        # /api/<plugin-id>/<inner-path> = /api/<plugin-id>/health 等
        if prefix:
            log.info(
                "mock-api: register_http_router caller prefix=%r IGNORED, "
                "qwenpaw 框架强制用 /api/%s/*",
                prefix, PLUGIN_ID,
            )
        full_prefix = f"/api/{PLUGIN_ID}"
        self.routers.append((router, full_prefix, tags))
        log.info("mock-api: mounted under %s tags=%s", full_prefix, tags)


def build_app() -> FastAPI:
    """加载 plugin → 调 register(mock_api) → 把所有 router 挂到 FastAPI app"""
    log.info("loading opskeeper-teamharness plugin from %s", PLUGIN_PATH)
    plugin = load_opskeeper_teamharness_plugin()
    api = MockQwenPawAPI()
    plugin.register(api)

    app = FastAPI(
        title="mock-qwenpaw",
        version="0.1.0",
        description="Docker 演示用 worker — 模拟 qwenpaw runtime + opskeeper-teamharness plugin",
    )

    # 把 plugin register 的所有 router 接到 app
    for router, prefix, tags in api.routers:
        kwargs: dict[str, Any] = {}
        if prefix:
            kwargs["prefix"] = prefix
        if tags:
            kwargs["tags"] = tags
        app.include_router(router, **kwargs)
        log.info("mounted router prefix=%s tags=%s", prefix, tags)

    # 自身 liveness
    @app.get("/health")
    def health() -> dict[str, Any]:
        return {
            "ok": True,
            "worker": os.getenv("WORKER_NAME", "unknown"),
            "mock_qwenpaw": True,
            "uptime_seconds": int(time.time() - START_TIME),
            "registered": {
                "prompt_sections": len(api.prompt_sections),
                "skill_providers": len(api.skill_providers),
                "middlewares": len(api.middlewares),
                "routers": len(api.routers),
            },
        }

    @app.get("/")
    def root() -> dict[str, Any]:
        return {
            "service": "mock-qwenpaw",
            "worker": os.getenv("WORKER_NAME", "unknown"),
            "endpoints": [r.path for r in app.routes if hasattr(r, "path")],
        }

    return app


START_TIME = time.time()
app = build_app()


if __name__ == "__main__":
    port = int(os.getenv("WORKER_PORT", "8088"))
    host = os.getenv("WORKER_HOST", "0.0.0.0")
    log.info("starting uvicorn on %s:%d", host, port)
    uvicorn.run(app, host=host, port=port, log_level=_LOG_LEVEL.lower())
