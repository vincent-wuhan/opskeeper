#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import re
import sys
import zipfile
import tarfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DIST = ROOT / "plugins" / "opskeeper-teamharness" / "dist"


def fail(message: str) -> None:
    raise SystemExit(f"release verification failed: {message}")


def safe_archive_entries(archive: zipfile.ZipFile) -> list[zipfile.ZipInfo]:
    entries: list[zipfile.ZipInfo] = []
    total_size = 0
    for info in archive.infolist():
        name = info.filename
        if info.is_dir():
            continue
        if name.startswith("/") or ".." in Path(name).parts or "\\" in name:
            fail(f"unsafe archive entry: {name}")
        if any(part in {"node_modules", ".git", "__pycache__"} for part in Path(name).parts):
            fail(f"generated or VCS entry leaked into package: {name}")
        total_size += info.file_size
        entries.append(info)
    if len(entries) > 1000:
        fail(f"too many archive entries: {len(entries)}")
    if total_size > 50 * 1024 * 1024:
        fail(f"expanded archive is too large: {total_size} bytes")
    return entries


def verify_installer() -> Path:
    package = ROOT / "plugins" / "agentteams-plugin-installer" / "dist" / "agentteams-plugin-installer.zip"
    if not package.is_file():
        fail(f"missing installer package: {package}")
    with zipfile.ZipFile(package) as archive:
        names = set(archive.namelist())
        if not {"plugin.json", "main.js", "LICENSE", "NOTICE.md"}.issubset(names):
            fail("installer zip must contain plugin.json, main.js, LICENSE, and NOTICE.md at archive root")
        manifest = json.loads(archive.read("plugin.json"))
        if manifest.get("id") != "agentteams-plugin-installer":
            fail("installer manifest id mismatch")
        if manifest.get("entry", {}).get("dashboard") != "main.js":
            fail("installer dashboard entry mismatch")
        if archive.getinfo("main.js").file_size == 0:
            fail("installer main.js is empty")
        safe_archive_entries(archive)
    return package


def verify_teamharness() -> tuple[Path, str]:
    packages = sorted(DIST.glob("opskeeper-teamharness-qwenpaw-*.zip"))
    if len(packages) != 1:
        fail(f"expected exactly one TeamHarness qwenpaw zip, found {[str(p) for p in packages]}")
    package = packages[0]
    version_match = re.fullmatch(r"opskeeper-teamharness-qwenpaw-(.+)\.zip", package.name)
    if not version_match:
        fail(f"invalid TeamHarness package name: {package.name}")
    version = version_match.group(1)

    with zipfile.ZipFile(package) as archive:
        entries = safe_archive_entries(archive)
        names = {info.filename for info in entries}
        plugin_json = [name for name in names if Path(name).name == "plugin.json" and name.count("/") == 1]
        plugin_yaml = [name for name in names if Path(name).name == "plugin.yaml"]
        if len(plugin_json) != 1:
            fail("TeamHarness qwenpaw zip must contain exactly one top-level plugin.json")
        if len(plugin_yaml) != 1 or plugin_yaml[0].count("/") != 2:
            fail("TeamHarness qwenpaw zip must expose plugin.yaml under the single package directory")
        manifest = json.loads(archive.read(plugin_json[0]))
        package_root = plugin_json[0].split("/", 1)[0]
        yaml_text = archive.read(plugin_yaml[0]).decode()
        if manifest.get("id") != "opskeeper-teamharness":
            fail("TeamHarness qwenpaw id mismatch")
        if manifest.get("version") != version:
            fail("TeamHarness qwenpaw version does not match package name")
        if manifest.get("entry", {}).get("backend") != "plugin.py":
            fail("TeamHarness qwenpaw backend entry mismatch")
        if f"version: {version}" not in yaml_text:
            fail("TeamHarness plugin.yaml version does not match package name")
        for required in (
            f"{package_root}/plugin.py",
            f"{package_root}/task_trace.py",
            "opskeeper-teamharness/mcp/server.py",
            "opskeeper-teamharness/mcp/auth.py",
            "LICENSE",
            "NOTICE.md",
        ):
            if not any(name.endswith(required) for name in names):
                fail(f"TeamHarness package missing required file: {required}")
    return package, version


def write_checksums(packages: list[Path]) -> None:
    output = DIST / "SHA256SUMS.txt"
    lines = []
    for package in packages:
        digest = hashlib.sha256(package.read_bytes()).hexdigest()
        lines.append(f"{digest}  {package.name}")
    output.write_text("\n".join(lines) + "\n")


def verify_source_archive(version: str) -> None:
    package = DIST / f"opskeeper-teamharness-{version}-plugin-manager.tar.gz"
    try:
        with tarfile.open(package, "r:gz") as archive:
            members = archive.getmembers()
            names = [member.name for member in members if member.isfile()]
            for name in names:
                if name.startswith("/") or ".." in Path(name).parts or "\\" in name:
                    fail(f"unsafe source archive entry: {name}")
                if any(part in {"node_modules", ".git", "__pycache__"} for part in Path(name).parts):
                    fail(f"generated or VCS entry leaked into source archive: {name}")
            required = {
                "LICENSE", "NOTICE.md", "plugin.yaml", "README.md", "CHANGELOG.md",
                "adapters/qwenpaw/plugin.py", "mcp/server.py", "mcp/auth.py",
                "dashboard/plugin.json",
            }
            missing = sorted(required - set(names))
            if missing:
                fail(f"TeamHarness source archive is missing required files: {missing}")
    except (OSError, tarfile.TarError) as error:
        fail(f"invalid TeamHarness source archive {package}: {error}")


def main() -> int:
    installer = verify_installer()
    teamharness, version = verify_teamharness()
    verify_source_archive(version)
    source_package = DIST / f"opskeeper-teamharness-{version}-plugin-manager.tar.gz"
    write_checksums([installer, teamharness, source_package])
    print("verified packages:")
    print(f"  {installer.relative_to(ROOT)}")
    print(f"  {teamharness.relative_to(ROOT)}")
    print(f"  {source_package.relative_to(ROOT)}")
    print(f"  {DIST / 'SHA256SUMS.txt'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
