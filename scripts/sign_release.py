#!/usr/bin/env python3
"""Write the release metadata a tag needs, and refuse to guess the parts it cannot.

Signing a release in this repository is a manual act: the release workflow
never writes VERSION, RELEASE_VERSION.json or CHANGELOG.md, so a human edits
them, commits, and tags. check_release_version.py then judges the result.

The manual part is where the mistakes live, and every one of them is the same
kind: a value that the tree already knows has to be copied by hand into a file.
So this script copies those values, in the only order that satisfies the gate,
and refuses the two values the tree does not know.

That order is not a matter of taste. The gate asserts that everything between
backend_commit and the tag is release metadata, docs, tests or _test.go. Two of
the files a version bump touches -- plugin.yaml and dashboard/plugin.json -- are
NOT in that set. So a bump committed after backend_commit is recorded breaks the
gate no matter how correct it is, and the only order that works is:

    1. bump the plugin versions and commit that          (a normal commit)
    2. run this on the resulting commit                  (metadata only)
    3. commit the metadata, then tag

This script enforces step 1 rather than doing it, because whether a release
should carry 1.0.71 or 1.0.72 is a release decision and not a derivation.

It does not commit and it does not tag. The commit is the point at which the
metadata becomes the released thing, and that is a human's click.
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import tag_format  # noqa: E402

ROOT = Path(__file__).resolve().parents[1]

PLUGIN_YAML = ROOT / "plugins/opskeeper-teamharness/plugin.yaml"
DASHBOARD_PLUGIN = ROOT / "plugins/opskeeper-teamharness/dashboard/plugin.json"
VERSION_FILE = ROOT / "VERSION"
MANIFEST_FILE = ROOT / "RELEASE_VERSION.json"
CHANGELOG = ROOT / "CHANGELOG.md"


class Refusal(Exception):
    """Something a person has to decide, or do first."""


def git(*args: str) -> str:
    return subprocess.run(
        ["git", *args], cwd=ROOT, check=True, capture_output=True, text=True
    ).stdout.strip()


def dirty_paths() -> list[str]:
    output = subprocess.run(
        ["git", "status", "--porcelain"], cwd=ROOT, capture_output=True, text=True, check=True
    ).stdout
    return [line[3:] for line in output.splitlines() if line.strip()]


def plugin_yaml_version() -> str:
    text = PLUGIN_YAML.read_text(encoding="utf-8")
    metadata = re.search(r"^metadata:\n(?:[ \t]+.*\n)+", text, re.MULTILINE)
    if metadata is None:
        raise Refusal("plugin.yaml has no metadata block, so there is no version to sign")
    match = re.search(r"^[ \t]+version:[ \t]+['\"]?([^'\"\s#]+)", metadata.group(0), re.MULTILINE)
    if match is None:
        raise Refusal("plugin.yaml declares no version")
    return match.group(1)


def installer_version() -> str:
    path = ROOT / "plugins/agentteams-plugin-installer/dashboard/public/plugin.json"
    return json.loads(path.read_text(encoding="utf-8"))["version"]


def check_prerequisites(tag: str) -> None:
    """Refuse before touching anything, and say which step has to happen first."""
    # The grammar is read back out of the release workflow rather than restated
    # here. A pattern of its own is the third copy that let this repository ship
    # a tag its own release path rejects.
    if not tag_format.accepts(tag):
        raise Refusal(
            f"tag {tag!r} is not a tag the release workflow accepts; its grammar is "
            f"{tag_format.canonical_pattern()}"
        )
    current = VERSION_FILE.read_text(encoding="utf-8").strip()
    if current == tag:
        raise Refusal(f"VERSION is already {tag}, so there is nothing to sign")

    dirty = dirty_paths()
    if dirty:
        # Hashes describe a commit. Signing against an uncommitted tree would
        # record a web tree that no commit contains, and the next person to read
        # RELEASE_VERSION.json would be reading a fiction.
        raise Refusal(
            "the working tree is dirty, so the hashes would describe a commit that does not "
            "exist:\n  "
            + "\n  ".join(dirty[:10])
            + ("\n  ... and more" if len(dirty) > 10 else "")
        )

    harness = plugin_yaml_version()
    dashboard = json.loads(DASHBOARD_PLUGIN.read_text(encoding="utf-8"))
    if dashboard["version"] != harness:
        raise Refusal(
            f"plugin.yaml says {harness} and dashboard/plugin.json says {dashboard['version']}. "
            "Commit that bump first, then run this again: a plugin file changed after "
            "backend_commit is recorded is outside the release source boundary, so it would "
            "break the gate however correct it is."
        )
    expected_entry = f"dist/main-{harness}.js"
    if dashboard["entry"]["dashboard"] != expected_entry:
        raise Refusal(
            f"dashboard/plugin.json entry is {dashboard['entry']['dashboard']} but must be "
            f"{expected_entry}. Commit that rename first, for the same reason."
        )


def render_changelog_entry(manifest: dict) -> str:
    today = date.today().isoformat()
    return (
        f"## {manifest['version']} — {today}\n\n"
        f"- Bind the release to TeamHarness `{manifest['teamharness_version']}` and installer "
        f"`{manifest['installer_version']}`, with the source boundary at backend/plugin source "
        f"`{manifest['backend_commit']}` and the two source trees recorded in "
        f"`RELEASE_VERSION.json`.\n\n"
    )


def render_changelog(existing: str, entry: str) -> str:
    """Insert the new section directly under the file's preamble.

    The changelog states its own convention in the preamble -- latest first --
    so the insertion point is right below that line rather than at the end.
    """
    lines = existing.splitlines(keepends=True)
    for index, line in enumerate(lines):
        if line.startswith("## "):
            return "".join(lines[:index]) + entry + "".join(lines[index:])
    raise Refusal("CHANGELOG.md has no '## ' section to insert above")


def build(tag: str) -> dict:
    version = tag_format.version_from_tag(tag)
    return {
        "version": version,
        "release_tag": tag,
        "backend_commit": git("rev-parse", "HEAD"),
        "release_branch_head_at_signing": git("rev-parse", "HEAD"),
        "teamharness_version": plugin_yaml_version(),
        "teamharness_source_tree": git("rev-parse", "HEAD:plugins/opskeeper-teamharness"),
        "installer_version": installer_version(),
        "web_hash": git("rev-parse", "HEAD:web"),
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--tag", required=True, help="the tag this release will carry, e.g. v2026.10.01-rc5")
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="print the files that would change and the values in them, and write nothing",
    )
    args = parser.parse_args(argv)

    try:
        check_prerequisites(args.tag)
    except Refusal as refusal:
        print(f"sign-release refused: {refusal}", file=sys.stderr)
        return 2

    manifest = json.loads(MANIFEST_FILE.read_text(encoding="utf-8"))
    computed = build(args.tag)
    # manifest.get, not manifest[key]: a field the manifest has never carried is
    # something signing must add, and indexing it would raise instead of
    # reporting -- which is the difference between a message and a traceback
    # for whoever is holding the release branch.
    updates = {key: manifest.get(key) for key in computed if manifest.get(key) != computed[key]}

    entry = render_changelog_entry({**manifest, **computed})
    changelog = render_changelog(CHANGELOG.read_text(encoding="utf-8"), entry)
    version_line = f"{computed['release_tag']}\n"

    print(f"sign-release {args.tag} (dry run)" if args.dry_run else f"sign-release {args.tag}")
    print(f"  VERSION             {VERSION_FILE.read_text(encoding='utf-8').strip()!r} -> {computed['release_tag']!r}")
    for key, value in updates.items():
        print(f"  {key:<20} {manifest.get(key)!r} -> {value!r}")
    for key in computed:
        if key not in updates:
            print(f"  {key:<20} unchanged ({computed[key]!r})")
    print(f"  CHANGELOG.md        new section for {computed['version']}")
    print()
    print("  next: commit these, then tag. This script does neither, and does not")
    print("  touch plugin.yaml or dashboard/plugin.json -- a plugin file changed after")
    print("  backend_commit is recorded would fall outside the release source boundary.")

    if args.dry_run:
        return 0

    MANIFEST_FILE.write_text(json.dumps({**manifest, **computed}, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    VERSION_FILE.write_text(version_line, encoding="utf-8")
    CHANGELOG.write_text(changelog, encoding="utf-8")
    print()
    print(f"  written. verify with: make version-check")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
