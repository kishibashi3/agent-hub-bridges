"""Unit tests for `agent_hub_bridges.codex.config` (bridge-codex)."""

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


def test_config_defaults(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """bridge-codex デフォルト: sandbox=danger-full-access, approval_bypass=True."""
    cfg = Config.from_env_and_args(
        user="bridge-codex",
        display_name="Bridge Codex",
        tenant="alice",
        workdir=str(tmp_path),
    )
    assert cfg.user == "bridge-codex"
    assert cfg.workdir == tmp_path.resolve()
    assert cfg.sandbox_mode == "danger-full-access"
    assert cfg.approval_bypass is True
    assert cfg.model is None


def test_default_sandbox_mode_constant() -> None:
    assert DEFAULT_SANDBOX_MODE == "danger-full-access"


def test_sandbox_mode_env_override(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    monkeypatch.setenv("CODEX_SANDBOX_MODE", "workspace-write")
    cfg = Config.from_env_and_args(
        user="bridge-codex", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.sandbox_mode == "workspace-write"


def test_approval_bypass_unset_defaults_true(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """CODEX_APPROVAL_BYPASS 未設定 → True (デーモンデフォルト)。"""
    cfg = Config.from_env_and_args(
        user="bridge-codex", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.approval_bypass is True


def test_approval_bypass_env_empty_gives_false(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """CODEX_APPROVAL_BYPASS="" → False (明示的無効化)。"""
    monkeypatch.setenv("CODEX_APPROVAL_BYPASS", "")
    cfg = Config.from_env_and_args(
        user="bridge-codex", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.approval_bypass is False


def test_approval_bypass_env_non_empty_gives_true(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    monkeypatch.setenv("CODEX_APPROVAL_BYPASS", "1")
    cfg = Config.from_env_and_args(
        user="bridge-codex", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.approval_bypass is True


def test_invalid_sandbox_mode_raises(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    with pytest.raises(ValueError, match="Invalid sandbox_mode"):
        Config.from_env_and_args(
            user="bridge-codex",
            display_name=None,
            tenant=None,
            workdir=str(tmp_path),
            sandbox_mode="ultra-danger",
        )


def test_config_is_frozen(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    cfg = Config.from_env_and_args(
        user="bridge-codex", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    with pytest.raises(AttributeError):
        cfg.user = "evil"  # type: ignore[misc]
