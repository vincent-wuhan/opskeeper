import asyncio
import importlib.util
import json
import sys
import unittest
from enum import Enum
from pathlib import Path
from types import ModuleType, SimpleNamespace
from typing import Any
from unittest.mock import patch


class _ToolResultState(str, Enum):
    DENIED = "denied"


class _TextBlock:
    def __init__(self, text: str):
        self.text = text


class _ToolResponse:
    def __init__(
        self,
        content: list[Any],
        state: _ToolResultState,
        metadata: dict[str, Any],
    ):
        self.content = content
        self.state = state
        self.metadata = metadata


class _MiddlewareBase:
    pass


def _install_runtime_stubs() -> dict[str, Any]:
    agentscope = ModuleType("agentscope")
    agentscope_middleware = ModuleType("agentscope.middleware")
    agentscope_message = ModuleType("agentscope.message")
    agentscope_tool = ModuleType("agentscope.tool")
    agentscope_middleware.MiddlewareBase = _MiddlewareBase
    agentscope_message.TextBlock = _TextBlock
    agentscope_message.ToolResultState = _ToolResultState
    agentscope_tool.ToolResponse = _ToolResponse
    agentscope.middleware = agentscope_middleware
    agentscope.message = agentscope_message
    agentscope.tool = agentscope_tool

    qwenpaw = ModuleType("qwenpaw")
    qwenpaw.__path__ = []
    qwenpaw_loop = ModuleType("qwenpaw.loop")
    qwenpaw_loop.__path__ = []
    gates = ModuleType("qwenpaw.loop.gates")

    class StopAction(str, Enum):
        BYPASS = "bypass"
        TERMINATE = "terminate"

    class StopHandlerResult:
        def __init__(self, action: StopAction = StopAction.TERMINATE, **kwargs: Any):
            self.action = action
            self.kwargs = kwargs

    gates.StopAction = StopAction
    gates.StopHandlerResult = StopHandlerResult
    runtime = ModuleType("qwenpaw.runtime")
    runtime.__path__ = []
    hooks = ModuleType("qwenpaw.runtime.hooks")
    phases = ModuleType("qwenpaw.runtime.phases")

    class HookAction(str, Enum):
        CONTINUE = "continue"
        SKIP_AGENT = "skip_agent"

    class HookResult:
        def __init__(self, action: HookAction = HookAction.CONTINUE):
            self.action = action

    class HookBase:
        pass

    class Phase(str, Enum):
        PRE_EXECUTE = "pre_execute"
        FINALLY = "finally"

    hooks.HookAction = HookAction
    hooks.HookBase = HookBase
    hooks.HookResult = HookResult
    phases.Phase = Phase
    qwenpaw.runtime = runtime
    runtime.hooks = hooks
    runtime.phases = phases
    qwenpaw.loop = qwenpaw_loop
    qwenpaw_loop.gates = gates

    installed = {
        "agentscope": agentscope,
        "agentscope.middleware": agentscope_middleware,
        "agentscope.message": agentscope_message,
        "agentscope.tool": agentscope_tool,
        "qwenpaw": qwenpaw,
        "qwenpaw.runtime": runtime,
        "qwenpaw.runtime.hooks": hooks,
        "qwenpaw.runtime.phases": phases,
        "qwenpaw.loop": qwenpaw_loop,
        "qwenpaw.loop.gates": gates,
    }
    saved = {name: sys.modules.get(name) for name in installed}
    sys.modules.update(installed)
    return saved


def _restore_runtime_stubs(saved: dict[str, Any]) -> None:
    for name, module in saved.items():
        if module is None:
            sys.modules.pop(name, None)
        else:
            sys.modules[name] = module


def _load_plugin():
    path = Path(__file__).with_name("plugin.py")
    spec = importlib.util.spec_from_file_location("opskeeper_manager_gate_under_test", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("cannot load plugin.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ManagerGateTest(unittest.TestCase):
    def setUp(self):
        self.saved_modules = _install_runtime_stubs()
        self.module = _load_plugin()
        self.gate = self.module.ManagerDispatchGate()

    def tearDown(self):
        _restore_runtime_stubs(self.saved_modules)

    def test_marker_lifecycle_records_pending_and_consumes_result(self):
        message = "OPSKEEPER TASK task-001"
        self.assertEqual(self.gate.record("session-1", message), ("task-001",))
        self.assertEqual(self.gate.pending_markers("session-1"), ("task-001",))
        self.assertEqual(self.gate.pending_markers("session-2"), ())
        self.assertEqual(
            self.gate.consume_result("session-2", "OPSKEEPER_RESULT task-001 {}"),
            ("task-001",),
        )
        self.assertEqual(
            self.gate.consume_result("session-1", "OPSKEEPER_RESULT task-001 {}"),
            (),
        )
        self.assertEqual(self.gate.pending_markers("session-1"), ())

    def test_result_with_manager_mention_prefix_is_consumed(self):
        self.gate.record("session-1", "OPSKEEPER TASK OPSKEEPER-GATE-001")
        message = (
            "@manager:matrix-local.agentteams.io:18080 "
            "OPSKEEPER_RESULT OPSKEEPER-GATE-001 {\"count\":0}"
        )
        self.assertEqual(
            self.gate.consume_result("session-1", message),
            ("OPSKEEPER-GATE-001",),
        )
        self.assertEqual(self.gate.pending_markers("session-1"), ())

    def test_result_with_plain_manager_prefix_is_consumed(self):
        self.gate.record("session-1", "OPSKEEPER TASK OPSKEEPER-GATE-001")
        self.assertEqual(
            self.gate.consume_result(
                "session-1",
                'manager OPSKEEPER_RESULT OPSKEEPER-GATE-001 {"count":0}',
            ),
            ("OPSKEEPER-GATE-001",),
        )
        self.assertEqual(self.gate.pending_markers("session-1"), ())

    def test_markdown_bold_result_is_consumed(self):
        self.gate.record("session-1", "OPSKEEPER TASK OPSKEEPER-GATE-001")
        message = "**OPSKEEPER_RESULT OPSKEEPER-GATE-001**"
        self.assertEqual(
            self.gate.consume_result("session-1", message),
            ("OPSKEEPER-GATE-001",),
        )
        self.assertEqual(self.gate.pending_markers("session-1"), ())

    def test_markdown_bold_manager_mention_prefix_is_consumed(self):
        self.gate.record("session-1", "OPSKEEPER TASK OPSKEEPER-GATE-001")
        message = (
            "**@manager:matrix-local.agentteams.io:18080** "
            "OPSKEEPER_RESULT OPSKEEPER-GATE-001 {\"ok\":true}"
        )
        self.assertEqual(
            self.gate.consume_result("session-1", message),
            ("OPSKEEPER-GATE-001",),
        )
        self.assertEqual(self.gate.pending_markers("session-1"), ())

    def test_result_consumes_marker_across_dispatch_and_execution_sessions(self):
        self.gate.record_request_origin(
            "matrix:entry-room",
            "OPSKEEPER TASK OPSKEEPER-GATE-001",
        )
        self.gate.record(
            "matrix:entry-room",
            "OPSKEEPER TASK OPSKEEPER-GATE-001",
        )
        self.gate.record(
            "matrix:execution-room",
            "OPSKEEPER TASK OPSKEEPER-GATE-001",
            "matrix:entry-room",
        )
        self.assertEqual(
            self.gate.consume_result_with_origins(
                "matrix:execution-room",
                "**OPSKEEPER_RESULT OPSKEEPER-GATE-001**",
            ),
            {"OPSKEEPER-GATE-001": "matrix:entry-room"},
        )
        self.assertEqual(self.gate.pending_markers("matrix:entry-room"), ())
        self.assertEqual(self.gate.pending_markers("matrix:execution-room"), ())

    def test_result_relays_from_request_origin_without_dispatch_pending(self):
        self.gate.record_request_origin(
            "matrix:entry-room",
            "OPSKEEPER TASK OPSKEEPER-GATE-001",
        )
        self.assertEqual(
            self.gate.consume_result_with_origins(
                "matrix:execution-room",
                "**@manager:hs** OPSKEEPER_RESULT OPSKEEPER-GATE-001 {}",
            ),
            {"OPSKEEPER-GATE-001": "matrix:entry-room"},
        )

    def test_task_contract_with_result_instruction_is_new_task(self):
        message = (
            "@manager:hs OPSKEEPER TASK OPSKEEPER-GATE-001\\n"
            "Worker result line: OPSKEEPER_RESULT OPSKEEPER-GATE-001 {}"
        )
        self.assertTrue(self.module._has_new_task(message))
        self.assertEqual(
            self.gate.record_request_origin("matrix:entry-room", message),
            ("OPSKEEPER-GATE-001",),
        )

    def test_worker_prompt_requires_manager_matrix_prefix(self):
        prompt_path = (
            Path(__file__).resolve().parents[2]
            / "prompts"
            / "agent"
            / "worker.md"
        )
        prompt = prompt_path.read_text(encoding="utf-8")
        self.assertIn(
            "`@manager:<server> OPSKEEPER_RESULT <task_id> {json}`",
            prompt,
        )
        self.assertIn("不能省略", prompt)

    def test_marker_extraction_falls_back_to_long_task_id(self):
        self.assertEqual(
            self.gate.record(
                "session-1",
                "manager 转发 **OPSKEEPER-PROTOCOL-001**，请执行只读验证。",
            ),
            ("OPSKEEPER-PROTOCOL-001",),
        )
        self.gate.clear("session-1")

    def test_pending_marker_expires_after_ttl(self):
        with patch.object(
            self.module.time,
            "monotonic",
            side_effect=[100.0, 701.0],
        ):
            self.gate.record("session-1", "OPSKEEPER TASK task-001")
            self.assertEqual(self.gate.pending_markers("session-1"), ())

    def test_invalid_ttl_falls_back_to_default(self):
        with patch.dict("os.environ", {self.module._MANAGER_GATE_TTL_ENV: "bad"}):
            self.assertEqual(self.gate.ttl_seconds(), 600.0)

    def _dispatch_middleware(
        self,
        middleware: Any,
        text: str,
        target: str | None = None,
        agent: Any | None = None,
    ):
        arguments = {"message": text}
        if target is not None:
            arguments["target"] = target
        input_kwargs = {
            "tool_call": SimpleNamespace(
                name="message",
                input=json.dumps(arguments),
            ),
        }

        async def next_handler(**_kwargs):
            yield "sent"

        async def run():
            events = []
            async for event in middleware.on_acting(
                agent=agent or SimpleNamespace(name="unknown-runtime-agent"),
                input_kwargs=input_kwargs,
                next_handler=next_handler,
            ):
                events.append(event)
            return events

        return asyncio.run(run())

    def test_dispatch_records_in_source_and_target_sessions(self):
        source_session = "matrix:manager-room"
        target_session = "matrix:!worker-room:hs"
        self.module._MANAGER_DISPATCH_GATE._pending.clear()
        self.module._MANAGER_DISPATCH_GATE._origins.clear()
        middleware = self.module._readonly_enforcement_factory(
            SimpleNamespace(session_id=source_session),
            None,
        )
        self.assertEqual(
            self._dispatch_middleware(
                middleware,
                "OPSKEEPER TASK task-001",
                target="room:!worker-room:hs",
            ),
            ["sent"],
        )
        self.assertEqual(
            self.module._MANAGER_DISPATCH_GATE.pending_markers(source_session),
            ("task-001",),
        )
        self.assertEqual(
            self.module._MANAGER_DISPATCH_GATE.pending_markers(target_session),
            ("task-001",),
        )

    def test_dispatch_records_source_room_for_worker_result_relay(self):
        source_session = "matrix:manager-room"
        target_session = "matrix:!worker-room:hs"
        self.module._MANAGER_DISPATCH_GATE._pending.clear()
        self.module._MANAGER_DISPATCH_GATE._origins.clear()
        self.module._MANAGER_DISPATCH_GATE.clear(source_session)
        self.module._MANAGER_DISPATCH_GATE.clear(target_session)
        middleware = self.module._readonly_enforcement_factory(
            SimpleNamespace(session_id=source_session),
            None,
        )
        self._dispatch_middleware(
            middleware,
            "OPSKEEPER TASK task-001",
            target="room:!worker-room:hs",
        )
        self.assertEqual(
            self.module._MANAGER_DISPATCH_GATE.consume_result_with_origins(
                target_session,
                "@manager:hs OPSKEEPER_RESULT task-001 {}",
            ),
            {"task-001": source_session},
        )

    def test_dispatch_queues_react_pending_stop(self):
        source_session = "matrix:manager-room"
        agent = SimpleNamespace(name="manager", _gate_pending_stop=None)
        self.module._MANAGER_DISPATCH_GATE.clear(source_session)
        middleware = self.module._readonly_enforcement_factory(
            SimpleNamespace(session_id=source_session),
            None,
        )
        self.assertEqual(
            self._dispatch_middleware(
                middleware,
                "OPSKEEPER TASK task-001",
                agent=agent,
            ),
            ["sent"],
        )
        self.assertIsNotNone(agent._gate_pending_stop)
        self.assertEqual(agent._gate_pending_stop.action.value, "terminate")
        self.module._MANAGER_DISPATCH_GATE.clear(source_session)

    def test_duplicate_pending_task_is_denied(self):
        source_session = "matrix:room-1"
        self.module._MANAGER_DISPATCH_GATE.clear(source_session)

    def test_dispatch_records_rewritten_task_id(self):
        source_session = "matrix:room-1"
        self.module._MANAGER_DISPATCH_GATE.clear(source_session)
        middleware = self.module._readonly_enforcement_factory(
            SimpleNamespace(session_id=source_session),
            None,
        )
        self.assertEqual(
            self._dispatch_middleware(
                middleware,
                "manager 转发 **OPSKEEPER-PROTOCOL-001**，请执行只读验证。",
            ),
            ["sent"],
        )
        self.assertEqual(
            self.module._MANAGER_DISPATCH_GATE.pending_markers(source_session),
            ("OPSKEEPER-PROTOCOL-001",),
        )
        self.module._MANAGER_DISPATCH_GATE.clear(source_session)
        middleware = self.module._readonly_enforcement_factory(
            SimpleNamespace(session_id=source_session),
            None,
        )
        self.assertEqual(
            self._dispatch_middleware(middleware, "OPSKEEPER TASK task-001"),
            ["sent"],
        )
        denied = self._dispatch_middleware(middleware, "OPSKEEPER TASK task-001")
        self.assertEqual(denied[0].state, _ToolResultState.DENIED)
        self.module._MANAGER_DISPATCH_GATE.clear(source_session)

    def _registered_hook(self):
        registered = []

        class FakeApi:
            def register_runtime_hook(self, hook):
                registered.append(hook)

        self.module._register_manager_gate_hook(FakeApi())
        self.assertEqual(len(registered), 1)
        return registered[0]

    def test_stop_handler_terminates_manager_turn_while_pending(self):
        registered = []

        class FakeApi:
            def register_agent_stop_handler(self, **kwargs):
                registered.append(kwargs)

        self.module._register_manager_stop_handler(FakeApi())
        self.assertEqual(len(registered), 1)
        self.assertEqual(registered[0]["name"], "opskeeper_manager_dispatch_gate")
        self.module._MANAGER_DISPATCH_GATE.clear("matrix:room-1")
        self.module._MANAGER_DISPATCH_GATE.record(
            "matrix:room-1",
            "OPSKEEPER TASK task-001",
        )
        with patch.dict(
            "os.environ",
            {"AGENTTEAMS_MANAGER_RUNTIME": "qwenpaw"},
            clear=False,
        ):
            result = asyncio.run(registered[0]["handler"]({}))
        self.assertEqual(result.action.value, "terminate")
        self.module._MANAGER_DISPATCH_GATE.clear("matrix:room-1")

    def _hook_context(self, message: str, sender: str = "@worker:hs"):
        request = SimpleNamespace(
            input=[SimpleNamespace(content=[SimpleNamespace(text=message)])],
            channel_meta={"sender_id": sender},
        )
        return SimpleNamespace(
            agent=SimpleNamespace(name="unknown-runtime-agent"),
            request=request,
            session_id="matrix:room-1",
            context_injections=[],
            inject_context=lambda *args, **kwargs: None,
        )

    def test_hook_skips_empty_continuation_without_result(self):
        self.module._MANAGER_DISPATCH_GATE.record(
            "matrix:room-1",
            "OPSKEEPER TASK task-001",
        )
        result = asyncio.run(self._registered_hook().run(self._hook_context("")))
        self.assertEqual(result.action.value, "skip_agent")
        self.module._MANAGER_DISPATCH_GATE.clear("matrix:room-1")

    def test_hook_does_not_record_input_task_before_dispatch(self):
        self.module._MANAGER_DISPATCH_GATE.clear("matrix:room-1")
        result = asyncio.run(
            self._registered_hook().run(
                self._hook_context("OPSKEEPER TASK OPSKEEPER-PROTOCOL-001")
            )
        )
        self.assertEqual(result.action.value, "continue")
        self.assertEqual(
            self.module._MANAGER_DISPATCH_GATE.pending_markers("matrix:room-1"),
            (),
        )
        self.module._MANAGER_DISPATCH_GATE.clear("matrix:room-1")

    def test_hook_records_request_room_before_dispatch(self):
        source_session = "matrix:manager-room"
        target_session = "matrix:!worker-room:hs"
        self.module._MANAGER_DISPATCH_GATE.clear(source_session)
        self.module._MANAGER_DISPATCH_GATE.clear(target_session)
        context = self._hook_context(
            "OPSKEEPER TASK task-001",
            sender="@admin:hs",
        )
        context.session_id = source_session
        result = asyncio.run(self._registered_hook().run(context))
        self.assertEqual(result.action.value, "continue")
        self.assertEqual(
            self.module._MANAGER_DISPATCH_GATE.pending_markers(source_session),
            (),
        )
        self.module._MANAGER_DISPATCH_GATE.record(
            target_session,
            "OPSKEEPER TASK task-001",
            target_session,
        )
        self.assertEqual(
            self.module._MANAGER_DISPATCH_GATE.consume_result_with_origins(
                target_session,
                "@manager:hs OPSKEEPER_RESULT task-001 {}",
            ),
            {"task-001": source_session},
        )

    def test_hook_skips_rich_continuation_without_result(self):
        self.module._MANAGER_DISPATCH_GATE.record(
            "matrix:room-1",
            "OPSKEEPER TASK task-001",
        )
        result = asyncio.run(
            self._registered_hook().run(
                self._hook_context("历史上下文看起来包含新信息，但没有任务结果。")
            )
        )
        self.assertEqual(result.action.value, "skip_agent")
        self.module._MANAGER_DISPATCH_GATE.clear("matrix:room-1")

    def test_hook_does_not_treat_mismatched_result_as_new_task(self):
        self.module._MANAGER_DISPATCH_GATE.record(
            "matrix:room-1",
            "OPSKEEPER TASK task-001",
        )
        result = asyncio.run(
            self._registered_hook().run(
                self._hook_context(
                    "@manager:matrix-local.agentteams.io:18080 "
                    "OPSKEEPER_RESULT OPSKEEPER-GATE-002 {}"
                )
            )
        )
        self.assertEqual(result.action.value, "skip_agent")
        self.assertEqual(
            self.module._MANAGER_DISPATCH_GATE.pending_markers("matrix:room-1"),
            ("task-001",),
        )
        self.module._MANAGER_DISPATCH_GATE.clear("matrix:room-1")

    def test_hook_allows_matching_worker_result(self):
        self.module._MANAGER_DISPATCH_GATE.record(
            "matrix:room-1",
            "OPSKEEPER TASK task-001",
        )
        result = asyncio.run(
            self._registered_hook().run(
                self._hook_context("OPSKEEPER_RESULT task-001 {}")
            )
        )
        self.assertEqual(result.action.value, "continue")
        self.assertEqual(self.module._MANAGER_DISPATCH_GATE.pending_markers("matrix:room-1"), ())

    def test_hook_consumes_result_with_manager_mention_prefix(self):
        self.module._MANAGER_DISPATCH_GATE.record(
            "matrix:room-1",
            "OPSKEEPER TASK OPSKEEPER-GATE-001",
        )
        message = (
            "@manager:matrix-local.agentteams.io:18080 "
            "OPSKEEPER_RESULT OPSKEEPER-GATE-001 {\"count\":0}"
        )
        result = asyncio.run(self._registered_hook().run(self._hook_context(message)))
        self.assertEqual(result.action.value, "continue")
        self.assertEqual(
            self.module._MANAGER_DISPATCH_GATE.pending_markers("matrix:room-1"),
            (),
        )

    def test_hook_consumes_markdown_bold_worker_result(self):
        self.module._MANAGER_DISPATCH_GATE.clear("matrix:room-1")
        self.module._MANAGER_DISPATCH_GATE.record(
            "matrix:room-1",
            "OPSKEEPER TASK OPSKEEPER-GATE-001",
        )
        message = "**OPSKEEPER_RESULT OPSKEEPER-GATE-001**\n\n```json\n{\"ok\":true}\n```"
        result = asyncio.run(self._registered_hook().run(self._hook_context(message)))
        self.assertEqual(result.action.value, "continue")
        self.assertEqual(
            self.module._MANAGER_DISPATCH_GATE.pending_markers("matrix:room-1"),
            (),
        )
        self.module._MANAGER_DISPATCH_GATE.clear("matrix:room-1")

    def test_hook_instructs_manager_to_relay_worker_room_result_to_origin(self):
        source_session = "matrix:manager-room"
        worker_session = "matrix:!worker-room:hs"
        self.module._MANAGER_DISPATCH_GATE.clear(source_session)
        self.module._MANAGER_DISPATCH_GATE.clear(worker_session)
        self.module._MANAGER_DISPATCH_GATE.record(
            worker_session,
            "OPSKEEPER TASK task-001",
            source_session,
        )
        context = self._hook_context(
            "@manager:hs OPSKEEPER_RESULT task-001 {}",
        )
        context.session_id = worker_session
        context.inject_context = lambda *_args, **_kwargs: (_ for _ in ()).throw(
            AssertionError("direct relay should make prompt fallback unnecessary")
        )
        with patch.object(
            self.module,
            "_relay_matrix_completion",
            return_value="$relay-event",
        ) as relay:
            result = asyncio.run(self._registered_hook().run(context))
        self.assertEqual(result.action.value, "continue")
        relay.assert_called_once()
        self.assertEqual(relay.call_args.args[0], "task-001")
        self.assertEqual(relay.call_args.args[1], source_session)
        self.module._MANAGER_DISPATCH_GATE.clear(source_session)
        self.module._MANAGER_DISPATCH_GATE.clear(worker_session)

    def test_completion_notice_is_not_treated_as_new_dispatch(self):
        self.assertFalse(
            self.module._extract_task_markers(
                "@admin:hs OPSKEEPER_COMPLETE task-001 result passed"
            )
        )

    def test_hook_allows_admin_and_new_task(self):
        self.module._MANAGER_DISPATCH_GATE.record(
            "matrix:room-1",
            "OPSKEEPER TASK task-001",
        )
        with patch.dict(
            "os.environ",
            {"AGENTTEAMS_ADMIN_MATRIX_ID": "@admin:hs"},
            clear=False,
        ):
            hook = self._registered_hook()
            admin_result = asyncio.run(
                self._registered_hook().run(self._hook_context("请处理", "@admin:hs"))
            )
            new_task_result = asyncio.run(
                hook.run(self._hook_context("OPSKEEPER TASK task-002"))
            )
        self.assertEqual(admin_result.action.value, "continue")
        self.assertEqual(new_task_result.action.value, "continue")
        self.module._MANAGER_DISPATCH_GATE.clear("matrix:room-1")


if __name__ == "__main__":
    unittest.main(verbosity=2)
