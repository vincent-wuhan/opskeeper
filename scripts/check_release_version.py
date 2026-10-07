#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import re
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
METADATA_PATHS = {
    "CHANGELOG.md",
    "RELEASE_VERSION.json",
    "VERSION",
    "Makefile",
    "scripts/audit_open_source.py",
    "scripts/check_release_version.py",
    "scripts/deterministic_archive.py",
    "scripts/fetch_onnxruntime.sh",
    "docs/OPEN_SOURCE_GATE.md",
    "docs/PROVENANCE.md",
    "NOTICE.md",
    ".github/workflows/release.yml",
    ".github/workflows/audit-open-source.yml",
}
ALLOWED_RELEASE_DELTA_PREFIXES = {
    "docs/",
    "scripts/",
    "testdata/",
    "tests/",
}


def run(*arguments: str, root: Path = ROOT) -> subprocess.CompletedProcess[str]:
    return subprocess.run(arguments, cwd=root, text=True, capture_output=True, check=False)


class Findings:
    """Every drifted fact, not the first one.

    This gate runs at signing time, which is the worst possible moment for
    fail-fast: the person reading it is holding a release branch, and the
    previous shape made them rediscover the same drift one run at a time --
    fix one, push, read the log, find the next. The open-source gate had this
    exact defect (see tests/test_audit_open_source.py), and here the number of
    findings is not a small constant: a tree that has moved since the manifest
    was signed drifts in the plugin versions, in both source-tree hashes and
    in the release boundary at once, and only the first of those was ever
    printed.

    So a check records what it expected and what it found, and every finding
    is printed together.
    """

    def __init__(self) -> None:
        self.items: list[str] = []

    def require(self, condition: bool, message: str, *, expected: object = None, actual: object = None) -> bool:
        if condition:
            return True
        if expected is None and actual is None:
            self.items.append(message)
        else:
            self.items.append(f"{message} (expected {expected!r}, found {actual!r})")
        return False

    def __len__(self) -> int:
        return len(self.items)


def yaml_metadata_version(content: str) -> str:
    """The version declared inside plugin.yaml's metadata block.

    Raises ValueError rather than exiting: the caller collects findings, so a
    file that cannot be parsed is one finding among the others instead of the
    one that hides them.
    """
    metadata = re.search(r"^metadata:\n(?:[ \t]+.*\n)+", content, re.MULTILINE)
    if metadata is None:
        raise ValueError("TeamHarness plugin metadata block is missing")
    match = re.search(r"^[ \t]+version:[ \t]+['\"]?([^'\"\s#]+)", metadata.group(0), re.MULTILINE)
    if match is None:
        raise ValueError("TeamHarness plugin metadata version is missing")
    return match.group(1)


def commit_exists(commit: str, root: Path = ROOT) -> bool:
    return run("git", "rev-parse", "--verify", f"{commit}^{{commit}}", root=root).returncode == 0


def allowed_release_delta(path: str) -> bool:
    return (
        path in METADATA_PATHS
        or path.startswith(tuple(ALLOWED_RELEASE_DELTA_PREFIXES))
        or path in {"README.md", "README_ZH.md"}
        or path.endswith("_test.go")
    )


def render_preview(
    manifest: dict,
    findings: Findings,
    *,
    plugin_yaml: str,
    plugin_json: dict,
    installer_json: dict,
    harness_version: str | None,
    web_tree: str,
    harness_tree: str,
    root: Path,
) -> None:
    """Print, field by field, what the tree says against what the manifest says.

    A drifted field is shown with the value signing would have to write. A
    field that already agrees is shown as agreeing, because "nothing to do
    here" is information too: it is what stops someone from touching a file
    that is not part of the drift.
    """
    try:
        declared = yaml_metadata_version(plugin_yaml)
    except ValueError:
        declared = "<unreadable>"

    rows = [
        ("VERSION", (root / "VERSION").read_text(encoding="utf-8").strip(), manifest["release_tag"], True),
        ("manifest version", manifest["version"], manifest["release_tag"].removeprefix("v"), False),
        ("plugin.yaml version", declared, manifest["teamharness_version"], True),
        ("dashboard plugin version", plugin_json["version"], manifest["teamharness_version"], True),
        ("installer plugin version", installer_json["version"], manifest["installer_version"], True),
        ("web_hash", web_tree, manifest["web_hash"], True),
        ("teamharness_source_tree", harness_tree, manifest.get("teamharness_source_tree"), True),
        ("backend_commit", manifest["backend_commit"], "the commit the release branch forks from", False),
    ]
    print("release preflight: manifest versus tree")
    print(f"  {'field':<26} {'tree says':<44} manifest says")
    for name, tree_value, manifest_value, is_drift in rows:
        if is_drift and tree_value != manifest_value:
            mark = "  <-- signing must set this to the tree's value"
        elif name in {"VERSION", "manifest version"}:
            mark = "  (a release decision: the tag is not derivable from the tree)"
        else:
            mark = ""
        print(f"  {name:<26} {tree_value:<44} {manifest_value}{mark}")

    if harness_version is not None:
        expected_entry = f"dist/main-{harness_version}.js"
        entry = plugin_json["entry"]["dashboard"]
        note = "" if entry == expected_entry else f"  <-- signing must set it to {expected_entry}"
        print(f"  {'dashboard entry':<26} {entry:<44} {expected_entry}{note}")

    changelog = (root / "CHANGELOG.md").read_text(encoding="utf-8")
    for label, needle in (
        ("CHANGELOG version heading", manifest["version"]),
        ("CHANGELOG backend binding", manifest["backend_commit"]),
        ("CHANGELOG plugin binding", manifest["teamharness_version"]),
    ):
        present = needle in changelog
        print(f"  {label:<26} {'present' if present else 'MISSING':<44} {needle}")

    backend_commit = manifest["backend_commit"]
    if not commit_exists(backend_commit, root=root):
        print()
        print("  the recorded backend_commit is not in this history, so the source")
        print("  boundary cannot be measured; with OPSKEEPER_REQUIRE_FULL_HISTORY=1")
        print("  that is a hard failure rather than a skipped check.")
        return
    changed = run("git", "diff", "--name-only", backend_commit, "HEAD", root=root).stdout.splitlines()
    outside = sorted(path for path in changed if not allowed_release_delta(path))
    print()
    print(f"  source boundary: {len(changed)} changed path(s) since backend_commit, "
          f"{len(outside)} outside it")
    if outside:
        print("  signing means moving backend_commit forward to a commit where the")
        print("  remainder is release metadata, docs, tests and _test.go only:")
        for path in outside[:10]:
            print(f"    - {path}")
        if len(outside) > 10:
            print(f"    ... and {len(outside) - 10} more")
        print("  choosing where that line falls is a release decision, not a default.")


def main(root: Path = ROOT, preview: bool = False) -> int:
    """Verify the release metadata, or print what signing would have to write.

    `preview` exists because the five findings this gate can report are all of
    the form "the manifest says X, the tree says Y" -- and answering "what is Y"
    is the whole job of the person signing. Reading Y out of a red log means
    running the gate, copying a hash by hand, pushing, and running it again,
    which is the same five-round trip the reporting fix was meant to end.

    So preview prints the tree's own value beside the manifest's for every
    field that drifts, and never guesses the two it cannot know: the tag and
    the version. Those are release decisions, and a tool that invented them
    would be inventing a release.

    Preview shares this module's constants and its arithmetic on purpose. A
    second script that recomputed the hashes would be a second thing that can
    disagree with the gate -- the shape this repository keeps paying for.
    """
    findings = Findings()
    require = findings.require

    manifest = json.loads((root / "RELEASE_VERSION.json").read_text(encoding="utf-8"))
    root_version = (root / "VERSION").read_text(encoding="utf-8").strip()
    plugin_yaml = (root / "plugins/opskeeper-teamharness/plugin.yaml").read_text(encoding="utf-8")
    # dashboard/plugin.json is the copy that ships: build-package.sh puts it in
    # the dashboard zip as plugin.json, Dockerfile.opskeeper copies it, and
    # self_check.py reads it. dashboard/public/plugin.json is a second copy that
    # only this gate ever read, and vite copies it into dist/ where the zip
    # never looks. Reading the public/ copy meant the gate could bind a release
    # to a version nothing ships -- so the shipping copy is authoritative here,
    # and the duplicate is checked against it rather than ignored.
    dashboard_plugin = root / "plugins/opskeeper-teamharness/dashboard/plugin.json"
    plugin_json = json.loads(dashboard_plugin.read_text(encoding="utf-8"))
    duplicate = root / "plugins/opskeeper-teamharness/dashboard/public/plugin.json"
    duplicate_json = json.loads(duplicate.read_text(encoding="utf-8")) if duplicate.exists() else None
    installer_json = json.loads(
        (root / "plugins/agentteams-plugin-installer/dashboard/public/plugin.json").read_text(encoding="utf-8")
    )

    expected_version = "2026.09.14-rc4"
    expected_tag = "v2026.09.14-rc4"
    require(manifest["version"] == expected_version, "manifest version drifted", expected=expected_version, actual=manifest["version"])
    require(manifest["release_tag"] == expected_tag, "manifest release tag drifted", expected=expected_tag, actual=manifest["release_tag"])
    require(manifest["release_candidate"] is True, "manifest release candidate flag drifted", expected=True, actual=manifest["release_candidate"])
    expected_baseline = "release/20260922@d2920895363edca87e34ee00fcb33eb1c6090723"
    require(
        manifest["release_baseline_ref"] == expected_baseline,
        "manifest main baseline drifted",
        expected=expected_baseline,
        actual=manifest["release_baseline_ref"],
    )
    require(
        manifest["release_branch_head_at_signing"] == manifest["backend_commit"],
        "manifest signing base drifted",
        expected=manifest["backend_commit"],
        actual=manifest["release_branch_head_at_signing"],
    )
    require(root_version == expected_tag, "VERSION drifted from the release tag", expected=expected_tag, actual=root_version)
    changelog = (root / "CHANGELOG.md").read_text(encoding="utf-8")
    require(f"## {expected_version} — 2026-09-14" in changelog, "release changelog entry is missing")
    require(manifest["backend_commit"] in changelog, "release changelog backend binding is missing")
    require(manifest["teamharness_version"] in changelog, "release changelog plugin binding is missing")
    require(re.fullmatch(r"[0-9a-f]{40}", manifest["backend_commit"]) is not None, "backend commit is invalid", actual=manifest["backend_commit"])
    require(manifest["repository"] == "https://github.com/vincent-wuhan/opskeeper", "manifest repository drifted", expected="https://github.com/vincent-wuhan/opskeeper", actual=manifest["repository"])
    require(manifest["license"] == "Apache-2.0", "manifest license drifted", expected="Apache-2.0", actual=manifest["license"])

    try:
        harness_version = yaml_metadata_version(plugin_yaml)
    except ValueError as error:
        require(False, str(error))
        harness_version = None
    if harness_version is not None:
        require(harness_version == manifest["teamharness_version"], "plugin.yaml version drifted", expected=manifest["teamharness_version"], actual=harness_version)
    require(plugin_json["version"] == manifest["teamharness_version"], "dashboard plugin version drifted", expected=manifest["teamharness_version"], actual=plugin_json["version"])
    if duplicate_json is not None and duplicate_json != plugin_json:
        for field in sorted({"version", "entry"} & set(plugin_json) | {"version", "entry"} & set(duplicate_json)):
            if duplicate_json.get(field) != plugin_json.get(field):
                require(
                    False,
                    f"dashboard/public/plugin.json disagrees with the shipped dashboard/plugin.json ({field})",
                    expected=f"dashboard/plugin.json {plugin_json.get(field)!r}",
                    actual=f"dashboard/public/plugin.json {duplicate_json.get(field)!r}",
                )
    require(installer_json["version"] == manifest["installer_version"], "installer plugin version drifted", expected=manifest["installer_version"], actual=installer_json["version"])
    require(
        plugin_json["entry"]["dashboard"] == f"dist/main-{harness_version}.js",
        "dashboard entry drifted",
        expected=f"dist/main-{harness_version}.js",
        actual=plugin_json["entry"]["dashboard"],
    )

    web_tree = run("git", "rev-parse", "HEAD:web", root=root).stdout.strip()
    harness_tree = run("git", "rev-parse", "HEAD:plugins/opskeeper-teamharness", root=root).stdout.strip()
    require(web_tree == manifest["web_hash"], "web source tree hash drifted", expected=manifest["web_hash"], actual=web_tree)
    require(manifest.get("teamharness_source_tree") == harness_tree, "TeamHarness source tree hash drifted", expected=harness_tree, actual=manifest.get("teamharness_source_tree"))

    if preview:
        render_preview(manifest, findings, plugin_yaml=plugin_yaml, plugin_json=plugin_json,
                       installer_json=installer_json, harness_version=harness_version,
                       web_tree=web_tree, harness_tree=harness_tree, root=root)
        return 0

    backend_commit = manifest["backend_commit"]
    if commit_exists(backend_commit, root=root):
        ancestry = run("git", "merge-base", "--is-ancestor", backend_commit, "HEAD", root=root).returncode == 0
        require(ancestry, "release commit is not descended from backend_commit", actual=backend_commit)
        changed = run("git", "diff", "--name-only", backend_commit, "HEAD", root=root).stdout.splitlines()
        outside = sorted(path for path in changed if not allowed_release_delta(path))
        require(
            not outside,
            "release commit contains changes outside its source boundary",
            expected="only release metadata, docs, tests and _test.go may differ",
            actual=f"{len(outside)} of {len(changed)} paths, first: {outside[0] if outside else '-'}",
        )
        if outside:
            for path in outside[:20]:
                print(f"  outside source boundary: {path}")
            if len(outside) > 20:
                print(f"  ... and {len(outside) - 20} more")
    elif os.environ.get("OPSKEEPER_REQUIRE_FULL_HISTORY") == "1":
        raise SystemExit("release version check failed: full backend history is required")

    if findings:
        print(f"release version check failed: {len(findings)} finding(s)")
        for item in findings.items:
            print(f"  - {item}")
        return 1

    print("release version check passed")
    return 0


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--preview",
        action="store_true",
        help="print what signing would have to write, and exit 0 without judging",
    )
    raise SystemExit(main(preview=parser.parse_args().preview))
