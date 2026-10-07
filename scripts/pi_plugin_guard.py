#!/usr/bin/env python3
"""校验 OPC / Pient / 垂域插件之间的架构边界。"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path
from typing import Any


SEMVER = re.compile(r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$")
IDENTIFIER = re.compile(r"^[a-z0-9][a-z0-9._-]*$")
CAPABILITY = re.compile(r"^[a-z0-9][a-z0-9._-]*(?::[a-z0-9][a-z0-9._-]*)*$")
HOST = re.compile(r"^(?=.{1,253}$)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+(?::\d{1,5})?$")
URL = re.compile(r"\bhttps?://([^/\s\"'`]+)")
IMPORT_FROM = re.compile(r"(?:^|[;\s])(?:import|export)\s+[^;\n]*?from\s+['\"]([^'\"]+)['\"]", re.MULTILINE)
BARE_IMPORT = re.compile(r"(?:^|[;\s])import\s+['\"]([^'\"]+)['\"]", re.MULTILINE)
DYNAMIC_IMPORT = re.compile(r"(?:^|[;\s()])import\(\s*['\"]([^'\"]+)['\"]", re.MULTILINE)
REQUIRE = re.compile(r"(?:^|[;\s])require\(\s*['\"]([^'\"]+)['\"]", re.MULTILINE)
FORBIDDEN_PROCESS_MODULES = {
    "child_process",
    "node:child_process",
    "cluster",
    "node:cluster",
    "worker_threads",
    "node:worker_threads",
}
FORBIDDEN_DEPENDENCIES = {
    "playwright",
    "@playwright/test",
    "puppeteer",
    "puppeteer-core",
    "selenium-webdriver",
    "chromedriver",
    "electron",
}
SOURCE_SUFFIXES = {".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}
CONTROL_PLANE_DIRS = ("api", "cmd", "core", "sdk", "web")
CONTROL_PLANE_SUFFIXES = {".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}


class GuardError(Exception):
    pass


def load_json(path: Path) -> Any:
    try:
        with path.open(encoding="utf-8") as handle:
            return json.load(handle)
    except (OSError, json.JSONDecodeError) as error:
        raise GuardError(f"{path}: cannot read JSON: {error}") from error


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise GuardError(f"{label}: must be an object")
    return value


def require_string_list(value: Any, label: str, pattern: re.Pattern[str] | None = None) -> list[str]:
    if not isinstance(value, list) or not value or any(not isinstance(item, str) or not item for item in value):
        raise GuardError(f"{label}: must be a non-empty string array")
    if len(value) != len(set(value)):
        raise GuardError(f"{label}: contains duplicates")
    if pattern:
        for item in value:
            if not pattern.fullmatch(item):
                raise GuardError(f"{label}: invalid entry {item!r}")
    return value


def module_specs(source: str) -> list[str]:
    specs: list[str] = []
    for regex in (IMPORT_FROM, BARE_IMPORT, DYNAMIC_IMPORT, REQUIRE):
        specs.extend(match.group(1) for match in regex.finditer(source))
    return specs


def production_files(plugin_root: Path) -> list[Path]:
    allowed_dirs = [plugin_root / name for name in ("extensions", "skills", "src")]
    files: list[Path] = []
    for directory in allowed_dirs:
        if not directory.exists():
            continue
        for path in directory.rglob("*"):
            if path.is_file() and path.suffix in SOURCE_SUFFIXES and "node_modules" not in path.parts:
                files.append(path)
    return files


def normalized_host(host: str) -> str:
    return host.rstrip(".").lower()


def validate_package(plugin_root: Path) -> tuple[dict[str, Any], str]:
    package_path = plugin_root / "package.json"
    package = require_mapping(load_json(package_path), str(package_path))
    for field in ("name", "version", "license"):
        if not isinstance(package.get(field), str) or not package[field].strip():
            raise GuardError(f"{package_path}: {field} is required")
    if not SEMVER.fullmatch(package["version"]):
        raise GuardError(f"{package_path}: version must be exact semver")
    if package["license"].strip().upper() == "UNLICENSED":
        raise GuardError(f"{package_path}: UNLICENSED is not valid for a distributable plugin")
    if not (plugin_root / "LICENSE").is_file():
        raise GuardError(f"{plugin_root}: LICENSE file is required")

    scripts = require_mapping(package.get("scripts"), f"{package_path}: scripts")
    for script_name in ("test", "typecheck"):
        if not isinstance(scripts.get(script_name), str) or not scripts[script_name].strip():
            raise GuardError(f"{package_path}: scripts.{script_name} is required")
    for script_name in ("start", "dev", "preinstall", "postinstall", "prepare"):
        if scripts.get(script_name) is not None:
            raise GuardError(f"{package_path}: plugins must not define scripts.{script_name}")

    peer_dependencies = require_mapping(
        package.get("peerDependencies"), f"{package_path}: peerDependencies"
    )
    pi_peers = [name for name in peer_dependencies if "pi-coding-agent" in name or name == "pi"]
    if not pi_peers:
        raise GuardError(f"{package_path}: a Pi runtime peer dependency is required")
    for name in pi_peers:
        constraint = peer_dependencies[name]
        if not isinstance(constraint, str) or not constraint.strip() or constraint.strip() in {"*", "latest"}:
            raise GuardError(f"{package_path}: Pi peer dependency {name} must have a bounded range")

    all_dependencies = set()
    for field in ("dependencies", "devDependencies", "peerDependencies"):
        value = package.get(field, {})
        dependencies = require_mapping(value, f"{package_path}: {field}") if value else {}
        overlap = all_dependencies.intersection(dependencies)
        if overlap:
            raise GuardError(f"{package_path}: {field} duplicates {sorted(overlap)}")
        all_dependencies.update(dependencies)
    forbidden = all_dependencies.intersection(FORBIDDEN_DEPENDENCIES)
    if forbidden:
        raise GuardError(f"{package_path}: forbidden daemon/browser dependencies: {sorted(forbidden)}")

    pi = require_mapping(package.get("pi"), f"{package_path}: pi")
    extensions = require_string_list(pi.get("extensions"), f"{package_path}: pi.extensions")
    if any(not item.startswith("./extensions/") for item in extensions):
        raise GuardError(f"{package_path}: pi.extensions entries must point under ./extensions/")
    skills = pi.get("skills", [])
    if skills and any(not isinstance(item, str) or not item.startswith("./skills/") for item in skills):
        raise GuardError(f"{package_path}: pi.skills entries must point under ./skills/")

    files = package.get("files", [])
    if not isinstance(files, list) or not {"extensions", "src"}.issubset(set(files)):
        raise GuardError(f"{package_path}: files must include extensions and src")
    if skills and "skills" not in files:
        raise GuardError(f"{package_path}: files must include skills when skills are declared")

    return package, package["name"]


def validate_manifest(plugin_root: Path, package: dict[str, Any]) -> dict[str, Any]:
    manifest_path = plugin_root / "pient-plugin.json"
    manifest = require_mapping(load_json(manifest_path), str(manifest_path))
    if manifest.get("schemaVersion") != 1:
        raise GuardError(f"{manifest_path}: schemaVersion must be 1")
    plugin_id = manifest.get("id")
    if not isinstance(plugin_id, str) or not IDENTIFIER.fullmatch(plugin_id):
        raise GuardError(f"{manifest_path}: id must be a lowercase machine identifier")
    if plugin_id != plugin_root.name:
        raise GuardError(f"{manifest_path}: id must match directory name")

    package_reference = manifest.get("package")
    match = re.fullmatch(r"npm:(?:@[^@/\s]+/)?[^@/\s]+@(\S+)", package_reference or "")
    if not match:
        raise GuardError(f"{manifest_path}: package must use npm:<name>@<exact-version>")
    referenced_version = match.group(1)
    referenced_name = package_reference[4 : -len(referenced_version) - 1]
    if referenced_name != package["name"] or referenced_version != package["version"]:
        raise GuardError(
            f"{manifest_path}: package identity must match package.json exactly"
        )

    runtime = require_mapping(manifest.get("runtime"), f"{manifest_path}: runtime")
    for field in ("pient", "pi"):
        constraint = runtime.get(field)
        if not isinstance(constraint, str) or not constraint.strip() or constraint.strip() in {"*", "latest"}:
            raise GuardError(f"{manifest_path}: runtime.{field} must be bounded")
    require_string_list(manifest.get("capabilities"), f"{manifest_path}: capabilities", CAPABILITY)

    permissions = require_mapping(manifest.get("permissions"), f"{manifest_path}: permissions")
    network = require_string_list(permissions.get("network"), f"{manifest_path}: permissions.network", HOST)
    secrets = permissions.get("secrets", [])
    if not isinstance(secrets, list) or any(
        not isinstance(secret, str) or not re.fullmatch(r"[A-Z][A-Z0-9_]+", secret) for secret in secrets
    ):
        raise GuardError(f"{manifest_path}: permissions.secrets must be uppercase environment names")
    if len(secrets) != len(set(secrets)):
        raise GuardError(f"{manifest_path}: permissions.secrets contains duplicates")
    if permissions.get("filesystem") != "plugin-sandbox-only":
        raise GuardError(f"{manifest_path}: permissions.filesystem must be plugin-sandbox-only")

    process = require_mapping(manifest.get("process"), f"{manifest_path}: process")
    if process.get("daemon") is not False or process.get("background") != "pi-turn-only":
        raise GuardError(f"{manifest_path}: plugins must be non-daemon and pi-turn-only")
    install = require_mapping(manifest.get("install"), f"{manifest_path}: install")
    if install.get("mode") != "user-initiated" or install.get("bundled") is not False:
        raise GuardError(f"{manifest_path}: install must be user-initiated and non-bundled")

    if not production_files(plugin_root):
        raise GuardError(f"{plugin_root}: no production source found under extensions/, skills/, or src/")
    return manifest


def validate_plugin_isolation(plugin_root: Path) -> None:
    for source_path in production_files(plugin_root):
        relative = source_path.relative_to(plugin_root)
        source = source_path.read_text(encoding="utf-8")
        for spec in module_specs(source):
            if spec in FORBIDDEN_PROCESS_MODULES:
                raise GuardError(f"{source_path}: forbidden process module {spec}")
            if spec.startswith(("@/", "~/")) or "plugins/pi/" in spec or spec.startswith("opskeeper"):
                raise GuardError(f"{source_path}: plugin may not import OPC/Pient application code ({spec})")
            if spec.startswith("."):
                target = (source_path.parent / spec).resolve()
                if plugin_root.resolve() not in target.parents and target != plugin_root.resolve():
                    raise GuardError(f"{source_path}: relative import escapes plugin root ({spec})")


def validate_network_declarations(plugin_root: Path, manifest: dict[str, Any]) -> None:
    allowed = {normalized_host(host) for host in manifest["permissions"]["network"]}
    for source_path in production_files(plugin_root):
        source = source_path.read_text(encoding="utf-8")
        for match in URL.finditer(source):
            literal = match.group(0)
            host = normalized_host(match.group(1))
            if literal.startswith("http://"):
                raise GuardError(f"{source_path}: production network access must use HTTPS ({literal})")
            if host not in allowed:
                raise GuardError(f"{source_path}: URL host {host} is not declared in pient-plugin.json")


def validate_control_plane_isolation(root: Path) -> None:
    for directory_name in CONTROL_PLANE_DIRS:
        directory = root / directory_name
        if not directory.is_dir():
            continue
        for path in directory.rglob("*"):
            if not path.is_file() or path.suffix not in CONTROL_PLANE_SUFFIXES or "node_modules" in path.parts:
                continue
            source = path.read_text(encoding="utf-8", errors="replace")
            if "plugins/pi/" in source or "../plugins/pi/" in source:
                raise GuardError(
                    f"{path}: control-plane code may not import Pi plugin implementation code"
                )


def validate_manifest_locations(root: Path) -> None:
    for path in root.rglob("pient-plugin.json"):
        if "node_modules" in path.parts or ".git" in path.parts:
            continue
        plugin_root = path.parent
        if plugin_root.parent != root / "plugins" / "pi":
            raise GuardError(f"{path}: Pi plugin manifests must live in plugins/pi/<plugin-id>/")


def run(root: Path) -> list[str]:
    errors: list[str] = []
    workspace = root / "plugins" / "pi"
    if not workspace.is_dir():
        return [f"missing plugin workspace: {workspace}"]

    plugin_roots = sorted(path for path in workspace.iterdir() if path.is_dir())
    discovered: list[Path] = []
    for plugin_root in plugin_roots:
        has_package = (plugin_root / "package.json").is_file()
        has_manifest = (plugin_root / "pient-plugin.json").is_file()
        if not has_package and not has_manifest:
            continue
        discovered.append(plugin_root)
        try:
            if not has_package or not has_manifest:
                missing = "package.json" if not has_package else "pient-plugin.json"
                raise GuardError(f"{plugin_root}: missing {missing}")
            package, _ = validate_package(plugin_root)
            manifest = validate_manifest(plugin_root, package)
            validate_plugin_isolation(plugin_root)
            validate_network_declarations(plugin_root, manifest)
        except GuardError as error:
            errors.append(str(error))

    try:
        validate_manifest_locations(root)
        validate_control_plane_isolation(root)
    except GuardError as error:
        errors.append(str(error))

    if not discovered and not errors:
        print("pi-plugin-guard: no Pi plugins declared; workspace contract ready")
    elif not errors:
        names = ", ".join(path.name for path in discovered)
        print(f"pi-plugin-guard: {len(discovered)} plugin(s) valid ({names})")
    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", nargs="?", default=".")
    args = parser.parse_args()
    errors = run(Path(args.root).resolve())
    for error in errors:
        print(f"pi-plugin-guard: FAIL {error}", file=sys.stderr)
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
