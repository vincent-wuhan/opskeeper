"""opskeeper-teamharness integration with QwenPaw 2 public plugin APIs.

Mirrors teamharness/adapters/qwenpaw/plugin.py structure:
- _sanitizer_factory: redact sensitive fields in tool outputs
- on_acting hook: log every tool invocation to opskeeper audit
- task_trace: track Worker task lifecycle
"""
from __future__ import annotations

import asyncio
import base64
import datetime as dt
import hashlib
import hmac
import functools
import importlib.util
import inspect
import json
import logging
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import zipfile
from collections import deque
from pathlib import Path
from typing import Any, AsyncGenerator, Callable, Optional


PLUGIN_DIR = Path(__file__).resolve().parent
ASSET_DIR = PLUGIN_DIR / "opskeeper-teamharness"
if not (ASSET_DIR / "plugin.yaml").exists():
    ASSET_DIR = PLUGIN_DIR.parent.parent


def _read_prompt(name: str) -> str:
    """Read a prompt file from assets/prompts/. Returns empty if missing."""
    candidates = [
        ASSET_DIR / "prompts" / name,
        PLUGIN_DIR / "prompts" / name,
        PLUGIN_DIR.parent / "prompts" / name,
    ]
    for path in candidates:
        try:
            return path.read_text(encoding="utf-8").strip()
        except OSError:
            continue
    return ""


def team_prompt(_agent: Any) -> str:
    """Manager team prompt override (opskeeper 协同规约)."""
    return _read_prompt("team/OPSKEEPER-TEAMS.md")


def worker_prompt(_agent: Any) -> str:
    """Worker prompt override (6 Worker 通用规则)."""
    return _read_prompt("agent/worker.md")


def manager_prompt(_agent: Any) -> str:
    """Manager agents.md prompt override."""
    return _read_prompt("manager/AGENTS.md")


# ===== sensitive field sanitization =====

_SANITIZER_KEYWORDS_ENV = "AGENTTEAMS_OUTPUT_SANITIZE_KEYWORDS"
_PERMISSION_MODE_ENV = "OPSKEEPER_PERMISSION_MODE"
_PLUGIN_VERSION = "1.0.70"
_COPAW_DIAGNOSTICS_LOGGER = logging.getLogger("opskeeper-teamharness.copaw-diagnostics")
_READ_ONLY_LOGGER = logging.getLogger("opskeeper-teamharness.readonly")
_MANAGER_GATE_LOGGER = logging.getLogger("opskeeper-teamharness.manager-gate")
_MANAGER_GATE_LOGGER.setLevel(logging.INFO)
_WORKFLOW_PROJECTOR_LOGGER = logging.getLogger(
    "opskeeper-teamharness.workflow-projector"
)
_MANAGER_GATE_TTL_ENV = "OPSKEEPER_MANAGER_GATE_TTL_SECONDS"
_MANAGER_GATE_STATE_FILE_ENV = "OPSKEEPER_MANAGER_GATE_STATE_FILE"
_WORKFLOW_PROJECTOR_STATE_FILE_ENV = "OPSKEEPER_WORKFLOW_PROJECTOR_STATE_FILE"
_OUTBOUND_LIMIT_ENV = "OPSKEEPER_OUTBOUND_LIMIT"
_OUTBOUND_WINDOW_SECONDS_ENV = "OPSKEEPER_OUTBOUND_WINDOW_SECONDS"
_DEFAULT_OUTBOUND_LIMIT = 12
_DEFAULT_OUTBOUND_WINDOW_SECONDS = 10.0
_DEFAULT_MANAGER_GATE_TTL_SECONDS = 600.0
_THINKING_SPAN_PATTERN = re.compile(
    r"<\s*think\s*>.*?<\s*/\s*think\s*>",
    re.DOTALL | re.IGNORECASE,
)
_THINKING_OPEN_PATTERN = re.compile(r"<\s*think\s*>", re.IGNORECASE)
_THINKING_CLOSE_PATTERN = re.compile(r"<\s*/\s*think\s*>", re.IGNORECASE)
_LEADING_PARTIAL_THINKING_PATTERN = re.compile(
    r"^\s*<\s*think\s*>.*\Z",
    re.DOTALL | re.IGNORECASE,
)
_ADMIN_STOP_PATTERN = re.compile(
    r"^\s*ADMIN\s+STOP\s+([A-Za-z0-9][A-Za-z0-9._:-]{0,127})\s*$",
    re.IGNORECASE,
)
_TASK_MARKER_PATTERN = re.compile(
    r"\bOPSKEEPER[\s_]+TASK[\s_]+([A-Za-z0-9][A-Za-z0-9._:-]{0,127})\b"
)
_WORKFLOW_INCIDENT_EXPLICIT_PATTERN = re.compile(
    r"(?:incident[_ -]?id|事故\s*(?:id|编号))\s*[:=]?\s*"
    r"([A-Za-z0-9][A-Za-z0-9._:-]{5,127})",
    re.IGNORECASE,
)
_WORKFLOW_INCIDENT_LOOSE_PATTERN = re.compile(
    r"\b(opskeeper(?:-[A-Za-z0-9_]+){2,})\b",
    re.IGNORECASE,
)
_FINAL_DEMO_INCIDENT_ID_PATTERN = re.compile(
    r"\bincident_id(?:=|:)\s*([0-9]{1,18})\b",
    re.IGNORECASE,
)
_FINAL_DEMO_CANDIDATE_A_PATTERN = re.compile(
    r"\bcandidate[\s_-]+a\b",
    re.IGNORECASE,
)
_MATRIX_CURRENT_MESSAGE_MARKER = "[Current message - respond to this]"
_WORKFLOW_ROLE_PATTERN = re.compile(
    r"@?opskeeper-(alerter|investigator|reviewer|repairer|verifier|reporter)"
    r"(?::[A-Za-z0-9_.:-]+)?",
    re.IGNORECASE,
)
_WORKFLOW_ADMIN_APPROVAL_PATTERN = re.compile(
    r"(?m)^\s*[^\r\n]{0,200}?"
    r"(?:批准|同意|approve(?:d)?)\b",
    re.IGNORECASE,
)
_WORKFLOW_ADMIN_REJECTION_PATTERN = re.compile(
    r"(?m)^\s*[^\r\n]{0,200}?"
    r"(?:拒绝|不同意|reject(?:ed)?)\b",
    re.IGNORECASE,
)
_WORKFLOW_AUTHORITY_TOKEN_PATTERN = re.compile(
    r"(?m)^OPSKEEPER_AUTHORITY_V1\s+"
    r"([A-Za-z0-9_-]+\.[0-9a-f]{64})$",
    re.IGNORECASE,
)
_WORKFLOW_PLAIN_STAGE_PATTERN = re.compile(
    r"\bworkflow_stage\s*[:=]", re.IGNORECASE
)
_WORKFLOW_AUTHORITY_MAX_AGE_SECONDS = 120
_WORKFLOW_AUTHORITY_CLOCK_SKEW_SECONDS = 5
_WORKFLOW_AUTHORITY_NONCES: dict[str, float] = {}
_WORKFLOW_AUTHORITY_NONCE_LOCK = threading.RLock()
_WORKFLOW_AUTHORITY_STEPS = {
    "preview_ready": ("review", "in_progress"),
    "awaiting_approval": ("approval", "in_progress"),
    "repair_dispatched": ("repair", "in_progress"),
    "verifying": ("verify", "in_progress"),
    "recovered": ("verify", "completed"),
}
_WORKFLOW_STEPS: tuple[tuple[str, str], ...] = (
    ("alert", "告警确认"),
    ("investigate", "根因诊断"),
    ("review", "方案评审"),
    ("approval", "人工审批"),
    ("repair", "修复执行"),
    ("verify", "独立验证"),
    ("report", "复盘归档"),
)
_WORKFLOW_STEP_BY_ROLE = {
    "alerter": "alert",
    "investigator": "investigate",
    "reviewer": "review",
    "repairer": "repair",
    "verifier": "verify",
    "reporter": "report",
}
_WORKFLOW_MAX_RUNS = 32
_OPSKEEPER_ROLE_MENTION_PATTERN = re.compile(
    r"@(?P<role>opskeeper-[a-z0-9_.-]+)(?::[a-z0-9_.-]+)?",
    re.IGNORECASE,
)
_MANAGER_ROLE_MENTION_PATTERN = re.compile(
    r"@manager(?::[a-z0-9_.:-]+)?(?![a-z0-9_.-])",
    re.IGNORECASE,
)
_COPAW_BASE_TOOLS = frozenset({"message", "filesync", "projectflow", "taskflow"})
_COPAW_NATIVE_TOOLS = {
    "opskeeper__recovery_execute": "recovery.execute",
    "opskeeper__recovery_verify": "recovery.verify",
    "opskeeper__incident_record": "incident.record",
    "opskeeper__incident_timeline": "incident.timeline",
    "opskeeper__task_state_put": "state.put",
    "opskeeper__state_get": "state.get",
    "opskeeper__knowledge_write": "knowledge.write",
}
_COPAW_DIAGNOSTICS = {
    "runtime": "copaw",
    "wrap_installed": False,
    "toolkit_validated": False,
    "readonly_middleware": False,
    "manager_gate": False,
    "native_tool_count": 0,
    "installed_native_tools": [],
    "missing_capabilities": [
        "copaw_toolkit_hook",
        "readonly_middleware",
        "manager_gate",
        "new_task_context",
        "native_mcp_tools",
    ],
    "install_error": None,
    "toolkit_error": None,
}


def _credential_value(text: str, indent: int, name: str) -> str:
    pattern = rf"(?m)^ {{{indent}}}{re.escape(name)}:\s*[\"']?([^\"'\n]+)[\"']?\s*$"
    match = re.search(pattern, text)
    return match.group(1).strip() if match else ""


def _credential_from_file(path: Path, name: str) -> str:
    try:
        text = path.read_text(encoding="utf-8")
        server_match = re.search(r"(?ms)^  mcp/opskeeper:\n(.*?)(?=^  \S|\Z)", text)
        if not server_match:
            return ""
        secrets_match = re.search(
            r"(?ms)^    secrets:\n(.*?)(?=^    \S|\Z)",
            server_match.group(1),
        )
        if not secrets_match:
            return ""
        return _credential_value(secrets_match.group(1), 6, name)
    except (OSError, UnicodeError):
        return ""


def _runtime_credential(name: str) -> str:
    value = os.environ.get(name, "").strip()
    decoded = _decode_runtime_credential(value)
    if decoded:
        return decoded

    explicit_path = os.environ.get("OPSKEEPER_CREDENTIALS_FILE", "").strip()
    candidates = [Path(explicit_path)] if explicit_path else []
    for directory in (ASSET_DIR, *ASSET_DIR.parents):
        if directory.name == ".qwenpaw":
            candidates.append(directory / "workspaces" / "default" / "credentials.yaml")

    for candidate in candidates:
        value = _credential_from_file(candidate, name)
        decoded = _decode_runtime_credential(value)
        if decoded:
            return decoded
    return ""


def _decode_runtime_credential(value: str) -> str:
    if not value:
        return ""
    if not value.upper().startswith("ENC:"):
        return value
    try:
        from qwenpaw.security.secret_store import decrypt
    except ImportError:
        return ""
    decoded = decrypt(value)
    return "" if decoded.upper().startswith("ENC:") else decoded


def _runtime_gateway_key() -> str:
    """Get the injected MCP key, falling back to QwenPaw's credential store."""
    return (
        _runtime_credential("OPSKEEPER_GATEWAY_KEY")
        or os.environ.get("AGENTTEAMS_WORKER_GATEWAY_KEY", "")
    )


def _runtime_backend_url() -> str:
    return _runtime_credential("OPSKEEPER_BACKEND_URL") or "http://opskeeper:8080"


def _runtime_tenant_id() -> str:
    return _runtime_credential("OPSKEEPER_TENANT_ID") or "default"


def _current_matrix_message(message: str) -> str:
    if _MATRIX_CURRENT_MESSAGE_MARKER in message:
        return message.rsplit(_MATRIX_CURRENT_MESSAGE_MARKER, 1)[-1]
    return message


def _final_demo_incident_id(message: str) -> str:
    current_message = _current_matrix_message(message)
    matches = _FINAL_DEMO_INCIDENT_ID_PATTERN.findall(current_message)
    return matches[-1] if matches else ""


def _dispatch_final_demo_approval(session_id: str, sender: str, message: str) -> bool:
    room_id = os.environ.get("OPSKEEPER_DEMO_MATRIX_ROOM", "").strip()
    backend = (
        os.environ.get("OPSKEEPER_MANAGER_URL", "").strip()
        or os.environ.get("OPSKEEPER_BACKEND_URL", "").strip()
    )
    token = os.environ.get("OPSKEEPER_DEMO_API_TOKEN", "").strip()
    incident_id = _final_demo_incident_id(message)
    if not room_id or not backend or not token or not incident_id:
        return False
    if session_id != f"matrix:{room_id}" or not sender:
        return False

    payload = json.dumps({"approver_id": sender}).encode("utf-8")
    request = urllib.request.Request(
        f"{backend.rstrip('/')}/api/v1/demo/incidents/{incident_id}/approve",
        data=payload,
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
            "X-Opskeeper-Version": "v1",
        },
        method="POST",
    )
    retryable_statuses = {502, 503, 504}
    for attempt in range(3):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                response.read()
            return True
        except urllib.error.HTTPError as error:
            error.read()
            if error.code == 409:
                return True
            if error.code in retryable_statuses and attempt < 2:
                time.sleep(1.5 * (attempt + 1))
                continue
            _MANAGER_GATE_LOGGER.warning(
                "Final demo deterministic approval failed incident=%s http=%s",
                incident_id,
                error.code,
            )
        except Exception:
            _MANAGER_GATE_LOGGER.warning(
                "Final demo deterministic approval failed incident=%s",
                incident_id,
                exc_info=True,
            )
        break
    return False

_TASK_RESULT_PATTERN = re.compile(
    r"(?m)^(?:[`*_]*(?:@[A-Za-z0-9._=-]+(?::[A-Za-z0-9._=-]+)+|manager)[`*_]*[ \t]+)?"
    r"[`*_]*[ \t]*OPSKEEPER[\s_]+RESULT[\s_]+"
    r"([A-Za-z0-9][A-Za-z0-9._:-]{0,127})(?:\s|[`*_]|$)"
)
_TASK_ID_PATTERN = re.compile(r"\bOPSKEEPER-[A-Za-z0-9][A-Za-z0-9._:-]{2,127}\b")
_TASK_COMPLETE_PATTERN = re.compile(r"\bOPSKEEPER_COMPLETE\s+[A-Za-z0-9][A-Za-z0-9._:-]{2,127}\b")
_WORKER_FILE_ARTIFACT_PATTERN = re.compile(
    r"(?:创建|写入?|creat(?:e|ing)|writ(?:e|ing))[\s\S]{0,160}?(?:plan|result|spec)\.md",
    re.IGNORECASE,
)
_READ_ONLY_ALLOWED_TOOLS = frozenset({
    "message",
    "teamharness.message",
    "read_file",
    "grep_search",
    "glob_search",
    "view_image",
    "view_video",
    "get_current_time",
    "get_token_usage",
    "opskeeper.metric.query",
    "opskeeper.query.promql",
    "opskeeper.query.incidents",
    "opskeeper.get.incident.detail",
    "opskeeper.incident.list",
    "opskeeper.incident.get",
    "opskeeper.analyze.database.status",
    "opskeeper.postgres.analyze.status",
    "opskeeper.host.get_load",
    "opskeeper.host.get_processes",
    "opskeeper.knowledge.query",
    "opskeeper.query.knowledge",
    "opskeeper.state.get",
    "opskeeper.task.state.put",
    "opskeeper.recovery.verify",
    "opskeeper.incident.record",
    "opskeeper.incident.timeline",
    "opskeeper.loop.correlate",
    "opskeeper.loop.investigate",
})
_ROLE_GATED_MUTATING_TOOLS = frozenset({
    # Reporter may persist postmortem knowledge. OpsKeeper rejects this tool
    # for every other AgentTeams role at role-token exchange and MCP layers.
    "opskeeper.knowledge.write",
    "opskeeper.incident.record",
})


def _is_proposal_bound_recovery_execute(normalized_name: str, arguments: dict[str, Any]) -> bool:
    if normalized_name != "opskeeper.recovery.execute":
        return False
    parameters = arguments.get("parameters")
    if not isinstance(parameters, dict):
        parameters = {}

    def required_id(value: Any) -> bool:
        return isinstance(value, str) and bool(value.strip())

    required_arguments = (
        arguments.get("incident_id"),
        arguments.get("proposal_id"),
        arguments.get("skill_id"),
        arguments.get("target"),
        arguments.get("resource_type"),
    )
    required_parameters = (
        parameters.get("command"),
        parameters.get("reason"),
    )
    if not all(required_id(value) for value in required_arguments):
        return False
    if not all(required_id(value) for value in required_parameters):
        return False
    if parameters.get("skip_audit") is True:
        return False

    command = parameters.get("command")
    if command == "resize_pool":
        return (
            parameters.get("incident_id") == arguments.get("incident_id")
            and required_id(parameters.get("pool_manifest_id"))
        )
    if command == "kill_process":
        return (
            parameters.get("incident_id") == arguments.get("incident_id")
            and required_id(parameters.get("fixture_manifest_id"))
        )
    if command == "restart_service":
        return bool(parameters.get("device_id")) and required_id(parameters.get("service"))
    return command == "noop"


def _extract_task_markers(message: str) -> tuple[str, ...]:
    if _TASK_COMPLETE_PATTERN.search(message):
        return ()
    markers = tuple(
        match.group(1)
        for match in _TASK_MARKER_PATTERN.finditer(message)
    )
    if markers:
        return markers

    for candidate in _TASK_ID_PATTERN.findall(message):
        if len(candidate) >= 12 and candidate.count("-") >= 2:
            return (candidate,)
    return ()


class ManagerDispatchGate:
    """Track dispatched OpsKeeper task markers for the current Manager process."""

    def __init__(self) -> None:
        self._pending: dict[tuple[str, str], float] = {}
        self._origins: dict[tuple[str, str], str] = {}
        self._request_origins = self._load_request_origins()
        self._lock = threading.RLock()

    @staticmethod
    def _state_path() -> Path:
        configured = os.getenv(_MANAGER_GATE_STATE_FILE_ENV, "").strip()
        if configured:
            return Path(configured).expanduser()
        return ASSET_DIR / ".manager-request-origins.json"

    @classmethod
    def _load_request_origins(cls) -> dict[str, str]:
        path = cls._state_path()
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return {}
        except (OSError, UnicodeError, json.JSONDecodeError):
            _MANAGER_GATE_LOGGER.warning(
                "Manager request-origin state ignored path=%s",
                path,
                exc_info=True,
            )
            return {}
        if not isinstance(payload, dict) or payload.get("version") != 1:
            return {}
        raw_origins = payload.get("origins")
        if not isinstance(raw_origins, dict):
            return {}
        now = time.time()
        return {
            marker: entry["origin"]
            for marker, entry in raw_origins.items()
            if isinstance(marker, str)
            and isinstance(entry, dict)
            and isinstance(entry.get("origin"), str)
            and isinstance(entry.get("expires_at"), (int, float))
            and float(entry["expires_at"]) > now
        }

    def _persist_request_origins(self) -> None:
        path = self._state_path()
        expires_at = time.time() + self.ttl_seconds()
        payload = {
            "version": 1,
            "origins": {
                marker: {"origin": origin, "expires_at": expires_at}
                for marker, origin in self._request_origins.items()
            },
        }
        temporary_name = ""
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            descriptor, temporary_name = tempfile.mkstemp(
                prefix=f".{path.name}.",
                dir=path.parent,
            )
            with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
                json.dump(payload, handle, ensure_ascii=False, separators=(",", ":"))
                handle.write("\n")
                handle.flush()
                os.fsync(handle.fileno())
            os.chmod(temporary_name, 0o600)
            os.replace(temporary_name, path)
            temporary_name = ""
        except OSError:
            _MANAGER_GATE_LOGGER.warning(
                "Manager request-origin state persist failed path=%s",
                path,
                exc_info=True,
            )
        finally:
            if temporary_name:
                try:
                    os.unlink(temporary_name)
                except OSError:
                    pass

    def record_request_origin(self, session_id: str, message: str) -> tuple[str, ...]:
        markers = _extract_task_markers(message)
        if not markers:
            return ()
        with self._lock:
            for marker in markers:
                self._request_origins[marker] = session_id
            self._persist_request_origins()
        _MANAGER_GATE_LOGGER.info(
            "Manager request origin recorded markers=%s origin=%s",
            list(markers),
            session_id,
        )
        return markers

    def record(
        self,
        session_id: str,
        message: str,
        origin_session_id: str = "",
    ) -> tuple[str, ...]:
        markers = _extract_task_markers(message)
        if not markers:
            return ()
        now = time.monotonic()
        with self._lock:
            for marker in markers:
                key = (session_id, marker)
                self._pending[key] = now
                self._origins[key] = (
                    self._request_origins.get(marker)
                    or origin_session_id
                    or session_id
                )
        return markers

    def pending_markers(self, session_id: str) -> tuple[str, ...]:
        now = time.monotonic()
        with self._lock:
            expired = [key for key, created_at in self._pending.items() if now - created_at > self.ttl_seconds()]
            for key in expired:
                del self._pending[key]
            return tuple(marker for session, marker in self._pending if session == session_id)

    def any_pending(self) -> bool:
        with self._lock:
            return bool(self._pending)

    def has_pending(self, session_id: str, marker: str) -> bool:
        with self._lock:
            return (session_id, marker) in self._pending

    def consume_result_with_origins(
        self,
        session_id: str,
        message: str,
    ) -> dict[str, str]:
        markers = tuple(match.group(1) for match in _TASK_RESULT_PATTERN.finditer(message))
        if not markers:
            return {}
        consumed: dict[str, str] = {}
        with self._lock:
            for marker in markers:
                request_origin = self._request_origins.pop(marker, "")
                matching_keys = [
                    key for key in self._pending if key[1] == marker
                ]
                if matching_keys:
                    origin = request_origin or self._origins.pop(
                        matching_keys[0],
                        session_id,
                    )
                    for key in matching_keys:
                        self._pending.pop(key, None)
                        self._origins.pop(key, None)
                    consumed[marker] = origin
                elif request_origin:
                    consumed[marker] = request_origin
            self._persist_request_origins()
        _MANAGER_GATE_LOGGER.info(
            "Manager worker results consumed session=%s markers=%s consumed=%s",
            session_id,
            list(markers),
            list(consumed),
        )
        return consumed

    def consume_result(self, session_id: str, message: str) -> tuple[str, ...]:
        return tuple(self.consume_result_with_origins(session_id, message))

    def clear(self, session_id: str) -> None:
        with self._lock:
            stale_origins = [key for key in self._origins if key[0] == session_id]
            for key in stale_origins:
                del self._origins[key]
            stale_request_origins = [
                marker for marker, origin in self._request_origins.items()
                if origin == session_id
            ]
        for marker in stale_request_origins:
            del self._request_origins[marker]
            self._pending = {
                key: created_at for key, created_at in self._pending.items() if key[0] != session_id
            }
            self._persist_request_origins()

    @staticmethod
    def ttl_seconds() -> float:
        try:
            value = float(os.getenv(_MANAGER_GATE_TTL_ENV, str(_DEFAULT_MANAGER_GATE_TTL_SECONDS)))
        except ValueError:
            return _DEFAULT_MANAGER_GATE_TTL_SECONDS
        if value <= 0:
            return _DEFAULT_MANAGER_GATE_TTL_SECONDS
        return min(value, 3600.0)


_MANAGER_DISPATCH_GATE = ManagerDispatchGate()
_STOPPED_INCIDENT_IDS: set[str] = set()


def _workflow_incident_id(message: str) -> str:
    match = _WORKFLOW_INCIDENT_EXPLICIT_PATTERN.search(message)
    if match:
        return match.group(1).rstrip("。，,；;）)]】】")
    match = _WORKFLOW_INCIDENT_LOOSE_PATTERN.search(message)
    if match:
        candidate = match.group(1)
        if _TASK_MARKER_PATTERN.search(message) and candidate.isupper():
            return ""
        return candidate
    return ""


def _workflow_role(message: str) -> str:
    match = _WORKFLOW_ROLE_PATTERN.search(message)
    return match.group(1).lower() if match else ""


def _verify_workflow_authority(
    sender: str, message: str, origin: str
) -> tuple[str, str] | None:
    global _WORKFLOW_AUTHORITY_NONCES
    match = _WORKFLOW_AUTHORITY_TOKEN_PATTERN.search(message)
    if not match:
        return None
    secret = os.environ.get("OPSKEEPER_WORKFLOW_AUTHORITY_SECRET", "").strip()
    expected_manager = os.environ.get(
        "OPSKEEPER_WORKFLOW_AUTHORITY_MANAGER_ID", ""
    ).strip()
    if len(secret) < 16 or not sender or not expected_manager or sender != expected_manager:
        return None
    encoded_claims, signature = match.group(1).split(".", 1)
    try:
        padding = "=" * (-len(encoded_claims) % 4)
        claims_bytes = base64.urlsafe_b64decode(encoded_claims + padding)
        expected_signature = hmac.new(
            secret.encode(), claims_bytes, hashlib.sha256
        ).hexdigest()
        if not hmac.compare_digest(signature, expected_signature):
            return None
        claims = json.loads(claims_bytes)
        issued_at = dt.datetime.fromisoformat(
            str(claims.get("issued_at", "")).replace("Z", "+00:00")
        )
        expires_at = dt.datetime.fromisoformat(
            str(claims.get("expires_at", "")).replace("Z", "+00:00")
        )
        now = dt.datetime.now(dt.timezone.utc)
        lifetime = (expires_at - issued_at).total_seconds()
        if (
            issued_at.tzinfo is None
            or expires_at.tzinfo is None
            or issued_at > now + dt.timedelta(seconds=_WORKFLOW_AUTHORITY_CLOCK_SKEW_SECONDS)
            or expires_at <= now
            or lifetime <= 0
            or lifetime > _WORKFLOW_AUTHORITY_MAX_AGE_SECONDS
        ):
            return None
        incident_id = str(claims.get("incident_id", ""))
        stage = str(claims.get("stage", ""))
        manager_id = str(claims.get("manager_id", ""))
        room_id = str(claims.get("room_id", ""))
        nonce = str(claims.get("nonce", ""))
        if (
            not incident_id
            or manager_id != expected_manager
            or stage not in _WORKFLOW_AUTHORITY_STEPS
            or not room_id
            or not origin.startswith("matrix:")
            or origin != f"matrix:{room_id}"
            or len(nonce) < 32
        ):
            return None
        with _WORKFLOW_AUTHORITY_NONCE_LOCK:
            expired_at = _WORKFLOW_AUTHORITY_NONCES.get(nonce)
            if expired_at is not None:
                return None
            stale_nonces = [
                value for value in _WORKFLOW_AUTHORITY_NONCES.values() if value <= now.timestamp()
            ]
            if stale_nonces:
                _WORKFLOW_AUTHORITY_NONCES = {
                    key: value
                    for key, value in _WORKFLOW_AUTHORITY_NONCES.items()
                    if value > now.timestamp()
                }
            _WORKFLOW_AUTHORITY_NONCES[nonce] = expires_at.timestamp()
        return incident_id, stage
    except (ValueError, TypeError, json.JSONDecodeError, UnicodeDecodeError):
        return None


class WorkflowProjector:
    """Build full AgentTeams workflow snapshots from authoritative transitions."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._runs, self._marker_runs = self._load()

    @staticmethod
    def _state_path() -> Path:
        configured = os.getenv(_WORKFLOW_PROJECTOR_STATE_FILE_ENV, "").strip()
        if configured:
            return Path(configured).expanduser()
        return ASSET_DIR / ".workflow-projector.json"

    @classmethod
    def _load(cls) -> tuple[dict[str, dict[str, Any]], dict[str, str]]:
        path = cls._state_path()
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return {}, {}
        except (OSError, UnicodeError, json.JSONDecodeError):
            _WORKFLOW_PROJECTOR_LOGGER.warning(
                "Workflow projector state ignored path=%s",
                path,
                exc_info=True,
            )
            return {}, {}
        if not isinstance(payload, dict) or payload.get("version") != 1:
            return {}, {}
        raw_runs = payload.get("runs")
        raw_markers = payload.get("markers")
        if not isinstance(raw_runs, dict) or not isinstance(raw_markers, dict):
            return {}, {}
        runs = {
            run_id: cls._sanitize_run(run_id, run)
            for run_id, run in raw_runs.items()
            if isinstance(run_id, str) and isinstance(run, dict)
        }
        markers = {
            marker: run_id
            for marker, run_id in raw_markers.items()
            if isinstance(marker, str) and isinstance(run_id, str) and run_id in runs
        }
        return runs, markers

    @staticmethod
    def _sanitize_run(run_id: str, raw: dict[str, Any]) -> dict[str, Any]:
        steps = {step_id: "pending" for step_id, _ in _WORKFLOW_STEPS}
        raw_steps = raw.get("steps") if isinstance(raw.get("steps"), dict) else {}
        for step_id, status in raw_steps.items():
            if step_id in steps and isinstance(status, str):
                steps[step_id] = status
        roles = {role: "pending" for role in _WORKFLOW_STEP_BY_ROLE}
        raw_roles = raw.get("workers") if isinstance(raw.get("workers"), dict) else {}
        for role, status in raw_roles.items():
            if role in roles and isinstance(status, str):
                roles[role] = status
        return {
            "run_id": run_id,
            "origin": str(raw.get("origin") or ""),
            "status": str(raw.get("status") or "pending"),
            "summary": str(raw.get("summary") or "OpsKeeper workflow accepted"),
            "steps": steps,
            "workers": roles,
            "created_at": float(raw.get("created_at") or time.time()),
            "updated_at": float(raw.get("updated_at") or time.time()),
            "signature": str(raw.get("signature") or ""),
        }

    def _persist(self) -> None:
        path = self._state_path()
        ordered_runs = sorted(
            self._runs.items(), key=lambda item: item[1]["updated_at"], reverse=True
        )[:_WORKFLOW_MAX_RUNS]
        kept_runs = dict(ordered_runs)
        kept_markers = {
            marker: run_id
            for marker, run_id in self._marker_runs.items()
            if run_id in kept_runs
        }
        payload = {"version": 1, "runs": kept_runs, "markers": kept_markers}
        temporary_name = ""
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            descriptor, temporary_name = tempfile.mkstemp(
                prefix=f".{path.name}.", dir=path.parent
            )
            with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
                json.dump(payload, handle, ensure_ascii=False, separators=(",", ":"))
                handle.write("\n")
            os.chmod(temporary_name, 0o600)
            os.replace(temporary_name, path)
            temporary_name = ""
        except OSError:
            _WORKFLOW_PROJECTOR_LOGGER.warning(
                "Workflow projector state persist failed path=%s",
                path,
                exc_info=True,
            )
        finally:
            if temporary_name:
                try:
                    os.unlink(temporary_name)
                except OSError:
                    pass

    @staticmethod
    def _new_run(run_id: str, origin: str) -> dict[str, Any]:
        now = time.time()
        return {
            "run_id": run_id,
            "origin": origin,
            "status": "assigned",
            "summary": "Incident accepted; waiting for Manager dispatch",
            "steps": {step_id: "pending" for step_id, _ in _WORKFLOW_STEPS},
            "workers": {role: "pending" for role in _WORKFLOW_STEP_BY_ROLE},
            "created_at": now,
            "updated_at": now,
            "signature": "",
        }

    @staticmethod
    def _signature(run: dict[str, Any]) -> str:
        material = {
            "status": run["status"],
            "steps": run["steps"],
            "workers": run["workers"],
        }
        return hashlib.sha256(
            json.dumps(material, sort_keys=True, separators=(",", ":")).encode()
        ).hexdigest()

    def _run_for_message(
        self, origin: str, message: str, marker: str = ""
    ) -> tuple[str, dict[str, Any]] | None:
        run_id = _workflow_incident_id(message) or self._marker_runs.get(marker, "")
        if not run_id:
            candidates = [
                run for run in self._runs.values() if run["origin"] == origin
            ]
            if candidates:
                run_id = max(candidates, key=lambda run: run["updated_at"])["run_id"]
        run = self._runs.get(run_id)
        if run is None:
            return None
        return run_id, run

    def _finish_mutation(self, run: dict[str, Any]) -> dict[str, Any] | None:
        run["updated_at"] = time.time()
        signature = self._signature(run)
        if signature == run["signature"]:
            return None
        run["signature"] = signature
        return self.payload(run["run_id"])

    def record_request(self, origin: str, message: str) -> dict[str, Any] | None:
        if (
            _TASK_RESULT_PATTERN.search(message)
            or _TASK_COMPLETE_PATTERN.search(message)
            or _WORKFLOW_AUTHORITY_TOKEN_PATTERN.search(message)
            or _WORKFLOW_PLAIN_STAGE_PATTERN.search(message)
        ):
            return None
        run_id = _workflow_incident_id(message)
        if not run_id:
            markers = _extract_task_markers(message)
            run_id = markers[0] if markers else ""
        if not run_id or not origin.startswith("matrix:!"):
            return None
        with self._lock:
            run = self._runs.get(run_id)
            if run is None or run["status"] in {"completed", "failed", "blocked"}:
                run = self._new_run(run_id, origin)
                self._runs[run_id] = run
            run["origin"] = origin
            run["status"] = "assigned"
            run["summary"] = "Incident accepted; waiting for Manager dispatch"
            run["steps"]["alert"] = "pending"
            run["workers"]["alerter"] = "pending"
            payload = self._finish_mutation(run)
            self._persist()
            return payload

    def record_authority_stage(
        self, origin: str, run_id: str, stage: str
    ) -> dict[str, Any] | None:
        selected_step, selected_status = _WORKFLOW_AUTHORITY_STEPS.get(stage, ("", ""))
        if not selected_step or not run_id or not origin.startswith("matrix:!"):
            return None
        with self._lock:
            run = self._runs.get(run_id)
            if run is None:
                run = self._new_run(run_id, origin)
                self._runs[run_id] = run
            run["origin"] = origin
            run["status"] = "in_progress"
            run["summary"] = f"Manager authority stage: {stage}"
            reached_stage = False
            for step_id, _ in _WORKFLOW_STEPS:
                if step_id == selected_step:
                    reached_stage = True
                    run["steps"][step_id] = selected_status
                    continue
                if not reached_stage and run["steps"][step_id] not in {"completed", "failed"}:
                    run["steps"][step_id] = "completed"
                elif reached_stage and step_id not in {"approval", "report"}:
                    run["steps"][step_id] = "pending"
            if selected_step == "awaiting_approval":
                run["workers"]["reviewer"] = "completed"
            elif selected_step == "repair_dispatched":
                run["workers"]["reviewer"] = "completed"
                run["workers"]["repairer"] = "in_progress"
            elif selected_step == "verifying":
                run["workers"]["repairer"] = "completed"
                run["workers"]["verifier"] = "in_progress"
            elif selected_step == "recovered":
                run["workers"]["repairer"] = "completed"
                run["workers"]["verifier"] = "completed"
            payload = self._finish_mutation(run)
            self._persist()
            return payload

    def record_dispatch(
        self, origin: str, message: str, fallback_run_id: str = ""
    ) -> dict[str, Any] | None:
        markers = _extract_task_markers(message)
        if not markers:
            return None
        role = _workflow_role(message)
        step_id = _WORKFLOW_STEP_BY_ROLE.get(role, "")
        if not step_id:
            return None
        with self._lock:
            selected = self._run_for_message(origin, message, markers[0])
            if selected is None and fallback_run_id:
                self._runs.setdefault(
                    fallback_run_id, self._new_run(fallback_run_id, origin)
                )
                selected = fallback_run_id, self._runs[fallback_run_id]
            if selected is None:
                return None
            _, run = selected
            run["origin"] = origin
            run["status"] = "in_progress"
            run["summary"] = f"Dispatched {role}: {step_id}"
            for candidate_id, _ in _WORKFLOW_STEPS:
                if candidate_id == step_id:
                    break
                if run["steps"][candidate_id] not in {"completed", "failed"}:
                    run["steps"][candidate_id] = "completed"
            run["steps"][step_id] = "in_progress"
            run["workers"][role] = "in_progress"
            self._marker_runs[markers[0]] = run["run_id"]
            payload = self._finish_mutation(run)
            self._persist()
            return payload

    def record_result(self, marker: str, message: str) -> dict[str, Any] | None:
        run_id = self._marker_runs.get(marker, "")
        run = self._runs.get(run_id)
        if run is None:
            return None
        role = _workflow_role(message)
        if not role:
            matching = [
                candidate_role
                for candidate_role, status in run["workers"].items()
                if status == "in_progress"
            ]
            role = matching[0] if matching else ""
        step_id = _WORKFLOW_STEP_BY_ROLE.get(role, "")
        if not step_id:
            return None
        lowered = message.lower()
        if any(word in lowered for word in ("failed", "error", "timeout", "失败")):
            result_status = "failed"
            overall_status = "failed"
            summary = f"{role} failed"
        elif any(word in lowered for word in ("partial", "revision", "部分完成")):
            result_status = "revision"
            overall_status = "revision"
            summary = f"{role} requires revision"
        else:
            result_status = "completed"
            overall_status = "in_progress"
            summary = f"{role} completed"
        with self._lock:
            run["steps"][step_id] = result_status
            run["workers"][role] = result_status
            run["status"] = overall_status
            run["summary"] = summary
            if result_status == "completed" and role == "reviewer":
                run["summary"] = "Repair preview evidence required before approval"
            if result_status == "completed" and role == "reporter":
                run["status"] = "completed"
                run["summary"] = "Incident workflow completed"
            payload = self._finish_mutation(run)
            self._persist()
            return payload

    def record_admin_decision(
        self, origin: str, message: str, approved: bool
    ) -> dict[str, Any] | None:
        with self._lock:
            selected = self._run_for_message(origin, message)
            if selected is None:
                return None
            _, run = selected
            if run["steps"]["approval"] not in {"pending", "in_progress"}:
                return None
            if approved:
                run["steps"]["approval"] = "completed"
                run["status"] = "in_progress"
                run["summary"] = "Human approval granted"
            else:
                run["steps"]["approval"] = "failed"
                run["status"] = "blocked"
                run["summary"] = "Human approval rejected"
            payload = self._finish_mutation(run)
            self._persist()
            return payload

    def record_stop(self, origin: str, message: str) -> dict[str, Any] | None:
        with self._lock:
            selected = self._run_for_message(origin, message)
            if selected is None:
                return None
            _, run = selected
            run["status"] = "failed"
            run["summary"] = "Incident stopped by administrator"
            payload = self._finish_mutation(run)
            self._persist()
            return payload

    def payload(self, run_id: str) -> dict[str, Any]:
        run = self._runs[run_id]
        domain = os.environ.get("AGENTTEAMS_MATRIX_DOMAIN", "").strip()
        coordinator = f"@manager:{domain}" if domain else "manager"
        return {
            "type": "opskeeper-workflow",
            "runId": run_id,
            "status": run["status"],
            "title": f"OpsKeeper 事故处理 {run_id}",
            "summary": run["summary"],
            "ownerRole": "manager",
            "ownerAgentId": "opskeeper-manager",
            "coordinator": coordinator,
            "sharedPath": "",
            "subagents": [
                {
                    "id": f"opskeeper-{role}",
                    "name": name,
                    "status": run["workers"][role],
                }
                for role, name in (
                    ("alerter", "告警确认"),
                    ("investigator", "根因诊断"),
                    ("reviewer", "方案评审"),
                    ("repairer", "修复执行"),
                    ("verifier", "独立验证"),
                    ("reporter", "复盘归档"),
                )
            ],
            "steps": [
                {
                    "id": step_id,
                    "name": name,
                    "status": run["steps"][step_id],
                }
                for step_id, name in _WORKFLOW_STEPS
            ],
        }


_WORKFLOW_PROJECTOR = WorkflowProjector()


class _FallbackMiddlewareBase:
    def is_implemented(self, hook_name: str) -> bool:
        return callable(getattr(self, hook_name, None))


def _middleware_base() -> type[Any]:
    try:
        from agentscope.middleware import MiddlewareBase
    except ImportError:
        return _FallbackMiddlewareBase
    return MiddlewareBase


def _sanitizer_rules() -> list[str]:
    raw = os.getenv(_SANITIZER_KEYWORDS_ENV, "")
    return [v.strip() for v in raw.split(",") if v.strip()]


_SENSITIVE_PATTERNS = [
    re.compile(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----", re.IGNORECASE),
    re.compile(r"\bAuthorization\s*:\s*(?:Bearer|Basic)\s+\S+", re.IGNORECASE),
    re.compile(
        r"\b(?:access[_-]?key[_-]?secret|client[_-]?secret|secret[_-]?key|api[_-]?key|token)\b"
        r"\s*[:=]\s*['\"]?[A-Za-z0-9_./+=:-]{16,}",
        re.IGNORECASE,
    ),
]


def _redact_string(s: str, rules: list[str]) -> str:
    out = s
    for rule in rules:
        out = re.sub(re.escape(rule), "[REDACTED]", out, flags=re.IGNORECASE)
    for pat in _SENSITIVE_PATTERNS:
        out = pat.sub("[REDACTED]", out)
    return out


def _sanitize_value(value: Any, rules: list[str]) -> None:
    if not rules and not _SENSITIVE_PATTERNS:
        return
    if isinstance(value, dict):
        for k, v in list(value.items()):
            if isinstance(v, str):
                value[k] = _redact_string(v, rules)
            else:
                _sanitize_value(v, rules)
    elif isinstance(value, list):
        for item in value:
            _sanitize_value(item, rules)
    elif hasattr(value, "content") and isinstance(value.content, str):
        value.content = _redact_string(value.content, rules)
    elif hasattr(value, "output") and isinstance(value.output, str):
        value.output = _redact_string(value.output, str)


def _sanitizer_factory(_ctx: Any, _agent_config: Any):
    class OpskeeperSanitizer(_middleware_base()):
        async def on_acting(
            self,
            agent: Any,
            input_kwargs: dict[str, Any],
            next_handler: Callable[..., AsyncGenerator[Any, None]],
        ) -> AsyncGenerator[Any, None]:
            rules = _sanitizer_rules()
            async for event in next_handler(**input_kwargs):
                _sanitize_value(event, rules)
                yield event

    return OpskeeperSanitizer()


def _strip_thinking(text: str) -> str:
    result = _THINKING_SPAN_PATTERN.sub("", text)
    leading_partial = _LEADING_PARTIAL_THINKING_PATTERN.match(result)
    if leading_partial is not None:
        result = result[:leading_partial.start()] + result[leading_partial.end():]
    changed = result != text
    return result.lstrip("\r\n") if changed else result


def _sanitize_reply_value(value: Any) -> None:
    if isinstance(value, str):
        return
    if isinstance(value, list):
        for item in value:
            _sanitize_reply_value(item)
        return
    if isinstance(value, dict):
        for key, nested in value.items():
            if key == "text" and isinstance(nested, str):
                value[key] = _strip_thinking(nested)
            else:
                _sanitize_reply_value(nested)
        return
    text = getattr(value, "text", None)
    if isinstance(text, str):
        value.text = _strip_thinking(text)
    content = getattr(value, "content", None)
    if content is not None:
        if isinstance(content, str):
            value.content = _strip_thinking(content)
        else:
            _sanitize_reply_value(content)


def _sanitize_reply_event(event: Any) -> Any:
    _sanitize_reply_value(event)
    return event


def _could_start_thinking_opening(candidate: str) -> bool:
    if not candidate.startswith("<"):
        return False
    index = 1
    while index < len(candidate) and candidate[index].isspace():
        index += 1
    for expected in "think":
        if index >= len(candidate):
            return True
        if candidate[index].lower() != expected:
            return False
        index += 1
    while index < len(candidate) and candidate[index].isspace():
        index += 1
    return index == len(candidate)


class _ReplyThinkingStream:
    def __init__(self) -> None:
        self._states: dict[str, tuple[str, bool]] = {}

    def _state(self, key: str) -> tuple[str, bool]:
        return self._states.get(key, ("", False))

    def push(self, key: str, text: str) -> str:
        pending, inside_thinking = self._state(key)
        pending += text
        output = ""
        while pending:
            if not inside_thinking:
                opening = _THINKING_OPEN_PATTERN.search(pending)
                if opening is not None:
                    output += pending[:opening.start()]
                    pending = pending[opening.end():]
                    inside_thinking = True
                    continue
                suffix_length = 0
                for index in range(len(pending) - 1, -1, -1):
                    if pending[index] != "<":
                        continue
                    if _could_start_thinking_opening(pending[index:]):
                        suffix_length = len(pending) - index
                    break
                output += pending[:len(pending) - suffix_length]
                pending = pending[len(pending) - suffix_length:] if suffix_length else ""
                break

            closing = _THINKING_CLOSE_PATTERN.search(pending)
            if closing is None:
                pending = ""
                break
            pending = pending[closing.end():]
            inside_thinking = False

        self._states[key] = (pending, inside_thinking)
        return output

def _sanitize_reply_stream_value(
    value: Any,
    stream: _ReplyThinkingStream,
    path: str = "",
) -> None:
    if isinstance(value, str):
        return
    if isinstance(value, (list, tuple)):
        for index, item in enumerate(value):
            _sanitize_reply_stream_value(item, stream, f"{path}[{index}]")
        return
    if isinstance(value, dict):
        for key, nested in value.items():
            nested_path = f"{path}.{key}"
            if key == "text" and isinstance(nested, str):
                value[key] = stream.push(nested_path, nested)
            else:
                _sanitize_reply_stream_value(nested, stream, nested_path)
        return
    text = getattr(value, "text", None)
    if isinstance(text, str):
        value.text = stream.push(f"{path}.text", text)
    content = getattr(value, "content", None)
    if content is not None:
        if isinstance(content, str):
            value.content = stream.push(f"{path}.content", content)
        else:
            _sanitize_reply_stream_value(content, stream, f"{path}.content")


def _sanitize_reply_stream_event(event: Any, stream: _ReplyThinkingStream) -> Any:
    _sanitize_reply_stream_value(event, stream)
    return event


def _outbound_environment_int(name: str, default: int, minimum: int, maximum: int) -> int:
    try:
        value = int(os.getenv(name, str(default)))
    except (TypeError, ValueError):
        return default
    return value if minimum <= value <= maximum else default


class OutboundSafetyMiddleware(_middleware_base()):
    def __init__(self, monotonic: Callable[[], float] = time.monotonic) -> None:
        self._monotonic = monotonic
        self._limit = _outbound_environment_int(
            _OUTBOUND_LIMIT_ENV,
            _DEFAULT_OUTBOUND_LIMIT,
            1,
            1000,
        )
        self._window_seconds = float(
            _outbound_environment_int(
                _OUTBOUND_WINDOW_SECONDS_ENV,
                int(_DEFAULT_OUTBOUND_WINDOW_SECONDS),
                1,
                3600,
            )
        )
        self._reservations: deque[tuple[int, float]] = deque()
        self._next_reservation_token = 0
        self._lock = threading.Lock()

    def _reserve(self) -> int:
        with self._lock:
            now = self._monotonic()
            cutoff = now - self._window_seconds
            while self._reservations and self._reservations[0][1] <= cutoff:
                self._reservations.popleft()
            if len(self._reservations) >= self._limit:
                raise RuntimeError(
                    "OpsKeeper outbound rate limit exceeded; stopping this turn"
                )
            token = self._next_reservation_token
            self._next_reservation_token += 1
            self._reservations.append((token, now))
            return token

    def _release(self, token: int) -> None:
        with self._lock:
            self._reservations = deque(
                reservation
                for reservation in self._reservations
                if reservation[0] != token
            )

    async def on_reply(
        self,
        agent: Any,
        input_kwargs: dict[str, Any],
        next_handler: Callable[..., AsyncGenerator[Any, None]],
    ) -> AsyncGenerator[Any, None]:
        stream_sanitizer = _ReplyThinkingStream()
        self._reserve()
        async for event in next_handler(**input_kwargs):
            yield _sanitize_reply_stream_event(event, stream_sanitizer)

    async def on_acting(
        self,
        agent: Any,
        input_kwargs: dict[str, Any],
        next_handler: Callable[..., AsyncGenerator[Any, None]],
    ) -> AsyncGenerator[Any, None]:
        tool_name, _arguments = _extract_tool_call(input_kwargs)
        is_message_attempt = _is_message_tool(_normalize_tool_name(tool_name))
        reservation = self._reserve() if is_message_attempt else None
        terminal_success = False
        try:
            async for event in next_handler(**input_kwargs):
                if hasattr(event, "content") and hasattr(event, "state"):
                    terminal_success = _is_successful_tool_response(event)
                yield event
        except BaseException:
            if reservation is not None:
                self._release(reservation)
            raise
        if reservation is not None and not terminal_success:
            self._release(reservation)


def _is_successful_tool_response(event: Any) -> bool:
    if not hasattr(event, "content") or not hasattr(event, "state"):
        return False
    return not _has_failed_tool_result([event])


def _outbound_safety_factory(_context: Any, _agent_config: Any):
    return OutboundSafetyMiddleware()


def _extract_tool_call(input_kwargs: dict[str, Any]) -> tuple[str, dict[str, Any]]:
    tool_call = input_kwargs.get("tool_call")
    raw_name = getattr(tool_call, "name", None)
    if raw_name is None and isinstance(tool_call, dict):
        raw_name = tool_call.get("name") or tool_call.get("tool_name")
    if raw_name is None:
        raw_name = input_kwargs.get("tool_name") or input_kwargs.get("name") or ""

    raw_arguments = getattr(tool_call, "input", None)
    if raw_arguments is None and isinstance(tool_call, dict):
        raw_arguments = tool_call.get("input") or tool_call.get("arguments")
    if raw_arguments is None:
        raw_arguments = input_kwargs.get("tool_input") or input_kwargs.get("arguments")

    arguments: dict[str, Any] = {}
    if isinstance(raw_arguments, dict):
        arguments = raw_arguments
    elif isinstance(raw_arguments, str) and raw_arguments.strip():
        try:
            decoded = json.loads(raw_arguments)
            if isinstance(decoded, dict):
                arguments = decoded
        except json.JSONDecodeError:
            arguments = {"input": raw_arguments}

    return str(raw_name), arguments


def _string_value(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "\n".join(part for part in (_string_value(item) for item in value) if part)
    if isinstance(value, dict):
        for key in ("text", "body", "content", "message"):
            if key in value:
                nested = _string_value(value[key])
                if nested:
                    return nested
    for attribute in ("text", "body", "content", "message"):
        nested = getattr(value, attribute, None)
        if nested is not None and not callable(nested):
            nested_text = _string_value(nested)
            if nested_text:
                return nested_text
    return ""


def _message_text(arguments: dict[str, Any]) -> str:
    for key in ("message", "content", "text", "body", "input"):
        if key in arguments:
            value = _string_value(arguments[key])
            if value:
                return value
    return ""


def _message_target_sessions(arguments: dict[str, Any]) -> tuple[str, ...]:
    target_values: list[str] = []
    for key in (
        "target",
        "targetRoom",
        "target_room",
        "roomId",
        "room_id",
        "room",
    ):
        value = arguments.get(key)
        if isinstance(value, (str, int)):
            target_values.append(str(value))
        elif isinstance(value, dict):
            for nested_key in ("id", "roomId", "room_id"):
                nested_value = value.get(nested_key)
                if isinstance(nested_value, (str, int)):
                    target_values.append(str(nested_value))

    sessions: list[str] = []
    for value in target_values:
        room_id = value.strip()
        if room_id.startswith("room:"):
            room_id = room_id[len("room:"):].strip()
        elif room_id.startswith("matrix:"):
            room_id = room_id[len("matrix:"):].strip()
        if room_id and f"matrix:{room_id}" not in sessions:
            sessions.append(f"matrix:{room_id}")
    return tuple(sessions)


def _dispatch_gate_sessions(
    source_session_id: str,
    arguments: dict[str, Any],
) -> tuple[str, ...]:
    sessions = [source_session_id]
    sessions.extend(
        session_id
        for session_id in _message_target_sessions(arguments)
        if session_id not in sessions
    )
    return tuple(sessions)


def _request_text(request: Any) -> str:
    input_messages = getattr(request, "input", None) or []
    parts: list[str] = []
    for message in input_messages:
        content = getattr(message, "content", None)
        if content is None:
            parts.append(_string_value(message))
        elif isinstance(content, list):
            parts.extend(_string_value(item) for item in content)
        else:
            parts.append(_string_value(content))
    return "\n".join(part for part in parts if part)


def _request_sender(request: Any) -> str:
    metadata = getattr(request, "channel_meta", None)
    if not isinstance(metadata, dict):
        metadata = getattr(request, "metadata", None)
    if isinstance(metadata, dict):
        sender = metadata.get("sender_id") or metadata.get("acl_sender_id") or metadata.get("user_id")
        if sender:
            return str(sender)
    return ""


def _relay_matrix_completion(marker: str, origin_session_id: str, result_body: str) -> str:
    base_url = os.environ.get("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
    token = os.environ.get("AGENTTEAMS_MANAGER_MATRIX_TOKEN", "").strip()
    room_id = origin_session_id
    if room_id.startswith("matrix:"):
        room_id = room_id[len("matrix:"):]
    if not base_url or not token or not room_id.startswith("!"):
        raise RuntimeError("Matrix completion relay is not configured")

    domain = os.environ.get("AGENTTEAMS_MATRIX_DOMAIN", "").strip()
    admin_id = os.environ.get("AGENTTEAMS_ADMIN_MATRIX_ID", "").strip() or (
        f"@admin:{domain}" if domain else "@admin"
    )
    body = (
        f"{admin_id} OPSKEEPER_COMPLETE {marker}\n"
        f"Worker result received in the execution room. Result:\n{result_body[:4000]}"
    )
    request = urllib.request.Request(
        f"{base_url}/_matrix/client/v3/rooms/"
        f"{urllib.parse.quote(room_id, safe='')}/send/m.room.message/{uuid.uuid4()}",
        data=json.dumps({"msgtype": "m.text", "body": body}).encode(),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    with urllib.request.urlopen(request, timeout=5) as response:
        payload = json.loads(response.read().decode())
    event_id = str(payload.get("event_id", ""))
    if not event_id:
        raise RuntimeError("Matrix completion relay returned no event_id")
    return event_id


def _send_matrix_workflow(
    origin_session_id: str,
    workflow: dict[str, Any],
) -> str:
    base_url = os.environ.get("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
    token = os.environ.get("AGENTTEAMS_MANAGER_MATRIX_TOKEN", "").strip()
    room_id = origin_session_id
    if room_id.startswith("matrix:"):
        room_id = room_id[len("matrix:"):]
    if not base_url or not token or not room_id.startswith("!"):
        raise RuntimeError("Matrix workflow projection is not configured")

    steps = workflow.get("steps")
    step_lines = "\n".join(
        f"- {step.get('name', step.get('id', 'stage'))}: {step.get('status', 'pending')}"
        for step in steps
        if isinstance(step, dict)
    )
    body = (
        f"[OpsKeeper Workflow] {workflow.get('title', 'OpsKeeper workflow')}\n"
        f"runId: {workflow.get('runId', '')}\n"
        f"Status: {workflow.get('status', '')}\n"
        f"Summary: {workflow.get('summary', '')}\n"
        "Stages:\n"
        f"{step_lines}"
    )
    content = {
        "msgtype": "m.notice",
        "body": body,
        "agentteams.workflow": workflow,
    }
    request = urllib.request.Request(
        f"{base_url}/_matrix/client/v3/rooms/"
        f"{urllib.parse.quote(room_id, safe='')}/send/m.room.message/"
        f"{uuid.uuid4()}",
        data=json.dumps(content, ensure_ascii=False).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    with urllib.request.urlopen(request, timeout=5) as response:
        payload = json.loads(response.read().decode())
    event_id = str(payload.get("event_id", ""))
    if not event_id:
        raise RuntimeError("Matrix workflow projection returned no event_id")
    return event_id


def _send_matrix_notice(origin_session_id: str, body: str) -> str:
    base_url = os.environ.get("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
    token = os.environ.get("AGENTTEAMS_MANAGER_MATRIX_TOKEN", "").strip()
    room_id = origin_session_id
    if room_id.startswith("matrix:"):
        room_id = room_id[len("matrix:"):]
    if not base_url or not token or not room_id.startswith("!"):
        raise RuntimeError("Matrix workflow projection is not configured")

    content = {"msgtype": "m.notice", "body": body}
    request = urllib.request.Request(
        f"{base_url}/_matrix/client/v3/rooms/"
        f"{urllib.parse.quote(room_id, safe='')}/send/m.room.message/"
        f"{uuid.uuid4()}",
        data=json.dumps(content, ensure_ascii=False).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    with urllib.request.urlopen(request, timeout=5) as response:
        payload = json.loads(response.read().decode())
    event_id = str(payload.get("event_id", ""))
    if not event_id:
        raise RuntimeError("Matrix notice returned no event_id")
    return event_id


async def _emit_workflow_projection(
    origin_session_id: str,
    workflow: dict[str, Any] | None,
) -> str:
    if not workflow:
        return ""
    try:
        event_id = await asyncio.to_thread(
            _send_matrix_workflow,
            origin_session_id,
            workflow,
        )
        _WORKFLOW_PROJECTOR_LOGGER.info(
            "Workflow projected run=%s origin=%s event=%s",
            workflow.get("runId", ""),
            origin_session_id,
            event_id,
        )
        return event_id
    except Exception:
        _WORKFLOW_PROJECTOR_LOGGER.warning(
            "Workflow projection failed run=%s origin=%s",
            workflow.get("runId", ""),
            origin_session_id,
            exc_info=True,
        )
        return ""


def _extract_session_id(context: Any) -> str:
    session_id = getattr(context, "session_id", "")
    if session_id:
        return str(session_id)
    request = getattr(context, "request", None)
    if request is not None:
        session_id = getattr(request, "session_id", "")
        if session_id:
            return str(session_id)
    return os.getenv("AGENTTEAMS_SESSION_ID", "matrix:default")


def _manager_identity(agent: Any) -> tuple[str, str]:
    agent_name = os.getenv("AGENTTEAMS_AGENT_NAME", "")
    if not agent_name:
        agent_name = str(
            getattr(agent, "name", "")
            or os.getenv("AGENTTEAMS_WORKER_NAME", "")
            or "default"
        )
    role = os.getenv("AGENTTEAMS_WORKER_ROLE", "") or os.getenv("AGENTTEAMS_AGENT_ROLE", "")
    return agent_name.strip().lower(), role.strip().lower()


def _is_manager_agent(agent: Any) -> bool:
    agent_name, role = _manager_identity(agent)
    worker_name = os.getenv("AGENTTEAMS_WORKER_NAME", "").strip().lower()
    manager_runtime = os.getenv("AGENTTEAMS_MANAGER_RUNTIME", "").strip().lower()
    if role in {"worker", "standalone"} or worker_name:
        return False
    if role in {"manager", "leader", "team_leader"} or "manager" in agent_name:
        return True
    return manager_runtime in {"qwenpaw", "copaw"}


def _is_message_for_agent(message: str, agent: Any) -> bool:
    mentions = [
        match.group("role").lower()
        for match in _OPSKEEPER_ROLE_MENTION_PATTERN.finditer(message)
    ]
    manager_mention = bool(_MANAGER_ROLE_MENTION_PATTERN.search(message))
    if not mentions and not manager_mention:
        return True
    agent_name, agent_role = _manager_identity(agent)
    current_identities = {agent_name}
    if agent_role in {"manager", "leader", "team_leader"} or "manager" in agent_name:
        current_identities.add("manager")
    return agent_name in mentions or (
        manager_mention and "manager" in current_identities
    )


def _gated_prompt(
    provider: Callable[[Any], str],
    condition: Callable[[Any], bool],
) -> Callable[[Any], str]:
    def gated_provider(agent: Any) -> str:
        return provider(agent) if condition(agent) else ""

    return gated_provider


def _register_prompt_sections(api: Any) -> None:
    sections = (
        (
            "opskeeper_team_context",
            team_prompt,
            lambda agent: _is_manager_agent(agent),
            40,
        ),
        (
            "opskeeper_worker_context",
            worker_prompt,
            lambda agent: not _is_manager_agent(agent),
            30,
        ),
        (
            "opskeeper_manager_context",
            manager_prompt,
            lambda agent: _is_manager_agent(agent),
            30,
        ),
    )
    for name, provider, condition, priority in sections:
        try:
            api.register_prompt_section(
                name,
                after="workspace",
                provider=provider,
                condition=condition,
                priority=priority,
            )
        except TypeError:
            api.register_prompt_section(
                name,
                after="workspace",
                provider=_gated_prompt(provider, condition),
                priority=priority,
            )
        except Exception:
            pass


def _is_admin_sender(sender: str) -> bool:
    admin_id = os.getenv("AGENTTEAMS_ADMIN_MATRIX_ID", "").strip()
    return bool(admin_id and sender and sender == admin_id)


def _has_new_task(message: str) -> bool:
    if _TASK_MARKER_PATTERN.search(message):
        return True
    if _TASK_RESULT_PATTERN.search(message):
        return False
    return bool(_extract_task_markers(message))


def _normalize_tool_name(name: str) -> str:
    normalized = name.strip().lower().replace("__", ".")
    match = re.search(r"(?:^|\.)opskeeper\.(.+)$", normalized)
    if match:
        return f"opskeeper.{match.group(1).replace('_', '.')}"
    return normalized


def _is_message_tool(normalized_name: str) -> bool:
    return normalized_name in {"message", "teamharness.message"}


def _permission_mode() -> str:
    mode = os.getenv(_PERMISSION_MODE_ENV, "read_only").strip().lower()
    return "standard" if mode == "standard" else "read_only"


def _denied_tool_response(tool_name: str, reason: str = "") -> Any:
    from agentscope.message import TextBlock
    from agentscope.tool import ToolResponse

    denial = f"[DENIED] {tool_name} is not allowed in read-only mode."
    if reason:
        denial = f"{denial} {reason}"
    try:
        text_block = TextBlock(type="text", text=denial)
    except TypeError:
        text_block = TextBlock(text=denial)
    metadata = {"opskeeper.permission_mode": "read_only"}
    try:
        from agentscope.message import ToolResultState
    except ImportError:
        return ToolResponse(
            content=[text_block],
            metadata={**metadata, "opskeeper.tool_result_state": "denied"},
            is_interrupted=True,
        )
    return ToolResponse(
        content=[text_block],
        state=ToolResultState.DENIED,
        metadata=metadata,
    )


def _as_copaw_toolkit_middleware(middleware: Any) -> Callable[..., Any]:
    async def copaw_middleware(
        input_kwargs: dict[str, Any],
        next_handler: Callable[..., Any],
    ) -> AsyncGenerator[Any, None]:
        def legacy_next_handler(**kwargs: Any) -> Any:
            source = kwargs or input_kwargs

            async def events() -> AsyncGenerator[Any, None]:
                async for event in await next_handler(**source):
                    yield event

            return events()

        async for event in middleware.on_acting(
            None,
            input_kwargs,
            legacy_next_handler,
        ):
            yield event

    return copaw_middleware


def _has_failed_tool_result(events: list[Any]) -> bool:
    for event in events:
        raw_state = getattr(event, "state", "")
        state = str(getattr(raw_state, "value", raw_state)).lower()
        if state in {"error", "denied", "interrupted"}:
            return True
    return False


def _queue_manager_stop_after_dispatch(agent: Any) -> None:
    try:
        from qwenpaw.loop.gates import StopAction, StopHandlerResult
    except ImportError:
        _MANAGER_GATE_LOGGER.debug("QwenPaw stop gates unavailable", exc_info=True)
        return
    agent._gate_pending_stop = StopHandlerResult(
        action=StopAction.TERMINATE,
        reason="OpsKeeper task dispatched; waiting for matching worker result",
    )
    _MANAGER_GATE_LOGGER.warning(
        "Manager dispatch queued ReAct stop agent_type=%s pending=%s",
        type(agent).__name__,
        _MANAGER_DISPATCH_GATE.any_pending(),
    )


def _requires_worker_file_artifacts(message_text: str) -> bool:
    for match in _WORKER_FILE_ARTIFACT_PATTERN.finditer(message_text):
        prefix = message_text[max(0, match.start() - 24) : match.start()].lower()
        if any(
            negation in prefix
            for negation in ("不要", "禁止", "不得", "不需要", "do not", "don't")
        ):
            continue
        return True
    return False


def _readonly_enforcement_factory(context: Any, _agent_config: Any):
    factory_session_id = _extract_session_id(context)

    class OpskeeperReadOnlyMiddleware(_middleware_base()):
        def __init__(self) -> None:
            self._task_message_sent = False
            self._denial_counts: dict[tuple[str, str], int] = {}

        def _denied(
            self,
            tool_name: str,
            arguments: dict[str, Any],
            reason: str = "",
        ) -> Any:
            result = _denied_tool_response(tool_name, reason)
            _audit_tool_call(tool_name, arguments, result)
            signature = (
                _normalize_tool_name(tool_name),
                json.dumps(arguments, ensure_ascii=False, sort_keys=True, default=str),
            )
            self._denial_counts[signature] = self._denial_counts.get(signature, 0) + 1
            if self._denial_counts[signature] >= 3:
                _READ_ONLY_LOGGER.error(
                    "OpsKeeper repeated read-only denials tool=%s attempts=%s",
                    tool_name,
                    self._denial_counts[signature],
                )
                raise RuntimeError(
                    f"OpsKeeper repeated read-only denials for {tool_name}; "
                    "stop and report the boundary error"
                )
            return result

        async def on_acting(
            self,
            agent: Any,
            input_kwargs: dict[str, Any],
            next_handler: Callable[..., AsyncGenerator[Any, None]],
        ) -> AsyncGenerator[Any, None]:
            tool_name, arguments = _extract_tool_call(input_kwargs)
            normalized_name = _normalize_tool_name(tool_name)
            if (
                _permission_mode() == "read_only"
                and normalized_name not in _READ_ONLY_ALLOWED_TOOLS
                and normalized_name not in _ROLE_GATED_MUTATING_TOOLS
                and not _is_proposal_bound_recovery_execute(normalized_name, arguments)
            ):
                _READ_ONLY_LOGGER.warning(
                    "read-only boundary denied tool=%s normalized=%s",
                    tool_name,
                    normalized_name,
                )
                yield self._denied(tool_name, arguments)
                return

            events: list[Any] = []
            message_text = (
                _message_text(arguments) if _is_message_tool(normalized_name) else ""
            )
            task_markers = _extract_task_markers(message_text)
            if (
                _is_manager_agent(agent)
                and _is_message_tool(normalized_name)
                and task_markers
                and _requires_worker_file_artifacts(message_text)
            ):
                _READ_ONLY_LOGGER.warning(
                    "Manager dispatch denied because it requires file artifacts markers=%s",
                    list(task_markers),
                )
                yield self._denied(
                    tool_name,
                    arguments,
                    "OpsKeeper dispatches must not require plan.md, result.md, or spec.md; use a direct room result and incident.record.",
                )
                return
            if (
                not _is_manager_agent(agent)
                and _is_message_tool(normalized_name)
                and factory_session_id.startswith("matrix:!")
            ):
                target_sessions = _message_target_sessions(arguments)
                if target_sessions and factory_session_id not in target_sessions:
                    _READ_ONLY_LOGGER.warning(
                        "Worker message target denied current=%s targets=%s",
                        factory_session_id,
                        list(target_sessions),
                    )
                    yield self._denied(
                        tool_name,
                        arguments,
                        f"Message target must be the current project room ({factory_session_id}).",
                    )
                    return
            if self._task_message_sent and task_markers:
                _READ_ONLY_LOGGER.warning(
                    "Manager one-dispatch boundary denied a second task message markers=%s",
                    list(task_markers),
                )
                yield self._denied(
                    "message",
                    arguments,
                    "Only one OpsKeeper task dispatch is allowed per Manager turn.",
                )
                return
            gate_sessions = _dispatch_gate_sessions(factory_session_id, arguments)
            if any(
                _MANAGER_DISPATCH_GATE.has_pending(session_id, marker)
                for session_id in gate_sessions
                for marker in task_markers
            ):
                _READ_ONLY_LOGGER.warning(
                    "Manager dispatch gate denied duplicate task markers=%s",
                    list(task_markers),
                )
                yield self._denied(
                    "message",
                    arguments,
                    "A matching OpsKeeper task is already pending its Worker result.",
                )
                return

            async for event in next_handler():
                events.append(event)
                yield event

            if (
                _is_message_tool(normalized_name)
                and task_markers
                and not _has_failed_tool_result(events)
            ):
                self._task_message_sent = True
                gate_sessions = _dispatch_gate_sessions(factory_session_id, arguments)
                recorded: tuple[str, ...] = ()
                for session_id in gate_sessions:
                    recorded += _MANAGER_DISPATCH_GATE.record(
                        session_id,
                        message_text,
                        factory_session_id,
                    )
                if _is_manager_agent(agent):
                    workflow = _WORKFLOW_PROJECTOR.record_dispatch(
                        factory_session_id,
                        message_text,
                    )
                    await _emit_workflow_projection(factory_session_id, workflow)
                _queue_manager_stop_after_dispatch(agent)
                _MANAGER_GATE_LOGGER.info(
                    "Manager dispatch registered sessions=%s markers=%s origin=%s",
                    list(gate_sessions),
                    list(recorded),
                    factory_session_id,
                )

    return OpskeeperReadOnlyMiddleware()


# ===== audit hook =====


def _audit_tool_call(name: str, arguments: dict[str, Any], result: Any) -> None:
    """记录每次工具调用到 opskeeper audit 端点（best-effort）。"""
    try:
        import json
        import urllib.request

        backend = os.environ.get("OPSKEEPER_BACKEND_URL", "http://opskeeper:8080")
        key = os.environ.get("OPSKEEPER_GATEWAY_KEY", "")
        if not key:
            return
        body = json.dumps({
            "event": "tool_call",
            "tool": name,
            "arguments_keys": list(arguments.keys()) if isinstance(arguments, dict) else [],
            "actor": os.environ.get("OPSKEEPER_ACTOR", "qwenpaw-worker"),
        }).encode()
        req = urllib.request.Request(
            f"{backend}/v1/audit/events",
            data=body,
            headers={
                "Authorization": f"Bearer {key}",
                "Content-Type": "application/json",
            },
            method="POST",
        )
        urllib.request.urlopen(req, timeout=2).read()
    except Exception:
        # audit 失败不影响主流程
        pass


def audit_hook_factory(_ctx: Any, _agent_config: Any):
    """返回带 audit 的 middleware（qwenpaw 调用前 / 调用后钩子）。"""
    try:
        from agentscope.middleware import MiddlewareBase
    except ImportError:
        return None

    class OpskeeperAuditMiddleware(MiddlewareBase):
        async def on_acting(
            self,
            agent: Any,
            input_kwargs: dict[str, Any],
            next_handler: Callable[..., AsyncGenerator[Any, None]],
        ) -> AsyncGenerator[Any, None]:
            tool_name, arguments = _extract_tool_call(input_kwargs)
            async for event in next_handler(**input_kwargs):
                _audit_tool_call(tool_name, arguments, event)
                yield event

    return OpskeeperAuditMiddleware()


# ===== qwenpaw plugin entrypoint =====
#
# `qwenpaw plugin install` 会 import 本文件并取 `plugin` 单例,调用 `plugin.register(api)`
# 把 6 类资源挂到 qwenpaw runtime(对齐 AgentTeams teamharness reference 模式):
#   - register_prompt_section  (注入 3 段 prompt: team / worker / manager)
#   - register_skill_provider  (skill 目录)
#   - register_middleware      (sanitizer + audit)
#   - register_runtime_hook    (task_trace lifecycle)
#   - register_http_router     (/health + /sync)
#   - stdio MCP server         (由 plugin.yaml mcp.servers[].command 触发 spawn,
#                                不在 register() 内)
#
# 缺失此函数会导致 `qwenpaw plugin install` 成功但 qwenpaw runtime 不会发现 plugin,
# 6 个 Worker skill / prompt / middleware / audit / sync 全部静默失效。


def _load_task_trace_module():
    """Load task_trace.py lazily (avoid hard import when qwenpaw absent)."""
    path = PLUGIN_DIR / "task_trace.py"
    if not path.is_file():
        return None
    spec = importlib.util.spec_from_file_location(
        "opskeeper_teamharness_task_trace",
        path,
    )
    if spec is None or spec.loader is None:
        return None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _investigate_via_mcp(arguments: dict[str, Any]) -> dict[str, Any]:
    """Call loop.investigate through the signed backend MCP endpoint."""
    mcp_dir = PLUGIN_DIR / "opskeeper-teamharness" / "mcp"
    if str(mcp_dir) not in sys.path:
        sys.path.insert(0, str(mcp_dir))
    from auth import get_backend_url, sign_request

    request = {
        "jsonrpc": "2.0",
        "id": f"dashboard-{int(time.time() * 1000)}",
        "method": "tools/call",
        "params": {"name": "loop.investigate", "arguments": arguments},
    }
    body = json.dumps(request, ensure_ascii=False).encode("utf-8")
    gateway_key = _runtime_gateway_key()
    if not gateway_key:
        raise RuntimeError("OpsKeeper GatewayKey is unavailable in the QwenPaw runtime")
    backend_url = _runtime_backend_url()
    tenant_id = _runtime_tenant_id()
    headers = sign_request(body, key=gateway_key)
    headers["X-Opskeeper-Tenant"] = tenant_id
    req = urllib.request.Request(
        backend_url + "/api/v1/mcp",
        data=body,
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=180) as response:
            payload = json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")[:300]
        raise RuntimeError(f"OpsKeeper MCP HTTP {exc.code}: {detail}") from exc

    if payload.get("error"):
        error = payload["error"]
        raise RuntimeError(error.get("message", "OpsKeeper MCP call failed"))

    result = payload.get("result") or {}
    content = result.get("content") or []
    text = content[0].get("text", "{}") if content and isinstance(content[0], dict) else "{}"
    report = json.loads(text)
    metadata = result.get("_meta") or {}
    return {"data": report, "audit_log_id": metadata.get("audit_log_id")}


def _copaw_diagnostics() -> dict[str, Any]:
    return json.loads(json.dumps(_COPAW_DIAGNOSTICS, ensure_ascii=False))


def _copaw_diagnostics_startup_hook() -> dict[str, Any]:
    diagnostics = _copaw_diagnostics()
    _COPAW_DIAGNOSTICS_LOGGER.info(
        "OpsKeeper TeamHarness capabilities %s",
        json.dumps(diagnostics, ensure_ascii=False, sort_keys=True),
    )
    return diagnostics


def _call_opskeeper_mcp_server(name: str, arguments: dict[str, Any]) -> Any:
    mcp_dir = ASSET_DIR / "mcp"
    if str(mcp_dir) not in sys.path:
        sys.path.insert(0, str(mcp_dir))
    from server import handle_tools_call

    response = handle_tools_call({
        "id": f"copaw-{uuid.uuid4().hex}",
        "params": {"name": name, "arguments": arguments},
    })
    if response.get("error"):
        raise RuntimeError(response["error"].get("message", "OpsKeeper MCP call failed"))
    return response.get("result")


def _tool_names(toolkit: Any) -> set[str]:
    names: set[str] = set()
    for attribute in ("tools", "_tools", "tool_functions", "_tool_functions"):
        candidate = getattr(toolkit, attribute, None)
        if isinstance(candidate, dict):
            names.update(str(key) for key in candidate)
        elif isinstance(candidate, (list, tuple, set)):
            for item in candidate:
                names.add(str(getattr(item, "name", item)))
    registry = getattr(toolkit, "tool_registry", None)
    if isinstance(registry, dict):
        names.update(str(key) for key in registry)
    return names


def _validate_copaw_toolkit(toolkit: Any) -> None:
    try:
        readonly_middleware = _readonly_enforcement_factory(None, None)
        outbound_middleware = _outbound_safety_factory(None, None)
        sanitizer_middleware = _sanitizer_factory(None, None)
        if (
            readonly_middleware is None
            or outbound_middleware is None
            or sanitizer_middleware is None
        ):
            raise RuntimeError("OpsKeeper middleware constructors are unavailable")
        toolkit.register_middleware(_as_copaw_toolkit_middleware(readonly_middleware))
        toolkit.register_middleware(_as_copaw_toolkit_middleware(outbound_middleware))
        toolkit.register_middleware(_as_copaw_toolkit_middleware(sanitizer_middleware))
        names = _tool_names(toolkit)
        missing_base = sorted(_COPAW_BASE_TOOLS - names)
        if missing_base:
            raise RuntimeError(f"AgentTeams CoPaw tools missing: {missing_base}")
        for tool_name, route in _COPAW_NATIVE_TOOLS.items():
            if tool_name in names:
                continue
            def native_tool(arguments: dict[str, Any], _route: str = route) -> Any:
                return _call_opskeeper_mcp_server(_route, arguments)
            toolkit.register_tool_function(
                native_tool,
                "basic",
                func_name=tool_name,
                func_description=f"Invoke OpsKeeper MCP tool {route}.",
                json_schema={
                    "type": "function",
                    "function": {
                        "name": tool_name,
                        "description": f"Invoke OpsKeeper MCP tool {route}.",
                        "parameters": {
                            "type": "object",
                            "properties": {
                                "arguments": {
                                    "type": "object",
                                    "description": "MCP tool arguments",
                                }
                            },
                            "required": ["arguments"],
                            "additionalProperties": True,
                        },
                    },
                },
                namesake_strategy="override",
            )
        names = _tool_names(toolkit)
        missing_native = sorted(set(_COPAW_NATIVE_TOOLS) - names)
        if missing_native:
            raise RuntimeError(f"OpsKeeper CoPaw tools missing: {missing_native}")
        _COPAW_DIAGNOSTICS.update({
            "toolkit_validated": True,
            "readonly_middleware": True,
            "manager_gate": True,
            "new_task_context": True,
            "prompt_capabilities": ["team", "worker", "manager"],
            "skill_capabilities": sorted(
                path.parent.name
                for base in (ASSET_DIR / "skills" / "agent", ASSET_DIR / "skills" / "team")
                for path in base.glob("*/SKILL.md")
            ),
            "native_tool_count": len(_COPAW_NATIVE_TOOLS),
            "installed_native_tools": sorted(_COPAW_NATIVE_TOOLS),
            "missing_capabilities": [],
            "toolkit_error": None,
        })
        _COPAW_DIAGNOSTICS_LOGGER.info(
            "OpsKeeper TeamHarness capabilities %s",
            json.dumps(_copaw_diagnostics(), ensure_ascii=False, sort_keys=True),
        )
    except Exception as exc:
        _COPAW_DIAGNOSTICS.update({
            "toolkit_validated": False,
            "readonly_middleware": False,
            "manager_gate": False,
            "new_task_context": False,
            "native_tool_count": len(_tool_names(toolkit) & set(_COPAW_NATIVE_TOOLS)),
            "installed_native_tools": sorted(_tool_names(toolkit) & set(_COPAW_NATIVE_TOOLS)),
            "missing_capabilities": [
                "readonly_middleware",
                "manager_gate",
                "new_task_context",
                "native_mcp_tools",
            ],
            "toolkit_error": f"{type(exc).__name__}: {exc}",
        })
        raise


def _install_copaw_compat() -> dict[str, Any]:
    try:
        from copaw.agents.react_agent import CoPawAgent
    except Exception as exc:
        raise RuntimeError(f"cannot import CoPawAgent: {exc}") from exc
    create_toolkit = getattr(CoPawAgent, "_create_toolkit", None)
    if not callable(create_toolkit):
        raise RuntimeError("CoPawAgent._create_toolkit is unavailable")
    if getattr(create_toolkit, "_opskeeper_teamharness_hook", False):
        _COPAW_DIAGNOSTICS["wrap_installed"] = True
        return _copaw_diagnostics()
    original = create_toolkit

    @functools.wraps(original)
    def _opskeeper_create_toolkit(*args: Any, **kwargs: Any) -> Any:
        toolkit = original(*args, **kwargs)
        _validate_copaw_toolkit(toolkit)
        return toolkit

    _opskeeper_create_toolkit._opskeeper_teamharness_hook = True
    try:
        CoPawAgent._create_toolkit = staticmethod(_opskeeper_create_toolkit)
    except Exception as exc:
        raise RuntimeError(f"cannot install CoPaw toolkit hook: {exc}") from exc
    signature_hash = hashlib.sha256(
        f"{inspect.signature(original)}@{len(_COPAW_NATIVE_TOOLS)}".encode()
    ).hexdigest()
    _COPAW_DIAGNOSTICS.update({
        "wrap_installed": True,
        "copaw_toolkit_hook": True,
        "signature_hash": signature_hash,
        "missing_capabilities": [
            "readonly_middleware",
            "manager_gate",
            "new_task_context",
            "native_mcp_tools",
        ],
        "install_error": None,
    })
    return _copaw_diagnostics()


def build_investigate_router():
    """Expose a server-side signed RCA proxy for the Dashboard extension."""
    try:
        from fastapi import APIRouter, Header, HTTPException
    except ImportError:
        return None

    router = APIRouter()

    @router.post("/investigate")
    def investigate(
        payload: dict[str, Any],
        x_teamharness_runtime_key: str = Header(default=""),
    ) -> dict[str, Any]:
        expected = _runtime_gateway_key()
        if not expected or not hmac.compare_digest(x_teamharness_runtime_key, expected):
            raise HTTPException(status_code=401, detail="unauthorized")

        incident_id = str(payload.get("incident_id") or "").strip()
        if not incident_id:
            raise HTTPException(status_code=400, detail="incident_id is required")
        alert_group = payload.get("alert_group")
        correlation_hints = payload.get("correlation_hints")
        arguments = {
            "incident_id": incident_id,
            "alert_group": alert_group if isinstance(alert_group, list) else [],
            "correlation_hints": correlation_hints if isinstance(correlation_hints, dict) else {},
        }
        try:
            return _investigate_via_mcp(arguments)
        except (json.JSONDecodeError, RuntimeError) as exc:
            raise HTTPException(status_code=502, detail=str(exc)) from exc

    return router


def _register_incident_stop_hook(api: Any) -> None:
    try:
        from qwenpaw.runtime.hooks import HookAction, HookBase, HookResult
        from qwenpaw.runtime.phases import Phase
    except ImportError:
        _MANAGER_GATE_LOGGER.debug("QwenPaw runtime hooks unavailable", exc_info=True)
        return

    class IncidentStopHook(HookBase):
        phase = Phase.PRE_EXECUTE
        name = "opskeeper_incident_stop"
        priority = 0

        async def run(self, ctx: Any) -> Any:
            message = _request_text(getattr(ctx, "request", None))
            sender = _request_sender(getattr(ctx, "request", None))
            stop_match = _ADMIN_STOP_PATTERN.match(message)
            if stop_match and _is_admin_sender(sender):
                _STOPPED_INCIDENT_IDS.add(stop_match.group(1))
                workflow = _WORKFLOW_PROJECTOR.record_stop(
                    _extract_session_id(ctx),
                    message,
                )
                await _emit_workflow_projection(_extract_session_id(ctx), workflow)
                return HookResult(action=HookAction.SKIP_AGENT)
            if not _is_admin_sender(sender) and any(
                incident_id in message for incident_id in _STOPPED_INCIDENT_IDS
            ):
                return HookResult(action=HookAction.SKIP_AGENT)
            return HookResult()

    api.register_runtime_hook(IncidentStopHook())


def _register_manager_gate_hook(api: Any) -> None:
    """Register the plugin-native Manager continuation gate when QwenPaw is present."""
    try:
        from qwenpaw.runtime.hooks import HookAction, HookBase, HookResult
        from qwenpaw.runtime.phases import Phase
    except ImportError:
        _MANAGER_GATE_LOGGER.debug("QwenPaw runtime hooks unavailable", exc_info=True)
        return

    class ManagerContinuationGateHook(HookBase):
        phase = Phase.PRE_EXECUTE
        name = "opskeeper_manager_continuation_gate"
        priority = 5

        async def run(self, ctx: Any) -> Any:
            agent = getattr(ctx, "agent", None)
            if not _is_manager_agent(agent):
                return HookResult()
            session_id = _extract_session_id(ctx)
            message = _request_text(ctx.request)
            current_message = _current_matrix_message(message)
            if not _is_message_for_agent(message, agent) and "@opskeeper-" in message.lower():
                return HookResult(action=HookAction.SKIP_AGENT)
            consumed_results = _MANAGER_DISPATCH_GATE.consume_result_with_origins(
                session_id,
                message,
            )
            if consumed_results:
                for marker in consumed_results:
                    workflow = _WORKFLOW_PROJECTOR.record_result(marker, message)
                    await _emit_workflow_projection(
                        consumed_results[marker],
                        workflow,
                    )
                relays = [
                    (marker, origin)
                    for marker, origin in consumed_results.items()
                    if origin and origin != session_id
                ]
                failed_relays = []
                for marker, origin in relays:
                    try:
                        event_id = await asyncio.to_thread(
                            _relay_matrix_completion,
                            marker,
                            origin,
                            message,
                        )
                        _MANAGER_GATE_LOGGER.info(
                            "Manager relayed worker result marker=%s origin=%s event=%s",
                            marker,
                            origin,
                            event_id,
                        )
                    except Exception:
                        _MANAGER_GATE_LOGGER.warning(
                            "Manager worker result relay failed marker=%s origin=%s",
                            marker,
                            origin,
                            exc_info=True,
                        )
                        failed_relays.append(
                            f"task {marker}: send one concise completion message to the original "
                            f"Matrix session {origin} using target room {origin}; address @admin and "
                            f"include OPSKEEPER_COMPLETE {marker}. Then stop this turn."
                        )
                if failed_relays:
                    ctx.inject_context(
                        "OpsKeeper worker result relay is required before any next action. "
                        + " ".join(failed_relays),
                        priority=0,
                        source="opskeeper-manager-result-relay",
                    )
                    return HookResult()
                if relays:
                    return HookResult(action=HookAction.SKIP_AGENT)
                return HookResult()
            sender = _request_sender(ctx.request)
            approved = bool(_WORKFLOW_ADMIN_APPROVAL_PATTERN.search(current_message))
            rejected = bool(_WORKFLOW_ADMIN_REJECTION_PATTERN.search(current_message))
            admin_sender = _is_admin_sender(sender)
            if (approved or rejected) and not admin_sender:
                _MANAGER_GATE_LOGGER.warning(
                    "Ignored non-admin workflow decision sender=%s",
                    sender,
                )
            if admin_sender:
                if approved and not rejected and (
                    not _final_demo_incident_id(message)
                    or not _FINAL_DEMO_CANDIDATE_A_PATTERN.search(
                        _current_matrix_message(message)
                    )
                ):
                    try:
                        event_id = await asyncio.to_thread(
                            _send_matrix_notice,
                            session_id,
                            "审批未执行：审批指令不完整。请发送：@manager 已批准 "
                            "incident_id=<当前事件ID> Candidate A",
                        )
                        _MANAGER_GATE_LOGGER.info(
                            "Rejected incomplete admin approval event=%s",
                            event_id,
                        )
                    except Exception:
                        _MANAGER_GATE_LOGGER.warning(
                            "Failed to notify incomplete admin approval",
                            exc_info=True,
                        )
                    return HookResult(action=HookAction.SKIP_AGENT)
                deterministic_approval = approved and not rejected and (
                    await asyncio.to_thread(
                        _dispatch_final_demo_approval,
                        session_id,
                        sender or "",
                        message,
                    )
                )
                if approved or rejected:
                    workflow = _WORKFLOW_PROJECTOR.record_admin_decision(
                        session_id,
                        message,
                        approved and not rejected,
                    )
                    await _emit_workflow_projection(session_id, workflow)
                if deterministic_approval:
                    _MANAGER_GATE_LOGGER.info(
                        "Consumed final-demo admin approval sender=%s",
                        sender,
                    )
                    return HookResult(action=HookAction.SKIP_AGENT)
            authority = _verify_workflow_authority(sender, message, session_id)
            if _is_manager_agent(agent) and authority:
                incident_id, authority_stage = authority
                workflow = _WORKFLOW_PROJECTOR.record_authority_stage(
                    session_id,
                    incident_id,
                    authority_stage,
                )
                await _emit_workflow_projection(session_id, workflow)
                return HookResult(action=HookAction.SKIP_AGENT)
            if _workflow_incident_id(message):
                workflow = _WORKFLOW_PROJECTOR.record_request(session_id, message)
                await _emit_workflow_projection(session_id, workflow)
            if _has_new_task(message):
                _MANAGER_DISPATCH_GATE.record_request_origin(session_id, message)
                return HookResult()
            if not _MANAGER_DISPATCH_GATE.pending_markers(session_id):
                return HookResult()

            sender = _request_sender(ctx.request)
            if _is_admin_sender(sender):
                ctx.inject_context(
                    "OpsKeeper tasks are still pending. Handle this admin instruction, "
                    "but do not duplicate existing dispatches.",
                    priority=5,
                    source="opskeeper-manager-gate",
                )
                return HookResult()
            _MANAGER_GATE_LOGGER.info(
                "Manager continuation without task result skipped session=%s sender=%s",
                session_id,
                sender or "<unknown>",
            )
            return HookResult(action=HookAction.SKIP_AGENT)

    api.register_runtime_hook(ManagerContinuationGateHook())


def _register_manager_stop_handler(api: Any) -> None:
    try:
        from qwenpaw.loop.gates import StopAction, StopHandlerResult
    except ImportError:
        _MANAGER_GATE_LOGGER.debug("QwenPaw stop gates unavailable", exc_info=True)
        return

    async def stop_after_dispatch(_ctx: Any) -> Any:
        runtime = os.getenv("AGENTTEAMS_MANAGER_RUNTIME", "").strip().lower()
        if runtime not in {"qwenpaw", "copaw"} or not _MANAGER_DISPATCH_GATE.any_pending():
            return StopHandlerResult(action=StopAction.BYPASS)
        return StopHandlerResult(
            action=StopAction.TERMINATE,
            reason="OpsKeeper task dispatched; waiting for matching worker result",
        )

    api.register_agent_stop_handler(
        handler=stop_after_dispatch,
        priority=0,
        name="opskeeper_manager_dispatch_gate",
    )


class OpskeeperTeamHarnessPlugin:
    """qwenpaw 2.x public plugin entry point.

    qwenpaw imports this module, picks up the `plugin` singleton below, and
    calls `register(api)` once during worker boot. All 6 register_* calls are
    idempotent — repeated register() (e.g. on plugin reload) re-registers with
    qwenpaw internal stores keyed by id/name.
    """

    def register(self, api: Any) -> None:
        copaw_api_methods = {
            name.lstrip("_")
            for name in ("register_provider", "register_startup_hook", "register_shutdown_hook", "register_control_command")
        }
        runtime = os.getenv("AGENTTEAMS_MANAGER_RUNTIME", "").strip().lower()
        copaw_requested = runtime == "copaw"
        if not copaw_requested and runtime != "qwenpaw":
            try:
                copaw_requested = importlib.util.find_spec("copaw") is not None
            except (ImportError, ValueError):
                copaw_requested = False
        if copaw_requested:
            if set(dir(api)) >= {"register_provider", "register_startup_hook", "register_shutdown_hook", "register_control_command"}:
                diagnostics = _install_copaw_compat()
                try:
                    api.register_startup_hook(
                        "opskeeper_teamharness_diagnostics",
                        _copaw_diagnostics_startup_hook,
                        priority=0,
                    )
                except Exception as exc:
                    raise RuntimeError(f"cannot register CoPaw diagnostics startup hook: {exc}") from exc
                return diagnostics
            missing = sorted(copaw_api_methods - set(dir(api)))
            raise RuntimeError(f"CoPaw PluginApi is incomplete; missing: {missing}")

        # 1) Prompt sections — role-gated Manager team / Worker / Manager agents prompts
        _register_prompt_sections(api)

        # 2) Skill provider — qwenpaw 2 runtime 期望 flat 布局（每个 skill 子目录里直接放 SKILL.md），
        # 优先注册 ASSET_DIR/qwenpaw-skills/<name>/SKILL.md；如缺则回退嵌套布局 skills/agent/<name>/。
        candidates: list[Path] = []
        for candidate in (
            ASSET_DIR / "qwenpaw-skills",
            PLUGIN_DIR / "qwenpaw-skills",
            PLUGIN_DIR.parent / "qwenpaw-skills",
            ASSET_DIR / "skills" / "agent",
            PLUGIN_DIR / "skills" / "agent",
        ):
            if candidate.is_dir():
                candidates.append(candidate)
        for skills_dir in candidates:
            try:
                api.register_skill_provider(
                    skills_dir,
                    enabled_by_default=True,
                    channels=["all"],
                )
            except Exception:
                pass
        # Manager-side coordination skill lives under skills/team/。
        for skills_dir in (
            ASSET_DIR / "skills" / "team",
            PLUGIN_DIR / "skills" / "team",
        ):
            if skills_dir.is_dir():
                try:
                    api.register_skill_provider(
                        skills_dir,
                        enabled_by_default=True,
                        channels=["all"],
                    )
                except Exception:
                    pass

        # 3) Middleware — read-only enforcement (10) + outbound safety (20)
        #    + sanitizer (30) + audit (20)
        try:
            api.register_middleware(_readonly_enforcement_factory, priority=10)
        except Exception:
            pass
        api.register_middleware(_outbound_safety_factory, priority=20)
        try:
            api.register_middleware(_sanitizer_factory, priority=30)
        except Exception:
            pass
        try:
            api.register_middleware(audit_hook_factory, priority=20)
        except Exception:
            pass

        # 4) Runtime hooks — task_trace TraceEnter / TraceExit
        _register_manager_stop_handler(api)
        _register_incident_stop_hook(api)
        _register_manager_gate_hook(api)
        trace = _load_task_trace_module()
        if trace is not None:
            enter = getattr(trace, "TraceEnter", None)
            exit_hook = getattr(trace, "TraceExit", None)
            if enter is not None:
                try:
                    api.register_runtime_hook(enter())
                except Exception:
                    pass
            if exit_hook is not None:
                try:
                    api.register_runtime_hook(exit_hook())
                except Exception:
                    pass

        # 5) HTTP router — /health + /sync,供 opskeeper PluginSyncClient 调用
        self._register_http(api)

    def _register_http(self, api: Any) -> None:
        try:
            from fastapi import APIRouter
        except ImportError:
            return
        router = APIRouter()

        @router.get("/health")
        def health() -> dict[str, Any]:
            copaw_runtime = os.getenv("AGENTTEAMS_MANAGER_RUNTIME", "").strip().lower() == "copaw"
            if copaw_runtime or _COPAW_DIAGNOSTICS["wrap_installed"]:
                return {
                    "ok": _COPAW_DIAGNOSTICS["wrap_installed"] and _COPAW_DIAGNOSTICS["toolkit_validated"],
                    "plugin": "opskeeper-teamharness",
                    "version": _PLUGIN_VERSION,
                    "capabilities": _copaw_diagnostics(),
                }
            return {
                "ok": True,
                "plugin": "opskeeper-teamharness",
                "version": _PLUGIN_VERSION,
            }

        @router.post("/sync")
        def sync_endpoint() -> dict[str, Any]:
            """Trigger in-memory plugin config reload.

            Called by opskeeper PluginSyncClient (POST /v1/plugins/{id}/sync →
            WorkerHTTPClient → Worker POST /api/opskeeper-teamharness/sync).
            qwenpaw runtime re-reads plugin.yaml + skills directory + MCP server
            config; stdio MCP subprocess is restarted by qwenpaw internal logic.
            """
            return {
                "ok": True,
                "plugin": "opskeeper-teamharness",
                "managedBy": "opskeeper-teamharness-plugin",
            }

        # Install plugin router — POST /opskeeper-teamharness/install-plugin.
        # Built in module scope (build_install_plugin_router) so unit tests can
        # exercise it without booting the full qwenpaw runtime.
        install_router = build_install_plugin_router()
        if install_router is not None:
            router.include_router(install_router)

        investigate_router = build_investigate_router()
        if investigate_router is not None:
            router.include_router(investigate_router)

        try:
            api.register_http_router(
                router,
                prefix="/opskeeper-teamharness",
                tags=["opskeeper"],
            )
        except Exception:
            pass


# qwenpaw entry: import this module → take the `plugin` singleton → call plugin.register(api)
plugin = OpskeeperTeamHarnessPlugin()
# =============================================================================
# Plugin install endpoint — POST /opskeeper-teamharness/install-plugin
#
# Called by opskeeper PluginSyncClient over Controller-discovered worker HTTP
# (POST /api/opskeeper-teamharness/install-plugin). Accepts a multipart upload
# of a qwenpaw plugin package (zip containing a single directory with
# plugin.json), extracts it, runs `qwenpaw plugin install <path> --force`,
# and triggers the in-process sync endpoint so prompts/skills/MCP re-apply.
#
# Environment-agnostic: identical code path for Docker and K8s — the worker
# container already ships with qwenpaw on PATH; opskeeper never shells out
# from the Manager.
# =============================================================================

_INSTALL_LOG = logging.getLogger("opskeeper-teamharness.install")
_INSTALL_TMP_PREFIX = "opskeeper-plugin-install-"

# worker 端 install 体积上限。默认 32 MiB。
# 生产运维对齐 manager 端 OPSKEEPER_PLUGIN_MAX_ZIP_BYTES：把 helm values.manager.plugin.maxZipBytes
# 设到 worker 这个上限以下，否则 manager 接受但 worker 拒绝，会得到 413。
# 这条规则也在 deploy/helm/values.yaml 注释里强调。
_DEFAULT_MAX_INSTALL_BYTES = 32 * 1024 * 1024  # 32 MiB

try:
    _MAX_INSTALL_BYTES = int(
        os.environ.get("OPSKEEPER_WORKER_MAX_PLUGIN_BYTES", str(_DEFAULT_MAX_INSTALL_BYTES))
    )
except (TypeError, ValueError):
    # 非法 env 不让 worker 启动失败：降级为默认并记日志
    _MAX_INSTALL_BYTES = _DEFAULT_MAX_INSTALL_BYTES
    logging.getLogger("opskeeper-teamharness.install").warning(
        "OPSKEEPER_WORKER_MAX_PLUGIN_BYTES invalid, falling back to %d bytes",
        _DEFAULT_MAX_INSTALL_BYTES,
    )


def _resolve_qwenpaw_bin() -> str:
    """Locate the qwenpaw binary on PATH (mirrors worker.py:_run_qwenpaw)."""
    found = shutil.which("qwenpaw")
    if found:
        return found
    import sys

    return str(Path(sys.executable).with_name("qwenpaw"))


def _validate_zip_against_zip_slip(zf, target_dir):
    """Reject any entry whose resolved path escapes target_dir."""
    target_root = target_dir.resolve()
    for name in zf.namelist():
        resolved = (target_dir / name).resolve()
        try:
            resolved.relative_to(target_root)
        except ValueError:
            raise RuntimeError(f"unsafe zip entry path: {name}")


def _extract_plugin_zip(zip_bytes):
    """Extract a qwenpaw plugin zip and return the inner package directory.

    Mirrors worker.py:_extract_qwenpaw_plugin_zip contract: exactly one
    top-level directory containing plugin.json must be present.
    """
    tmp = Path(tempfile.mkdtemp(prefix=_INSTALL_TMP_PREFIX))
    zip_path = tmp / "package.zip"
    zip_path.write_bytes(zip_bytes)
    with zipfile.ZipFile(zip_path) as zf:
        _validate_zip_against_zip_slip(zf, tmp)
        zf.extractall(tmp)
    packages = [p for p in tmp.iterdir() if p.is_dir() and (p / "plugin.json").is_file()]
    if len(packages) != 1:
        shutil.rmtree(tmp, ignore_errors=True)
        raise RuntimeError(
            f"expected exactly one plugin package dir containing plugin.json; "
            f"found {len(packages)}"
        )
    package_dir = packages[0]
    persist = Path(tempfile.mkdtemp(prefix=_INSTALL_TMP_PREFIX))
    final = persist / package_dir.name
    shutil.move(str(package_dir), str(final))
    shutil.rmtree(tmp, ignore_errors=True)
    _INSTALL_LOG.info(
        "extracted plugin zip bytes=%d pkg=%s persist_dir=%s",
        len(zip_bytes),
        final.name,
        persist,
    )
    return final


def _get_qwenpaw_api_base() -> Optional[str]:
    """Return the running qwenpaw API base URL, or None.

    Mirrors qwenpaw.cli.plugin_commands._get_api_base: prefers the live
    last_api host/port (written by the running app), falls back to
    localhost:8088 for development.
    """
    try:
        from qwenpaw.config.utils import read_last_api  # type: ignore
    except Exception:
        read_last_api = None  # type: ignore

    host = port = None
    if read_last_api is not None:
        try:
            info = read_last_api()
        except Exception:
            info = None
        if info:
            host, port = info

    if not host or not port:
        host, port = "127.0.0.1", 8088
    return f"http://{host}:{port}/api"


def install_via_api(package_path, *, force: bool = True, timeout: float = 600.0) -> dict:
    """POST `/api/plugins/install` to the running qwenpaw process.

    Mirrors qwenpaw.cli.plugin_commands._api_install_plugin but bumps
    the timeout from the CLI's 120s ceiling — plugin hot-reload can take
    longer than 120s on first install when many agents need to be
    reloaded. Returns a result dict shaped like install_via_subprocess:

    - exitCode 0  + "api": True on success
    - exitCode <0 + "api": True when the HTTP request failed (caller
      should fall back to subprocess)
    - exitCode <0 + "api": False when the API replied with a non-2xx
      (do NOT fall back; the install itself was rejected)

    Using the in-process loader avoids the CLI subprocess overhead and
    removes the 120s hard cap that was previously failing installs on
    heavy hot-reload.
    """
    base = _get_qwenpaw_api_base()
    if base is None:
        return {"exitCode": -1, "api": False, "stderr": "qwenpaw API base unresolved"}

    url = f"{base}/plugins/install"
    payload = json.dumps({"source": str(package_path), "force": force}).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=payload,
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
        try:
            body = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            body = {"raw": raw[:4096].decode("utf-8", errors="replace")}
        name = body.get("name") or package_path.name
        return {
            "exitCode": 0,
            "api": True,
            "plugin": name,
            "stdout": f"hot-installed via API: {name}",
            "stderr": "",
        }
    except urllib.error.HTTPError as exc:
        try:
            detail = json.loads(exc.read()).get("detail", str(exc))
        except Exception:
            detail = str(exc)
        return {
            "exitCode": exc.code,
            "api": True,
            "stdout": "",
            "stderr": f"qwenpaw API install rejected: {detail}"[:4096],
        }
    except (urllib.error.URLError, ConnectionError, TimeoutError, OSError) as exc:
        # API unreachable / connection refused / timeout — signal caller to
        # fall back to subprocess. We surface the error class for logging.
        return {
            "exitCode": -1,
            "api": False,
            "stdout": "",
            "stderr": f"qwenpaw API unreachable at {url}: {exc}"[:4096],
        }


def install_via_subprocess(package_path, *, timeout: float = 600.0):
    """Run `qwenpaw plugin install <path> --force` and return a result dict.

    把异常分类为结构化错误:
    - FileNotFoundError → qwenpaw binary 不在 PATH(典型:worker 镜像漏装)
    - subprocess.TimeoutExpired → qwenpaw 子进程挂死(plugin 包异常)
    - 其他 Exception → 未知错误

    调用方根据 exitCode 判断,FileNotFoundError 转 503(运维缺失依赖),TimeoutExpired 转 504。
    """
    qwenpaw_bin = _resolve_qwenpaw_bin()
    cmd = [qwenpaw_bin, "plugin", "install", str(package_path), "--force"]
    _INSTALL_LOG.info("running cmd=%s timeout=%.0fs", cmd, timeout)
    try:
        completed = subprocess.run(cmd, check=False, capture_output=True, text=True, timeout=timeout)
    except FileNotFoundError as exc:
        _INSTALL_LOG.error("qwenpaw binary not found: %s", exc)
        return {
            "exitCode": 127,
            "stdout": "",
            "stderr": f"qwenpaw binary not found at {qwenpaw_bin!r}: {exc}\n"
                      f"hint: install qwenpaw into worker image (pip install qwen-cli) or "
                      f"set QWENPAW_BIN env to absolute path",
        }
    except subprocess.TimeoutExpired as exc:
        _INSTALL_LOG.error("qwenpaw plugin install timeout after %.0fs: %s", timeout, exc)
        return {
            "exitCode": 124,
            "stdout": (exc.stdout or b"")[-4096:] if isinstance(exc.stdout, bytes) else (exc.stdout or "")[-4096:],
            "stderr": (exc.stderr or b"")[-4096:] if isinstance(exc.stderr, bytes) else (exc.stderr or "")[-4096:] + f"\n[timeout after {timeout:.0f}s]",
        }
    return {
        "exitCode": completed.returncode,
        "stdout": (completed.stdout or "")[-4096:],
        "stderr": (completed.stderr or "")[-4096:],
    }


def install_plugin_package(package_path) -> dict:
    """Try the in-process API first, fall back to CLI subprocess.

    Strategy:
    1. POST /api/plugins/install (in-process loader). Bypasses CLI
       startup overhead and the CLI's hard 120s timeout.
    2. Only fall back to subprocess if the API itself is unreachable
       (connection refused / DNS / timeout). If the API returned a
       non-2xx, the install was rejected — we surface that as-is.

    The API field in the result discriminates the two cases:
    - api=True  → API responded; honour the exit code (0 = success,
      non-2xx = rejected by qwenpaw). Do NOT fall back to subprocess.
    - api=False → API unreachable; fall back to subprocess.
    """
    api_result = install_via_api(package_path, force=True, timeout=600.0)
    if api_result.get("api"):
        return api_result
    _INSTALL_LOG.warning(
        "API install unreachable, falling back to subprocess: %s",
        api_result.get("stderr", ""),
    )
    return install_via_subprocess(package_path, timeout=600.0)


def build_install_plugin_router():
    """Construct the FastAPI router exposing install-plugin + health.

    Lives at module level so unit tests can drive the handlers via FastAPI
    TestClient without going through the full qwenpaw boot sequence.
    """
    try:
        from fastapi import APIRouter, File, HTTPException, UploadFile
    except ImportError:
        return None

    router = APIRouter()

    @router.get("/install-plugin/health")
    def install_health():
        return {
            "ok": True,
            "qwenpaw": _resolve_qwenpaw_bin(),
            "maxBytes": _MAX_INSTALL_BYTES,
        }

    @router.post("/install-plugin")
    async def install_plugin(file=File(...)):
        if not getattr(file, "filename", None):
            raise HTTPException(status_code=400, detail="missing file field")
        contents = await file.read()
        if not contents:
            raise HTTPException(status_code=400, detail="plugin package is empty")
        if len(contents) > _MAX_INSTALL_BYTES:
            raise HTTPException(
                status_code=413,
                detail=f"plugin package exceeds {_MAX_INSTALL_BYTES // (1024 * 1024)} MiB limit",
            )
        try:
            package_dir = _extract_plugin_zip(contents)
        except RuntimeError as exc:
            _INSTALL_LOG.warning("zip extract failed: %s", exc)
            raise HTTPException(status_code=400, detail=str(exc)) from exc

        try:
            # install_plugin_package runs a synchronous subprocess (qwenpaw CLI /
            # in-process API call); on the FastAPI event loop this would block
            # every other request (verified: GET /api/version timed out for 30s+
            # after a previous install). asyncio.to_thread pushes the work to a
            # thread-pool worker so the event loop stays responsive.
            result = await asyncio.to_thread(install_plugin_package, package_dir)
        finally:
            try:
                shutil.rmtree(package_dir.parent, ignore_errors=True)
            except Exception:
                pass

        if result["exitCode"] != 0:
            _INSTALL_LOG.error(
                "qwenpaw plugin install failed exit=%d stderr=%s",
                result["exitCode"],
                result["stderr"],
            )
            # Map 常见 exit code 到 HTTP status:
            #   127 = command not found (binary missing)
            #   124 = timeout
            #   其它 = generic install failure
            if result["exitCode"] == 127:
                status = 503  # Service Unavailable: 运维缺失 qwenpaw 依赖
            elif result["exitCode"] == 124:
                status = 504  # Gateway Timeout: qwenpaw 挂死
            else:
                status = 500
            raise HTTPException(
                status_code=status,
                detail={
                    "error": "qwenpaw plugin install failed",
                    "exitCode": result["exitCode"],
                    "stderr": result["stderr"],
                    "hint": (
                        "exit 127 = qwenpaw binary not found in PATH; "
                        "check worker image Dockerfile has qwen-cli installed"
                        if status == 503 else
                        "exit 124 = qwenpaw subprocess timeout; check plugin package size"
                        if status == 504 else
                        "check stderr for qwenpaw plugin error details"
                    ),
                },
            )
        return {
            "ok": True,
            "plugin": file.filename,
            "exitCode": result["exitCode"],
            "stdout": result["stdout"],
        }

    return router
