from __future__ import annotations

import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parent


def _skill(relative_path: str) -> str:
    return (ROOT / relative_path).read_text(encoding="utf-8")


class PgPoolContractTest(unittest.TestCase):
    def test_coordination_defines_closed_loop_and_safe_recovery(self) -> None:
        content = _skill("team/opskeeper-coordination/SKILL.md")
        for expected in (
            "PostgreSQL 连接池耗尽分支",
            "fault_family=capacity/connection_pool",
            "pool_manifest_id",
            "proposal_id",
            "OPSKEEPER_PERMISSION_MODE=read_only",
            "recovery.execute",
            "resize_pool",
            "禁止重启共享 PostgreSQL",
            "waiters 下降",
            "由 Manager\n调度 Worker",
        ):
            with self.subTest(expected=expected):
                self.assertIn(expected, content)

    def test_investigator_requires_pool_evidence(self) -> None:
        content = _skill("agent/opskeeper-investigator/SKILL.md")
        for expected in (
            "postgres.analyze_status",
            "loop.investigate",
            "capacity/connection_pool",
            "pool_manifest_id",
            "pg_stat_activity",
            "区分应用池耗尽与共享数据库容量不足",
        ):
            with self.subTest(expected=expected):
                self.assertIn(expected, content)

    def test_repairer_is_proposal_bound_and_case_scoped(self) -> None:
        content = _skill("agent/opskeeper-repairer/SKILL.md")
        for expected in (
            "recovery.execute",
            "proposal_id",
            "skip_audit",
            "resize_pool",
            "pool_manifest_id",
            "action_fingerprint",
            "禁止重启共享 PostgreSQL",
        ):
            with self.subTest(expected=expected):
                self.assertIn(expected, content)

    def test_verifier_requires_recovery_signals_not_command_success(self) -> None:
        content = _skill("agent/opskeeper-verifier/SKILL.md")
        for expected in (
            "probe success",
            "active/capacity",
            "waiters",
            "请求延迟",
            "pass=false",
        ):
            with self.subTest(expected=expected):
                self.assertIn(expected, content)

    def test_stage_workers_record_incident_timeline(self) -> None:
        expectations = {
            "agent/opskeeper-alerter/SKILL.md": ("incident.record",),
            "agent/opskeeper-investigator/SKILL.md": ("incident.record",),
            "agent/opskeeper-reviewer/SKILL.md": ("incident.record",),
            "agent/opskeeper-repairer/SKILL.md": ("incident.record", "action_fingerprint"),
            "agent/opskeeper-verifier/SKILL.md": ("incident.record", "recovery_signal=true"),
            "agent/opskeeper-postmortem/SKILL.md": ("incident.record", "incident.closed"),
        }
        for relative_path, expected in expectations.items():
            with self.subTest(path=relative_path):
                content = _skill(relative_path)
                for text in expected:
                    self.assertIn(text, content)


if __name__ == "__main__":
    unittest.main()
