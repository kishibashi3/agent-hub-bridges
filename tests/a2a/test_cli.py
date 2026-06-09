"""CLI surface tests for `agent_hub_bridges.a2a.cli` (= parity with slack)."""

from __future__ import annotations

from typing import Any

import pytest

from agent_hub_bridges import __version__
from agent_hub_bridges.a2a import cli as a2a_cli


@pytest.fixture
def _full_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.setenv("A2A_AGENT_URL", "https://a2a.example.com")
    monkeypatch.delenv("A2A_AGENT_CARD_PATH", raising=False)
    monkeypatch.delenv("AGENT_HUB_PARTICIPANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)


def test_cli_version(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit) as exc_info:
        a2a_cli.main(["--version"])
    assert exc_info.value.code == 0
    out = capsys.readouterr().out
    assert __version__ in out
    assert "agent-hub-bridge-a2a" in out


def test_cli_user_defaults_to_a2a_agent(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(a2a_cli, "run_worker", fake_run_worker)
    rc = a2a_cli.main([])
    assert rc == 0
    assert captured["config"].user == "a2a-agent"


def test_cli_user_from_env(monkeypatch: pytest.MonkeyPatch, _full_env: None) -> None:
    monkeypatch.setenv("AGENT_HUB_PARTICIPANT", "external-agent")
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(a2a_cli, "run_worker", fake_run_worker)
    rc = a2a_cli.main([])
    assert rc == 0
    assert captured["config"].user == "external-agent"


def test_cli_user_cli_overrides_env(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    monkeypatch.setenv("AGENT_HUB_PARTICIPANT", "from-env")
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(a2a_cli, "run_worker", fake_run_worker)
    rc = a2a_cli.main(["--participant", "from-cli"])
    assert rc == 0
    assert captured["config"].user == "from-cli"


def test_cli_missing_a2a_url_returns_2(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://h")
    monkeypatch.setenv("GITHUB_PAT", "g")
    monkeypatch.delenv("A2A_AGENT_URL", raising=False)
    rc = a2a_cli.main([])
    assert rc == 2
    err = capsys.readouterr().err
    assert "A2A_AGENT_URL" in err


def test_cli_missing_agent_hub_url_returns_2(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.delenv("AGENT_HUB_URL", raising=False)
    monkeypatch.setenv("GITHUB_PAT", "g")
    monkeypatch.setenv("A2A_AGENT_URL", "https://a2a")
    rc = a2a_cli.main([])
    assert rc == 2
    err = capsys.readouterr().err
    assert "AGENT_HUB_URL" in err


def test_cli_keyboard_interrupt_returns_130(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    async def fake_run_worker(config: Any) -> None:
        raise KeyboardInterrupt

    monkeypatch.setattr(a2a_cli, "run_worker", fake_run_worker)
    rc = a2a_cli.main([])
    assert rc == 130
