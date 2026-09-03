import asyncio
import importlib.util
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
    def __init__(self, content: list[Any], state: _ToolResultState, metadata: dict[str, Any]):
        self.content = content
        self.state = state
        self.metadata = metadata


class _MiddlewareBase:
    pass


def _install_agentscope_stubs() -> dict[str, Any]:
    agentscope = ModuleType("agentscope")
    middleware = ModuleType("agentscope.middleware")
    message = ModuleType("agentscope.message")
    tool = ModuleType("agentscope.tool")

    middleware.MiddlewareBase = _MiddlewareBase
    message.TextBlock = _TextBlock
    message.ToolResultState = _ToolResultState
    tool.ToolResponse = _ToolResponse
    agentscope.middleware = middleware
    agentscope.message = message
    agentscope.tool = tool

    installed = {
        "agentscope": agentscope,
        "agentscope.middleware": middleware,
        "agentscope.message": message,
        "agentscope.tool": tool,
    }
    saved = {name: sys.modules.get(name) for name in installed}
    sys.modules.update(installed)
    return saved


def _restore_agentscope(saved: dict[str, Any]) -> None:
    for name, module in saved.items():
        if module is None:
            sys.modules.pop(name, None)
        else:
            sys.modules[name] = module


def _load_plugin():
    path = Path(__file__).with_name("plugin.py")
    spec = importlib.util.spec_from_file_location("opskeeper_readonly_plugin_under_test", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("cannot load plugin.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ReadOnlyEnforcementTest(unittest.TestCase):
    def setUp(self):
        self.saved_modules = _install_agentscope_stubs()
        self.module = _load_plugin()

    def tearDown(self):
        _restore_agentscope(self.saved_modules)

    def _invoke(self, tool_name: str, permission_mode: str | None = None):
        middleware = self.module._readonly_enforcement_factory(None, None)
        executed = False

        async def next_handler(**_kwargs):
            nonlocal executed
            executed = True
            yield "allowed"

        environment = {} if permission_mode is None else {
            self.module._PERMISSION_MODE_ENV: permission_mode,
        }
        input_kwargs = {
            "tool_call": SimpleNamespace(name=tool_name, input="{}"),
        }

        async def run():
            events = []
            async for event in middleware.on_acting(
                agent=None,
                input_kwargs=input_kwargs,
                next_handler=next_handler,
            ):
                events.append(event)
            return events

        with patch.dict("os.environ", environment, clear=False):
            events = asyncio.run(run())
        return events, executed

    def test_default_mode_is_read_only(self):
        self.assertEqual(self.module._permission_mode(), "read_only")

    def test_write_file_is_denied_without_execution(self):
        events, executed = self._invoke("write_file")
        self.assertFalse(executed)
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0].state, _ToolResultState.DENIED)
        self.assertIn("write_file", events[0].content[0].text)

    def test_prefixed_mutating_opskeeper_tool_is_denied(self):
        events, executed = self._invoke("opskeeper__state_put")
        self.assertFalse(executed)
        self.assertEqual(events[0].state, _ToolResultState.DENIED)

    def test_shell_and_browser_are_denied(self):
        for tool_name in ("execute_shell_command", "browser_use"):
            with self.subTest(tool_name=tool_name):
                events, executed = self._invoke(tool_name)
                self.assertFalse(executed)
                self.assertEqual(events[0].state, _ToolResultState.DENIED)

    def test_readonly_tools_are_allowed(self):
        for tool_name in (
            "message",
            "teamharness__message",
            "read_file",
            "opskeeper__metric_query",
            "opskeeper__postgres_analyze_status",
        ):
            with self.subTest(tool_name=tool_name):
                events, executed = self._invoke(tool_name)
                self.assertTrue(executed)
                self.assertEqual(events, ["allowed"])

    def test_standard_mode_explicitly_allows_mutation(self):
        events, executed = self._invoke("write_file", permission_mode="standard")
        self.assertTrue(executed)
        self.assertEqual(events, ["allowed"])

    def test_invalid_permission_mode_fails_closed(self):
        events, executed = self._invoke("edit_file", permission_mode="write-anything")
        self.assertFalse(executed)
        self.assertEqual(events[0].state, _ToolResultState.DENIED)

    def test_enforcement_is_registered_outside_sanitizer_and_audit(self):
        registrations = []

        class FakeApi:
            def register_prompt_section(self, *args, **kwargs):
                pass

            def register_skill_provider(self, *args, **kwargs):
                pass

            def register_middleware(self, factory, priority):
                registrations.append((factory, priority))

            def register_runtime_hook(self, *args, **kwargs):
                pass

        with patch.object(self.module, "_load_task_trace_module", return_value=None):
            self.module.plugin.register(FakeApi())
        priorities = {factory: priority for factory, priority in registrations}
        self.assertEqual(
            priorities[self.module._readonly_enforcement_factory],
            10,
        )
        self.assertLess(
            priorities[self.module._readonly_enforcement_factory],
            priorities[self.module._sanitizer_factory],
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
