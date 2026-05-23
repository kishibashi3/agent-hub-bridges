"""Unit tests for `agent_hub_bridges.codex.config`."""

from __future__ import annotations

from pathlib import Path

import pytest

from agent_hub_bridges.codex.config import DEFAULT_SANDBOX_MODE, Config


@pytest.fixture
def _hub_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_WORKDIR", raising=False)
    monkeypatch.delenv("AGENT_HUB_MODEL", raising=False)
    monkeypatch.delenv("CODEX_CLI_PATH", raising=False)
    monkeypatch.delenv("CODEX_SANDBOX_MODE", raising=False)
    monkeypatch.delenv("CODEX_APPROVAL_BYPASS", raising=False)


def test_config_happy_path(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    cfg = Config.from_env_and_args(
        user="codex-impl",
        display_name="Codex Implementer",
        tenant="alice",
        workdir=str(tmp_path),
    )
    assert cfg.user == "codex-impl"
    assert cfg.workdir == tmp_path.resolve()
    assert cfg.sandbox_mode == DEFAULT_SANDBOX_MODE
    assert cfg.approval_bypass is False
    assert cfg.model is None


def test_config_workdir_defaults_to_cwd(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    monkeypatch.chdir(tmp_path)
    cfg = Config.from_env_and_args(
        user="codex-impl", display_name=None, tenant=None, workdir=None
    )
    assert cfg.workdir == tmp_path.resolve()


def test_sandbox_mode_env_override(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    monkeypatch.setenv("CODEX_SANDBOX_MODE", "workspace-write")
    cfg = Config.from_env_and_args(
        user="codex-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.sandbox_mode == "workspace-write"


def test_sandbox_mode_cli_overrides_env(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    monkeypatch.setenv("CODEX_SANDBOX_MODE", "workspace-write")
    cfg = Config.from_env_and_args(
        user="codex-impl",
        display_name=None,
        tenant=None,
        workdir=str(tmp_path),
        sandbox_mode="danger-full-access",
    )
    assert cfg.sandbox_mode == "danger-full-access"


def test_invalid_sandbox_mode_raises(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    with pytest.raises(ValueError, match="Invalid sandbox_mode"):
        Config.from_env_and_args(
            user="codex-impl",
            display_name=None,
            tenant=None,
            workdir=str(tmp_path),
            sandbox_mode="ultra-danger",
        )


def test_approval_bypass_env_non_empty(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """CODEX_APPROVAL_BYPASS に non-empty 文字列 → True."""
    monkeypatch.setenv("CODEX_APPROVAL_BYPASS", "1")
    cfg = Config.from_env_and_args(
        user="codex-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.approval_bypass is True


def test_approval_bypass_env_empty(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """CODEX_APPROVAL_BYPASS が空文字 → False."""
    monkeypatch.setenv("CODEX_APPROVAL_BYPASS", "")
    cfg = Config.from_env_and_args(
        user="codex-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.approval_bypass is False


def test_approval_bypass_cli_true(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """CLI approval_bypass=True は env より優先。"""
    monkeypatch.delenv("CODEX_APPROVAL_BYPASS", raising=False)
    cfg = Config.from_env_and_args(
        user="codex-impl",
        display_name=None,
        tenant=None,
        workdir=str(tmp_path),
        approval_bypass=True,
    )
    assert cfg.approval_bypass is True


def test_model_env_override(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    monkeypatch.setenv("AGENT_HUB_MODEL", "o3")
    cfg = Config.from_env_and_args(
        user="codex-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.model == "o3"


def test_config_is_frozen(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    cfg = Config.from_env_and_args(
        user="codex-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    with pytest.raises((AttributeError, Exception)):
        cfg.user = "evil"  # type: ignore[misc]
