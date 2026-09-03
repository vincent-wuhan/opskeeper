#!/usr/bin/env python3
"""opskeeper-teamharness Skill Registry — Nacos + 本地降级。

设计目标（手册要求：Skill 工程体系 & 生态复用 25%）：

  - Skill 资产从插件 zip 内嵌改为**运行时从 Nacos 拉取**，支持热加载 / 版本切换。
  - 离线 / Nacos 不可达时降级到本地 `plugins/opskeeper-teamharness/skills/`。
  - 30 秒一次 polling refresh（可配），变更触发 callback。

Wire 协议：

  Nacos 2.x Config:
    Group:    opskeeper
    Namespace: skills-<env>           （dev/staging/prod）
    DataId:   <skill-id>.yaml          （例: opskeeper-investigator.yaml）
    Content:  见 skill_meta.yaml 规范

  skill_meta.yaml schema：

    ```yaml
    name: opskeeper-investigator
    version: 1.2.0
    description: ...
    inputs:
      incident_id: {type: string, required: true}
    outputs:
      RootCauseJSON: {type: object}
    sample_inputs:
      - {incident_id: "inc-001"}
    est_cost_tokens: 4000
    blast_radius_default: cluster
    safety_level_default: L0
    artifacts:
      skill_md_path: skills/agent/opskeeper-investigator/SKILL.md
    ```

本模块提供：

  - SkillRegistry.list_skills() / get_skill() / watch_skills()
  - NacosSkillSource（HTTP 长轮询 Config listener）
  - LocalSkillSource（本地 zip 内嵌降级）
  - CompositeSkillSource（Nacos 优先，失败回退本地）

零硬依赖：Nacos 客户端只用 ``urllib``（同 plugin 主体保持一致），不强制
``nacos-sdk-python``，避免 plugin 体积膨胀。
"""
from .sources import (
    LocalSkillSource,
    NacosSkillSource,
    CompositeSkillSource,
)
from .schema import SkillMeta, SkillNotFoundError

__all__ = [
    "LocalSkillSource",
    "NacosSkillSource",
    "CompositeSkillSource",
    "SkillMeta",
    "SkillNotFoundError",
]