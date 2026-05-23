"""Unit tests for ClaudePCLIEngine.

subprocess は mock して、コマンドライン組み立て・MCP config ファイル・
close 処理を確認する。
"""

from __future__ import annotations

import json
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from agent_hub_bridges.claude_p.engine import (
    ClaudePCLIEngine,
    EngineResult,
    _write_mcp_config,
)

# ---------- helpers ----------


def _make_config(
    tmp_path: Path,
    *,
    user: str = "claude-p-test",
    tenant: str | None = "t1",
    permission_bypass: bool = True,
    model: str | None = None,
) -> MagicMock:
    cfg = MagicMock()
    cfg.user = user
    cfg.tenant = tenant
    cfg.workdir = tmp_path
    cfg.claudep_cli_path = "claude"
    cfg.permission_bypass = permission_bypass
    cfg.model = model
    cfg.agent_hub_url = "http://localhost:3000/mcp"
    cfg.github_pat = "ghp_test"
    return cfg


def _make_engine(tmp_path: Path, **kwargs) -> ClaudePCLIEngine:
    cfg = _make_config(tmp_path, **kwargs)
    mcp_path = tmp_path / "mcp.json"
    mcp_path.write_text("{}")
    return ClaudePCLIEngine(
        config=cfg,
        mcp_config_path=mcp_path,
        cli_path="/usr/bin/claude",
        timeout_s=10.0,
    )


# ---------- MCP config ファイル ----------


class TestWriteMcpConfig:
    def test_file_created(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.chdir(tmp_path)
        cfg = _make_config(tmp_path)
        path = _write_mcp_config(cfg)
        assert path.exists()
        path.unlink()

    def test_file_mode_600(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.chdir(tmp_path)
        cfg = _make_config(tmp_path)
        path = _write_mcp_config(cfg)
        mode = path.stat().st_mode & 0o777
        assert mode == 0o600
        path.unlink()

    def test_contains_agent_hub_url(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.chdir(tmp_path)
        cfg = _make_config(tmp_path)
        path = _write_mcp_config(cfg)
        data = json.loads(path.read_text())
        path.unlink()
        assert data["mcpServers"]["agent-hub"]["url"] == "http://localhost:3000/mcp"

    def test_contains_user_id_header(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.chdir(tmp_path)
        cfg = _make_config(tmp_path, user="myuser")
        path = _write_mcp_config(cfg)
        data = json.loads(path.read_text())
        path.unlink()
        assert data["mcpServers"]["agent-hub"]["headers"]["X-User-Id"] == "myuser"

    def test_contains_tenant_header_when_set(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.chdir(tmp_path)
        cfg = _make_config(tmp_path, tenant="alice")
        path = _write_mcp_config(cfg)
        data = json.loads(path.read_text())
        path.unlink()
        assert data["mcpServers"]["agent-hub"]["headers"]["X-Tenant-Id"] == "alice"

    def test_omits_tenant_header_when_none(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.chdir(tmp_path)
        cfg = _make_config(tmp_path, tenant=None)
        path = _write_mcp_config(cfg)
        data = json.loads(path.read_text())
        path.unlink()
        assert "X-Tenant-Id" not in data["mcpServers"]["agent-hub"]["headers"]

    def test_contains_authorization_header(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.chdir(tmp_path)
        cfg = _make_config(tmp_path)
        path = _write_mcp_config(cfg)
        data = json.loads(path.read_text())
        path.unlink()
        assert "Authorization" in data["mcpServers"]["agent-hub"]["headers"]
        assert "ghp_test" in data["mcpServers"]["agent-hub"]["headers"]["Authorization"]


# ---------- コマンドライン組み立て ----------


class TestBuildCmd:
    def test_cmd_starts_with_claude(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        cmd = engine._build_cmd("hello")
        assert cmd[0] == "/usr/bin/claude"

    def test_cmd_has_print_flag(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        cmd = engine._build_cmd("hello")
        assert "-p" in cmd

    def test_cmd_has_mcp_config(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        cmd = engine._build_cmd("hello")
        assert "--mcp-config" in cmd
        idx = cmd.index("--mcp-config")
        assert cmd[idx + 1] == str(engine._mcp_config_path)

    def test_cmd_has_no_session_persistence(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        cmd = engine._build_cmd("hello")
        assert "--no-session-persistence" in cmd

    def test_cmd_permission_bypass_present_by_default(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, permission_bypass=True)
        cmd = engine._build_cmd("hello")
        assert "--dangerously-skip-permissions" in cmd

    def test_cmd_permission_bypass_absent_when_false(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, permission_bypass=False)
        cmd = engine._build_cmd("hello")
        assert "--dangerously-skip-permissions" not in cmd

    def test_cmd_model_absent_when_none(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, model=None)
        cmd = engine._build_cmd("hello")
        assert "--model" not in cmd

    def test_cmd_model_present_when_set(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, model="claude-sonnet-4-6")
        cmd = engine._build_cmd("hello")
        assert "--model" in cmd
        idx = cmd.index("--model")
        assert cmd[idx + 1] == "claude-sonnet-4-6"

    def test_cmd_prompt_is_last_arg(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        prompt = "do something"
        cmd = engine._build_cmd(prompt)
        assert cmd[-1] == prompt


# ---------- engine.close() ----------


class TestClose:
    def test_close_removes_mcp_config(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        assert engine._mcp_config_path.exists()
        engine.close()
        assert not engine._mcp_config_path.exists()

    def test_close_is_idempotent(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        engine.close()
        engine.close()  # should not raise


# ---------- run() subprocess mock ----------


class TestRun:
    @pytest.mark.asyncio
    async def test_run_returns_engine_result(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)

        mock_proc = AsyncMock()
        mock_proc.returncode = 0
        mock_proc.communicate = AsyncMock(return_value=(b"response", b""))

        with patch("asyncio.create_subprocess_exec", return_value=mock_proc):
            result = await engine.run(peer="@alice", prompt="hello")

        assert isinstance(result, EngineResult)
        assert result.returncode == 0
        assert "response" in result.stdout

    @pytest.mark.asyncio
    async def test_run_nonzero_returncode(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)

        mock_proc = AsyncMock()
        mock_proc.returncode = 1
        mock_proc.communicate = AsyncMock(return_value=(b"", b"error"))

        with patch("asyncio.create_subprocess_exec", return_value=mock_proc):
            result = await engine.run(peer="@alice", prompt="hello")

        assert result.returncode == 1
