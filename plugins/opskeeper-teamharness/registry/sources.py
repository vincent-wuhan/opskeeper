#!/usr/bin/env python3
"""opskeeper-teamharness Skill Registry 三种 source。

  - NacosSkillSource       从 Nacos Config 拉取（Nacos 2.x HTTP API）
  - LocalSkillSource       从本地 zip 内嵌目录读取（降级 / 默认）
  - CompositeSkillSource   Nacos 优先 + 本地 fallback

Nacos Config 协议：

  - 列出 Config：GET /nacos/v2/cs/configs?dataId=&group=&namespaceId=&pageNo=1&pageSize=100
  - 单条拉取：  GET /nacos/v2/cs/configs?dataId=<id>&group=<g>&namespaceId=<ns>
  - 长轮询：    GET /nacos/v2/cs/configs/listener （带 Listening-Configs 头）

详见 https://nacos.io/zh-cn/docs/open-api.html
"""
from __future__ import annotations

import json
import logging
import os
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable, Optional

from .schema import SkillMeta, SkillNotFoundError, parse_skill_yaml

log = logging.getLogger("opskeeper-registry")


# ── Nacos HTTP helper ──────────────────────────────────────────────────────
def _nacos_request(
    method: str, url: str, *, params: dict[str, str] | None = None,
    headers: dict[str, str] | None = None, body: bytes | None = None,
    timeout: int = 5,
) -> bytes:
    if params:
        url = url + "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(
        url, data=body, headers=headers or {}, method=method.upper(),
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.read()


# ── Nacos source ───────────────────────────────────────────────────────────
class NacosSkillSource:
    """从 Nacos 2.x HTTP API 拉取 Skill metadata。"""

    def __init__(
        self,
        server_addr: str = "127.0.0.1:8848",
        namespace: str = "skills-dev",
        group: str = "opskeeper",
        *,
        username: Optional[str] = None,
        password: Optional[str] = None,
    ) -> None:
        self.base_url = f"http://{server_addr}/nacos/v2/cs/configs"
        self.namespace = namespace
        self.group = group
        self._auth: Optional[str] = None
        if username and password:
            self._auth = "Basic " + _b64(f"{username}:{password}")

    def _headers(self) -> dict[str, str]:
        h = {"Content-Type": "application/x-www-form-urlencoded"}
        if self._auth:
            h["Authorization"] = self._auth
        return h

    def get_skill(self, name: str) -> SkillMeta:
        """按 Skill name 拉单条 meta（dataId = "<name>.yaml"）。"""
        params = {
            "dataId": f"{name}.yaml",
            "group": self.group,
            "namespaceId": self.namespace,
        }
        try:
            body = _nacos_request(
                "GET", self.base_url, params=params, headers=self._headers(),
            )
        except urllib.error.HTTPError as e:
            # 显式关闭 404 响应体，防止 ResourceWarning: implicit cleanup
            try:
                e.read()
            except Exception:
                pass
            if e.code == 404:
                raise SkillNotFoundError(name) from e
            raise
        text = body.decode("utf-8")
        meta = parse_skill_yaml(text)
        return SkillMeta(
            **{**meta.to_dict(), "source": "nacos"},
        )

    def list_skills(self) -> list[SkillMeta]:
        """列出 namespace+group 下所有 Skill meta（dataId 后缀 .yaml）。"""
        params = {
            "group": self.group,
            "namespaceId": self.namespace,
            "pageNo": "1",
            "pageSize": "200",
        }
        try:
            body = _nacos_request(
                "GET", self.base_url + "/list", params=params, headers=self._headers(),
            )
        except urllib.error.HTTPError as e:
            log.warning("Nacos list failed: HTTP %d", e.code)
            return []
        data = json.loads(body)
        page_items = data.get("data", {}).get("pageItems") or []
        metas: list[SkillMeta] = []
        for item in page_items:
            data_id = item.get("dataId", "")
            if not data_id.endswith(".yaml"):
                continue
            try:
                meta = self.get_skill(data_id[:-5])  # strip ".yaml"
            except Exception as e:
                log.warning("skip bad skill meta %s: %s", data_id, e)
                continue
            metas.append(meta)
        return metas


# ── Local source ──────────────────────────────────────────────────────────
class LocalSkillSource:
    """从插件 zip 内嵌目录读取 Skill meta（无网络依赖）。"""

    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        # 默认指向 plugins/opskeeper-teamharness/skills
        # 约定：每个 Skill 一个目录，meta 在 `<root>/<skill-name>/skill_meta.yaml`
        # （与 Nacos dataId 一致；旧路径 SKILL.md 仍保留）

    def get_skill(self, name: str) -> SkillMeta:
        path = self.root / name / "skill_meta.yaml"
        if not path.exists():
            raise SkillNotFoundError(name)
        text = path.read_text(encoding="utf-8")
        meta = parse_skill_yaml(text)
        return SkillMeta(**{**meta.to_dict(), "source": "local"})

    def list_skills(self) -> list[SkillMeta]:
        metas: list[SkillMeta] = []
        if not self.root.exists():
            return metas
        for child in sorted(self.root.iterdir()):
            if not child.is_dir():
                continue
            meta_path = child / "skill_meta.yaml"
            if not meta_path.exists():
                continue
            try:
                metas.append(self.get_skill(child.name))
            except Exception as e:
                log.warning("skip bad local skill meta %s: %s", child.name, e)
        return metas


# ── Composite source ──────────────────────────────────────────────────────
class CompositeSkillSource:
    """Nacos 优先 + 本地 fallback。

    - list_skills: 合并两边，去重（name 同名优先 Nacos）
    - get_skill:  Nacos 拿不到时回退本地
    - watch_skills: 30s 一次 polling，差异触发 callback
    """

    def __init__(
        self,
        primary: NacosSkillSource,
        fallback: LocalSkillSource,
        *,
        poll_interval_seconds: float = 30.0,
    ) -> None:
        self.primary = primary
        self.fallback = fallback
        self.poll_interval = poll_interval_seconds
        self._stop = threading.Event()
        self._last_snapshot: dict[str, str] = {}

    def get_skill(self, name: str) -> SkillMeta:
        try:
            return self.primary.get_skill(name)
        except SkillNotFoundError:
            return self.fallback.get_skill(name)
        except (urllib.error.URLError, OSError) as e:
            log.warning("Nacos unreachable for %s: %s; falling back to local", name, e)
            return self.fallback.get_skill(name)

    def list_skills(self) -> list[SkillMeta]:
        merged: dict[str, SkillMeta] = {}
        # 本地先入（兜底）
        for m in self.fallback.list_skills():
            merged[m.name] = m
        # Nacos 覆盖（更高优先级）
        try:
            for m in self.primary.list_skills():
                merged[m.name] = m
        except (urllib.error.URLError, OSError) as e:
            log.warning("Nacos list_skills failed: %s", e)
        return sorted(merged.values(), key=lambda m: m.name)

    def watch_skills(
        self,
        callback: Callable[[list[SkillMeta], list[SkillMeta]], None],
    ) -> threading.Thread:
        """启动后台 polling 线程；callback(added, removed) 在每次变化时触发。

        返回 Thread，调用方可调用 ``stop()`` 终止。
        """
        def _run() -> None:
            while not self._stop.is_set():
                try:
                    current = {m.name: m.version for m in self.list_skills()}
                    if current != self._last_snapshot and self._last_snapshot:
                        before = set(self._last_snapshot) - set(current)
                        after = set(current) - set(self._last_snapshot)
                        all_now = {m.name: m for m in self.list_skills()}
                        added = [all_now[n] for n in after]
                        removed_names = before
                        try:
                            callback(added, removed_names)
                        except Exception as e:
                            log.exception("skill watch callback failed: %s", e)
                    self._last_snapshot = current
                except Exception as e:
                    log.exception("skill watch poll iteration failed: %s", e)
                self._stop.wait(self.poll_interval)

        t = threading.Thread(target=_run, name="opskeeper-skill-watch", daemon=True)
        t.start()
        return t

    def stop(self) -> None:
        self._stop.set()


def _b64(s: str) -> str:
    import base64
    return base64.b64encode(s.encode("utf-8")).decode("ascii")


__all__ = ["NacosSkillSource", "LocalSkillSource", "CompositeSkillSource"]