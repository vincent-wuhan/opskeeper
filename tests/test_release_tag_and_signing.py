"""Tests for the release tag grammar and the metadata the signing tool writes.

Two defects meet here, and both were invisible from inside the repository:

  1. The release workflows stated their tag grammar in bash, once per job, and
     those copies demanded `rc.4` while every tag this project names is written
     `rc4`. Both publish jobs would exit 2 on the repository's own VERSION.
  2. The grammar was never a single thing: it lived in two places in the
     workflow and nowhere else, so nothing could disagree with it loudly.

So the grammar now lives in one module that reads it back out of the workflow,
and these tests hold that module to the repository's own history -- the four
released tags in CHANGELOG.md are the witness, and they are the reason a
gate exists rather than a comment.
"""
from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[1]
SCRIPTS = REPO_ROOT / "scripts"


def load(name: str):
    sys.path.insert(0, str(SCRIPTS))
    spec = importlib.util.spec_from_file_location(name, SCRIPTS / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def bind_paths(module, root: Path) -> None:
    """Point a module's import-time paths at a throwaway tree.

    sign_release.py resolves its files at import, so rebinding ROOT alone would
    leave it reading the real repository -- which is the kind of test that
    passes while measuring nothing.
    """
    module.ROOT = root
    module.PLUGIN_YAML = root / "plugins/opskeeper-teamharness/plugin.yaml"
    module.DASHBOARD_PLUGIN = root / "plugins/opskeeper-teamharness/dashboard/plugin.json"
    module.VERSION_FILE = root / "VERSION"
    module.MANIFEST_FILE = root / "RELEASE_VERSION.json"
    module.CHANGELOG = root / "CHANGELOG.md"
    module.tag_format.ROOT = root


def scaffold(root: Path, *, version: str = "1.0.59", dashboard_version: str | None = None) -> Path:
    plugin = root / "plugins/opskeeper-teamharness"
    (plugin / "dashboard").mkdir(parents=True, exist_ok=True)
    installer = root / "plugins/agentteams-plugin-installer/dashboard/public"
    installer.mkdir(parents=True, exist_ok=True)
    (plugin / "plugin.yaml").write_text(f"metadata:\n  version: {version}\n  name: t\n", encoding="utf-8")
    shown = dashboard_version or version
    (plugin / "dashboard/plugin.json").write_text(
        json.dumps({"version": shown, "entry": {"dashboard": f"dist/main-{shown}.js"}}), encoding="utf-8"
    )
    (installer / "plugin.json").write_text(json.dumps({"version": "1.4.3"}), encoding="utf-8")
    (root / "VERSION").write_text("v2026.09.14-rc4\n", encoding="utf-8")
    (root / "RELEASE_VERSION.json").write_text(
        json.dumps(
            {
                "version": "2026.09.14-rc4",
                "release_tag": "v2026.09.14-rc4",
                "teamharness_version": "1.0.59",
                "installer_version": "1.4.3",
            }
        ),
        encoding="utf-8",
    )
    (root / "CHANGELOG.md").write_text(
        "# Changelog\n\nlatest first.\n\n## 2026.09.14-rc4 — 2026-09-14\n\n- a\n", encoding="utf-8"
    )
    (root / "web").mkdir(exist_ok=True)
    (root / "web/main.tsx").write_text("export {}\n", encoding="utf-8")
    return root


def commit_all(root: Path, message: str = "tree") -> str:
    def git(*args: str) -> str:
        return subprocess.run(["git", *args], cwd=root, check=True, capture_output=True, text=True).stdout.strip()

    subprocess.run(["git", "init", "-q"], cwd=root, check=True, capture_output=True)
    git("-c", "user.email=t@example.com", "-c", "user.name=t", "add", "-A")
    git("-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", message)
    return git("rev-parse", "HEAD")


# --- the grammar ------------------------------------------------------------


def test_this_repository_names_tags_its_own_grammar_accepts():
    """The defect this exists for, stated as the assertion that would have caught it.

    The grammar is read back out of the release workflow, so this test cannot
    pass by agreeing with a stale copy of the pattern in the test file.
    """
    module = load("tag_format")
    version = (REPO_ROOT / "VERSION").read_text(encoding="utf-8").strip()
    assert module.accepts(version), (
        f"VERSION is {version}, which the release workflow's grammar "
        f"{module.canonical_pattern()} rejects -- the publish jobs would exit 2 on it"
    )


def test_every_released_tag_in_the_changelog_is_accepted():
    """The four tags this project actually shipped are the witness.

    A grammar check that only looks at VERSION would pass today and fail the
    day someone rolled VERSION back to a shape the history never used.
    """
    module = load("tag_format")
    import re

    changelog = (REPO_ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
    released = re.findall(r"^## (\S+)\s+—\s+\d{4}-\d{2}-\d{2}", changelog, re.MULTILINE)
    assert released, "the changelog states no released version, so it witnesses nothing"
    for heading in released:
        assert module.accepts(f"v{heading}"), f"v{heading} is a tag this project shipped"


@pytest.mark.parametrize("tag", ["v1.2", "v1.2.3-rc", "v1.2.3-rcx", "1.2.3", "v1.2.3.4", ""])
def test_the_grammar_still_rejects_things_that_are_not_tags(tag):
    module = load("tag_format")
    assert not module.accepts(tag)


def test_two_workflows_stating_different_grammars_is_an_error_not_a_pick(tmp_path):
    """Two copies is the condition that produced the defect, so refuse it.

    Picking one silently would restore exactly the failure mode: whichever copy
    the reader happened to open would be the grammar, and the other would be a
    comment that looks like a rule.
    """
    module = load("tag_format")
    workflows = tmp_path / ".github/workflows"
    workflows.mkdir(parents=True)
    (workflows / "a.yml").write_text(
        '          if [[ ! "$tag" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+$ ]]; then\n', encoding="utf-8"
    )
    (workflows / "b.yml").write_text(
        '          if [[ ! "$tag" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+(-rc\\.[0-9]+)?$ ]]; then\n', encoding="utf-8"
    )
    module.WORKFLOWS = workflows
    with pytest.raises(ValueError, match="disagree"):
        module.canonical_pattern()


# --- the signing tool -------------------------------------------------------


def test_signing_refuses_a_dirty_tree(tmp_path):
    """Hashes describe a commit; an uncommitted tree has no hash to record.

    Signing here would write a web tree into RELEASE_VERSION.json that no
    commit contains, and the next person to read the manifest would be reading
    a fiction -- which is the same failure as recording an unverifiable claim.
    """
    root = scaffold(tmp_path / "repo")
    commit_all(root)
    (root / "web/main.tsx").write_text("export { const edited = 1 }\n", encoding="utf-8")

    module = load("sign_release")
    bind_paths(module, root)
    with pytest.raises(module.Refusal, match="dirty"):
        module.check_prerequisites("v2026.10.01-rc5")


def test_signing_refuses_a_plugin_bump_that_was_not_committed_first(tmp_path):
    """The ordering the gate's boundary rule forces, refused rather than explained.

    plugin.yaml and dashboard/plugin.json are outside the release source
    boundary, so a bump committed after backend_commit is recorded breaks the
    gate however correct it is. The tool will not perform the bump: whether a
    release carries 1.0.71 or 1.0.72 is a release decision, not a derivation.
    """
    root = scaffold(tmp_path / "repo", version="1.0.70", dashboard_version="1.0.59")
    commit_all(root)

    module = load("sign_release")
    bind_paths(module, root)
    with pytest.raises(module.Refusal, match="Commit that bump first"):
        module.check_prerequisites("v2026.10.01-rc5")


def test_signing_never_writes_a_plugin_file(tmp_path):
    """Only release metadata is written, because only it may follow backend_commit.

    A tool that also "helpfully" bumped the plugin version would produce a tree
    the gate rejects -- the failure this tool exists to prevent.
    """
    root = scaffold(tmp_path / "repo")
    head = commit_all(root)
    plugin_before = (root / "plugins/opskeeper-teamharness/plugin.yaml").read_text(encoding="utf-8")

    module = load("sign_release")
    bind_paths(module, root)
    computed = module.build("v2026.10.01-rc5")

    assert computed["backend_commit"] == head
    assert computed["teamharness_version"] == "1.0.59"
    assert computed["web_hash"]
    assert (root / "plugins/opskeeper-teamharness/plugin.yaml").read_text(encoding="utf-8") == plugin_before


def test_a_dry_run_writes_nothing(tmp_path, capsys):
    """Inspection before mutation, because the values are the release's."""
    root = scaffold(tmp_path / "repo")
    commit_all(root)
    before = {path: path.read_bytes() for path in (root / "VERSION", root / "RELEASE_VERSION.json", root / "CHANGELOG.md")}

    module = load("sign_release")
    bind_paths(module, root)
    assert module.main(["--tag", "v2026.10.01-rc5", "--dry-run"]) == 0

    out = capsys.readouterr().out
    assert "dry run" in out
    for path, content in before.items():
        assert path.read_bytes() == content


def test_the_changelog_entry_goes_above_the_previous_release(tmp_path):
    """The changelog states its own convention -- latest first -- so read the file.

    Appending at the end would be a silent reordering of a document whose first
    line says it is sorted the other way.
    """
    root = scaffold(tmp_path / "repo")
    entry = "## 2026.10.01-rc5 — 2026-10-07\n\n- new\n\n"
    module = load("sign_release")
    result = module.render_changelog((root / "CHANGELOG.md").read_text(encoding="utf-8"), entry)
    assert result.index("2026.10.01-rc5") < result.index("2026.09.14-rc4")
