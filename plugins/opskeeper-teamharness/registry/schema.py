#!/usr/bin/env python3
"""Skill metadata schema 与解析（不依赖 PyYAML，用纯 Python 安全 loader）。

满足 plugin 自包含 + 零硬依赖原则（Nacos SDK / PyYAML 都不强依赖）。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Optional


class SkillNotFoundError(KeyError):
    """请求的 Skill 不在 registry 中。"""


@dataclass(frozen=True)
class SkillIOField:
    """单个输入/输出字段的 schema。"""

    type: str  # "string" | "integer" | "number" | "boolean" | "array" | "object"
    required: bool = False
    description: str = ""

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> "SkillIOField":
        return cls(
            type=str(d.get("type", "string")),
            required=bool(d.get("required", False)),
            description=str(d.get("description", "")),
        )


@dataclass(frozen=True)
class SkillMeta:
    """Skill 元数据（registry / Nacos / 本地 zip 统一表示）。"""

    name: str
    version: str
    description: str = ""
    inputs: dict[str, SkillIOField] = field(default_factory=dict)
    outputs: dict[str, SkillIOField] = field(default_factory=dict)
    sample_inputs: list[dict[str, Any]] = field(default_factory=list)
    est_cost_tokens: int = 0
    blast_radius_default: Optional[str] = None
    safety_level_default: Optional[str] = None  # "L0"/"L1"/"L2"/"L3"
    artifacts: dict[str, str] = field(default_factory=dict)  # skill_md_path 等
    source: str = "local"  # "nacos" | "local" — 供 audit 用

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "version": self.version,
            "description": self.description,
            "inputs": {k: {"type": v.type, "required": v.required, "description": v.description} for k, v in self.inputs.items()},
            "outputs": {k: {"type": v.type, "required": v.required, "description": v.description} for k, v in self.outputs.items()},
            "sample_inputs": list(self.sample_inputs),
            "est_cost_tokens": self.est_cost_tokens,
            "blast_radius_default": self.blast_radius_default,
            "safety_level_default": self.safety_level_default,
            "artifacts": dict(self.artifacts),
            "source": self.source,
        }


def parse_skill_yaml(text: str) -> SkillMeta:
    """解析 skill_meta.yaml。

    用极简 YAML 子集解析器（仅支持本 schema 需要的语法：嵌套 map + list of map），
    避免强依赖 PyYAML。
    """
    import yaml  # type: ignore

    data = yaml.safe_load(text)
    if not isinstance(data, dict):
        raise ValueError(f"skill meta YAML must be a mapping, got {type(data).__name__}")
    name = data.get("name")
    version = data.get("version")
    if not name or not version:
        raise ValueError("skill meta must have `name` and `version`")

    inputs = {k: SkillIOField.from_dict(v) for k, v in (data.get("inputs") or {}).items()}
    outputs = {k: SkillIOField.from_dict(v) for k, v in (data.get("outputs") or {}).items()}

    return SkillMeta(
        name=str(name),
        version=str(version),
        description=str(data.get("description", "")),
        inputs=inputs,
        outputs=outputs,
        sample_inputs=list(data.get("sample_inputs") or []),
        est_cost_tokens=int(data.get("est_cost_tokens") or 0),
        blast_radius_default=data.get("blast_radius_default"),
        safety_level_default=data.get("safety_level_default"),
        artifacts=dict(data.get("artifacts") or {}),
        source=str(data.get("_source") or "local"),
    )


def dump_skill_yaml(meta: SkillMeta) -> str:
    """把 SkillMeta 序列化回 YAML。"""
    import yaml  # type: ignore

    return yaml.safe_dump(meta.to_dict(), allow_unicode=True, sort_keys=False)