#!/usr/bin/env python3
from __future__ import annotations

import json
import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
AUDITOR = Path(__file__).resolve().relative_to(ROOT)
SKIPPED_PARTS = {".git", ".venv", "__pycache__", ".pytest_cache", "node_modules"}
SKIPPED_SUFFIX_PARTS = {
    ("web", "dist"),
    ("plugins", "agentteams-plugin-installer", "dashboard", "dist"),
    ("plugins", "opskeeper-teamharness", "dist"),
}
ONGRID_ALLOWLIST = {
    Path("NOTICE.md"),
    Path("TRADEMARK.md"),
    Path("docs/BRAND_GOVERNANCE.md"),
    Path("docs/PROVENANCE.md"),
    Path("docs/ACKNOWLEDGMENTS.md"),
    Path("docs/OPEN_SOURCE_GATE.md"),
}
REQUIRED_FILES = (
    Path("LICENSE"),
    Path("NOTICE.md"),
    Path("TRADEMARK.md"),
    Path("RELEASE_VERSION.json"),
    Path("docs/ACKNOWLEDGMENTS.md"),
    Path("docs/OPEN_SOURCE_GATE.md"),
    Path("README.md"),
)
FORBIDDEN_PATH_PARTS = {
    "deliverables",
    "superpowers",
    ".comet",
    ".codex",
    ".agents",
}
FORBIDDEN_PATTERNS = {
    "private repository owner": re.compile(r"louloulin", re.I),
    "private user path": re.compile(r"(?:/Users/|/home/[A-Za-z0-9_.-]+)"),
    "public demo IP": re.compile(r"(?:8\.160\.172\.235|47\.116\.105\.82|124\.221\.146\.145)"),
    "private target IP": re.compile(r"172\.29\.\d{1,3}\.\d{1,3}"),
    "internal task ID": re.compile(r"\b(?:LUM|BENY)-\d+[A-Za-z0-9_-]*\b", re.I),
    "event-stage language": re.compile(r"(?:复赛|决赛|比赛|参赛|赛道|评委|评审交付|GOAI 2026|Agent Infra Submission)", re.I),
    "private demo tenant": re.compile(r"\bgoai-demo\b", re.I),
    "GitHub token": re.compile(r"\bghp_[A-Za-z0-9_]{20,}\b"),
    "Anthropic token": re.compile(r"\bsk-ant-[A-Za-z0-9_-]{20,}\b"),
    "OpenAI-style token": re.compile(r"\bsk-[A-Za-z0-9_-]{30,}\b"),
    "AWS access key": re.compile(r"\bAKIA[0-9A-Z]{16}\b"),
    "private key block": re.compile(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----"),
    "credential-bearing URL": re.compile(
        r"\b(?:https?|postgres|postgresql|mysql|redis|mongodb|amqp)://[^/\s:@\"']+:(?!\$\{)[A-Za-z0-9_.~+=-]{16,}@", re.I
    ),
}


def fail(message: str) -> None:
    raise SystemExit(f"open-source gate failed: {message}")


def auditable(path: Path) -> bool:
    relative = path.relative_to(ROOT)
    if not path.is_file() or relative == AUDITOR:
        return False
    parts = relative.parts
    if SKIPPED_PARTS.intersection(parts):
        return False
    return not any(parts[: length] == prefix for prefix in SKIPPED_SUFFIX_PARTS for length in [len(prefix)])


def text_files() -> list[Path]:
    files: list[Path] = []
    for path in sorted(ROOT.rglob("*")):
        if not auditable(path):
            continue
        try:
            path.read_text(encoding="utf-8")
        except (UnicodeDecodeError, OSError):
            continue
        files.append(path.relative_to(ROOT))
    return files


def check_paths() -> None:
    for path in ROOT.rglob("*"):
        relative = path.relative_to(ROOT)
        if FORBIDDEN_PATH_PARTS.intersection(relative.parts):
            fail(f"private path admitted: {relative}")
        lower_name = relative.name.lower()
        if re.search(r"(?:^|[-_])(?:lum|beny)[-_]\d+", lower_name):
            fail(f"internal task filename admitted: {relative}")


def check_required_files() -> None:
    for required in REQUIRED_FILES:
        if not (ROOT / required).is_file():
            fail(f"missing required file: {required}")
    try:
        manifest = json.loads((ROOT / "RELEASE_VERSION.json").read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        fail(f"invalid RELEASE_VERSION.json: {error}")
    if manifest.get("repository") != "https://github.com/vincent-wuhan/opskeeper":
        fail("RELEASE_VERSION.json names the wrong repository")
    if manifest.get("license") != "Apache-2.0":
        fail("RELEASE_VERSION.json records a non-Apache release")


def check_acknowledgments() -> None:
    acknowledgment = (ROOT / "docs/ACKNOWLEDGMENTS.md").read_text(encoding="utf-8")
    for term in ("GoAI AgentTeams", "AgentTeams Dashboard", "OnGrid", "not claims of code derivation"):
        if term not in acknowledgment:
            fail(f"missing acknowledgment boundary: {term}")


def main() -> int:
    check_paths()
    check_required_files()
    files = text_files()
    if not files:
        fail("no auditable text files found")

    for relative in files:
        content = (ROOT / relative).read_text(encoding="utf-8")
        for label, pattern in FORBIDDEN_PATTERNS.items():
            if label == "private user path" and relative.name.endswith("_test.go"):
                continue
            match = pattern.search(content)
            if match:
                preview = match.group(0)[:120]
                fail(f"{label} found in {relative}: {preview}")
        if re.search(r"ongrid", content, re.I) and relative not in ONGRID_ALLOWLIST:
            fail(f"OnGrid outside compliance allowlist: {relative}")

    check_acknowledgments()
    print(f"open-source gate passed: {len(files)} text files audited")
    return 0


if __name__ == "__main__":
    sys.exit(main())
