#!/usr/bin/env python3
from __future__ import annotations

import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
AUDITOR = Path(__file__).resolve().relative_to(ROOT)
SKIPPED_PARTS = {
    ".git", ".venv", "__pycache__", ".pytest_cache", "node_modules",
    "dist", "release",
}
ONGRID_ALLOWLIST = {
    Path("README.md"),
    Path("NOTICE.md"),
    Path("TRADEMARK.md"),
    Path("docs/ACKNOWLEDGMENTS.md"),
    Path("docs/OPEN_SOURCE_GATE.md"),
}
FORBIDDEN_PATTERNS = {
    "GitHub token": re.compile(r"ghp_[A-Za-z0-9_]+"),
    "OpenAI-style token": re.compile(r"sk-[A-Za-z0-9_-]{20,}"),
    "provider token variable": re.compile(r"(?:ANTHROPIC_AUTH_TOKEN|OPENAI_API_KEY|LLM_API_KEY)", re.I),
    "non-public repository reference": re.compile(
        r"github\.com/(?!vincent-wuhan/opskeeper\b)[A-Za-z0-9_.-]+/opskeeper",
        re.I,
    ),
    "source provenance marker": re.compile(r"source_(?:repository|branch|commit)", re.I),
    "internal workspace name": re.compile(r"(?:internal|private)[-_/.](?:stage|workspace)", re.I),
    "multica task id": re.compile(r"(?:LUM|BENY)-\d+\b", re.I),
    "competition material": re.compile(r"(?:复赛|决赛|比赛|评审交付|deliverables)", re.I),
    "public demo IP": re.compile(r"(?:8\.160\.172\.235|47\.116\.105\.82|124\.221\.146\.145)"),
    "private target IP": re.compile(r"172\.29\.\d{1,3}\.\d{1,3}"),
    "host absolute path": re.compile(r"(?:/Users/|/home/[A-Za-z0-9_.-]+)"),
}


def fail(message: str) -> None:
    raise SystemExit(f"open-source gate failed: {message}")


def text_files() -> list[Path]:
    files: list[Path] = []
    for path in ROOT.rglob("*"):
        relative = path.relative_to(ROOT)
        if not path.is_file() or relative == AUDITOR or SKIPPED_PARTS.intersection(relative.parts):
            continue
        try:
            path.read_text()
        except (UnicodeDecodeError, OSError):
            continue
        files.append(relative)
    return files


def main() -> int:
    files = text_files()
    if not files:
        fail("no auditable text files found")

    for relative in files:
        content = (ROOT / relative).read_text()
        for label, pattern in FORBIDDEN_PATTERNS.items():
            if pattern.search(content):
                fail(f"{label} found in {relative}")
        if re.search(r"ongrid", content, re.I) and relative not in ONGRID_ALLOWLIST:
            fail(f"OnGrid reference outside acknowledgment allowlist: {relative}")

    readme = (ROOT / "README.md").read_text()
    for acknowledgment in ("GoAI AgentTeams", "AgentTeams Dashboard", "OnGrid"):
        if acknowledgment not in readme:
            fail(f"missing acknowledgment: {acknowledgment}")

    for required in (Path("LICENSE"), Path("NOTICE.md"), Path("RELEASE_VERSION.json")):
        if not (ROOT / required).is_file():
            fail(f"missing required file: {required}")

    print(f"open-source gate passed: {len(files)} text files audited")
    return 0


if __name__ == "__main__":
    sys.exit(main())
