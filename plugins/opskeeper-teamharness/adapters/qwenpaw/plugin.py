"""opskeeper-teamharness integration with QwenPaw 2 public plugin APIs.

Mirrors teamharness/adapters/qwenpaw/plugin.py structure:
- _sanitizer_factory: redact sensitive fields in tool outputs
- on_acting hook: log every tool invocation to opskeeper audit
- task_trace: track Worker task lifecycle
"""
from __future__ import annotations

import asyncio
import importlib.util
import json
import logging
import os
import re
import shutil
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import zipfile
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
_PLUGIN_VERSION = "1.0.29"
_READ_ONLY_LOGGER = logging.getLogger("opskeeper-teamharness.readonly")
_MANAGER_GATE_LOGGER = logging.getLogger("opskeeper-teamharness.manager-gate")
_MANAGER_GATE_TTL_ENV = "OPSKEEPER_MANAGER_GATE_TTL_SECONDS"
_DEFAULT_MANAGER_GATE_TTL_SECONDS = 600.0
_TASK_MARKER_PATTERN = re.compile(
    r"\bOPSKEEPER[\s_]+TASK[\s_]+([A-Za-z0-9][A-Za-z0-9._:-]{0,127})\b"
)
_TASK_RESULT_PATTERN = re.compile(
    r"(?m)^(?:[`*_]*(?:@[A-Za-z0-9._=-]+(?::[A-Za-z0-9._=-]+)+|manager)[`*_]*[ \t]+)?"
    r"[`*_]*[ \t]*OPSKEEPER[\s_]+RESULT[\s_]+"
    r"([A-Za-z0-9][A-Za-z0-9._:-]{0,127})(?:\s|[`*_]|$)"
)
_TASK_ID_PATTERN = re.compile(r"\bOPSKEEPER-[A-Za-z0-9][A-Za-z0-9._:-]{2,127}\b")
_TASK_COMPLETE_PATTERN = re.compile(r"\bOPSKEEPER_COMPLETE\s+[A-Za-z0-9][A-Za-z0-9._:-]{2,127}\b")
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
    "opskeeper.incident.list",
    "opskeeper.incident.get",
    "opskeeper.postgres.analyze.status",
    "opskeeper.host.get_load",
    "opskeeper.host.get_processes",
    "opskeeper.knowledge.query",
    "opskeeper.state.get",
    "opskeeper.recovery.verify",
    "opskeeper.loop.correlate",
    "opskeeper.loop.investigate",
})


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
        self._request_origins: dict[str, str] = {}
        self._lock = threading.RLock()

    def record_request_origin(self, session_id: str, message: str) -> tuple[str, ...]:
        markers = _extract_task_markers(message)
        if not markers:
            return ()
        with self._lock:
            for marker in markers:
                self._request_origins[marker] = session_id
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
    """Return a qwenpaw MiddlewareBase instance, or None if unavailable."""
    try:
        from agentscope.middleware import MiddlewareBase
    except ImportError:
        return None

    class OpskeeperSanitizer(MiddlewareBase):
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
    manager_runtime = os.getenv("AGENTTEAMS_MANAGER_RUNTIME", "").strip().lower()
    return (
        role in {"manager", "leader", "team_leader"}
        or manager_runtime in {"qwenpaw", "copaw"}
        or "manager" in agent_name
    )


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


def _permission_mode() -> str:
    mode = os.getenv(_PERMISSION_MODE_ENV, "read_only").strip().lower()
    return "standard" if mode == "standard" else "read_only"


def _denied_tool_response(tool_name: str) -> Any:
    from agentscope.message import TextBlock, ToolResultState
    from agentscope.tool import ToolResponse

    return ToolResponse(
        content=[TextBlock(text=f"[DENIED] {tool_name} is not allowed in read-only mode.")],
        state=ToolResultState.DENIED,
        metadata={"opskeeper.permission_mode": "read_only"},
    )


def _has_failed_tool_result(events: list[Any]) -> bool:
    for event in events:
        state = str(getattr(event, "state", "")).lower()
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


def _readonly_enforcement_factory(context: Any, _agent_config: Any):
    try:
        from agentscope.middleware import MiddlewareBase
    except ImportError:
        return None

    factory_session_id = _extract_session_id(context)

    class OpskeeperReadOnlyMiddleware(MiddlewareBase):
        def __init__(self) -> None:
            self._task_message_sent = False

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
            ):
                result = _denied_tool_response(tool_name)
                _READ_ONLY_LOGGER.warning(
                    "read-only boundary denied tool=%s normalized=%s",
                    tool_name,
                    normalized_name,
                )
                _audit_tool_call(tool_name, arguments, result)
                yield result
                return

            events: list[Any] = []
            message_text = _message_text(arguments) if normalized_name == "message" else ""
            task_markers = _extract_task_markers(message_text)
            if self._task_message_sent and task_markers:
                result = _denied_tool_response("message")
                _READ_ONLY_LOGGER.warning(
                    "Manager one-dispatch boundary denied a second task message markers=%s",
                    list(task_markers),
                )
                _audit_tool_call(tool_name, arguments, result)
                yield result
                return
            gate_sessions = _dispatch_gate_sessions(factory_session_id, arguments)
            if any(
                _MANAGER_DISPATCH_GATE.has_pending(session_id, marker)
                for session_id in gate_sessions
                for marker in task_markers
            ):
                result = _denied_tool_response("message")
                _READ_ONLY_LOGGER.warning(
                    "Manager dispatch gate denied duplicate task markers=%s",
                    list(task_markers),
                )
                _audit_tool_call(tool_name, arguments, result)
                yield result
                return

            async for event in next_handler():
                events.append(event)
                yield event

            if (
                normalized_name == "message"
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
            session_id = _extract_session_id(ctx)
            message = _request_text(ctx.request)
            consumed_results = _MANAGER_DISPATCH_GATE.consume_result_with_origins(
                session_id,
                message,
            )
            if consumed_results:
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
        # 1) Prompt sections — 注入 Manager team / Worker / Manager agents 三段 prompt
        try:
            api.register_prompt_section(
                "opskeeper_team_context",
                after="workspace",
                provider=team_prompt,
                priority=40,
            )
        except Exception:
            pass
        try:
            api.register_prompt_section(
                "opskeeper_worker_context",
                after="workspace",
                provider=worker_prompt,
                priority=30,
            )
        except Exception:
            pass
        try:
            api.register_prompt_section(
                "opskeeper_manager_context",
                after="workspace",
                provider=manager_prompt,
                priority=30,
            )
        except Exception:
            pass

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

        # 3) Middleware — read-only enforcement (10) + sanitizer (30) + audit (20)
        try:
            api.register_middleware(_readonly_enforcement_factory, priority=10)
        except Exception:
            pass
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
