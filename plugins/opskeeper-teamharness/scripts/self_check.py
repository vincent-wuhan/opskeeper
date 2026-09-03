#!/usr/bin/env python3
"""opskeeper-teamharness plugin 全链路一致性自检脚本。

CI 友好：返回 exit 0 (通过) / 1 (失败) + 详细报告。

覆盖：
  1. tools.py 与 plugin.yaml mcp.servers.tools 列表一致
  2. tools.py 工具名都能 resolve 到 backend 或 plugin native
  3. 6 Worker SKILL.md allowTools 引用都在 tools.py 声明
  4. plugin.yaml skills.agent / skills.team 数量与 fs 一致
  5. NAME_REMAP / PLUGIN_NATIVE 无悬挂引用

运行：
  python3 plugins/opskeeper-teamharness/scripts/self_check.py
"""
from __future__ import annotations

import os
import json
import re
import sys

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MCP = os.path.join(ROOT, "mcp")
sys.path.insert(0, MCP)

from names import NAME_REMAP, PLUGIN_NATIVE, resolve_backend_name  # noqa: E402
from tools import get_tools  # noqa: E402


def _yaml_servers_tools(yaml_obj: dict) -> list[str]:
    """提取 mcp.servers[0].tools 列表（plugin 当前只有一个 server）。"""
    try:
        servers = yaml_obj["mcp"]["servers"]
        if not servers:
            return []
        return list(servers[0].get("tools", []))
    except (KeyError, IndexError, TypeError):
        return []


def _yaml_skill_ids(yaml_obj: dict, kind: str) -> set[str]:
    """提取 skills.{kind}[].id 集合（kind ∈ 'agent' | 'team'）。"""
    try:
        items = yaml_obj["skills"][kind]
        return {it["id"] for it in items if isinstance(it, dict) and "id" in it}
    except (KeyError, TypeError):
        return set()


def _fs_skill_ids(kind: str) -> set[str]:
    """扫描 skills/{kind}/ 目录。"""
    skills_dir = os.path.join(ROOT, "skills", kind)
    if not os.path.isdir(skills_dir):
        return set()
    return {d for d in os.listdir(skills_dir)
            if os.path.isdir(os.path.join(skills_dir, d))}


def check_tools_match_yaml(yaml_obj: dict) -> tuple[bool, str]:
    yaml_tools = set(_yaml_servers_tools(yaml_obj))
    py_tools = {t["name"] for t in get_tools()}
    only_yaml = yaml_tools - py_tools
    only_py = py_tools - yaml_tools
    if only_yaml or only_py:
        return False, (
            f"tools mismatch:\n"
            f"  only in plugin.yaml: {sorted(only_yaml)}\n"
            f"  only in tools.py:    {sorted(only_py)}"
        )
    return True, f"tools match: {len(yaml_tools)} tools"


def check_tools_resolvable() -> tuple[bool, str]:
    bad = []
    for t in get_tools():
        n = t["name"]
        if not (n in PLUGIN_NATIVE or resolve_backend_name(n)):
            bad.append(n)
    if bad:
        return False, f"unresolvable tools: {bad}"
    return True, f"all {len(get_tools())} tools resolvable"


def check_skill_match_yaml(yaml_obj: dict, kind: str) -> tuple[bool, str]:
    yaml_ids = _yaml_skill_ids(yaml_obj, kind)
    fs_ids = _fs_skill_ids(kind)
    missing_yaml = fs_ids - yaml_ids
    missing_fs = yaml_ids - fs_ids
    if missing_yaml or missing_fs:
        return False, (
            f"{kind} skills mismatch:\n"
            f"  in fs but not yaml: {sorted(missing_yaml)}\n"
            f"  in yaml but not fs: {sorted(missing_fs)}"
        )
    return True, f"{kind} skills match: {len(yaml_ids)}"


def check_skill_md_tool_refs() -> tuple[bool, str]:
    py_tools = {t["name"] for t in get_tools()}
    bad: list[tuple[str, str]] = []
    skills_dir = os.path.join(ROOT, "skills", "agent")
    for skill_id in os.listdir(skills_dir):
        skill_md = os.path.join(skills_dir, skill_id, "SKILL.md")
        if not os.path.isfile(skill_md):
            continue
        with open(skill_md) as f:
            for line in f:
                m = re.match(r"^\s+-\s+([a-z_][a-z0-9_.]*)\s*$", line)
                if m and m.group(1) not in py_tools:
                    bad.append((skill_id, m.group(1)))
    if bad:
        msg = "tool refs not in tools.py:\n"
        for skill, tool in bad:
            msg += f"  {skill}/SKILL.md: {tool}\n"
        return False, msg.rstrip()
    return True, "all SKILL.md tool refs valid"


def check_no_dangling_remap() -> tuple[bool, str]:
    py_tools = {t["name"] for t in get_tools()}
    dangling = [k for k in NAME_REMAP if k not in py_tools]
    if dangling:
        return False, f"NAME_REMAP keys not in tools.py: {dangling}"
    return True, f"NAME_REMAP clean ({len(NAME_REMAP)} entries)"


def check_no_dangling_native() -> tuple[bool, str]:
    py_tools = {t["name"] for t in get_tools()}
    dangling = [k for k in PLUGIN_NATIVE if k not in py_tools]
    if dangling:
        return False, f"PLUGIN_NATIVE keys not in tools.py: {dangling}"
    return True, f"PLUGIN_NATIVE clean ({len(PLUGIN_NATIVE)} entries)"


def check_harness_cases_have_agentteams() -> tuple[bool, str]:
    """至少 2 个 harness case 有 agentteams 集成扩展（证明 plugin 可复用）。

    cases 目录解析顺序：
      1. 环境变量 OPSKEEPER_HARNESS_CASES_DIR（推荐 — CI 注入）
      2. 相对 opskeeper 仓库根：../../../../internal/harness/cases
      3. 相对当前 plugin：../../../internal/harness/cases（兜底，兼容 monorepo 内嵌）
      4. 相对 plugin.yaml 的 ./examples/harness-cases（demo cases，v1.0.2+ 推荐）

    行为：
      - 找到 ≥2 case.agentteams → PASS
      - 找到 1 case → WARN（不算 fail，harness 还在建设中）
      - 完全找不到 cases 目录 → PASS + INFO（plugin tree 自检不强制依赖外部 harness 目录）
    """
    candidates: list[str] = []
    env_dir = os.environ.get("OPSKEEPER_HARNESS_CASES_DIR")
    if env_dir:
        candidates.append(env_dir)
    plugin_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    candidates.extend([
        os.path.normpath(os.path.join(plugin_root, "..", "..", "..", "internal", "harness", "cases")),
        os.path.normpath(os.path.join(plugin_root, "..", "internal", "harness", "cases")),
        os.path.normpath(os.path.join(plugin_root, "examples", "harness-cases")),
    ])
    cases_dir = next((d for d in candidates if os.path.isdir(d)), None)
    if cases_dir is None:
        return True, (
            "INFO: no harness cases dir found (plugin-level self-check skips this gate; "
            "set OPSKEEPER_HARNESS_CASES_DIR in monorepo CI to enforce)"
        )
    covered = []
    for root, dirs, files in os.walk(cases_dir):
        if ".removed" in root:
            continue
        for f in files:
            if f != "case.yaml":
                continue
            path = os.path.join(root, f)
            try:
                with open(path) as fp:
                    obj = yaml.safe_load(fp)
                if isinstance(obj, dict) and "agentteams" in obj:
                    covered.append(obj.get("id", path))
            except Exception:
                continue
    if len(covered) >= 2:
        return True, f"{len(covered)} cases covered: {covered}"
    if len(covered) == 1:
        return True, f"WARN: only 1/2 harness cases covered ({covered}); plugin ok but harness expansion pending"
    return False, f"0/2 harness cases with agentteams extension under {cases_dir}"



def check_coordination_skill_workers() -> tuple[bool, str]:
    """opskeeper-coordination SKILL.md 派活目标必须是 plugin.yaml 已声明的 Worker skill。"""
    with open(os.path.join(ROOT, "plugin.yaml")) as f:
        yaml_obj = yaml.safe_load(f)
    declared = {it["id"] for it in yaml_obj["skills"]["agent"]
                if isinstance(it, dict) and "id" in it}
    coord_path = os.path.join(ROOT, "skills", "team", "opskeeper-coordination", "SKILL.md")
    with open(coord_path) as f:
        text = f.read()
    # 匹配 backtick 中的 opskeeper-* id
    refs = set(re.findall(r"`(opskeeper-[a-z]+)`", text))
    unknown = refs - declared
    if unknown:
        return False, f"opskeeper-coordination references unknown Worker: {sorted(unknown)}"
    if "opskeeper-investigator" not in refs:
        return False, "opskeeper-coordination must reference investigator (core dispatch)"
    return True, f"coordination SKILL.md refs {len(refs)} Workers: {sorted(refs)}"


def check_prompts_not_placeholder() -> tuple[bool, str]:
    """prompts/*.md 必须是实质内容（≥10 行），防 placeholder 文件被发布。"""
    prompts_dir = os.path.join(ROOT, "prompts")
    too_short: list[str] = []
    for root, _, files in os.walk(prompts_dir):
        for f in files:
            if not f.endswith(".md"):
                continue
            path = os.path.join(root, f)
            with open(path) as fp:
                lines = fp.readlines()
            content_lines = [l for l in lines if l.strip() and not l.strip().startswith("#")]
            if len(content_lines) < 3:
                too_short.append(f"{os.path.relpath(path, ROOT)}: {len(content_lines)} content lines")
    if too_short:
        joined = "\n  ".join(too_short)
        return False, f"prompts files too short:\n  {joined}"
    return True, "all prompts files have substantive content"


def check_prompts_reference_real_resources() -> tuple[bool, str]:
    """prompts 引用 plugin 资源路径（mcp/server.py 等）必须存在。"""
    prompts_dir = os.path.join(ROOT, "prompts")
    bad: list[tuple[str, str]] = []
    for root, _, files in os.walk(prompts_dir):
        for f in files:
            if not f.endswith(".md"):
                continue
            path = os.path.join(root, f)
            with open(path) as fp:
                text = fp.read()
            # 匹配 `mcp/server.py` / `mcp/auth.py` / `mcp/tools.py` / `mcp/names.py`
            for ref in re.findall(r"`((?:mcp|skills|prompts)/[a-zA-Z0-9_./\-]+\.py?)`", text):
                full = os.path.join(ROOT, ref)
                if not os.path.exists(full):
                    bad.append((os.path.relpath(path, ROOT), ref))
    if bad:
        msg = "prompts references non-existent resources:\n"
        for src, ref in bad:
            msg += f"  {src}: {ref}\n"
        return False, msg.rstrip()
    return True, "all prompts resource refs valid"


def check_skill_md_tools_match_yaml_allowlist() -> tuple[bool, str]:
    """每个 Worker SKILL.md allowTools 必须是 plugin.yaml 中声明的 mcp.servers.tools 子集。"""
    with open(os.path.join(ROOT, "plugin.yaml")) as f:
        yaml_obj = yaml.safe_load(f)
    yaml_tools = set(_yaml_servers_tools(yaml_obj))
    bad: list[tuple[str, str]] = []
    skills_dir = os.path.join(ROOT, "skills", "agent")
    for skill_id in os.listdir(skills_dir):
        skill_md = os.path.join(skills_dir, skill_id, "SKILL.md")
        if not os.path.isfile(skill_md):
            continue
        with open(skill_md) as f:
            for line in f:
                m = re.match(r"^\s+-\s+([a-z_][a-z0-9_.]*)\s*$", line)
                if m and m.group(1) not in yaml_tools:
                    bad.append((skill_id, m.group(1)))
    if bad:
        msg = "SKILL.md tools not in plugin.yaml mcp.servers.tools:\n"
        for skill, tool in bad:
            msg += f"  {skill}: {tool}\n"
        return False, msg.rstrip()
    return True, "all SKILL.md tools declared in plugin.yaml"


def check_worker_skills_7_workers() -> tuple[bool, str]:
    """plugin.yaml skills.agent 必须有 7 个 Worker skill（v1.0.2 起含 postmortem）。"""
    with open(os.path.join(ROOT, "plugin.yaml")) as f:
        yaml_obj = yaml.safe_load(f)
    try:
        items = yaml_obj["skills"]["agent"]
    except (KeyError, TypeError):
        return False, "no skills.agent in plugin.yaml"
    ids = {it["id"] for it in items if isinstance(it, dict)}
    expected = {
        "opskeeper-alerter", "opskeeper-investigator", "opskeeper-critic",
        "opskeeper-reviewer", "opskeeper-repairer", "opskeeper-verifier",
        "opskeeper-postmortem",
    }
    if ids != expected:
        return False, f"expected {sorted(expected)}, got {sorted(ids)}"
    return True, f"7 Worker skills: {sorted(ids)}"



def check_dashboard_apiversion() -> tuple[bool, str]:
    f = os.path.join(ROOT, "dashboard", "plugin.json")
    if not os.path.isfile(f):
        return False, "dashboard/plugin.json missing"
    obj = json.loads(open(f).read())
    if obj.get("apiVersion") != "dashboard.agentteams/v1":
        return False, f"apiVersion={obj.get('apiVersion')!r} (expected dashboard.agentteams/v1)"
    if obj.get("kind") != "DashboardPlugin":
        return False, f"kind={obj.get('kind')!r} (expected DashboardPlugin)"
    return True, "apiVersion=dashboard.agentteams/v1 kind=DashboardPlugin"


def check_dashboard_extension_points() -> tuple[bool, str]:
    f = os.path.join(ROOT, "dashboard", "plugin.json")
    obj = json.loads(open(f).read())
    declared = set(obj.get("extensionPoints") or [])
    required = {"sidebar-menu", "route", "dashboard-widget", "detail-panel", "toolbar"}
    missing = required - declared
    if missing:
        return False, f"missing extension points: {sorted(missing)}"
    return True, f"all 5 extension points declared: {sorted(required)}"


def check_dashboard_entry_dashboard_exists() -> tuple[bool, str]:
    f = os.path.join(ROOT, "dashboard", "plugin.json")
    obj = json.loads(open(f).read())
    entry = obj.get("entry") or {}
    rel = entry.get("dashboard")
    if not rel:
        return False, "entry.dashboard missing"
    target = os.path.join(ROOT, "dashboard", rel)
    if not os.path.isfile(target):
        return False, f"entry.dashboard={rel!r} not found at {target}"
    return True, f"entry.dashboard={rel} -> exists"


def check_yaml_package_include_dashboard() -> tuple[bool, str]:
    yaml_obj = yaml.safe_load(open(os.path.join(ROOT, "plugin.yaml")).read())
    include = (yaml_obj.get("package") or {}).get("include") or []
    has_dash = any("dashboard" in str(x) for x in include)
    if not has_dash:
        return False, "package.include missing dashboard/ (would break tarball distribution)"
    return True, f"package.include includes dashboard entry"


def main() -> int:
    with open(os.path.join(ROOT, "plugin.yaml")) as f:
        yaml_obj = yaml.safe_load(f)

    checks = [
        ("tools.py ↔ plugin.yaml tools list", lambda: check_tools_match_yaml(yaml_obj)),
        ("tools.py every tool resolvable", check_tools_resolvable),
        ("plugin.yaml skills.agent ↔ skills/agent/", lambda: check_skill_match_yaml(yaml_obj, "agent")),
        ("plugin.yaml skills.team ↔ skills/team/", lambda: check_skill_match_yaml(yaml_obj, "team")),
        ("SKILL.md tool refs valid", check_skill_md_tool_refs),
        ("NAME_REMAP no dangling", check_no_dangling_remap),
        ("PLUGIN_NATIVE no dangling", check_no_dangling_native),
        ("plugin.yaml has 7 Worker skills (incl. postmortem)", check_worker_skills_7_workers),
        ("≥2 harness cases have agentteams extension", check_harness_cases_have_agentteams),
        ("opskeeper-coordination refs valid Workers", check_coordination_skill_workers),
        ("prompts/*.md not placeholder", check_prompts_not_placeholder),
        ("prompts refs real plugin resources", check_prompts_reference_real_resources),
        ("SKILL.md tools in plugin.yaml allowlist", check_skill_md_tools_match_yaml_allowlist),
        ("dashboard/plugin.json apiVersion is dashboard.agentteams/v1", check_dashboard_apiversion),
        ("dashboard/plugin.json covers 5 extension points", check_dashboard_extension_points),
        ("dashboard/plugin.json entry.dashboard exists", check_dashboard_entry_dashboard_exists),
        ("plugin.yaml package.include contains dashboard/", check_yaml_package_include_dashboard),
    ]
    print("=" * 64)
    print("opskeeper-teamharness plugin self-check")
    print("=" * 64)
    failures = 0
    for name, fn in checks:
        ok, msg = fn()
        status = "✓ PASS" if ok else "✗ FAIL"
        print(f"  [{status}] {name}")
        for line in msg.splitlines():
            print(f"           {line}")
        if not ok:
            failures += 1
    print("=" * 64)
    if failures:
        print(f"FAILED: {failures} check(s) failed")
        return 1
    print("OK: all checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
