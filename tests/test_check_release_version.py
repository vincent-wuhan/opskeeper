"""Tests for the release version gate.

The gate reports facts about one commit: the manifest's plugin versions, the
two source-tree hashes, and whether the release commit touched anything
outside its source boundary. On a development commit those facts are all false
at once, and the gate is red by construction -- that is intended, and it is
why the gate runs in release.yml rather than in front of every push.

What was NOT intended is how it reported them. `require` raised on the first
drift, so a signing session discovered the same drift one run at a time: fix
one, re-run, read the log, find the next. Five findings, five runs, and the
first run's message ("plugin.yaml version drifted") says nothing about the
other four.

So the first test below is the one that matters: it plants several drifts at
once and asserts that they are ALL reported. Restoring the fail-fast shape
fails it, which is the point -- an assertion about reporting behaviour is
invisible to a person reading a red log.
"""
from __future__ import annotations

import importlib.util
import json
import subprocess
from pathlib import Path

import pytest

GATE_REL = Path("scripts/check_release_version.py")
REPO_ROOT = Path(__file__).resolve().parents[1]

VERSION = "2026.09.14-rc4"
TAG = "v2026.09.14-rc4"
BASELINE = "release/20260922@d2920895363edca87e34ee00fcb33eb1c6090723"
BACKEND_COMMIT = "305bec8417ef25234f3fa10734ddd8761e25c398"


def load_gate():
    spec = importlib.util.spec_from_file_location("check_release_version", REPO_ROOT / GATE_REL)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def scaffold(root: Path, *, manifest_overrides: dict | None = None) -> Path:
    """A tree the gate accepts, apart from what a case changes.

    The version constants are the gate's own, so a case that means to fail on
    a drift fails on the drift it planted rather than on a scaffolding gap.
    """
    harness_version = "1.0.59"
    installer_version = "1.4.3"

    (root / "scripts").mkdir(parents=True, exist_ok=True)
    (root / "VERSION").write_text(f"{TAG}\n", encoding="utf-8")
    (root / "CHANGELOG.md").write_text(
        f"## {VERSION} — 2026-09-14\n\n{BACKEND_COMMIT} teamharness {harness_version}\n",
        encoding="utf-8",
    )
    manifest = {
        "version": VERSION,
        "release_tag": TAG,
        "release_candidate": True,
        "release_baseline_ref": BASELINE,
        "backend_commit": BACKEND_COMMIT,
        "release_branch_head_at_signing": BACKEND_COMMIT,
        "teamharness_version": harness_version,
        "teamharness_source_tree": "tree-of-the-plugin",
        "installer_version": installer_version,
        "web_hash": "tree-of-the-web",
        "repository": "https://github.com/vincent-wuhan/opskeeper",
        "license": "Apache-2.0",
    }
    manifest.update(manifest_overrides or {})
    (root / "RELEASE_VERSION.json").write_text(json.dumps(manifest), encoding="utf-8")

    plugin_root = root / "plugins/opskeeper-teamharness"
    (plugin_root / "dashboard/public").mkdir(parents=True, exist_ok=True)
    (plugin_root / "plugin.yaml").write_text(
        f"metadata:\n  version: {harness_version}\n  name: teamharness\n", encoding="utf-8"
    )
    dashboard_manifest = {
        "version": harness_version,
        "entry": {"dashboard": f"dist/main-{harness_version}.js"},
    }
    # Both copies exist in the real tree, and the gate now reads the one that
    # ships. A scaffold that omits the duplicate would make "no disagreement
    # finding" vacuously true.
    (plugin_root / "dashboard/plugin.json").write_text(json.dumps(dashboard_manifest), encoding="utf-8")
    (plugin_root / "dashboard/public/plugin.json").write_text(json.dumps(dashboard_manifest), encoding="utf-8")
    installer_root = root / "plugins/agentteams-plugin-installer/dashboard/public"
    installer_root.mkdir(parents=True, exist_ok=True)
    (installer_root / "plugin.json").write_text(json.dumps({"version": installer_version}), encoding="utf-8")
    (root / "web").mkdir(parents=True, exist_ok=True)
    (root / "web/main.tsx").write_text("export {}\n", encoding="utf-8")
    return root


def make_consistent(root: Path) -> Path:
    """Turn a scaffold into the one tree the gate accepts.

    Two of the assertions are about git trees -- `HEAD:web` and
    `HEAD:plugins/opskeeper-teamharness` -- so a directory that is not a
    repository cannot be consistent no matter what the manifest says, and the
    gate would report a hash drift against an empty string. That is a
    scaffolding artefact, so the scaffolding has to be a repository.

    The commit at the end exists because the manifest is rewritten afterwards:
    the hashes have to describe a commit before they can be written into a
    file that is itself part of the tree being hashed.
    """
    def git(*args: str) -> str:
        return subprocess.run(
            ["git", *args],
            cwd=root,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()

    subprocess.run(["git", "init", "-q"], cwd=root, check=True, capture_output=True)
    git("-c", "user.email=t@example.com", "-c", "user.name=t", "add", "-A")
    git("-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "scaffold")

    manifest = json.loads((root / "RELEASE_VERSION.json").read_text(encoding="utf-8"))
    manifest["web_hash"] = git("rev-parse", "HEAD:web")
    manifest["teamharness_source_tree"] = git("rev-parse", "HEAD:plugins/opskeeper-teamharness")
    (root / "RELEASE_VERSION.json").write_text(json.dumps(manifest), encoding="utf-8")
    git("-c", "user.email=t@example.com", "-c", "user.name=t", "add", "-A")
    git("-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "hashes")
    return root


def test_every_drift_is_reported_not_only_the_first(tmp_path, capsys):
    """Five simultaneous drifts must produce five findings.

    This is the regression. With the fail-fast shape the gate printed one line
    and exited, so a signing session needed one run per drift.
    """
    root = scaffold(
        tmp_path / "repo",
        manifest_overrides={
            "teamharness_version": "1.0.59",
            "web_hash": "a-web-tree-that-is-not-the-ones-head-web",
            "teamharness_source_tree": "a-plugin-tree-that-is-not-the-one",
            "release_candidate": False,
            "license": "MIT",
        },
    )
    # The plugin side drifts away from the manifest in two places.
    plugin_root = root / "plugins/opskeeper-teamharness"
    (plugin_root / "plugin.yaml").write_text("metadata:\n  version: 1.0.70\n  name: teamharness\n", encoding="utf-8")
    moved = {"version": "1.0.70", "entry": {"dashboard": "dist/main-1.0.70.js"}}
    (plugin_root / "dashboard/plugin.json").write_text(json.dumps(moved), encoding="utf-8")
    (plugin_root / "dashboard/public/plugin.json").write_text(json.dumps(moved), encoding="utf-8")

    module = load_gate()
    assert module.main(root) == 1

    out = capsys.readouterr().out
    for expected in (
        "plugin.yaml version drifted",
        "dashboard plugin version drifted",
        "web source tree hash drifted",
        "TeamHarness source tree hash drifted",
        "manifest release candidate flag drifted",
        "manifest license drifted",
    ):
        assert expected in out, f"{expected!r} was not reported; the gate still stops early:\n{out}"


def test_a_finding_names_what_it_expected_and_what_it_found(tmp_path, capsys):
    """A red line that only says "drifted" costs a bisect.

    The two facts a signing session needs are the manifest's value and the
    tree's value; printing only the message sends the reader to git.
    """
    root = scaffold(tmp_path / "repo", manifest_overrides={"teamharness_version": "1.0.59"})
    (root / "plugins/opskeeper-teamharness/plugin.yaml").write_text(
        "metadata:\n  version: 1.0.70\n  name: teamharness\n", encoding="utf-8"
    )

    module = load_gate()
    assert module.main(root) == 1

    out = capsys.readouterr().out
    assert "expected '1.0.59'" in out
    assert "found '1.0.70'" in out


def test_a_consistent_tree_passes(tmp_path, capsys):
    """The gate must still say yes.

    A gate that reports every drift is only useful if it stays quiet when the
    facts agree; otherwise "collect everything" degenerates into "always red".
    """
    root = make_consistent(scaffold(tmp_path / "repo"))

    module = load_gate()
    assert module.main(root) == 0
    assert "release version check passed" in capsys.readouterr().out


def test_an_unreadable_plugin_manifest_is_one_finding_among_the_others(tmp_path, capsys):
    """A file that cannot be parsed must not hide the rest.

    The parser used to exit the process, so a malformed plugin.yaml would
    have hidden every other drift -- the same fail-fast shape once more, in
    the one place where the value being parsed comes from a file rather than
    from a comparison.
    """
    root = scaffold(tmp_path / "repo", manifest_overrides={"license": "MIT"})
    (root / "plugins/opskeeper-teamharness/plugin.yaml").write_text("name: no metadata block\n", encoding="utf-8")

    module = load_gate()
    assert module.main(root) == 1

    out = capsys.readouterr().out
    assert "TeamHarness plugin metadata block is missing" in out
    assert "manifest license drifted" in out


@pytest.mark.parametrize(
    "path,allowed",
    [
        ("docs/anything.md", True),
        ("scripts/anything.py", True),
        ("tests/anything_test.go", True),
        ("core/manager/biz/x.go", False),
        ("CHANGELOG.md", True),
        ("web/src/App.tsx", False),
        ("core/manager/biz/x_test.go", True),
    ],
)
def test_the_source_boundary_rule_keeps_its_meaning(path, allowed):
    """The boundary is a rule about paths, so pin it directly.

    The whole finding above is about reporting; this one is about the rule
    being reported. It must not drift while the reporting is being fixed.
    """
    module = load_gate()
    assert module.allowed_release_delta(path) is allowed


def test_preview_names_the_values_signing_would_have_to_write(tmp_path, capsys):
    """The red log says a field drifted; only the tree says what it drifted to.

    Answering that question by reading the log means running the gate, copying
    a hash by hand, pushing, and running it again -- the five-round trip the
    reporting fix exists to end. So preview prints the tree's own value next to
    the manifest's, for every field that drifts.
    """
    root = make_consistent(scaffold(tmp_path / "repo"))
    (root / "plugins/opskeeper-teamharness/plugin.yaml").write_text(
        "metadata:\n  version: 1.0.70\n  name: teamharness\n", encoding="utf-8"
    )

    module = load_gate()
    assert module.main(root, preview=True) == 0

    out = capsys.readouterr().out
    assert "1.0.70" in out and "1.0.59" in out
    assert "signing must set this to the tree's value" in out
    web_tree = subprocess.run(
        ["git", "rev-parse", "HEAD:web"], cwd=root, check=True, capture_output=True, text=True
    ).stdout.strip()
    assert web_tree in out


def test_preview_marks_a_field_that_agrees_as_agreeing(tmp_path, capsys):
    """Silence is information: it is what stops a field being touched needlessly.

    A preview that marks everything would be read as "all of this must change",
    and someone would bump an installer version that has not moved.
    """
    root = make_consistent(scaffold(tmp_path / "repo"))
    (root / "plugins/opskeeper-teamharness/plugin.yaml").write_text(
        "metadata:\n  version: 1.0.71\n  name: teamharness\n", encoding="utf-8"
    )

    module = load_gate()
    assert module.main(root, preview=True) == 0

    out = capsys.readouterr().out
    installer_line = next(line for line in out.splitlines() if "installer plugin version" in line)
    assert "signing must set" not in installer_line


def test_preview_refuses_to_invent_the_tag(tmp_path, capsys):
    """The tag is a release decision, so the tool must not propose one.

    VERSION cannot be derived from the tree at all -- every candidate is
    equally consistent with the evidence. A preview that printed a plausible
    new tag would be manufacturing a release, and its number would be read as
    a proposal rather than a fact.
    """
    root = make_consistent(scaffold(tmp_path / "repo"))

    module = load_gate()
    assert module.main(root, preview=True) == 0

    out = capsys.readouterr().out
    version_line = next(line for line in out.splitlines() if line.strip().startswith("VERSION"))
    assert "release decision" in version_line


def test_preview_does_not_judge_a_tree_it_would_reject(tmp_path, capsys):
    """Preview reports; the gate judges. Mixing the two makes CI ambiguous.

    If preview returned the gate's verdict, a CI step wired to it would stop
    failing on development commits -- which is precisely the arrangement that
    put a release-time assertion in front of every push before.
    """
    root = scaffold(tmp_path / "repo", manifest_overrides={"license": "MIT"})

    module = load_gate()
    assert module.main(root, preview=True) == 0
    assert "release version check passed" not in capsys.readouterr().out


def test_the_gate_reads_the_copy_that_ships(tmp_path, capsys):
    """The gate must bind the release to what the runtime loads.

    There are two plugin.json files under dashboard/. build-package.sh puts
    dashboard/plugin.json into the zip, Dockerfile.opskeeper copies it, and
    self_check.py reads it -- dashboard/public/plugin.json is copied by vite
    into dist/ where the zip never looks. The gate used to read the second one,
    so a release signed after someone bumped only the shipping copy would bind
    the manifest to a version that nothing ships.

    Here the shipped copy agrees with the manifest and the stray does not, so
    the verdict has to come from the shipped copy.
    """
    root = make_consistent(scaffold(tmp_path / "repo"))
    (root / "plugins/opskeeper-teamharness/dashboard/public/plugin.json").write_text(
        json.dumps({"version": "1.0.99", "entry": {"dashboard": "dist/main-1.0.99.js"}}), encoding="utf-8"
    )

    module = load_gate()
    assert module.main(root) == 1

    out = capsys.readouterr().out
    assert "dashboard plugin version drifted" not in out
    assert "disagrees with the shipped dashboard/plugin.json" in out
    assert "1.0.99" in out


def test_the_shipped_copy_alone_is_enough_to_judge(tmp_path, capsys):
    """A tree with only the shipping copy must not grow a phantom finding.

    The disagreement check is conditional on the duplicate existing; without
    this, "delete the stray copy" would turn into a red gate, and the fix for a
    stale duplicate would be to keep it.
    """
    root = make_consistent(scaffold(tmp_path / "repo"))
    (root / "plugins/opskeeper-teamharness/dashboard/public/plugin.json").unlink()

    module = load_gate()
    assert module.main(root) == 0
    assert "disagrees" not in capsys.readouterr().out
