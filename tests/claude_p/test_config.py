"""Unit tests for `agent_hub_bridges.claude_p.config`."""

from __future__ import annotations

from pathlib import Path

import pytest

from agent_hub_bridges.claude_p.config import Config


@pytest.fixture
def _hub_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_WORKDIR", raising=False)
    monkeypatch.delenv("AGENT_HUB_MODEL", raising=False)
    monkeypatch.delenv("CLAUDE_CLI_PATH", raising=False)


def test_config_happy_path(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    cfg = Config.from_env_and_args(
        user="claude-p-impl",
        display_name="Claude-P",
        tenant="alice",
        workdir=str(tmp_path),
    )
    assert cfg.user == "claude-p-impl"
    assert cfg.workdir == tmp_path.resolve()
    assert cfg.permission_bypass is True  # default
    assert cfg.model is None


def test_permission_bypass_default_true(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """permission_bypass デフォルト True (daemon 用途)."""
    cfg = Config.from_env_and_args(
        user="claude-p-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.permission_bypass is True


def test_permission_bypass_false_when_specified(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    cfg = Config.from_env_and_args(
        user="claude-p-impl",
        display_name=None,
        tenant=None,
        workdir=str(tmp_path),
        permission_bypass=False,
    )
    assert cfg.permission_bypass is False


def test_model_env_override(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    monkeypatch.setenv("AGENT_HUB_MODEL", "claude-opus-4-7")
    cfg = Config.from_env_and_args(
        user="claude-p-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.model == "claude-opus-4-7"


def test_workdir_defaults_to_cwd(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    monkeypatch.chdir(tmp_path)
    cfg = Config.from_env_and_args(
        user="claude-p-impl", display_name=None, tenant=None, workdir=None
    )
    assert cfg.workdir == tmp_path.resolve()


def test_config_is_frozen(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    cfg = Config.from_env_and_args(
        user="claude-p-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    with pytest.raises((AttributeError, Exception)):
        cfg.user = "evil"  # type: ignore[misc]
