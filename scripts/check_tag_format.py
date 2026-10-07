#!/usr/bin/env python3
"""Every tag this repository has ever named must pass the gate that releases it.

The release workflow states its tag grammar in a bash test, once per job. Those
copies once demanded `rc.4` while this project's tags are written `rc4`, so both
jobs would have exited 2 on the repository's own VERSION -- and no check noticed,
because nothing in the repository looked at tag shape.

This gate makes the workflow's own grammar the thing that is judged, so the
history in the repository is the witness: VERSION, the manifest, and every
changelog heading have to be tags the release path accepts. It also refuses a
grammar loosened into accepting everything, because a gate that accepts `.*` is
indistinguishable from a gate that is not there.
"""
from __future__ import annotations

import json
import re
import sys

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))

import tag_format  # noqa: E402

# A released version is a "## <version> — <ISO date>" heading. The changelog
# also carries sections that are not releases -- "## vNext — ..." is a plan, not
# a tag -- so the shape is part of what is being judged: a heading that stops
# looking like a release is not silently dropped from the witness.
CHANGELOG_HEADING = re.compile(r"^## (\S+)\s+—\s+\d{4}-\d{2}-\d{2}", re.MULTILINE)

# Things that must never pass, chosen so that loosening the grammar into "any
# v-prefixed string" or "anything" is caught rather than celebrated.
MUST_REJECT = [
    ("v1.2", "two components"),
    ("v1.2.3.4", "four components"),
    ("v1.2.3-rc", "rc with no number"),
    ("v1.2.3-rcx", "rc with a non-numeric suffix"),
    ("1.2.3", "no leading v"),
    ("v1.2.3extra", "trailing characters"),
    ("v1.2.3-rc4-rc5", "two suffixes"),
    ("", "the empty string"),
    ("main", "a branch name"),
]


def main() -> int:
    problems: list[str] = []

    try:
        pattern = tag_format.canonical_pattern()
    except (LookupError, ValueError) as error:
        print(f"tag format check failed: {error}")
        return 1
    print(f"release tag grammar (read from the workflows): {pattern}")

    stated = tag_format.workflow_patterns()
    for workflow, workflow_pattern in sorted(stated.items()):
        if workflow_pattern != pattern:
            problems.append(f"{workflow.name} states a different grammar: {workflow_pattern}")

    version = (tag_format.ROOT / "VERSION").read_text(encoding="utf-8").strip()
    if not tag_format.accepts(version):
        problems.append(
            f"VERSION is {version}, which the release workflow's grammar rejects; "
            "the publish jobs exit 2 on a tag this repository cannot name"
        )

    manifest_path = tag_format.ROOT / "RELEASE_VERSION.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    release_tag = manifest["release_tag"]
    if not tag_format.accepts(release_tag):
        problems.append(f"RELEASE_VERSION.json release_tag is {release_tag}, which the grammar rejects")
    if manifest["version"] != tag_format.version_from_tag(release_tag):
        problems.append(
            f"RELEASE_VERSION.json version {manifest['version']!r} is not {release_tag} without its v"
        )

    changelog = (tag_format.ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
    headings = CHANGELOG_HEADING.findall(changelog)
    if not headings:
        problems.append("CHANGELOG.md has no '## ' headings, so the history is not a witness to anything")
    for heading in headings:
        if not tag_format.accepts(f"v{heading}"):
            problems.append(
                f"CHANGELOG.md documents {heading}, which as the tag v{heading} the grammar rejects"
            )

    for tag, why in MUST_REJECT:
        if tag_format.accepts(tag):
            problems.append(f"the grammar accepts {tag!r} ({why}); it has been loosened")

    if problems:
        print(f"tag format check failed: {len(problems)} finding(s)")
        for problem in problems:
            print(f"  - {problem}")
        return 1

    print(
        f"tag format check passed: the grammar accepts VERSION ({version}) and all "
        f"{len(headings)} changelog tags, and still rejects {len(MUST_REJECT)} things that are not tags"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
