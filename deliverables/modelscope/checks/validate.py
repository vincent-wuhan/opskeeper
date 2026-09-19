#!/usr/bin/env python3

import argparse
import hashlib
import json
import re
import sys
import urllib.request
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
MANIFEST_PATH = ROOT / "submission.json"
ASSET_ROOT = ROOT / "assets"
REQUIRED_TOP_LEVEL = {
    "schema_version",
    "state",
    "source",
    "entries",
    "submissions",
    "assets",
    "verification",
    "rollback",
}
REQUIRED_ENTRY_IDS = {
    "official_website",
    "opskeeper_service",
    "agentteams_rooms",
    "agentteams_dashboard",
    "roadshow_console",
}
AUTHORITATIVE_ENTRIES = {
    "official_website": {
        "url": "https://opskeeper.yueming.xin",
        "role": "Product official website",
        "auth_expected": False,
        "required_for_public": True,
    },
    "opskeeper_service": {
        "url": "https://opskeeper.yueming.xin",
        "role": "OpsKeeper service",
        "auth_expected": True,
        "required_for_public": True,
    },
    "agentteams_rooms": {
        "url": "https://rooms.yueming.xin",
        "role": "AgentTeams Element rooms",
        "auth_expected": True,
        "required_for_public": True,
    },
    "agentteams_dashboard": {
        "url": "https://teams.yueming.xin",
        "role": "AgentTeams Dashboard",
        "auth_expected": True,
        "required_for_public": True,
    },
    "roadshow_console": {
        "url": "https://opskeeper.yueming.xin/live-incident",
        "role": "Full-flow roadshow console",
        "auth_expected": True,
        "required_for_public": True,
    },
}
SHA256_PATTERN = re.compile(r"^[0-9a-f]{64}$")
BLOCKED_PATTERNS = [
    re.compile(r"(?i)(api[_-]?key|secret|password|passwd|token)"),
    re.compile(r"\b(postgres(?:ql)?):[^/\s]+:[^@\s]+@"),
    re.compile(r"\b(10|172\.(?:1[6-9]|2\d|3[01]))\.\d+\.\d+\b"),
    re.compile(r"\b192\.168\.\d+\.\d+\b"),
]


def fail(errors, message):
    errors.append(message)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def is_object(value):
    return isinstance(value, dict)


def as_object(value, label, errors):
    if not is_object(value):
        fail(errors, f"{label} must be an object")
        return {}
    return value


def is_nonempty_string(value):
    return isinstance(value, str) and bool(value.strip())


def is_optional_string(value):
    return value is None or is_nonempty_string(value)


def is_optional_https_url(value):
    return value is None or (
        isinstance(value, str) and value.startswith("https://")
    )


def is_boolean(value):
    return type(value) is bool


def is_result_shape(value):
    if value is None or is_nonempty_string(value):
        return True
    if not is_object(value):
        return False
    status = value.get("status")
    checked_at = value.get("checked_at_utc")
    return is_nonempty_string(status) and (
        checked_at is None or is_nonempty_string(checked_at)
    )


def is_pass_result(value):
    if value == "pass":
        return True
    return (
        is_object(value)
        and value.get("status") == "pass"
        and is_nonempty_string(value.get("checked_at_utc"))
    )


def require_fields(value, fields, label, errors):
    missing = set(fields) - value.keys()
    if missing:
        fail(errors, f"missing {label} fields: {sorted(missing)}")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--links", action="store_true")
    parser.add_argument("--public", action="store_true")
    args = parser.parse_args()
    errors = []

    try:
        manifest = json.loads(MANIFEST_PATH.read_text(encoding="utf-8"))
    except Exception as exc:
        print(f"manifest parse failed: {exc}", file=sys.stderr)
        return 1

    if not is_object(manifest):
        fail(errors, "manifest must be an object")
        manifest = {}

    missing = REQUIRED_TOP_LEVEL - manifest.keys()
    if missing:
        fail(errors, f"missing top-level fields: {sorted(missing)}")
    if manifest.get("schema_version") != 1:
        fail(errors, "schema_version must be 1")

    state = as_object(manifest.get("state"), "state", errors)
    require_fields(
        state,
        ("package_status", "visibility", "generated_at_utc", "operator"),
        "state",
        errors,
    )
    if not is_nonempty_string(state.get("package_status")):
        fail(errors, "state.package_status must be a non-empty string")
    if state.get("visibility") not in ("private", "public"):
        fail(errors, "state.visibility must be private or public")
    if not is_optional_string(state.get("generated_at_utc")):
        fail(errors, "state.generated_at_utc must be null or a non-empty string")
    if not is_nonempty_string(state.get("operator")):
        fail(errors, "state.operator must be a non-empty string")

    source = as_object(manifest.get("source"), "source", errors)
    source_fields = (
        "github_repository",
        "default_branch",
        "authoritative_commit",
        "release_tag",
        "teamharness_version",
        "modelscope_code_url",
        "mirror_status",
    )
    require_fields(source, source_fields, "source", errors)
    if source.get("github_repository") != "https://github.com/vincent-wuhan/opskeeper":
        fail(errors, "source.github_repository is not the authoritative repository")
    if source.get("default_branch") != "main":
        fail(errors, "source.default_branch must be main")
    for field in (
        "authoritative_commit",
        "release_tag",
        "teamharness_version",
    ):
        if not is_optional_string(source.get(field)):
            fail(errors, f"source.{field} must be null or a non-empty string")
    if not is_optional_https_url(source.get("modelscope_code_url")):
        fail(errors, "source.modelscope_code_url must be null or an HTTPS URL")
    if not is_nonempty_string(source.get("mirror_status")):
        fail(errors, "source.mirror_status must be a non-empty string")

    raw_entries = manifest.get("entries")
    entries = []
    if not isinstance(raw_entries, list):
        fail(errors, "entries must be a list")
    else:
        entries = raw_entries
        if len(entries) != len(REQUIRED_ENTRY_IDS):
            fail(errors, "entries must contain exactly five items")
        seen_ids = set()
        for index, entry in enumerate(entries):
            label = f"entries[{index}]"
            entry = as_object(entry, label, errors)
            require_fields(
                entry,
                (
                    "id",
                    "url",
                    "role",
                    "required_for_public",
                    "auth_expected",
                    "health",
                ),
                label,
                errors,
            )
            entry_id = entry.get("id")
            if not is_nonempty_string(entry_id):
                fail(errors, f"{label}.id must be a non-empty string")
                continue
            if entry_id in seen_ids:
                fail(errors, f"duplicate entry id: {entry_id}")
            seen_ids.add(entry_id)
            expected = AUTHORITATIVE_ENTRIES.get(entry_id)
            if expected is None:
                fail(errors, f"unknown entry id: {entry_id}")
                continue
            for field, expected_value in expected.items():
                actual = entry.get(field)
                if actual != expected_value or type(actual) is not type(expected_value):
                    fail(
                        errors,
                        f"entry {entry_id} has unexpected {field}: {actual!r}",
                    )
            if entry.get("health") not in ("unknown", "pass", "fail"):
                fail(errors, f"entry {entry_id} health must be unknown, pass, or fail")

        if seen_ids != REQUIRED_ENTRY_IDS:
            fail(errors, f"entry ids differ: {sorted(seen_ids)}")

    submissions = as_object(manifest.get("submissions"), "submissions", errors)
    submission_fields = ("creative_space", "code_repository", "project_practice")
    require_fields(
        submissions,
        submission_fields + ("model_repository",),
        "submissions",
        errors,
    )
    for field in submission_fields:
        submission = as_object(
            submissions.get(field), f"submissions.{field}", errors
        )
        require_fields(
            submission,
            ("url", "status", "visibility"),
            f"submissions.{field}",
            errors,
        )
        if not is_optional_https_url(submission.get("url")):
            fail(errors, f"submissions.{field}.url must be null or an HTTPS URL")
        if not is_nonempty_string(submission.get("status")):
            fail(errors, f"submissions.{field}.status must be a non-empty string")
        if submission.get("visibility") not in ("private", "public"):
            fail(
                errors,
                f"submissions.{field}.visibility must be private or public",
            )

    model_repository = as_object(
        submissions.get("model_repository"),
        "submissions.model_repository",
        errors,
    )
    require_fields(
        model_repository,
        ("created", "reason"),
        "submissions.model_repository",
        errors,
    )
    if model_repository.get("created") is not False:
        fail(errors, "submissions.model_repository.created must be false")
    if not is_nonempty_string(model_repository.get("reason")):
        fail(
            errors,
            "submissions.model_repository.reason must be a non-empty string",
        )

    listed = set()
    raw_assets = manifest.get("assets")
    assets = []
    if not isinstance(raw_assets, list):
        fail(errors, "assets must be a list")
    else:
        assets = raw_assets

    asset_root = ASSET_ROOT.resolve()
    if ASSET_ROOT.exists() and not ASSET_ROOT.is_dir():
        fail(errors, "asset root must be a directory")
    for index, asset in enumerate(assets):
        label = f"assets[{index}]"
        asset = as_object(asset, label, errors)
        require_fields(
            asset,
            (
                "path",
                "title",
                "source",
                "type",
                "purpose",
                "sha256",
                "public_safe",
                "approval",
            ),
            label,
            errors,
        )
        relative = asset.get("path", "")
        if not is_nonempty_string(relative):
            fail(errors, f"{label}.path must be a non-empty string")
            continue
        if relative in listed:
            fail(errors, f"duplicate asset path: {relative}")
        path = (ASSET_ROOT / relative).resolve()
        if not path.is_relative_to(asset_root):
            fail(errors, f"asset escapes root: {relative}")
            continue
        if not path.is_file():
            fail(errors, f"asset missing: {relative}")
            continue
        listed.add(relative)
        for field in ("title", "source", "type", "purpose"):
            if not is_nonempty_string(asset.get(field)):
                fail(
                    errors,
                    f"asset {field} must be a non-empty string: {relative}",
                )
        if not isinstance(asset.get("sha256"), str) or not SHA256_PATTERN.fullmatch(
            asset["sha256"]
        ):
            fail(errors, f"asset sha256 must be 64 lowercase hex characters: {relative}")
        actual = digest(path)
        if asset.get("sha256") != actual:
            fail(errors, f"asset digest mismatch: {relative}")
        if asset.get("public_safe") is not True:
            fail(errors, f"asset not human-approved: {relative}")
        if asset.get("approval") != "human":
            fail(errors, f"asset approval must be human: {relative}")
        for label, value in (
            ("title", asset.get("title", "")),
            ("source", asset.get("source", "")),
        ):
            if any(pattern.search(str(value)) for pattern in BLOCKED_PATTERNS):
                fail(errors, f"unsafe asset {label}: {relative}")

    if ASSET_ROOT.is_dir():
        for path in ASSET_ROOT.rglob("*"):
            if path.is_file():
                relative = path.relative_to(ASSET_ROOT).as_posix()
                if relative not in listed:
                    fail(errors, f"unlisted asset: {relative}")

    verification = as_object(manifest.get("verification"), "verification", errors)
    verification_fields = (
        "private_readback",
        "public_readback",
        "safety_scan",
        "current_result",
    )
    require_fields(verification, verification_fields, "verification", errors)
    for field in verification_fields[:3]:
        if not is_result_shape(verification.get(field)):
            fail(
                errors,
                f"verification.{field} must be null, a result string, or a result object",
            )
    if verification.get("current_result") not in ("pending", "pass", "fail"):
        fail(errors, "verification.current_result must be pending, pass, or fail")

    rollback = as_object(manifest.get("rollback"), "rollback", errors)
    require_fields(rollback, ("trigger", "actions"), "rollback", errors)
    if not is_nonempty_string(rollback.get("trigger")):
        fail(errors, "rollback.trigger must be a non-empty string")
    rollback_actions = rollback.get("actions")
    if not isinstance(rollback_actions, list) or not rollback_actions:
        fail(errors, "rollback.actions must be a non-empty list")
    elif not all(is_nonempty_string(action) for action in rollback_actions):
        fail(errors, "rollback.actions values must be non-empty strings")

    if args.public:
        if state.get("visibility") != "public":
            fail(errors, "public validation requires visibility=public")
        required_entries = [
            entry
            for entry in entries
            if is_object(entry)
            and entry.get("id") in AUTHORITATIVE_ENTRIES
            and entry.get("required_for_public") is True
        ]
        if any(entry.get("health") != "pass" for entry in required_entries):
            fail(errors, "required public entry has not passed health readback")
        for field in ("private_readback", "public_readback", "safety_scan"):
            if not is_pass_result(verification.get(field)):
                fail(errors, f"verification.{field} must be pass-shaped and non-null")
        if verification.get("current_result") != "pass":
            fail(errors, "manifest current_result must be pass before public")

    if args.links:
        for entry in entries:
            if not is_object(entry):
                continue
            url = entry.get("url")
            if not isinstance(url, str) or not url.startswith("https://"):
                continue
            if not args.public and not entry.get("required_for_public"):
                continue
            request = urllib.request.Request(
                url,
                method="HEAD",
                headers={"User-Agent": "OpsKeeper-ModelScope-Preflight/1.0"},
            )
            try:
                with urllib.request.urlopen(request, timeout=15) as response:
                    status = response.status
            except Exception:
                try:
                    request = urllib.request.Request(
                        url,
                        headers={"User-Agent": "OpsKeeper-ModelScope-Preflight/1.0"},
                    )
                    with urllib.request.urlopen(request, timeout=15) as response:
                        status = response.status
                except Exception as exc:
                    fail(errors, f"link failed {entry['id']}: {exc}")
                    continue
            if status < 200 or status >= 400:
                fail(errors, f"link failed {entry['id']}: HTTP {status}")

    if errors:
        print("\n".join(f"FAIL: {error}" for error in errors))
        return 1

    print("PASS: ModelScope submission manifest and assets")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
