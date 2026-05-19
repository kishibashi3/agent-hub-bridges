"""CLI surface tests for `agent_hub_bridges.gemini.cli` (= parity with claude / slack).

`run_worker` は monkeypatch で stub 化し、 `main()` の argparse 挙動と
exit code、 `--user` (required) + `--model` (gemini 固有) の resolution
を 確認する。
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from agent_hub_bridges import __version__
from agent_hub_bridges.gemini import cli as gemini_cli


@pytest.fixture
def _full_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.setenv("GEMINI_API_KEY", "test-gemini-key")
    monkeypatch.delenv("GEMINI_MODEL", raising=False)
    monkeypatch.delenv("GEMINI_CLI_PATH", raising=False)
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_WORKDIR", raising=False)


def test_cli_version(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit) as exc_info:
        gemini_cli.main(["--version"])
    assert exc_info.value.code == 0
    out = capsys.readouterr().out
    assert __version__ in out
    assert "agent-hub-bridge-gemini" in out


def test_cli_requires_user(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit) as exc_info:
        gemini_cli.main([])
    assert exc_info.value.code == 2
    err = capsys.readouterr().err
    assert "--user" in err


def test_cli_missing_env_returns_2(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str], tmp_path: Path
) -> None:
    monkeypatch.delenv("AGENT_HUB_URL", raising=False)
    monkeypatch.delenv("GITHUB_PAT", raising=False)
    monkeypatch.setenv("GEMINI_API_KEY", "k")
    rc = gemini_cli.main(["--user", "g", "--workdir", str(tmp_path)])
    assert rc == 2
    err = capsys.readouterr().err
    assert "error:" in err


def test_cli_missing_gemini_key_returns_2(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str], tmp_path: Path
) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://h")
    monkeypatch.setenv("GITHUB_PAT", "g")
    monkeypatch.delenv("GEMINI_API_KEY", raising=False)
    rc = gemini_cli.main(["--user", "g", "--workdir", str(tmp_path)])
    assert rc == 2
    err = capsys.readouterr().err
    assert "GEMINI_API_KEY" in err


def test_cli_calls_run_worker(
    monkeypatch: pytest.MonkeyPatch, _full_env: None, tmp_path: Path
) -> None:
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(gemini_cli, "run_worker", fake_run_worker)
    rc = gemini_cli.main(
        [
            "--user",
            "gemini-impl",
            "--display-name",
            "Gemini Impl",
            "--tenant",
            "alice",
            "--workdir",
            str(tmp_path),
            "--model",
            "gemini-pro",
        ]
    )
    assert rc == 0
    cfg = captured["config"]
    assert cfg.user == "gemini-impl"
    assert cfg.display_name == "Gemini Impl"
    assert cfg.tenant == "alice"
    assert cfg.workdir == tmp_path.resolve()
    assert cfg.gemini_model == "gemini-pro"


def test_cli_model_defaults_to_env_or_default(
    monkeypatch: pytest.MonkeyPatch, _full_env: None, tmp_path: Path
) -> None:
    monkeypatch.setenv("GEMINI_MODEL", "from-env-model")
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(gemini_cli, "run_worker", fake_run_worker)
    rc = gemini_cli.main(["--user", "g", "--workdir", str(tmp_path)])
    assert rc == 0
    assert captured["config"].gemini_model == "from-env-model"


def test_cli_keyboard_interrupt_returns_130(
    monkeypatch: pytest.MonkeyPatch, _full_env: None, tmp_path: Path
) -> None:
    async def fake_run_worker(config: Any) -> None:
        raise KeyboardInterrupt

    monkeypatch.setattr(gemini_cli, "run_worker", fake_run_worker)
    rc = gemini_cli.main(["--user", "g", "--workdir", str(tmp_path)])
    assert rc == 130
