#!/usr/bin/env python3

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from pi_plugin_guard import run


class PiPluginGuardTests(unittest.TestCase):
    def setUp(self) -> None:
        self._temporary_directory = tempfile.TemporaryDirectory()
        self.root = Path(self._temporary_directory.name)
        self.plugin = self.root / "plugins" / "pi" / "upup"
        self.plugin.mkdir(parents=True, exist_ok=True)
        (self.plugin / "extensions").mkdir(exist_ok=True)
        (self.plugin / "src").mkdir(exist_ok=True)
        (self.root / "core").mkdir(exist_ok=True)
        self.write_json(
            self.plugin / "package.json",
            {
                "name": "@example/upup-pi-plugin",
                "version": "0.1.0",
                "license": "MIT",
                "files": ["extensions", "src"],
                "scripts": {"test": "node --test", "typecheck": "tsc --noEmit"},
                "peerDependencies": {"@earendil-works/pi-coding-agent": "0.85.x"},
                "pi": {"extensions": ["./extensions/index.ts"]},
            },
        )
        self.write_json(
            self.plugin / "pient-plugin.json",
            {
                "schemaVersion": 1,
                "id": "upup",
                "package": "npm:@example/upup-pi-plugin@0.1.0",
                "runtime": {"pient": ">=0.2.0", "pi": "0.85.x"},
                "capabilities": ["investment.research"],
                "permissions": {
                    "network": ["api.example.com"],
                    "secrets": ["UPUP_API_TOKEN"],
                    "filesystem": "plugin-sandbox-only",
                },
                "process": {"daemon": False, "background": "pi-turn-only"},
                "install": {"mode": "user-initiated", "bundled": False},
            },
        )
        (self.plugin / "LICENSE").write_text("Test license\n", encoding="utf-8")
        (self.plugin / "extensions" / "index.ts").write_text(
            "export const extension = true;\n", encoding="utf-8"
        )
        (self.plugin / "src" / "value.ts").write_text(
            "export const value = 1;\n", encoding="utf-8"
        )

    def tearDown(self) -> None:
        self._temporary_directory.cleanup()

    @staticmethod
    def write_json(path: Path, value: object) -> None:
        path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")

    def assertFails(self, fragment: str) -> None:
        errors = run(self.root)
        self.assertTrue(any(fragment in error for error in errors), errors)

    def test_accepts_declared_mobile_safe_plugin(self) -> None:
        self.assertEqual(run(self.root), [])

    def test_rejects_floating_manifest_package(self) -> None:
        path = self.plugin / "pient-plugin.json"
        manifest = json.loads(path.read_text(encoding="utf-8"))
        manifest["package"] = "npm:@example/upup-pi-plugin@latest"
        self.write_json(path, manifest)
        self.assertFails("package identity must match package.json exactly")

    def test_rejects_undeclared_https_host(self) -> None:
        (self.plugin / "src" / "value.ts").write_text(
            'export const endpoint = "https://undeclared.example.com/v1";\n',
            encoding="utf-8",
        )
        self.assertFails("is not declared in pient-plugin.json")

    def test_rejects_control_plane_import(self) -> None:
        (self.root / "core" / "control.ts").write_text(
            'import "../plugins/pi/upup/src/value.js";\n', encoding="utf-8"
        )
        self.assertFails("control-plane code may not import Pi plugin implementation code")

    def test_rejects_daemon_module(self) -> None:
        (self.plugin / "src" / "value.ts").write_text(
            'import { spawn } from "node:child_process";\n', encoding="utf-8"
        )
        self.assertFails("forbidden process module node:child_process")

    def test_rejects_incomplete_plugin_declaration(self) -> None:
        (self.plugin / "pient-plugin.json").unlink()
        self.assertFails("missing pient-plugin.json")


if __name__ == "__main__":
    unittest.main()
