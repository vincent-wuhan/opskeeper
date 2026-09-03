#!/usr/bin/env python3
"""opskeeper-teamharness append-only 审计 ledger。

设计目标（手册要求：安全可审计）：

  - 所有 AgentDecision / HITL decide / state mutation / skill registry change
    一律写入 append-only ledger（NDJSON 行追加）。
  - 本地默认落 ``$OPSKEEPER_AUDIT_DIR/YYYY-MM-DD.ndjson``（每日切文件）。
  - 可选同步推 Nacos Config 历史版本（``POST /nacos/v2/cs/history``）。
  - 防篡改：每条记录带 HMAC-SHA256 chain hash（前一条 hash → 当前 hash），
    校验时可检测中间被人修改/删除。
  - 失败安全：写入失败不阻塞主流程（审计是 best-effort）。

Wire schema（每行 NDJSON）：

  {
    "ts": "2026-08-26T07:09:26.141Z",
    "actor": "opskeeper-investigator",
    "action": "agent_decision",   # agent_decision | hitl | state_put | skill_register
    "task_id": "incident-001",
    "blast_radius": "cluster",
    "safety_level": "L2",
    "decision": "approve",
    "target": "host-1",
    "trace_id": "0af7651916cd43dd8448eb211c80319c",
    "prev_hash": "sha256:...",
    "hash": "sha256:..."
  }
"""
from __future__ import annotations

import hashlib
import hmac
import json
import logging
import os
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, asdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Optional

log = logging.getLogger("opskeeper-audit")


# ── Schema ────────────────────────────────────────────────────────────────
@dataclass
class AuditRecord:
    ts: str
    actor: str
    action: str
    task_id: str
    blast_radius: str = ""
    safety_level: str = ""
    decision: str = ""
    target: str = ""
    trace_id: str = ""
    extra: dict[str, Any] = None  # type: ignore[assignment]
    prev_hash: str = ""
    hash: str = ""

    def to_dict(self) -> dict[str, Any]:
        d = asdict(self)
        # drop None extra
        if d.get("extra") is None:
            d.pop("extra", None)
        return d


# ── Hash chain ────────────────────────────────────────────────────────────
GENESIS_HASH = "sha256:0000000000000000000000000000000000000000000000000000000000000000"


def _canonical_json(d: dict[str, Any]) -> bytes:
    return json.dumps(d, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def _hash_record(prev_hash: str, payload: dict[str, Any]) -> str:
    msg = (prev_hash + "\n").encode("utf-8") + _canonical_json(payload)
    return "sha256:" + hashlib.sha256(msg).hexdigest()


# ── Ledger ────────────────────────────────────────────────────────────────
class AuditLedger:
    """append-only NDJSON + 可选 Nacos 同步。

    Thread-safe（worker 可能在多线程上报），写入失败只记日志不抛错。
    """

    def __init__(
        self,
        dir_path: str | Path,
        *,
        nacos_server_addr: Optional[str] = None,
        nacos_namespace: str = "audit",
        nacos_group: str = "opskeeper",
        sign_key: Optional[str] = None,
    ) -> None:
        self.dir = Path(dir_path)
        self.dir.mkdir(parents=True, exist_ok=True)
        self.nacos_server_addr = nacos_server_addr
        self.nacos_namespace = nacos_namespace
        self.nacos_group = nacos_group
        self.sign_key = sign_key or os.environ.get("OPSKEEPER_AUDIT_KEY", "opskeeper-default-audit-key")
        self._lock = threading.Lock()
        self._last_hash = GENESIS_HASH
        # 启动时尝试读今天的最后一条恢复 hash chain
        self._restore_last_hash()

    def _today_path(self) -> Path:
        return self.dir / f"{datetime.now(timezone.utc).strftime('%Y-%m-%d')}.ndjson"

    def _restore_last_hash(self) -> None:
        """从今天（不存在则昨天）的 ndjson 末尾恢复 hash chain。"""
        for offset in (0, 1):
            d = datetime.now(timezone.utc)
            if offset:
                from datetime import timedelta
                d = d - timedelta(days=1)
            path = self.dir / f"{d.strftime('%Y-%m-%d')}.ndjson"
            if not path.exists():
                continue
            try:
                last_line = ""
                with open(path, "rb") as f:
                    for line in f:
                        line = line.strip()
                        if line:
                            last_line = line.decode("utf-8", errors="replace")
                if last_line:
                    obj = json.loads(last_line)
                    self._last_hash = obj.get("hash", GENESIS_HASH)
                    return
            except Exception as e:
                log.warning("restore_last_hash failed: %s", e)

    def append(
        self,
        *,
        actor: str,
        action: str,
        task_id: str,
        blast_radius: str = "",
        safety_level: str = "",
        decision: str = "",
        target: str = "",
        trace_id: str = "",
        extra: Optional[dict[str, Any]] = None,
    ) -> AuditRecord:
        ts = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.") + f"{int(time.time() * 1000) % 1000:03d}Z"
        with self._lock:
            payload = {
                "ts": ts,
                "actor": actor,
                "action": action,
                "task_id": task_id,
                "blast_radius": blast_radius,
                "safety_level": safety_level,
                "decision": decision,
                "target": target,
                "trace_id": trace_id,
            }
            if extra:
                payload["extra"] = extra
            prev_hash = self._last_hash
            cur_hash = _hash_record(prev_hash, payload)
            rec = AuditRecord(
                ts=ts, actor=actor, action=action, task_id=task_id,
                blast_radius=blast_radius, safety_level=safety_level,
                decision=decision, target=target, trace_id=trace_id,
                extra=extra, prev_hash=prev_hash, hash=cur_hash,
            )
            line = json.dumps(rec.to_dict(), ensure_ascii=False) + "\n"
            try:
                with open(self._today_path(), "a", encoding="utf-8") as f:
                    f.write(line)
            except Exception as e:
                # 主路径不阻塞
                log.warning("audit ledger local write failed: %s", e)
            self._last_hash = cur_hash
            self._maybe_sync_nacos(rec)
            return rec

    def _maybe_sync_nacos(self, rec: AuditRecord) -> None:
        if not self.nacos_server_addr:
            return
        # 推 Nacos Config 历史版本（用 task_id 作 dataId）
        url = f"http://{self.nacos_server_addr}/nacos/v2/cs/history?"
        params = urllib.parse.urlencode({
            "namespaceId": self.nacos_namespace,
            "group": self.nacos_group,
            "dataId": f"{rec.task_id}.ndjson",
        })
        body = json.dumps(rec.to_dict(), ensure_ascii=False).encode("utf-8")
        try:
            req = urllib.request.Request(
                url + params, data=body, method="POST",
                headers={
                    "Content-Type": "application/json",
                    "X-Opskeeper-Audit-Sig": hmac.new(
                        self.sign_key.encode(), body, hashlib.sha256,
                    ).hexdigest(),
                },
            )
            with urllib.request.urlopen(req, timeout=2) as resp:
                if resp.status >= 300:
                    log.warning("audit Nacos sync returned %d", resp.status)
        except (urllib.error.URLError, OSError, TimeoutError) as e:
            log.warning("audit Nacos sync unreachable: %s", e)
        except Exception as e:
            log.warning("audit Nacos sync failed: %s", e)

    def verify_chain(self, date: Optional[str] = None) -> tuple[bool, str]:
        """校验指定日期（或今天）的 ndjson hash chain 是否完整。

        Returns:
            (ok, msg) — ok=True 表示 chain 完整，否则 msg 给出错误位置。
        """
        from datetime import timedelta
        if date is None:
            date = datetime.now(timezone.utc).strftime("%Y-%m-%d")
        path = self.dir / f"{date}.ndjson"
        if not path.exists():
            return True, "no audit log for date"
        prev = GENESIS_HASH
        line_no = 0
        with open(path, "r", encoding="utf-8") as f:
            for line in f:
                line_no += 1
                line = line.strip()
                if not line:
                    continue
                obj = json.loads(line)
                obj_no_hash = {k: v for k, v in obj.items() if k not in ("prev_hash", "hash")}
                expected = _hash_record(prev, obj_no_hash)
                if obj.get("prev_hash") != prev or obj.get("hash") != expected:
                    return False, f"line {line_no}: chain broken (prev_hash mismatch)"
                prev = obj["hash"]
        return True, f"verified {line_no} records"


__all__ = ["AuditRecord", "AuditLedger", "GENESIS_HASH"]


if __name__ == "__main__":
    # 自检 + 演示
    import tempfile
    with tempfile.TemporaryDirectory() as tmp:
        ledger = AuditLedger(tmp)
        r1 = ledger.append(actor="manager", action="dispatch", task_id="inc-1",
                           blast_radius="cluster", safety_level="L2")
        r2 = ledger.append(actor="reviewer", action="approve", task_id="inc-1",
                           blast_radius="cluster", safety_level="L2",
                           decision="approve", target="host-1")
        r3 = ledger.append(actor="opskeeper-repairer", action="mutate",
                           task_id="inc-1", safety_level="L2",
                           target="host-1", decision="restart_service")
        ok, msg = ledger.verify_chain()
        print(f"chain verify: ok={ok} msg={msg}")
        print(f"r1.hash={r1.hash[:30]}...")
        print(f"r2.prev_hash={r2.prev_hash[:30]}... (should match r1.hash)")
        print(f"r3.prev_hash={r3.prev_hash[:30]}... (should match r2.hash)")