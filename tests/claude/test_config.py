"""Unit tests for `agent_hub_bridges.claude.config`.

claude 固有の field (`anthropic_api_key`) + base の `workdir: Optional[Path]`
を required に絞り直した部分の挙動を 押さえる。
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from agent_hub_bridges.claude.config import Config


@pytest.fixture
def _hub_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_WORKDIR", raising=False)


def test_config_happy_path(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-xxx")
    cfg = Config.from_env_and_args(
        user="claude-impl",
        display_name="Claude Implementer",
        tenant="alice",
        workdir=str(tmp_path),
    )
    assert cfg.user == "claude-impl"
    assert cfg.display_name == "Claude Implementer"
    assert cfg.tenant == "alice"
    assert cfg.agent_hub_url == "http://localhost:3000/mcp"
    assert cfg.github_pat == "ghp_test"
    assert cfg.anthropic_api_key == "sk-ant-xxx"
    assert cfg.workdir == tmp_path.resolve()


def test_config_anthropic_key_optional(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """`ANTHROPIC_API_KEY` 未設定でも 起動可 (= CLI auth fallback 想定)."""
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    cfg = Config.from_env_and_args(
        user="claude-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.anthropic_api_key is None


def test_config_workdir_defaults_to_cwd(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """`workdir=None` で `os.getcwd()` を 使う (= claude では required field)."""
    monkeypatch.chdir(tmp_path)
    cfg = Config.from_env_and_args(
        user="claude-impl", display_name=None, tenant=None, workdir=None
    )
    assert cfg.workdir == tmp_path.resolve()


def test_config_missing_required_env(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.delenv("AGENT_HUB_URL", raising=False)
    monkeypatch.delenv("GITHUB_PAT", raising=False)
    with pytest.raises(ValueError, match="AGENT_HUB_URL"):
        Config.from_env_and_args(
            user="claude-impl", display_name=None, tenant=None, workdir=str(tmp_path)
        )


def test_config_bad_workdir(monkeypatch: pytest.MonkeyPatch, _hub_env: None) -> None:
    with pytest.raises(ValueError, match="workdir does not exist"):
        Config.from_env_and_args(
            user="claude-impl",
            display_name=None,
            tenant=None,
            workdir="/nonexistent/path/that/does/not/exist",
        )


def test_config_is_frozen(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """`@dataclass(frozen=True)` で 不変、 misuse 防止."""
    cfg = Config.from_env_and_args(
        user="claude-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    with pytest.raises((AttributeError, Exception)):
        cfg.user = "evil"  # type: ignore[misc]


# silence unused import warning in some IDEs
_ = os
