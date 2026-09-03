#!/usr/bin/env python3
"""opskeeper-teamharness audit/ledger.py 单元测试。

覆盖：
  - append 写文件 + hash chain 连续
  - verify_chain 通过 / 检测篡改（删行 / 改字段）
  - 进程重启后能恢复 hash chain（_restore_last_hash）
  - Nacos 同步失败不阻塞 append

运行：
  python3 plugins/opskeeper-teamharness/audit/test_ledger.py
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
import threading
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
PLUGIN_ROOT = HERE.parent
sys.path.insert(0, str(PLUGIN_ROOT))

from audit.ledger import (  # noqa: E402
    AuditLedger, GENESIS_HASH, _hash_record,
)


class BasicAppendTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.ledger = AuditLedger(self.tmp)

    def test_append_writes_file(self):
        self.ledger.append(actor="m", action="dispatch", task_id="inc-1")
        today = sorted(Path(self.tmp).glob("*.ndjson"))
        self.assertEqual(len(today), 1)
        lines = today[0].read_text(encoding="utf-8").strip().split("\n")
        self.assertEqual(len(lines), 1)
        obj = json.loads(lines[0])
        self.assertEqual(obj["actor"], "m")
        self.assertEqual(obj["action"], "dispatch")
        self.assertEqual(obj["task_id"], "inc-1")
        self.assertEqual(obj["prev_hash"], GENESIS_HASH)
        self.assertTrue(obj["hash"].startswith("sha256:"))

    def test_chain_continuity(self):
        r1 = self.ledger.append(actor="m", action="dispatch", task_id="inc-1")
        r2 = self.ledger.append(actor="m", action="approve", task_id="inc-1")
        self.assertEqual(r2.prev_hash, r1.hash)
        r3 = self.ledger.append(actor="m", action="mutate", task_id="inc-1")
        self.assertEqual(r3.prev_hash, r2.hash)


class VerifyChainTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.ledger = AuditLedger(self.tmp)

    def test_verify_chain_clean(self):
        for i in range(5):
            self.ledger.append(actor="m", action="x", task_id=f"t{i}")
        ok, msg = self.ledger.verify_chain()
        self.assertTrue(ok, msg)

    def test_verify_detects_tampering_modify_field(self):
        self.ledger.append(actor="m", action="x", task_id="t1")
        self.ledger.append(actor="m", action="x", task_id="t2")
        path = sorted(Path(self.tmp).glob("*.ndjson"))[0]
        # 篡改第二行的 actor
        lines = path.read_text(encoding="utf-8").strip().split("\n")
        obj = json.loads(lines[1])
        obj["actor"] = "evil"
        lines[1] = json.dumps(obj, ensure_ascii=False)
        path.write_text("\n".join(lines) + "\n", encoding="utf-8")
        ok, msg = self.ledger.verify_chain()
        self.assertFalse(ok)
        self.assertIn("line 2", msg)

    def test_verify_detects_tampering_delete_line(self):
        self.ledger.append(actor="m", action="x", task_id="t1")
        self.ledger.append(actor="m", action="x", task_id="t2")
        self.ledger.append(actor="m", action="x", task_id="t3")
        path = sorted(Path(self.tmp).glob("*.ndjson"))[0]
        lines = path.read_text(encoding="utf-8").strip().split("\n")
        del lines[1]  # 删中间一行
        path.write_text("\n".join(lines) + "\n", encoding="utf-8")
        ok, msg = self.ledger.verify_chain()
        self.assertFalse(ok)


class RestoreChainTests(unittest.TestCase):
    def test_restore_after_restart(self):
        tmp = tempfile.mkdtemp()
        l1 = AuditLedger(tmp)
        l1.append(actor="m", action="x", task_id="t1")
        r1 = l1.append(actor="m", action="x", task_id="t2")
        # 重启：新建 ledger 实例
        l2 = AuditLedger(tmp)
        r2 = l2.append(actor="m", action="x", task_id="t3")
        # 新 ledger 应能续上链（prev_hash == r1.hash）
        self.assertEqual(r2.prev_hash, r1.hash)


class ConcurrencyTests(unittest.TestCase):
    def test_thread_safe_append(self):
        tmp = tempfile.mkdtemp()
        ledger = AuditLedger(tmp)
        errors: list[Exception] = []

        def writer(prefix: str):
            try:
                for i in range(10):
                    ledger.append(actor=prefix, action="x", task_id=f"{prefix}-{i}")
            except Exception as e:
                errors.append(e)

        threads = [threading.Thread(target=writer, args=(f"t{i}",)) for i in range(5)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertEqual(errors, [])
        ok, msg = ledger.verify_chain()
        self.assertTrue(ok, msg)


class NacosSyncFailureTests(unittest.TestCase):
    def test_nacos_unreachable_does_not_block(self):
        tmp = tempfile.mkdtemp()
        # Nacos 指向不存在端口 — append 必须仍然成功
        ledger = AuditLedger(tmp, nacos_server_addr="127.0.0.1:1")
        rec = ledger.append(actor="m", action="dispatch", task_id="t1")
        self.assertTrue(rec.hash)
        ok, msg = ledger.verify_chain()
        self.assertTrue(ok, msg)


if __name__ == "__main__":
    unittest.main(verbosity=2)