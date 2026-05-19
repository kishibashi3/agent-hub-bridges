"""CLI surface tests for `agent_hub_bridges.claude.cli`.

`run_worker` は monkeypatch で stub 化し、 `main()` の argparse 挙動と
exit code のみを確認する。
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from agent_hub_bridges import __version__
from agent_hub_bridges.claude import cli as claude_cli


@pytest.fixture
def _hub_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_WORKDIR", raising=False)


def test_cli_version(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit) as exc_info:
        claude_cli.main(["--version"])
    assert exc_info.value.code == 0
    out = capsys.readouterr().out
    assert __version__ in out
    assert "agent-hub-bridge-claude" in out


def test_cli_requires_user(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit) as exc_info:
        claude_cli.main([])
    # argparse の missing required は exit code 2
    assert exc_info.value.code == 2
    err = capsys.readouterr().err
    assert "--user" in err


def test_cli_missing_env_returns_2(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str], tmp_path: Path
) -> None:
    monkeypatch.delenv("AGENT_HUB_URL", raising=False)
    monkeypatch.delenv("GITHUB_PAT", raising=False)
    rc = claude_cli.main(["--user", "test", "--workdir", str(tmp_path)])
    assert rc == 2
    err = capsys.readouterr().err
    assert "error:" in err


def test_cli_calls_run_worker(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """`run_worker` が 正しい Config で 呼ばれることを stub で 確認."""
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(claude_cli, "run_worker", fake_run_worker)

    rc = claude_cli.main(
        [
            "--user",
            "claude-impl",
            "--display-name",
            "Claude Impl",
            "--tenant",
            "alice",
            "--workdir",
            str(tmp_path),
        ]
    )
    assert rc == 0
    cfg = captured["config"]
    assert cfg.user == "claude-impl"
    assert cfg.display_name == "Claude Impl"
    assert cfg.tenant == "alice"
    assert cfg.workdir == tmp_path.resolve()


def test_cli_keyboard_interrupt_returns_130(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    async def fake_run_worker(config: Any) -> None:
        raise KeyboardInterrupt

    monkeypatch.setattr(claude_cli, "run_worker", fake_run_worker)

    rc = claude_cli.main(["--user", "claude-impl", "--workdir", str(tmp_path)])
    assert rc == 130
