#!/usr/bin/env python3
"""dev_install.py — build + upload + verify opskeeper-teamharness qwenpaw plugin.

Replaces the laborious host -> docker cp -> in-container extract -> POST chain
with one command. Talks to qwenpaw's in-process `/api/plugins/upload`
multipart endpoint, so the container fetches the zip via HTTP instead of
needing the host filesystem mounted.

Default flow::

    python3 scripts/dev_install.py --target http://127.0.0.1:17281

Subcommands for debugging:

  * ``build``  — write zip to a local path (or stdout -) and stop.
  * ``upload`` — POST the zip built from this checkout to one or more targets.
  * ``install`` — same as ``upload`` + verify via ``/api/plugins`` + ``/api/skills``.
  * ``all``    — alias for ``install`` (default).

Pure stdlib; the only network calls are urllib. No ruby, no qwenpaw CLI on
the host, no docker cp, no in-container extraction.
"""
from __future__ import annotations

import argparse
import io
import json
import os
import re
import sys
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
from pathlib import Path
from typing import Iterable, Sequence

PLUGIN_YAML_NAME = "plugin.yaml"
ASSET_ENTRIES = ("prompts", "skills", "mcp")
SKILL_GROUPS = ("agent", "team")
ADAPTER_FILES = ("plugin.py", "task_trace.py")

PLUGIN_ID = "opskeeper-teamharness"


def _script_dir() -> Path:
    return Path(__file__).resolve().parent


def _plugin_root() -> Path:
    return _script_dir().parent


def _adapter_dir() -> Path:
    return _plugin_root() / "adapters" / "qwenpaw"


def _read_metadata(plugin_yaml: Path) -> tuple[str, str]:
    """Extract metadata.name + metadata.version from plugin.yaml."""
    text = plugin_yaml.read_text(encoding="utf-8")
    name_m = re.search(r"^\s*name:\s*(\S+)", text, re.M)
    ver_m = re.search(r"^\s*version:\s*(\S+)", text, re.M)
    if not name_m or not ver_m:
        raise SystemExit(f"ERROR: cannot find metadata.name/version in {plugin_yaml}")
    return name_m.group(1), ver_m.group(1)


def _build_qwenpaw_manifest(name: str, version: str) -> bytes:
    manifest = {
        "id": PLUGIN_ID,
        "name": "Opskeeper TeamHarness",
        "version": version,
        "type": "general",
        "description": (
            "Opskeeper RCA/recovery plugin for AgentTeams QwenPaw workers. "
            "Provides 7 Worker skills + Manager dispatch + stdio MCP proxy to "
            "opskeeper backend /v1/mcp."
        ),
        "author": "opskeeper-v2",
        "entry": {"backend": "plugin.py"},
        "dependencies": [],
        "min_version": "2.0.1",
        "qwenpaw_version": {"min": "2.0.1", "max": "2.1.0"},
    }
    return (json.dumps(manifest, indent=2) + "\n").encode("utf-8")


def _iter_files(root: Path):
    for path in sorted(root.rglob("*")):
        if path.is_file():
            yield path


def _should_skip(path: Path) -> bool:
    name = path.name
    if name == "__pycache__":
        return True
    if name == ".DS_Store":
        return True
    if name.endswith(".pyc"):
        return True
    return False


def build_zip(plugin_root: Path) -> tuple[bytes, str, str]:
    """Return (zip_bytes, package_name, version) ready for `/api/plugins/upload`."""
    plugin_yaml = plugin_root / PLUGIN_YAML_NAME
    name, version = _read_metadata(plugin_yaml)
    package_name = f"{name}-qwenpaw-{version}"

    asset_dir = plugin_root
    adapter_dir = plugin_root / "adapters" / "qwenpaw"

    if not (adapter_dir / "plugin.py").exists():
        raise SystemExit(f"ERROR: missing adapter entry {adapter_dir / 'plugin.py'}")

    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as zf:
        zf.writestr(f"{package_name}/plugin.json", _build_qwenpaw_manifest(name, version))

        for entry in ASSET_ENTRIES:
            src = asset_dir / entry
            if not src.exists():
                raise SystemExit(f"ERROR: required asset missing: {src}")
            for f in _iter_files(src):
                if _should_skip(f):
                    continue
                arc = f"{package_name}/opskeeper-teamharness/{f.relative_to(asset_dir)}"
                zf.write(f, arc)

        for group in SKILL_GROUPS:
            group_src = asset_dir / "skills" / group
            if not group_src.is_dir():
                continue
            for skill in sorted(group_src.iterdir()):
                if not skill.is_dir():
                    continue
                for f in _iter_files(skill):
                    if _should_skip(f):
                        continue
                    arc = f"{package_name}/qwenpaw-skills/{skill.name}/{f.relative_to(skill)}"
                    zf.write(f, arc)

        for fname in ADAPTER_FILES:
            src = adapter_dir / fname
            if not src.exists():
                raise SystemExit(f"ERROR: required adapter file missing: {src}")
            zf.write(src, f"{package_name}/{fname}")

    return buf.getvalue(), package_name, version


def _http_post_multipart(url: str, file_bytes: bytes, filename: str, *, force: bool, timeout: float) -> dict:
    boundary = f"----opskeeper{int(time.time() * 1000)}"
    body = io.BytesIO()
    body.write(f"--{boundary}\r\n".encode())
    body.write(
        f'Content-Disposition: form-data; name="file"; filename="{filename}"\r\n'
        f"Content-Type: application/zip\r\n\r\n".encode()
    )
    body.write(file_bytes)
    body.write(f"\r\n--{boundary}--\r\n".encode())

    full_url = url + ("?force=true" if force else "?force=false")
    req = urllib.request.Request(
        full_url,
        data=body.getvalue(),
        headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = resp.read().decode("utf-8", errors="replace")
            try:
                return {"ok": True, "status": resp.status, "json": json.loads(payload), "raw": payload}
            except json.JSONDecodeError:
                return {"ok": True, "status": resp.status, "json": None, "raw": payload}
    except urllib.error.HTTPError as exc:
        body_txt = exc.read().decode("utf-8", errors="replace") if exc.fp else ""
        try:
            detail = json.loads(body_txt)
        except json.JSONDecodeError:
            detail = body_txt
        return {"ok": False, "status": exc.code, "error": "http_error", "detail": detail}
    except urllib.error.URLError as exc:
        return {"ok": False, "status": None, "error": "url_error", "detail": str(exc.reason)}


def _http_get_json(url: str, *, timeout: float):
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except (urllib.error.URLError, json.JSONDecodeError, TimeoutError) as exc:
        return {"_error": str(exc)}


def upload_to_target(target: str, zip_bytes: bytes, package_name: str, *, force: bool, timeout: float) -> dict:
    base = target.rstrip("/")
    url = f"{base}/api/plugins/upload"
    filename = f"{package_name}.zip"
    return _http_post_multipart(url, zip_bytes, filename, force=force, timeout=timeout)


def verify_install(target: str, *, expected_version: str, timeout: float) -> dict:
    base = target.rstrip("/")
    plugins = _http_get_json(f"{base}/api/plugins", timeout=timeout)
    if isinstance(plugins, dict) and plugins.get("_error"):
        return {"ok": False, "stage": "list_plugins", "error": plugins["_error"]}

    plugin_record = next(
        (p for p in plugins if isinstance(p, dict) and p.get("id") == PLUGIN_ID),
        None,
    )
    if plugin_record is None:
        return {
            "ok": False,
            "stage": "list_plugins",
            "error": f"plugin '{PLUGIN_ID}' not in /api/plugins",
            "loaded_plugins": [p.get("id") for p in plugins if isinstance(p, dict)],
        }
    if not plugin_record.get("loaded"):
        return {"ok": False, "stage": "list_plugins", "error": "plugin not loaded", "record": plugin_record}
    if plugin_record.get("version") != expected_version:
        return {
            "ok": False,
            "stage": "list_plugins",
            "error": f"version mismatch (expected {expected_version}, got {plugin_record.get('version')})",
            "record": plugin_record,
        }

    skills = _http_get_json(f"{base}/api/skills", timeout=timeout)
    if isinstance(skills, dict) and skills.get("_error"):
        return {"ok": False, "stage": "list_skills", "error": skills["_error"]}
    skill_names = {s.get("name") if isinstance(s, dict) else None for s in skills}
    skill_names.discard(None)
    expected = {
        "opskeeper-alerter",
        "opskeeper-investigator",
        "opskeeper-critic",
        "opskeeper-reviewer",
        "opskeeper-repairer",
        "opskeeper-verifier",
        "opskeeper-postmortem",
    }
    missing = sorted(expected - skill_names)
    if missing:
        return {
            "ok": False,
            "stage": "list_skills",
            "error": f"missing {len(missing)} opskeeper-* skill(s)",
            "missing": missing,
            "found": sorted(skill_names & expected),
        }

    return {
        "ok": True,
        "plugin": plugin_record,
        "skills_found": sorted(skill_names & expected),
    }


def cmd_build(args: argparse.Namespace) -> int:
    zip_bytes, package_name, version = build_zip(_plugin_root())
    if args.out == "-":
        sys.stdout.buffer.write(zip_bytes)
        return 0
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_bytes(zip_bytes)
    print(f"wrote {len(zip_bytes):,} bytes -> {out}  ({package_name} v{version})")
    return 0


def _emit_per_target(results: list[dict], *, as_json: bool) -> None:
    if as_json:
        print(json.dumps(results, indent=2, default=str))
    else:
        for r in results:
            tag = "OK " if r.get("ok") else "FAIL"
            print(f"[{tag}] {r['target']}  stage={r.get('stage', '-')}")
            if r.get("ok"):
                print(f"      plugin=v{r['plugin']['version']}  skills={len(r.get('skills_found', []))}")
            else:
                print(f"      error={r.get('error')}")
                detail = r.get("detail")
                if detail is not None:
                    print(f"      detail={detail!r}"[:400])


def _run_targets(args: argparse.Namespace, *, do_verify: bool) -> int:
    targets: Sequence[str] = args.target
    if not targets and not args.build_only:
        raise SystemExit("ERROR: --target URL is required (repeat for multi-worker install)")

    plugin_root = Path(args.plugin_dir).resolve() if args.plugin_dir else _plugin_root()
    zip_bytes, package_name, version = build_zip(plugin_root)

    if args.keep_zip:
        keep = Path(args.keep_zip).resolve()
        keep.parent.mkdir(parents=True, exist_ok=True)
        keep.write_bytes(zip_bytes)
        print(f"# kept {len(zip_bytes):,} bytes at {keep} for debugging")

    if args.build_only:
        print(f"# built {package_name} v{version} ({len(zip_bytes):,} bytes); --build-only set, skipping upload")
        return 0

    force = not args.no_force
    results: list[dict] = []
    for target in targets:
        up = upload_to_target(target, zip_bytes, package_name, force=force, timeout=args.timeout)
        if not up.get("ok"):
            results.append(
                {
                    "target": target,
                    "ok": False,
                    "stage": "upload",
                    "error": up.get("error"),
                    "detail": up.get("detail"),
                }
            )
            continue
        if up.get("status", 0) >= 300:
            results.append(
                {
                    "target": target,
                    "ok": False,
                    "stage": "upload",
                    "error": f"http {up.get('status')}",
                    "detail": up.get("json") or up.get("raw"),
                }
            )
            continue
        if not do_verify:
            results.append(
                {
                    "target": target,
                    "ok": True,
                    "stage": "upload",
                    "response": up.get("json") or up.get("raw"),
                }
            )
            continue
        ver = verify_install(target, expected_version=version, timeout=args.timeout)
        ver["target"] = target
        if ver.get("ok"):
            ver["response"] = up.get("json")
        results.append(ver)

    _emit_per_target(results, as_json=args.json)
    return 0 if all(r.get("ok") for r in results) else 1


def cmd_upload(args: argparse.Namespace) -> int:
    return _run_targets(args, do_verify=False)


def cmd_install(args: argparse.Namespace) -> int:
    return _run_targets(args, do_verify=True)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--plugin-dir", help="Path to opskeeper-teamharness/ (default: auto-detect)")
    parser.add_argument(
        "--target",
        action="append",
        default=[],
        help="qwenpaw base URL (e.g. http://127.0.0.1:17281). Repeat for multi-worker install.",
    )
    parser.add_argument("--timeout", type=float, default=120.0, help="HTTP timeout (s)")
    parser.add_argument("--keep-zip", help="If set, save the staged zip to this path")
    parser.add_argument("--no-force", action="store_true", help="Do NOT pass force=true (refuse to overwrite an installed plugin)")
    parser.add_argument("--build-only", action="store_true", help="Just build zip and exit")
    parser.add_argument("--json", action="store_true", help="Emit JSON instead of human output")

    sub = parser.add_subparsers(dest="cmd", required=False)
    p_build = sub.add_parser("build", help="Only build the zip")
    p_build.add_argument("out", help="Output zip path, or - for stdout")
    p_build.set_defaults(func=cmd_build)

    p_upload = sub.add_parser("upload", help="Build + POST, no verification")
    p_upload.set_defaults(func=cmd_upload)

    p_install = sub.add_parser("install", help="Build + POST + verify (default)")
    p_install.set_defaults(func=cmd_install)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.cmd is None:
        args.func = cmd_install
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
