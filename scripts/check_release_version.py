#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import re
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
METADATA_PATHS = {
    "RELEASE_VERSION.json",
    "VERSION",
    "Makefile",
    "scripts/check_release_version.py",
    "docs/OPEN_SOURCE_GATE.md",
    "docs/PROVENANCE.md",
    "NOTICE.md",
    ".github/workflows/release.yml",
    ".github/workflows/audit-open-source.yml",
}


def run(*arguments: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(arguments, cwd=ROOT, text=True, capture_output=True, check=False)


def require(condition: bool, message: str) -> None:
    if not condition:
        raise SystemExit(f"release version check failed: {message}")


def yaml_metadata_version(content: str) -> str:
    metadata = re.search(r"^metadata:\n(?:[ \t]+.*\n)+", content, re.MULTILINE)
    require(metadata is not None, "TeamHarness plugin metadata block is missing")
    match = re.search(r"^[ \t]+version:[ \t]+['\"]?([^'\"\s#]+)", metadata.group(0), re.MULTILINE)
    require(match is not None, "TeamHarness plugin metadata version is missing")
    return match.group(1)


def commit_exists(commit: str) -> bool:
    return run("git", "rev-parse", "--verify", f"{commit}^{{commit}}").returncode == 0


def main() -> int:
    manifest = json.loads((ROOT / "RELEASE_VERSION.json").read_text(encoding="utf-8"))
    root_version = (ROOT / "VERSION").read_text(encoding="utf-8").strip()
    plugin_yaml = (ROOT / "plugins/opskeeper-teamharness/plugin.yaml").read_text(encoding="utf-8")
    plugin_json = json.loads(
        (ROOT / "plugins/opskeeper-teamharness/dashboard/public/plugin.json").read_text(encoding="utf-8")
    )
    installer_json = json.loads(
        (ROOT / "plugins/agentteams-plugin-installer/dashboard/public/plugin.json").read_text(encoding="utf-8")
    )

    expected_version = "2026.09.13-rc1"
    expected_tag = "v2026.09.13-rc1"
    require(manifest["version"] == expected_version, "manifest version drifted")
    require(manifest["release_tag"] == expected_tag, "manifest release tag drifted")
    require(manifest["release_candidate"] is True, "manifest release candidate flag drifted")
    require(
        manifest["release_baseline_ref"] == "main@47f57f77bf5217b6546fb0fd92c86b52d33a8e1c",
        "manifest main baseline drifted",
    )
    require(
        manifest["release_branch_head_at_signing"] == manifest["backend_commit"],
        "manifest signing base drifted",
    )
    require(root_version == expected_tag, "VERSION drifted from the release tag")
    require(re.fullmatch(r"[0-9a-f]{40}", manifest["backend_commit"]) is not None, "backend commit is invalid")
    require(manifest["repository"] == "https://github.com/vincent-wuhan/opskeeper", "manifest repository drifted")
    require(manifest["license"] == "Apache-2.0", "manifest license drifted")

    harness_version = yaml_metadata_version(plugin_yaml)
    require(harness_version == manifest["teamharness_version"], "plugin.yaml version drifted")
    require(plugin_json["version"] == manifest["teamharness_version"], "dashboard plugin version drifted")
    require(installer_json["version"] == manifest["installer_version"], "installer plugin version drifted")
    require(plugin_json["entry"]["dashboard"] == f"dist/main-{harness_version}.js", "dashboard entry drifted")

    web_tree = run("git", "rev-parse", "HEAD:web").stdout.strip()
    harness_tree = run("git", "rev-parse", "HEAD:plugins/opskeeper-teamharness").stdout.strip()
    require(web_tree == manifest["web_hash"], "web source tree hash drifted")
    require(manifest.get("teamharness_source_tree") == harness_tree, "TeamHarness source tree hash drifted")

    backend_commit = manifest["backend_commit"]
    if commit_exists(backend_commit):
        ancestry = run("git", "merge-base", "--is-ancestor", backend_commit, "HEAD").returncode == 0
        require(ancestry, "release commit is not descended from backend_commit")
        changed = run("git", "diff", "--name-only", backend_commit, "HEAD").stdout.splitlines()
        require(set(changed).issubset(METADATA_PATHS), "release commit contains non-metadata changes")
    elif os.environ.get("OPSKEEPER_REQUIRE_FULL_HISTORY") == "1":
        raise SystemExit("release version check failed: full backend history is required")

    print("release version check passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
