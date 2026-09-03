#!/usr/bin/env python3
"""opskeeper-teamharness L0–L3 SafetyLevel 一等公民定义。

设计目标：
  - 把 ``blast_radius`` 二元判断升级为 4 级安全阶梯，对齐 OpsPilot Zero 等
    业界 AgentTeam 实践（手册明确推荐）。
  - 任何 dispatch / HITL 决策前必须先解析 SafetyLevel，再走对应流程。
  - 默认 ``blast_radius → level`` 映射表，但允许 opskeeper-teamharness 用户
    按 SKILL metadata 覆盖（见 ``skill_meta.yaml`` 的 ``safety_level`` 字段）。

Level 含义（v1）：

  - L0_readonly       只读诊断：metric.query / incident.get / knowledge.query；
                       无需 review，无需 HITL。
  - L1_low_risk_auto  低风险自动执行：单 host 范围、非破坏性（重启只读服务、清
                       缓存、调日志级别）；无需 HITL，但走 audit。
  - L2_canary_approval 灰度 + 双签：cluster / tenant_wide 范围的修复；必须经
                       opskeeper-reviewer.approve=true + HITL 双签才能执行。
  - L3_plan_only      只生成方案：根因未定、置信度 < 0.6、或者破坏性操作；
                       Worker 只产出 plan，禁止调任何 mutating 工具。

blast_radius → level 默认映射：

  - host         → L1
  - service      → L1
  - cluster      → L2
  - tenant_wide  → L2
  - region       → L3（破坏面太大，默认升到 L3）
  - account      → L3

confidence 也会影响 level：

  - confidence >= 0.85 → 不升级
  - 0.6 <= confidence < 0.85 → 不升级但 reviewer 必看
  - confidence < 0.6 → 强制升一级（如 L1→L2，L2→L3）

用法：

  >>> from safety.levels import SafetyLevel, resolve_safety_level
  >>> level = resolve_safety_level(blast_radius="cluster", confidence=0.92)
  >>> assert level == SafetyLevel.L2_canary_approval
  >>> level.requires_hitl()
  True
  >>> level.allows_mutating_without_reviewer()
  False
"""
from __future__ import annotations

from enum import Enum
from typing import Optional


class SafetyLevel(str, Enum):
    """4 级安全阶梯。字符串值是 wire format（state.json / hitl.decide 入参）。"""

    L0_readonly = "L0"
    L1_low_risk_auto = "L1"
    L2_canary_approval = "L2"
    L3_plan_only = "L3"

    @property
    def rank(self) -> int:
        return {
            SafetyLevel.L0_readonly: 0,
            SafetyLevel.L1_low_risk_auto: 1,
            SafetyLevel.L2_canary_approval: 2,
            SafetyLevel.L3_plan_only: 3,
        }[self]

    def requires_reviewer(self) -> bool:
        """是否需要 opskeeper-reviewer 审批。"""
        return self.rank >= SafetyLevel.L2_canary_approval.rank

    def requires_hitl(self) -> bool:
        """是否需要 HITL 双签。仅 L2 需要：L3 是 plan-only（不执行，何须签）。"""
        return self == SafetyLevel.L2_canary_approval

    def allows_mutating_without_reviewer(self) -> bool:
        """是否允许跳过 reviewer 直接执行 mutating 调用。"""
        return self == SafetyLevel.L1_low_risk_auto

    def allows_any_mutating(self) -> bool:
        """是否允许执行任何 mutating 调用。仅 L1 和 L2：L0 只读，L3 plan-only。"""
        return self in (SafetyLevel.L1_low_risk_auto, SafetyLevel.L2_canary_approval)

    def describe(self) -> str:
        return {
            SafetyLevel.L0_readonly: "只读诊断，无需审批",
            SafetyLevel.L1_low_risk_auto: "低风险自动执行（单 host 非破坏性），走 audit",
            SafetyLevel.L2_canary_approval: "灰度 + reviewer.approve + HITL 双签",
            SafetyLevel.L3_plan_only: "只生成方案，禁止 mutating",
        }[self]


# blast_radius → 默认 SafetyLevel
BLAST_RADIUS_TO_LEVEL: dict[str, SafetyLevel] = {
    "host": SafetyLevel.L1_low_risk_auto,
    "service": SafetyLevel.L1_low_risk_auto,
    "cluster": SafetyLevel.L2_canary_approval,
    "tenant_wide": SafetyLevel.L2_canary_approval,
    "region": SafetyLevel.L3_plan_only,
    "account": SafetyLevel.L3_plan_only,
}


# confidence 阈值
CONFIDENCE_LOW = 0.6       # 低于此值强制升级
CONFIDENCE_REVIEW = 0.85   # 0.6–0.85 之间：reviewer 必看但不升级


def resolve_safety_level(
    blast_radius: Optional[str] = None,
    confidence: Optional[float] = None,
    *,
    skill_default: Optional[SafetyLevel] = None,
    destructive: bool = False,
) -> SafetyLevel:
    """计算最终 SafetyLevel。

    Args:
        blast_radius: incident.labels.blast_radius，取自 opskeeper 后端。
        confidence: investigator 输出的根因置信度（0–1）。
        skill_default: Skill metadata 显式声明的默认 level（覆盖 blast_radius 默认）。
        destructive: 是否为破坏性操作（如 DROP / DELETE），强制升到 L2+。

    优先级（高 → 低）：

      1. destructive=true → 最低 L2_canary_approval
      2. skill_default（如有）→ 取 skill_default
      3. blast_radius → 默认映射表
      4. confidence < 0.6 → 在前面结果基础上强制 +1 级（L3 封顶）
    """
    # 1) destructive 强制 L2+
    if destructive:
        base = SafetyLevel.L2_canary_approval
    # 2) skill 元数据覆盖
    elif skill_default is not None:
        base = skill_default
    # 3) blast_radius 默认映射
    elif blast_radius is not None and blast_radius in BLAST_RADIUS_TO_LEVEL:
        base = BLAST_RADIUS_TO_LEVEL[blast_radius]
    else:
        # 未指定 blast_radius → 保守默认 L1
        base = SafetyLevel.L1_low_risk_auto

    # 4) 低置信度升级（封顶 L3）
    if confidence is not None and confidence < CONFIDENCE_LOW:
        if base.rank < SafetyLevel.L3_plan_only.rank:
            base = SafetyLevel(
                [l for l in SafetyLevel if l.rank == base.rank + 1][0]
            )

    return base


def dispatch_decision(level: SafetyLevel) -> dict:
    """返回 Manager dispatch 决策（喂给 opskeeper-coordination 决策表）。

    Returns:
        {
          "requires_reviewer": bool,
          "requires_hitl": bool,
          "can_run_mutating": bool,
          "plan_only": bool,
          "audit_level": "info" | "warn" | "critical"
        }
    """
    return {
        "requires_reviewer": level.requires_reviewer(),
        "requires_hitl": level.requires_hitl(),
        "can_run_mutating": level.allows_any_mutating()
        and (
            level.allows_mutating_without_reviewer()
            or level.requires_reviewer()  # reviewer 通过后即可
        ),
        "plan_only": level == SafetyLevel.L3_plan_only,
        "audit_level": {
            SafetyLevel.L0_readonly: "info",
            SafetyLevel.L1_low_risk_auto: "info",
            SafetyLevel.L2_canary_approval: "warn",
            SafetyLevel.L3_plan_only: "critical",
        }[level],
    }


__all__ = [
    "SafetyLevel",
    "BLAST_RADIUS_TO_LEVEL",
    "CONFIDENCE_LOW",
    "CONFIDENCE_REVIEW",
    "resolve_safety_level",
    "dispatch_decision",
]


if __name__ == "__main__":
    # 自检 + 用例展示
    cases = [
        ("host", 0.95, False, None),
        ("cluster", 0.95, False, None),
        ("cluster", 0.92, True, None),     # destructive → L2
        ("cluster", 0.5, False, None),     # 低置信度 → L3
        ("region", 0.95, False, None),     # region → L3
        ("host", 0.7, False, None),        # 0.6–0.85 → L1（不升级）
        (None, None, False, SafetyLevel.L0_readonly),  # skill 默认
    ]
    for br, conf, destr, sk in cases:
        lvl = resolve_safety_level(
            blast_radius=br, confidence=conf, skill_default=sk, destructive=destr,
        )
        print(f"blast={br} conf={conf} destr={destr} skill={sk} → {lvl.value} ({lvl.describe()})")