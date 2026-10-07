#!/usr/bin/env python3
from __future__ import annotations

import json
import re
import subprocess
import sys
from functools import lru_cache
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
AUDITOR = Path(__file__).resolve().relative_to(ROOT)
SKIPPED_PARTS = {".git", ".venv", "__pycache__", ".pytest_cache", "node_modules"}
SKIPPED_SUFFIX_PARTS = {
    ("web", "dist"),
    ("plugins", "agentteams-plugin-installer", "dashboard", "dist"),
    ("plugins", "opskeeper-teamharness", "dist"),
}
# ONGRID_ALLOWLIST is where the OnGrid brand may be named, each with the
# reason it is there.
#
# It is a map rather than a bare set for the same reason every other exemption
# in this repository is: a name that appears in an allowlist with no recorded
# reason is indistinguishable from a name somebody added to make a failure go
# away, and the second one is how the first stops meaning anything. Two of the
# six are documents that exist to talk *about* the rule -- a gate cannot
# describe itself without quoting itself -- and one of those is this
# repository's own architecture ledger, which records the decision to add the
# rule and therefore cannot avoid the word.
ONGRID_ALLOWLIST = {
    Path("NOTICE.md"): "the notice must name the parties whose terms it carries",
    Path("TRADEMARK.md"): "the trademark register is the canonical place the mark is listed",
    Path("docs/BRAND_GOVERNANCE.md"): "the document that defines the naming boundary states the boundary",
    Path("docs/PROVENANCE.md"): "provenance records where third-party material came from",
    Path("docs/ACKNOWLEDGMENTS.md"): "the acknowledgment of the upstream project names it by design",
    Path("docs/OPEN_SOURCE_GATE.md"): "this rule's own documentation; a gate cannot describe itself without quoting itself",
    Path("docs/opskeeper2-architecture.md"): "the architecture ledger records the decisions on the other rules, including this one's name and reason, so quoting it is unavoidable",
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


# DECOY_CREDENTIALS are credential-shaped strings that exist precisely so a
# scanner can find them: a test that proves a node never receives an API key
# has to name the key it is refusing to pass on, and the ledger documents that
# test by quoting it. The value is therefore exempted by exact match rather
# than by relaxing the pattern or by file name -- a real `sk-...` key dropped
# into the same test file, or into the same document, is still reported, which
# is the only property that makes this an exemption rather than a hole.
DECOY_CREDENTIALS = {
    "sk-decoy-openai-must-not-reach-a-node": (
        "a sentinel, not a credential: tests/e2e/node_agent_delivery_test.go sets it "
        "and then asserts no node process ever saw it"
    ),
    "ghp_AAAABBBBCCCCDDDDEEEEFFFF": (
        "not a credential, and not a new exemption shape either -- it is the same "
        "reason as the sentinel above, in a second vocabulary. Two tests assert "
        "that a GitHub-token-shaped string never reaches the audit chain: "
        "core/domains/server/secret/audit_test.go (a stored secret's every byte) and "
        "core/manager/server/chatdiagnose/audit_test.go (the user's question text). "
        "A redaction test has to contain the thing it redacts; deleting the literal "
        "would delete the assertion. The value is a run of repeated placeholder "
        "letters, so it cannot be a live token, and the exemption is by exact "
        "match -- another ghp_ value in either file is still reported."
    ),
}


def report(message: str) -> None:
    """Record one violation.

    This used to raise, which meant the gate named the first thing it found
    and stopped. With 25 violations across 18 files, "the first one" is not
    an answer a person can act on -- it is a reason to run the auditor again
    eighteen times, and it is why a red open-source gate sat here unnoticed
    for as long as it did. Every violation is now collected and printed
    together, and the exit status is still non-zero.
    """
    VIOLATIONS.append(message)


def fail(message: str) -> None:
    """Abort immediately, for the failures that make the rest meaningless.

    A missing LICENSE or an unreadable manifest means the content scan would
    be judging a tree that is already disqualified, so those keep stopping
    the run rather than joining the list.
    """
    raise SystemExit(f"open-source gate failed: {message}")


VIOLATIONS: list[str] = []


def is_this_gate_s_own_test(relative: Path) -> bool:
    """Whether this file is a test of the gate itself.

    `tests/test_audit_open_source.py` has to contain every string the gate
    looks for, because its whole job is to plant them and assert they are
    reported. It is the one file in the repository that is required to hold
    the forbidden literals, and it is identified by sharing the auditor's
    stem rather than by a hand-listed path, so renaming the gate does not
    quietly turn this into a hole.

    Note the shape of the exemption: it covers the *test of the gate*, not
    tests in general. A credential dropped into an unrelated `_test.go` is
    still reported, which is the property the decoy exemption exists to
    protect.
    """
    stem = relative.stem
    return stem == AUDITOR.stem + "_test" or stem == "test_" + AUDITOR.stem


def is_test(relative: Path) -> bool:
    """Whether this is a test file.

    Two of the rules below are about strings a test has to be able to write
    down: a test that proves no home directory path reaches a node has to
    contain one, and a test that proves the OnGrid boundary is honoured has to
    name OnGrid. Neither is a leak. The exemption is by file kind rather than
    by value for those two only, and it is deliberately not extended to the
    credential rules -- a real token dropped into a test file is still
    reported, which is what the decoy exemption above exists to prove.
    """
    name = relative.name
    return name.endswith("_test.go") or name.endswith("_test.py") or name.startswith("test_")


@lru_cache(maxsize=1)
def tracked_files() -> frozenset[str] | None:
    """The paths a release would actually contain, or None if unknowable.

    The gate answers one question -- "does an open-source release built from
    this commit carry private material" -- and that question is about the
    index, not about whatever happens to sit in a developer's working
    directory. Scanning the tree answered a different question, and answered
    it badly in both directions: an untracked `go.work` holding a home
    directory path failed the gate on one machine and passed on a clean
    checkout, and the first tracked private path the gate did find was
    reported alone, so the twenty-four behind it stayed invisible.

    `git ls-files` is the same answer a `git archive` of the release commit
    would give. When there is no index to ask -- a source tarball, a vendored
    copy -- this returns None and the caller scans the tree instead, because
    a tree that cannot be indexed still has to be auditable.
    """
    try:
        result = subprocess.run(
            ["git", "ls-files", "-z"],
            cwd=ROOT,
            capture_output=True,
            check=True,
            timeout=120,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    paths = {entry for entry in result.stdout.decode("utf-8").split("\0") if entry}
    return frozenset(paths) if paths else None


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


def check_paths(is_in_scope) -> None:
    for path in ROOT.rglob("*"):
        relative = path.relative_to(ROOT)
        if not auditable(path) or not is_in_scope(relative):
            continue
        if FORBIDDEN_PATH_PARTS.intersection(relative.parts):
            report(f"private path admitted: {relative}")
        lower_name = relative.name.lower()
        if re.search(r"(?:^|[-_])(?:lum|beny)[-_]\d+", lower_name):
            report(f"internal task filename admitted: {relative}")


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


LEDGER_REL = Path("docs/opskeeper2-architecture.md")

# The progress section's own reading of this gate. It is matched by its row
# label rather than by the shape of the number, because "17" appears in the
# ledger dozens of times -- as history, as an intermediate count, as part of
# a sentence about how the gate used to behave. Only the labelled row is a
# claim about the repository as it stands.
LEDGER_COUNT_RE = re.compile(r"^\| 开源门槛违规 \| \*\*(\d+) 项\*\* \|", re.M)


def check_ledger_violation_count(found: int) -> None:
    """The ledger states how many violations this gate currently finds.

    This gate is the only step in CI that is red, and until decision 176 the
    progress section did not say so -- the count existed only in decision
    records, where each number was true when written and is not evidence
    about today. A reader deciding what is left could not see the one number
    that decides whether a release ships.

    A missing ledger is a violation rather than a skip. The alternative --
    skipping when there is nothing to read -- means deleting one row of one
    document turns the check off, and a check that can be turned off by
    deleting a file is not a check. The cost is that every synthetic
    repository in tests/test_audit_open_source.py has to carry the row, which
    is one line in one scaffolding function.
    """
    ledger = ROOT / LEDGER_REL
    if not ledger.exists():
        report(
            f"{LEDGER_REL.as_posix()} is missing, so the violation count it states cannot be "
            f"checked; this run found {found}"
        )
        return
    text = ledger.read_text(encoding="utf-8")
    m = LEDGER_COUNT_RE.search(text)
    if m is None:
        report(
            f"{LEDGER_REL.as_posix()} has no `| 开源门槛违规 | **N 项** |` row, so the gate's "
            f"current count has no stated reading; this run found {found}"
        )
        return
    stated = int(m.group(1))
    if stated != found:
        report(
            f"the ledger states {stated} open-source violation(s); this run found {found}. "
            f"Both numbers are honest at their own moment -- fix or add violations and update "
            f"the row, do not delete it"
        )


def main() -> int:
    tracked = tracked_files()
    scope = "the tracked tree" if tracked is not None else "the whole working tree (no git index)"

    def is_in_scope(relative: Path) -> bool:
        return tracked is None or relative.as_posix() in tracked

    check_paths(is_in_scope)
    check_required_files()
    files = [path for path in text_files() if is_in_scope(path)]
    if not files:
        fail("no auditable text files found")

    for relative in files:
        content = (ROOT / relative).read_text(encoding="utf-8")
        test_file = is_test(relative)
        gate_own_test = is_this_gate_s_own_test(relative)
        for label, pattern in FORBIDDEN_PATTERNS.items():
            if label == "private user path" and test_file:
                continue
            if gate_own_test:
                continue
            match = pattern.search(content)
            if match and match.group(0) not in DECOY_CREDENTIALS:
                preview = match.group(0)[:120]
                report(f"{label} found in {relative}: {preview}")
        if re.search(r"ongrid", content, re.I) and relative not in ONGRID_ALLOWLIST and not test_file and not gate_own_test:
            report(f"OnGrid outside compliance allowlist: {relative}")

    check_acknowledgments()

    # Captured before the ledger check can append: the number it compares is
    # this run's finding, not one that already includes the comparison.
    found = len(VIOLATIONS)
    check_ledger_violation_count(found)

    if VIOLATIONS:
        print(f"open-source gate failed: {len(VIOLATIONS)} violation(s) in {scope}", file=sys.stderr)
        for violation in VIOLATIONS:
            print(f"  - {violation}", file=sys.stderr)
        print(
            f"audited {len(files)} text files. Each line above is a file a release would "
            f"ship. Removing a path from the index is not enough if the content is meant "
            f"to stay out of the release -- see docs/OPEN_SOURCE_GATE.md.",
            file=sys.stderr,
        )
        return 1

    print(f"open-source gate passed: {len(files)} text files audited in {scope}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
