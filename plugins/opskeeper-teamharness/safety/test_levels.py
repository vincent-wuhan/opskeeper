#!/usr/bin/env python3
"""opskeeper-teamharness safety/levels.py 单元测试。

覆盖：
  - 默认 blast_radius → level 映射
  - destructive=true 强制 L2+
  - confidence < 0.6 强制升级（封顶 L3）
  - skill_default 覆盖 blast_radius 默认
  - dispatch_decision 各 level 的 reviewer/HITL/mutating 标志
  - L0 / L3 不允许任何 mutating
  - 0.6–0.85 之间不升级（L1→L1，L2→L2）

运行：
  python3 plugins/opskeeper-teamharness/safety/test_levels.py
"""
from __future__ import annotations

import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))  # 让 `import safety.levels` 可用

from safety.levels import (  # noqa: E402
    BLAST_RADIUS_TO_LEVEL,
    CONFIDENCE_LOW,
    SafetyLevel,
    dispatch_decision,
    resolve_safety_level,
)


class SafetyLevelBasicTests(unittest.TestCase):
    def test_enum_values(self):
        self.assertEqual(SafetyLevel.L0_readonly.value, "L0")
        self.assertEqual(SafetyLevel.L1_low_risk_auto.value, "L1")
        self.assertEqual(SafetyLevel.L2_canary_approval.value, "L2")
        self.assertEqual(SafetyLevel.L3_plan_only.value, "L3")

    def test_rank_order(self):
        self.assertLess(SafetyLevel.L0_readonly.rank, SafetyLevel.L1_low_risk_auto.rank)
        self.assertLess(SafetyLevel.L1_low_risk_auto.rank, SafetyLevel.L2_canary_approval.rank)
        self.assertLess(SafetyLevel.L2_canary_approval.rank, SafetyLevel.L3_plan_only.rank)

    def test_requires_reviewer_hitl(self):
        self.assertFalse(SafetyLevel.L0_readonly.requires_reviewer())
        self.assertFalse(SafetyLevel.L0_readonly.requires_hitl())
        self.assertFalse(SafetyLevel.L1_low_risk_auto.requires_reviewer())
        self.assertTrue(SafetyLevel.L2_canary_approval.requires_reviewer())
        self.assertTrue(SafetyLevel.L2_canary_approval.requires_hitl())
        self.assertTrue(SafetyLevel.L3_plan_only.requires_reviewer())  # L3 也要走 reviewer 才能进 plan

    def test_allows_mutating(self):
        self.assertFalse(SafetyLevel.L0_readonly.allows_any_mutating())
        self.assertTrue(SafetyLevel.L1_low_risk_auto.allows_any_mutating())
        self.assertTrue(SafetyLevel.L2_canary_approval.allows_any_mutating())
        self.assertFalse(SafetyLevel.L3_plan_only.allows_any_mutating())

    def test_allows_mutating_without_reviewer(self):
        self.assertFalse(SafetyLevel.L0_readonly.allows_mutating_without_reviewer())
        self.assertTrue(SafetyLevel.L1_low_risk_auto.allows_mutating_without_reviewer())
        self.assertFalse(SafetyLevel.L2_canary_approval.allows_mutating_without_reviewer())
        self.assertFalse(SafetyLevel.L3_plan_only.allows_mutating_without_reviewer())


class ResolveTests(unittest.TestCase):
    def test_blast_radius_host_is_L1(self):
        self.assertEqual(
            resolve_safety_level(blast_radius="host", confidence=0.95),
            SafetyLevel.L1_low_risk_auto,
        )

    def test_blast_radius_cluster_is_L2(self):
        self.assertEqual(
            resolve_safety_level(blast_radius="cluster", confidence=0.95),
            SafetyLevel.L2_canary_approval,
        )

    def test_blast_radius_tenant_wide_is_L2(self):
        self.assertEqual(
            resolve_safety_level(blast_radius="tenant_wide", confidence=0.95),
            SafetyLevel.L2_canary_approval,
        )

    def test_blast_radius_region_is_L3(self):
        self.assertEqual(
            resolve_safety_level(blast_radius="region", confidence=0.95),
            SafetyLevel.L3_plan_only,
        )

    def test_blast_radius_account_is_L3(self):
        self.assertEqual(
            resolve_safety_level(blast_radius="account", confidence=0.95),
            SafetyLevel.L3_plan_only,
        )

    def test_destructive_forces_L2(self):
        """即便 blast_radius=host，destructive=true 也要升到 L2。"""
        self.assertEqual(
            resolve_safety_level(blast_radius="host", confidence=0.95, destructive=True),
            SafetyLevel.L2_canary_approval,
        )

    def test_confidence_low_upgrades_one_level(self):
        """confidence < 0.6 时 L1 → L2。"""
        self.assertEqual(
            resolve_safety_level(blast_radius="host", confidence=0.5),
            SafetyLevel.L2_canary_approval,
        )

    def test_confidence_low_caps_at_L3(self):
        """confidence < 0.6 时 L2 → L3（不会变 L4）。"""
        self.assertEqual(
            resolve_safety_level(blast_radius="cluster", confidence=0.3),
            SafetyLevel.L3_plan_only,
        )

    def test_confidence_in_review_band_no_upgrade(self):
        """confidence 0.6–0.85 之间不升级（保留原 level）。"""
        self.assertEqual(
            resolve_safety_level(blast_radius="cluster", confidence=0.7),
            SafetyLevel.L2_canary_approval,
        )

    def test_skill_default_overrides_blast_radius(self):
        """Skill metadata 显式 L0 时，blast_radius=cluster 也走 L0。"""
        self.assertEqual(
            resolve_safety_level(
                blast_radius="cluster", confidence=0.95,
                skill_default=SafetyLevel.L0_readonly,
            ),
            SafetyLevel.L0_readonly,
        )

    def test_destructive_overrides_skill_default(self):
        """即便 skill_default=L1，destructive=true 仍升到 L2。"""
        self.assertEqual(
            resolve_safety_level(
                blast_radius="host", confidence=0.95,
                skill_default=SafetyLevel.L1_low_risk_auto,
                destructive=True,
            ),
            SafetyLevel.L2_canary_approval,
        )

    def test_unknown_blast_radius_defaults_L1(self):
        """未识别 blast_radius → 保守默认 L1。"""
        self.assertEqual(
            resolve_safety_level(blast_radius="galaxy", confidence=0.95),
            SafetyLevel.L1_low_risk_auto,
        )

    def test_none_inputs_default_L1(self):
        """全 None → L1（保守默认）。"""
        self.assertEqual(
            resolve_safety_level(),
            SafetyLevel.L1_low_risk_auto,
        )


class DispatchDecisionTests(unittest.TestCase):
    def test_L0_dispatch(self):
        d = dispatch_decision(SafetyLevel.L0_readonly)
        self.assertFalse(d["requires_reviewer"])
        self.assertFalse(d["requires_hitl"])
        self.assertFalse(d["can_run_mutating"])
        self.assertFalse(d["plan_only"])
        self.assertEqual(d["audit_level"], "info")

    def test_L1_dispatch(self):
        d = dispatch_decision(SafetyLevel.L1_low_risk_auto)
        self.assertFalse(d["requires_reviewer"])
        self.assertFalse(d["requires_hitl"])
        self.assertTrue(d["can_run_mutating"])  # 单 host 非破坏性可走
        self.assertFalse(d["plan_only"])
        self.assertEqual(d["audit_level"], "info")

    def test_L2_dispatch(self):
        d = dispatch_decision(SafetyLevel.L2_canary_approval)
        self.assertTrue(d["requires_reviewer"])
        self.assertTrue(d["requires_hitl"])
        self.assertTrue(d["can_run_mutating"])  # reviewer 通过后即可
        self.assertFalse(d["plan_only"])
        self.assertEqual(d["audit_level"], "warn")

    def test_L3_dispatch_plan_only(self):
        d = dispatch_decision(SafetyLevel.L3_plan_only)
        self.assertTrue(d["requires_reviewer"])
        self.assertFalse(d["requires_hitl"])
        self.assertFalse(d["can_run_mutating"])
        self.assertTrue(d["plan_only"])
        self.assertEqual(d["audit_level"], "critical")


class BlastRadiusTableTests(unittest.TestCase):
    def test_blast_radius_table_complete(self):
        """blast_radius 映射表必须覆盖所有手册规范值。"""
        for br in ("host", "service", "cluster", "tenant_wide", "region", "account"):
            self.assertIn(br, BLAST_RADIUS_TO_LEVEL)


if __name__ == "__main__":
    # 顺便跑一下 __main__ 自检
    unittest.main(verbosity=2)