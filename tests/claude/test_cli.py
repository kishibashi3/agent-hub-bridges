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


def test_cli_add_dir_single(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """``--add-dir`` 1 件が config.add_dirs に届く。"""
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(claude_cli, "run_worker", fake_run_worker)

    other = tmp_path / "other"
    other.mkdir()

    rc = claude_cli.main(
        [
            "--user", "claude-impl",
            "--workdir", str(tmp_path),
            "--add-dir", str(other),
        ]
    )
    assert rc == 0
    cfg = captured["config"]
    assert len(cfg.add_dirs) == 1
    assert cfg.add_dirs[0] == other.resolve()


def test_cli_add_dir_multiple(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """``--add-dir`` を複数回指定すると全件 config.add_dirs に届く。"""
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(claude_cli, "run_worker", fake_run_worker)

    dir_a = tmp_path / "a"
    dir_b = tmp_path / "b"
    dir_a.mkdir()
    dir_b.mkdir()

    rc = claude_cli.main(
        [
            "--user", "claude-impl",
            "--workdir", str(tmp_path),
            "--add-dir", str(dir_a),
            "--add-dir", str(dir_b),
        ]
    )
    assert rc == 0
    cfg = captured["config"]
    assert len(cfg.add_dirs) == 2
    assert cfg.add_dirs[0] == dir_a.resolve()
    assert cfg.add_dirs[1] == dir_b.resolve()


def test_cli_no_add_dir_gives_empty(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """``--add-dir`` 未指定なら config.add_dirs は空 tuple。"""
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(claude_cli, "run_worker", fake_run_worker)

    rc = claude_cli.main(
        ["--user", "claude-impl", "--workdir", str(tmp_path)]
    )
    assert rc == 0
    assert captured["config"].add_dirs == ()


def test_cli_keyboard_interrupt_returns_130(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    async def fake_run_worker(config: Any) -> None:
        raise KeyboardInterrupt

    monkeypatch.setattr(claude_cli, "run_worker", fake_run_worker)

    rc = claude_cli.main(["--user", "claude-impl", "--workdir", str(tmp_path)])
    assert rc == 130


# --- issue #83: --mode flag ---


def test_cli_mode_default_stateful(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """``--mode`` 未指定なら config.mode == 'stateful' (default)."""
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(claude_cli, "run_worker", fake_run_worker)
    monkeypatch.delenv("AGENT_HUB_MODE", raising=False)

    rc = claude_cli.main(["--user", "claude-impl", "--workdir", str(tmp_path)])
    assert rc == 0
    assert captured["config"].mode == "stateful"


def test_cli_mode_explicit(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """``--mode stateless`` が config.mode に届く."""
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(claude_cli, "run_worker", fake_run_worker)

    rc = claude_cli.main(
        ["--user", "claude-impl", "--workdir", str(tmp_path), "--mode", "stateless"]
    )
    assert rc == 0
    assert captured["config"].mode == "stateless"


def test_cli_mode_invalid_rejected(
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
) -> None:
    """``--mode`` に無効な値を渡すと argparse が exit 2 で拒否する."""
    with pytest.raises(SystemExit) as exc_info:
        claude_cli.main(
            ["--user", "claude-impl", "--workdir", str(tmp_path), "--mode", "invalid"]
        )
    assert exc_info.value.code == 2
