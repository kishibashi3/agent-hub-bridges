"""CLI surface tests for `agent_hub_bridges.slack.cli`.

`run_worker` は monkeypatch で stub 化し、 `main()` の argparse 挙動と
exit code、 `--user` の default 解決順 (= claude と違って optional) を 確認。
"""

from __future__ import annotations

from typing import Any

import pytest

from agent_hub_bridges import __version__
from agent_hub_bridges.slack import cli as slack_cli


@pytest.fixture
def _full_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.setenv("SLACK_BOT_TOKEN", "xoxb-test")
    monkeypatch.setenv("SLACK_APP_TOKEN", "xapp-test")
    monkeypatch.delenv("SLACK_DEFAULT_CHANNEL", raising=False)
    monkeypatch.delenv("AGENT_HUB_USER", raising=False)
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)


def test_cli_version(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit) as exc_info:
        slack_cli.main(["--version"])
    assert exc_info.value.code == 0
    out = capsys.readouterr().out
    assert __version__ in out
    assert "agent-hub-bridge-slack" in out


def test_cli_user_defaults_to_slack_bot(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    """`--user` も env も 無ければ 'slack-bot' に倒れる (= 旧 repo 同等)."""
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(slack_cli, "run_worker", fake_run_worker)
    rc = slack_cli.main([])
    assert rc == 0
    assert captured["config"].user == "slack-bot"


def test_cli_user_from_env(monkeypatch: pytest.MonkeyPatch, _full_env: None) -> None:
    """`--user` 未指定でも env `AGENT_HUB_USER` があれば そちらを使う."""
    monkeypatch.setenv("AGENT_HUB_USER", "from-env")
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(slack_cli, "run_worker", fake_run_worker)
    rc = slack_cli.main([])
    assert rc == 0
    assert captured["config"].user == "from-env"


def test_cli_user_cli_overrides_env(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    monkeypatch.setenv("AGENT_HUB_USER", "from-env")
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(slack_cli, "run_worker", fake_run_worker)
    rc = slack_cli.main(["--user", "from-cli"])
    assert rc == 0
    assert captured["config"].user == "from-cli"


def test_cli_missing_env_returns_2(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.delenv("AGENT_HUB_URL", raising=False)
    monkeypatch.delenv("GITHUB_PAT", raising=False)
    monkeypatch.setenv("SLACK_BOT_TOKEN", "xoxb")
    monkeypatch.setenv("SLACK_APP_TOKEN", "xapp")
    rc = slack_cli.main(["--user", "slack-bot"])
    assert rc == 2
    err = capsys.readouterr().err
    assert "error:" in err


def test_cli_missing_slack_token_returns_2(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://h")
    monkeypatch.setenv("GITHUB_PAT", "g")
    monkeypatch.delenv("SLACK_BOT_TOKEN", raising=False)
    monkeypatch.delenv("SLACK_APP_TOKEN", raising=False)
    rc = slack_cli.main(["--user", "slack-bot"])
    assert rc == 2
    err = capsys.readouterr().err
    assert "SLACK_BOT_TOKEN" in err


def test_cli_keyboard_interrupt_returns_130(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    async def fake_run_worker(config: Any) -> None:
        raise KeyboardInterrupt

    monkeypatch.setattr(slack_cli, "run_worker", fake_run_worker)
    rc = slack_cli.main([])
    assert rc == 130


def test_cli_workdir_arg_silently_ignored(
    monkeypatch: pytest.MonkeyPatch, _full_env: None, tmp_path: Any
) -> None:
    """`--workdir` は 共通 parser で受理されるが、 slack では 無視される.

    Config 側 が workdir=None 固定なので、 渡しても 影響しない (=
    後方互換: 旧 user が systemd unit 等で workdir 指定していた場合に
    crashしないため)。
    """
    captured: dict[str, Any] = {}

    async def fake_run_worker(config: Any) -> None:
        captured["config"] = config

    monkeypatch.setattr(slack_cli, "run_worker", fake_run_worker)
    rc = slack_cli.main(["--workdir", str(tmp_path)])
    assert rc == 0
    assert captured["config"].workdir is None
