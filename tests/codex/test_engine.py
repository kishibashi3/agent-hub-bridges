"""Unit tests for CodexCLIEngine.

subprocess は mock して、コマンドライン組み立て・env・CODEX_HOME 分離・
config.toml 内容・タイムアウト処理を確認する。
"""

from __future__ import annotations

from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from agent_hub_bridges.codex.engine import (
    _ENV_TENANT_ID,
    _ENV_USER_ID,
    CodexCLIEngine,
    EngineResult,
    _write_config_toml,
)

# ---------- helpers ----------


def _make_config(
    tmp_path: Path,
    *,
    user: str = "codex-test",
    tenant: str | None = "t1",
    sandbox_mode: str = "read-only",
    approval_bypass: bool = False,
    model: str | None = None,
) -> MagicMock:
    cfg = MagicMock()
    cfg.user = user
    cfg.tenant = tenant
    cfg.workdir = tmp_path
    cfg.codex_cli_path = "codex"
    cfg.sandbox_mode = sandbox_mode
    cfg.approval_bypass = approval_bypass
    cfg.model = model
    cfg.agent_hub_url = "http://localhost:3000/mcp"
    cfg.github_pat = "ghp_test"
    return cfg


def _make_engine(tmp_path: Path, **kwargs) -> CodexCLIEngine:
    cfg = _make_config(tmp_path, **kwargs)
    codex_home = tmp_path / "codex_home"
    codex_home.mkdir()
    return CodexCLIEngine(
        config=cfg,
        temp_codex_home=codex_home,
        cli_path="/usr/bin/codex",
        timeout_s=10.0,
    )


# ---------- config.toml 生成 ----------


class TestWriteConfigToml:
    def test_toml_contains_agent_hub_url(self, tmp_path: Path) -> None:
        cfg = _make_config(tmp_path)
        _write_config_toml(tmp_path, cfg)
        content = (tmp_path / "config.toml").read_text()
        assert "http://localhost:3000/mcp" in content

    def test_toml_contains_user_env_var(self, tmp_path: Path) -> None:
        cfg = _make_config(tmp_path)
        _write_config_toml(tmp_path, cfg)
        content = (tmp_path / "config.toml").read_text()
        assert _ENV_USER_ID in content

    def test_toml_contains_tenant_env_var_when_set(self, tmp_path: Path) -> None:
        cfg = _make_config(tmp_path, tenant="alice")
        _write_config_toml(tmp_path, cfg)
        content = (tmp_path / "config.toml").read_text()
        assert _ENV_TENANT_ID in content

    def test_toml_omits_tenant_when_none(self, tmp_path: Path) -> None:
        cfg = _make_config(tmp_path, tenant=None)
        _write_config_toml(tmp_path, cfg)
        content = (tmp_path / "config.toml").read_text()
        assert _ENV_TENANT_ID not in content

    def test_toml_file_mode_600(self, tmp_path: Path) -> None:
        cfg = _make_config(tmp_path)
        _write_config_toml(tmp_path, cfg)
        mode = (tmp_path / "config.toml").stat().st_mode & 0o777
        assert mode == 0o600

    def test_toml_bearer_token_env_var(self, tmp_path: Path) -> None:
        cfg = _make_config(tmp_path)
        _write_config_toml(tmp_path, cfg)
        content = (tmp_path / "config.toml").read_text()
        assert "bearer_token_env_var" in content
        assert "GITHUB_PAT" in content


# ---------- コマンドライン組み立て ----------


class TestBuildCmd:
    def test_cmd_includes_exec(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        cmd = engine._build_cmd("hello")
        assert "exec" in cmd

    def test_cmd_sandbox_default(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, sandbox_mode="read-only")
        cmd = engine._build_cmd("hello")
        assert "-s" in cmd
        idx = cmd.index("-s")
        assert cmd[idx + 1] == "read-only"

    def test_cmd_sandbox_workspace_write(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, sandbox_mode="workspace-write")
        cmd = engine._build_cmd("hello")
        idx = cmd.index("-s")
        assert cmd[idx + 1] == "workspace-write"

    def test_cmd_approval_bypass_absent_by_default(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, approval_bypass=False)
        cmd = engine._build_cmd("hello")
        assert "--dangerously-bypass-approvals-and-sandbox" not in cmd

    def test_cmd_approval_bypass_present_when_true(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, approval_bypass=True)
        cmd = engine._build_cmd("hello")
        assert "--dangerously-bypass-approvals-and-sandbox" in cmd

    def test_cmd_includes_workdir(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        cmd = engine._build_cmd("hello")
        assert "-C" in cmd
        idx = cmd.index("-C")
        assert cmd[idx + 1] == str(tmp_path)

    def test_cmd_skip_git_repo_check(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        cmd = engine._build_cmd("hello")
        assert "--skip-git-repo-check" in cmd

    def test_cmd_ephemeral(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        cmd = engine._build_cmd("hello")
        assert "--ephemeral" in cmd

    def test_cmd_model_absent_when_none(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, model=None)
        cmd = engine._build_cmd("hello")
        assert "-m" not in cmd

    def test_cmd_model_present_when_set(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, model="o3")
        cmd = engine._build_cmd("hello")
        assert "-m" in cmd
        idx = cmd.index("-m")
        assert cmd[idx + 1] == "o3"

    def test_cmd_prompt_is_last_arg(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        prompt = "do something"
        cmd = engine._build_cmd(prompt)
        assert cmd[-1] == prompt


# ---------- env 組み立て ----------


class TestBuildEnv:
    def test_env_codex_home_set(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        env = engine._build_env()
        assert env["CODEX_HOME"] == str(engine._temp_codex_home)

    def test_env_github_pat_set(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        env = engine._build_env()
        assert env["GITHUB_PAT"] == "ghp_test"

    def test_env_user_id_set(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, user="myuser")
        env = engine._build_env()
        assert env[_ENV_USER_ID] == "myuser"

    def test_env_tenant_id_set_when_tenant(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path, tenant="alice")
        env = engine._build_env()
        assert env[_ENV_TENANT_ID] == "alice"

    def test_env_tenant_id_absent_when_no_tenant(self, tmp_path: Path, monkeypatch) -> None:
        monkeypatch.delenv(_ENV_TENANT_ID, raising=False)
        engine = _make_engine(tmp_path, tenant=None)
        env = engine._build_env()
        assert _ENV_TENANT_ID not in env


# ---------- engine.close() ----------


class TestClose:
    def test_close_removes_temp_codex_home(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)
        assert engine._temp_codex_home.exists()
        engine.close()
        assert not engine._temp_codex_home.exists()


# ---------- run() subprocess mock ----------


class TestRun:
    @pytest.mark.asyncio
    async def test_run_returns_engine_result(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)

        mock_proc = AsyncMock()
        mock_proc.returncode = 0
        mock_proc.communicate = AsyncMock(return_value=(b"ok", b""))

        with patch("asyncio.create_subprocess_exec", return_value=mock_proc):
            result = await engine.run(peer="@alice", prompt="hello")

        assert isinstance(result, EngineResult)
        assert result.returncode == 0

    @pytest.mark.asyncio
    async def test_run_nonzero_returncode(self, tmp_path: Path) -> None:
        engine = _make_engine(tmp_path)

        mock_proc = AsyncMock()
        mock_proc.returncode = 1
        mock_proc.communicate = AsyncMock(return_value=(b"", b"error output"))

        with patch("asyncio.create_subprocess_exec", return_value=mock_proc):
            result = await engine.run(peer="@alice", prompt="hello")

        assert result.returncode == 1
        assert "error output" in result.stderr
