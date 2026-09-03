#!/usr/bin/env python3
"""opskeeper-teamharness Skill Registry 测试（fake Nacos + 本地降级）。

覆盖：
  - LocalSkillSource 读取插件 zip 内嵌 meta（用临时目录造 skill_meta.yaml）
  - NacosSkillSource 通过 fake HTTP server 拉取 + 列出
  - CompositeSkillSource：Nacos 优先、本地 fallback、版本冲突去重
  - 离线场景：Nacos 不可达 → 自动 fallback 到本地，list/get 不抛错

运行：
  python3 plugins/opskeeper-teamharness/registry/test_registry.py
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
PLUGIN_ROOT = os.path.dirname(HERE)
sys.path.insert(0, PLUGIN_ROOT)

from registry.sources import (  # noqa: E402
    CompositeSkillSource,
    LocalSkillSource,
    NacosSkillSource,
)
from registry.schema import SkillMeta, SkillNotFoundError  # noqa: E402


SAMPLE_INVESTIGATOR_YAML = """\
name: opskeeper-investigator
version: 1.1.0
description: 事故根因诊断 worker（来自 Nacos）
inputs:
  incident_id:
    type: string
    required: true
outputs:
  root_cause_json:
    type: object
    required: true
est_cost_tokens: 4000
blast_radius_default: cluster
safety_level_default: L0
artifacts:
  skill_md_path: skills/agent/opskeeper-investigator/SKILL.md
"""

SAMPLE_NEW_YAML = """\
name: opskeeper-pg-advisor
version: 0.1.0
description: Postgres 性能顾问（Nacos 独有）
inputs:
  source_id:
    type: string
    required: true
outputs:
  advice:
    type: array
est_cost_tokens: 2000
blast_radius_default: cluster
safety_level_default: L0
"""


class FakeNacosHandler(BaseHTTPRequestHandler):
    """极简 fake Nacos 2.x，仅支持本测试需要的 GET endpoints。"""

    configs: dict[str, str] = {}  # dataId → yaml text
    server_version = "2.x"

    def log_message(self, *args, **kwargs):  # silence stderr noise
        pass

    def do_GET(self):
        from urllib.parse import urlparse, parse_qs

        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)

        # GET /nacos/v2/cs/configs?dataId=&group=&namespaceId=
        if parsed.path == "/nacos/v2/cs/configs" and "dataId" in qs:
            data_id = qs["dataId"][0]
            if data_id in FakeNacosHandler.configs:
                body = FakeNacosHandler.configs[data_id].encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/x-yaml")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            else:
                err = json.dumps({"code": 404, "message": "config not found"}).encode()
                self.send_response(404)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(err)))
                self.end_headers()
                self.wfile.write(err)
            return

        # GET /nacos/v2/cs/configs/list?group=&namespaceId=&pageNo=&pageSize=
        if parsed.path == "/nacos/v2/cs/configs/list":
            page_items = []
            for data_id in FakeNacosHandler.configs:
                if not data_id.endswith(".yaml"):
                    continue
                page_items.append({
                    "dataId": data_id,
                    "group": qs.get("group", [""])[0],
                    "namespaceId": qs.get("namespaceId", [""])[0],
                })
            body = json.dumps({
                "code": 0, "message": "success",
                "data": {"pageItems": page_items, "totalCount": len(page_items)},
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        self.send_response(404)
        self.end_headers()


class LocalSourceTests(unittest.TestCase):
    def setUp(self):
        # 用临时目录造两个 Skill，验证 LocalSkillSource
        self.tmp = tempfile.mkdtemp()
        for name in ("opskeeper-alerter", "opskeeper-investigator"):
            d = os.path.join(self.tmp, name)
            os.makedirs(d, exist_ok=True)
            with open(os.path.join(d, "skill_meta.yaml"), "w") as f:
                f.write(f"""\
name: {name}
version: 1.0.0
description: {name} test fixture
""")
        self.src = LocalSkillSource(self.tmp)

    def test_list_skills(self):
        metas = self.src.list_skills()
        names = sorted(m.name for m in metas)
        self.assertEqual(names, ["opskeeper-alerter", "opskeeper-investigator"])
        self.assertTrue(all(m.source == "local" for m in metas))

    def test_get_skill(self):
        m = self.src.get_skill("opskeeper-investigator")
        self.assertEqual(m.name, "opskeeper-investigator")
        self.assertEqual(m.version, "1.0.0")
        self.assertEqual(m.source, "local")

    def test_get_missing_skill_raises(self):
        with self.assertRaises(SkillNotFoundError):
            self.src.get_skill("does-not-exist")


class NacosSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(("127.0.0.1", 0), FakeNacosHandler)
        cls.port = cls.server.server_address[1]
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self):
        FakeNacosHandler.configs = {
            "opskeeper-investigator.yaml": SAMPLE_INVESTIGATOR_YAML,
            "opskeeper-pg-advisor.yaml": SAMPLE_NEW_YAML,
        }
        self.src = NacosSkillSource(
            server_addr=f"127.0.0.1:{self.port}",
            namespace="skills-test",
            group="opskeeper",
        )

    def test_get_skill(self):
        m = self.src.get_skill("opskeeper-investigator")
        self.assertEqual(m.name, "opskeeper-investigator")
        self.assertEqual(m.version, "1.1.0")
        self.assertEqual(m.source, "nacos")
        self.assertEqual(m.blast_radius_default, "cluster")

    def test_list_skills(self):
        metas = self.src.list_skills()
        names = sorted(m.name for m in metas)
        self.assertEqual(names, ["opskeeper-investigator", "opskeeper-pg-advisor"])

    def test_get_missing_404_raises_skill_not_found(self):
        with self.assertRaises(SkillNotFoundError):
            self.src.get_skill("totally-missing")


class CompositeSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(("127.0.0.1", 0), FakeNacosHandler)
        cls.port = cls.server.server_address[1]
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self):
        # 本地只有 alerter + investigator（v1.0.0）
        self.tmp = tempfile.mkdtemp()
        for name, ver in (("opskeeper-alerter", "1.0.0"),
                          ("opskeeper-investigator", "1.0.0")):
            d = os.path.join(self.tmp, name)
            os.makedirs(d, exist_ok=True)
            with open(os.path.join(d, "skill_meta.yaml"), "w") as f:
                f.write(f"name: {name}\nversion: {ver}\ndescription: local\n")

        # Nacos 提供 investigator v1.1.0 + 独有 pg-advisor
        FakeNacosHandler.configs = {
            "opskeeper-investigator.yaml": SAMPLE_INVESTIGATOR_YAML,
            "opskeeper-pg-advisor.yaml": SAMPLE_NEW_YAML,
        }
        nacos = NacosSkillSource(
            server_addr=f"127.0.0.1:{self.port}",
            namespace="skills-test", group="opskeeper",
        )
        local = LocalSkillSource(self.tmp)
        self.composite = CompositeSkillSource(nacos, local)

    def test_nacos_overrides_local(self):
        m = self.composite.get_skill("opskeeper-investigator")
        self.assertEqual(m.version, "1.1.0")  # Nacos 1.1.0 覆盖 local 1.0.0
        self.assertEqual(m.source, "nacos")

    def test_local_only_skill(self):
        m = self.composite.get_skill("opskeeper-alerter")
        self.assertEqual(m.version, "1.0.0")
        self.assertEqual(m.source, "local")

    def test_nacos_only_skill(self):
        m = self.composite.get_skill("opskeeper-pg-advisor")
        self.assertEqual(m.source, "nacos")

    def test_list_merges_dedup(self):
        metas = self.composite.list_skills()
        names = sorted(m.name for m in metas)
        # 3 个唯一 name：alerter / investigator（来自 Nacos） / pg-advisor
        self.assertEqual(names, ["opskeeper-alerter", "opskeeper-investigator", "opskeeper-pg-advisor"])
        # investigator 应该来自 Nacos（去重后保留 Nacos 版本）
        inv = next(m for m in metas if m.name == "opskeeper-investigator")
        self.assertEqual(inv.version, "1.1.0")
        self.assertEqual(inv.source, "nacos")


class CompositeOfflineTests(unittest.TestCase):
    """Nacos 不可达时，Composite 必须 fallback 到本地，list/get 不抛错。"""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        d = os.path.join(self.tmp, "opskeeper-alerter")
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "skill_meta.yaml"), "w") as f:
            f.write("name: opskeeper-alerter\nversion: 1.0.0\ndescription: offline\n")

        # Nacos 指向不存在的端口
        nacos = NacosSkillSource(server_addr="127.0.0.1:1", namespace="x", group="y")
        local = LocalSkillSource(self.tmp)
        self.composite = CompositeSkillSource(nacos, local)

    def test_list_falls_back_to_local(self):
        metas = self.composite.list_skills()
        names = [m.name for m in metas]
        self.assertEqual(names, ["opskeeper-alerter"])

    def test_get_falls_back_to_local(self):
        m = self.composite.get_skill("opskeeper-alerter")
        self.assertEqual(m.source, "local")


class WatchTests(unittest.TestCase):
    """watch_skills 后台轮询：Nacos 新增/删除 Skill 时触发 callback。"""

    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(("127.0.0.1", 0), FakeNacosHandler)
        cls.port = cls.server.server_address[1]
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self):
        FakeNacosHandler.configs = {
            "opskeeper-investigator.yaml": SAMPLE_INVESTIGATOR_YAML,
        }
        self.tmp = tempfile.mkdtemp()
        local = LocalSkillSource(self.tmp)
        nacos = NacosSkillSource(
            server_addr=f"127.0.0.1:{self.port}", namespace="x", group="y",
        )
        self.composite = CompositeSkillSource(nacos, local, poll_interval_seconds=0.1)

    def tearDown(self):
        self.composite.stop()

    def test_callback_fires_on_nacos_change(self):
        events = []
        # 先手动初始化 snapshot（避免首次回调）
        _ = self.composite.list_skills()
        self.composite._last_snapshot = {m.name: m.version for m in self.composite.list_skills()}

        def cb(added, removed):
            events.append(([m.name for m in added], list(removed)))

        t = self.composite.watch_skills(cb)
        try:
            # 触发变化：Nacos 新增 pg-advisor
            FakeNacosHandler.configs["opskeeper-pg-advisor.yaml"] = SAMPLE_NEW_YAML
            time.sleep(0.5)  # 等 polling 触发
            self.assertGreaterEqual(len(events), 1)
            last_added, last_removed = events[-1]
            self.assertEqual(last_added, ["opskeeper-pg-advisor"])
            self.assertEqual(last_removed, [])
        finally:
            self.composite.stop()
            t.join(timeout=2)


if __name__ == "__main__":
    unittest.main(verbosity=2)