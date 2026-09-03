"""Tests for opskeeper-teamharness install-plugin HTTP endpoint.

These tests exercise the FastAPI router registered by build_install_plugin_router.
Subprocess calls to `qwenpaw plugin install` are mocked so the suite runs in CI
without the qwenpaw binary. The goal is to lock down the install wiring
behavior on the worker side; opskeeper-side wiring is covered separately by
internal/manager/server/agentteams/plugin_http_test.go and
internal/agentteams/controller_discovery_test.go.
"""
from __future__ import annotations

import io
import sys
import zipfile
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient


PLUGIN_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(PLUGIN_DIR))

import plugin as _plugin  # noqa: E402


@pytest.fixture()
def client() -> TestClient:
    app = FastAPI()
    install_router = _plugin.build_install_plugin_router()
    assert install_router is not None, "fastapi must be importable"
    app.include_router(install_router, prefix="/opskeeper-teamharness")
    return TestClient(app)


def _make_plugin_zip() -> bytes:
    """Build a zip mimicking a qwenpaw plugin package:
    top-level dir + plugin.json + nested skill file.
    """
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr("opskeeper-fake/plugin.json", '{"id": "opskeeper-fake", "version": "1.0.0"}')
        zf.writestr("opskeeper-fake/README.md", "fake")
    return buf.getvalue()


def _make_two_plugin_dirs_zip() -> bytes:
    """Two top-level dirs each with plugin.json — must be rejected."""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr("a/plugin.json", "{}")
        zf.writestr("b/plugin.json", "{}")
    return buf.getvalue()


def _make_zip_slip_zip() -> bytes:
    """Entry that escapes the target dir — must be rejected."""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr("../escape/plugin.json", "{}")
    return buf.getvalue()


def _make_no_plugin_json_zip() -> bytes:
    """Top-level dir without plugin.json — must be rejected."""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr("notaplugin/README.md", "noop")
    return buf.getvalue()


def test_install_health_reports_qwenpaw_path(client: TestClient) -> None:
    res = client.get("/opskeeper-teamharness/install-plugin/health")
    assert res.status_code == 200
    body = res.json()
    assert body["ok"] is True
    assert isinstance(body["qwenpaw"], str) and body["qwenpaw"]
    assert body["maxBytes"] == _plugin._MAX_INSTALL_BYTES


def test_install_max_bytes_env_override(monkeypatch: pytest.MonkeyPatch) -> None:
    """OPSKEEPER_WORKER_MAX_PLUGIN_BYTES env 应能让 worker 端上限随运维调整，
    避免和 manager 端 OPSKEEPER_PLUGIN_MAX_ZIP_BYTES 错配时 manager 收下 / worker 拒收。"""
    import importlib

    custom_bytes = 7 * 1024 * 1024  # 7 MiB
    monkeypatch.setenv("OPSKEEPER_WORKER_MAX_PLUGIN_BYTES", str(custom_bytes))
    reloaded = importlib.reload(_plugin)
    try:
        assert reloaded._MAX_INSTALL_BYTES == custom_bytes
    finally:
        # 还原：避免污染其它测试
        monkeypatch.delenv("OPSKEEPER_WORKER_MAX_PLUGIN_BYTES", raising=False)
        importlib.reload(_plugin)


def test_install_max_bytes_env_invalid_falls_back_to_default(monkeypatch: pytest.MonkeyPatch) -> None:
    """非法 env 值应降级为默认 32 MiB 而不是让 worker 启动失败。"""
    import importlib

    monkeypatch.setenv("OPSKEEPER_WORKER_MAX_PLUGIN_BYTES", "not-a-number")
    reloaded = importlib.reload(_plugin)
    try:
        assert reloaded._MAX_INSTALL_BYTES == reloaded._DEFAULT_MAX_INSTALL_BYTES
    finally:
        monkeypatch.delenv("OPSKEEPER_WORKER_MAX_PLUGIN_BYTES", raising=False)
        importlib.reload(_plugin)


def test_install_rejects_empty_file(client: TestClient) -> None:
    res = client.post(
        "/opskeeper-teamharness/install-plugin",
        files={"file": ("empty.zip", b"", "application/zip")},
    )
    assert res.status_code == 400
    assert "empty" in res.json()["detail"].lower()


def test_install_rejects_oversize(client: TestClient) -> None:
    big = b"x" * (_plugin._MAX_INSTALL_BYTES + 1)
    res = client.post(
        "/opskeeper-teamharness/install-plugin",
        files={"file": ("big.zip", big, "application/zip")},
    )
    assert res.status_code == 413
    assert "exceeds" in res.json()["detail"]


def test_install_rejects_zip_slip(client: TestClient) -> None:
    bad = _make_zip_slip_zip()
    res = client.post(
        "/opskeeper-teamharness/install-plugin",
        files={"file": ("evil.zip", bad, "application/zip")},
    )
    assert res.status_code == 400
    assert "unsafe" in res.json()["detail"].lower()


def test_install_rejects_two_plugin_dirs(client: TestClient) -> None:
    res = client.post(
        "/opskeeper-teamharness/install-plugin",
        files={"file": ("multi.zip", _make_two_plugin_dirs_zip(), "application/zip")},
    )
    assert res.status_code == 400
    assert "exactly one" in res.json()["detail"].lower()


def test_install_rejects_zip_without_plugin_json(client: TestClient) -> None:
    res = client.post(
        "/opskeeper-teamharness/install-plugin",
        files={"file": ("noplugin.zip", _make_no_plugin_json_zip(), "application/zip")},
    )
    assert res.status_code == 400


def test_install_rejects_non_zip_bytes(client: TestClient) -> None:
    # zipfile.BadZipFile is a subclass of OSError in py3, not RuntimeError;
    # ensure the endpoint still rejects with 400 by wrapping it in RuntimeError.
    from zipfile import BadZipFile
    with patch.object(_plugin, "_extract_plugin_zip", side_effect=RuntimeError(str(BadZipFile("not a zip")))):
        res = client.post(
            "/opskeeper-teamharness/install-plugin",
            files={"file": ("notazip.zip", b"plain text not a zip", "application/zip")},
        )
    assert res.status_code == 400


def test_install_success_invokes_qwenpaw_subprocess(client: TestClient) -> None:
    fake = {"exitCode": 0, "stdout": "installed\n", "stderr": ""}
    with patch.object(_plugin, "install_via_subprocess", return_value=fake) as mock_install, \
         patch.object(_plugin, "shutil") as mock_shutil:
        res = client.post(
            "/opskeeper-teamharness/install-plugin",
            files={"file": ("pkg.zip", _make_plugin_zip(), "application/zip")},
        )
    assert res.status_code == 200, res.text
    body = res.json()
    assert body["ok"] is True
    assert body["plugin"] == "pkg.zip"
    assert body["exitCode"] == 0
    mock_install.assert_called_once()
    # cleanup invoked even on success
    mock_shutil.rmtree.assert_called()


def test_install_subprocess_failure_returns_500(client: TestClient) -> None:
    fake = {"exitCode": 1, "stdout": "", "stderr": "qwenpaw install failed: missing manifest"}
    with patch.object(_plugin, "install_via_subprocess", return_value=fake), \
         patch.object(_plugin, "shutil"):
        res = client.post(
            "/opskeeper-teamharness/install-plugin",
            files={"file": ("pkg.zip", _make_plugin_zip(), "application/zip")},
        )
    assert res.status_code == 500
    detail = res.json()["detail"]
    assert detail["exitCode"] == 1
    assert "qwenpaw install failed" in detail["stderr"]


def test_install_extract_failure_returns_400(client: TestClient) -> None:
    with patch.object(_plugin, "_extract_plugin_zip", side_effect=RuntimeError("boom")):
        res = client.post(
            "/opskeeper-teamharness/install-plugin",
            files={"file": ("pkg.zip", _make_plugin_zip(), "application/zip")},
        )
    assert res.status_code == 400
    assert res.json()["detail"] == "boom"


def test_install_via_subprocess_uses_qwenpaw_path(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    fake_bin = tmp_path / "fake-qwenpaw"
    fake_bin.write_text("#!/bin/sh\necho ok\n")
    fake_bin.chmod(0o755)
    monkeypatch.setattr(_plugin.shutil, "which", lambda _name: str(fake_bin))

    completed = MagicMock(returncode=0, stdout="installed\n", stderr="")
    fake_run = MagicMock(return_value=completed)
    monkeypatch.setattr(_plugin.subprocess, "run", fake_run)

    pkg_dir = tmp_path / "pkg"
    pkg_dir.mkdir()
    result = _plugin.install_via_subprocess(pkg_dir)
    assert result["exitCode"] == 0
    fake_run.assert_called_once()
    cmd = fake_run.call_args.args[0]
    assert cmd[0] == str(fake_bin)
    assert cmd[1:4] == ["plugin", "install", str(pkg_dir)]
    assert "--force" in cmd


def test_validate_zip_against_zip_slip_rejects_traversal(tmp_path: Path) -> None:
    zip_path = tmp_path / "evil.zip"
    with zipfile.ZipFile(zip_path, "w") as zf:
        zf.writestr("../escape/plugin.json", "{}")
    with zipfile.ZipFile(zip_path) as zf, pytest.raises(RuntimeError, match="unsafe"):
        _plugin._validate_zip_against_zip_slip(zf, tmp_path)


# =============================================================================
# Docker hardening — exit code 127 (binary missing) → 503, 124 (timeout) → 504
# =============================================================================

def _make_dummy_plugin_zip() -> bytes:
    """Build minimal plugin zip for 503/504 tests (single dir + plugin.json)."""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr("opskeeper-fake/plugin.json", "{}")
    return buf.getvalue()


def test_install_qwenpaw_binary_missing_returns_503(client) -> None:
    """exit 127 = command not found → 503 Service Unavailable."""
    fake = {"exitCode": 127, "stdout": "", "stderr": "qwenpaw binary not found at /usr/bin/qwenpaw"}
    with patch.object(_plugin, "install_via_subprocess", return_value=fake), patch.object(_plugin, "shutil"):
        res = client.post(
            "/opskeeper-teamharness/install-plugin",
            files={"file": ("pkg.zip", _make_dummy_plugin_zip(), "application/zip")},
        )
    assert res.status_code == 503, res.text
    detail = res.json()["detail"]
    assert detail["exitCode"] == 127
    assert "qwenpaw binary" in detail["hint"]


def test_install_qwenpaw_timeout_returns_504(client) -> None:
    """exit 124 = subprocess timeout → 504 Gateway Timeout."""
    fake = {"exitCode": 124, "stdout": "", "stderr": "[timeout after 120s]"}
    with patch.object(_plugin, "install_via_subprocess", return_value=fake), \
         patch.object(_plugin, "shutil"):
        import io, zipfile as _z
        buf = io.BytesIO()
        with _z.ZipFile(buf, "w") as zf:
            zf.writestr("opskeeper-fake/plugin.json", "{}")
        res = client.post(
            "/opskeeper-teamharness/install-plugin",
            files={"file": ("pkg.zip", buf.getvalue(), "application/zip")},
        )
    assert res.status_code == 504, res.text
    detail = res.json()["detail"]
    assert detail["exitCode"] == 124
    assert "timeout" in detail["hint"]


def test_install_via_subprocess_handles_filenotfounderror(tmp_path, monkeypatch) -> None:
    """subprocess.run raises FileNotFoundError when qwenpaw not on PATH.
    install_via_subprocess should catch and return exitCode=127 + structured stderr.
    """
    fake_bin = tmp_path / "missing-qwenpaw"
    monkeypatch.setattr(_plugin.shutil, "which", lambda _name: None)
    monkeypatch.setattr(sys, "executable", str(tmp_path / "no-python"))
    monkeypatch.setattr(_plugin.subprocess, "run",
                        lambda *a, **k: (_ for _ in ()).throw(FileNotFoundError("no such file")))
    pkg_dir = tmp_path / "pkg"
    pkg_dir.mkdir()
    result = _plugin.install_via_subprocess(pkg_dir)
    assert result["exitCode"] == 127
    assert "binary not found" in result["stderr"]
    assert "pip install qwen-cli" in result["stderr"]
