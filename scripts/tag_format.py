"""The one place that knows what a release tag looks like.

The grammar used to be written twice -- once in each "Resolve tag" step of the
release workflow -- and the two copies said something the repository's own
VERSION did not satisfy: they demanded `rc.4` while every tag in this project
is written `rc4`. The build job would have exited 2 on the project's own version,
and nothing in the repository noticed, because check_release_version.py never
looked at tag shape at all.

So the grammar lives here, the workflow is the source it is read back from, and
both the gate and the signing tool ask this module rather than spelling out a
pattern of their own. A third copy is what caused the defect.
"""
from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
WORKFLOWS = ROOT / ".github/workflows"

# The shape a step must have for its regex to be the tag grammar: a bash test
# against "$tag". Anything else in the file is not a grammar.
_RESOLVE_TAG_LINE = re.compile(
    r'^\s*if\s*\[\[\s*!\s*"\$tag"\s*=~\s*(?P<pattern>\^\S+?)\s*\]\]\s*;\s*then\s*$'
)


def workflow_patterns() -> dict[Path, str]:
    """Every tag grammar the release workflows currently state, by file."""
    found: dict[Path, str] = {}
    for workflow in sorted(WORKFLOWS.glob("*.yml")):
        for number, line in enumerate(workflow.read_text(encoding="utf-8").splitlines(), start=1):
            match = _RESOLVE_TAG_LINE.match(line)
            if match is not None:
                found[workflow] = match.group("pattern")
                break
    return found


def canonical_pattern() -> str:
    """The single grammar, read back out of the release workflow.

    Reading it back rather than restating it here is deliberate: a constant
    that nobody re-derives from the workflow is the third copy that this module
    exists to remove. If the workflow's grammar changes, this changes with it.
    """
    patterns = set(workflow_patterns().values())
    if not patterns:
        raise LookupError("no release workflow states a tag pattern, so there is no grammar to read")
    if len(patterns) > 1:
        listed = ", ".join(sorted(patterns))
        raise ValueError(f"the release workflows disagree about the tag grammar: {listed}")
    return patterns.pop()


def accepts(tag: str) -> bool:
    return re.fullmatch(canonical_pattern(), tag) is not None


def version_from_tag(tag: str) -> str:
    """The VERSION a tag carries -- the same value, without the leading v.

    The workflow compares VERSION to the tag with the v intact, so this strips
    it only for the manifest's `version` field, which the changelog heading and
    check_release_version.py both spell without it.
    """
    if not tag.startswith("v"):
        raise ValueError(f"a release tag must start with v, got {tag!r}")
    return tag[1:]
