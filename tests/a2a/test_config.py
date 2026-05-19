"""Unit tests for `agent_hub_bridges.a2a.config` (= parity with claude/slack/gemini)."""

from __future__ import annotations

import pytest

from agent_hub_bridges.a2a.config import DEFAULT_AGENT_CARD_PATH, Config


@pytest.fixture
def _full_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.setenv("A2A_AGENT_URL", "https://a2a.example.com")
    monkeypatch.delenv("A2A_AGENT_CARD_PATH", raising=False)
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_WORKDIR", raising=False)


def test_config_happy_path(_full_env: None) -> None:
    cfg = Config.from_env_and_args(user="a2a-agent", display_name=None, tenant=None)
    assert cfg.user == "a2a-agent"
    assert cfg.agent_hub_url == "http://localhost:3000/mcp"
    assert cfg.github_pat == "ghp_test"
    assert cfg.a2a_agent_url == "https://a2a.example.com"
    assert cfg.a2a_agent_card_path == DEFAULT_AGENT_CARD_PATH
    assert cfg.workdir is None
    assert cfg.display_name is None
    assert cfg.tenant is None


def test_config_card_path_override(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    monkeypatch.setenv("A2A_AGENT_CARD_PATH", "/custom/card.json")
    cfg = Config.from_env_and_args(user="a2a-agent", display_name=None, tenant=None)
    assert cfg.a2a_agent_card_path == "/custom/card.json"


def test_config_missing_a2a_url(monkeypatch: pytest.MonkeyPatch, _full_env: None) -> None:
    monkeypatch.delenv("A2A_AGENT_URL", raising=False)
    with pytest.raises(ValueError, match="A2A_AGENT_URL"):
        Config.from_env_and_args(user="a2a-agent", display_name=None, tenant=None)


def test_config_missing_agent_hub_url(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    monkeypatch.delenv("AGENT_HUB_URL", raising=False)
    with pytest.raises(ValueError, match="AGENT_HUB_URL"):
        Config.from_env_and_args(user="a2a-agent", display_name=None, tenant=None)


def test_config_missing_github_pat(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    monkeypatch.delenv("GITHUB_PAT", raising=False)
    with pytest.raises(ValueError, match="GITHUB_PAT"):
        Config.from_env_and_args(user="a2a-agent", display_name=None, tenant=None)


def test_config_is_frozen(_full_env: None) -> None:
    cfg = Config.from_env_and_args(user="a2a-agent", display_name=None, tenant=None)
    with pytest.raises((AttributeError, Exception)):
        cfg.user = "evil"  # type: ignore[misc]


def test_config_display_tenant_propagation(_full_env: None) -> None:
    cfg = Config.from_env_and_args(
        user="ext-agent", display_name="External Agent", tenant="alice"
    )
    assert cfg.display_name == "External Agent"
    assert cfg.tenant == "alice"
