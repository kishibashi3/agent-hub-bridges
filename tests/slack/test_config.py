"""Unit tests for `agent_hub_bridges.slack.config`.

slack 固有の field (`slack_bot_token` / `slack_app_token` /
`slack_default_channel`) + base の `workdir` を None で 通す挙動を 押さえる。
旧 repo の `Config` には 該当 test が無かったので 新規追加 (= bridge-claude
の `test_config.py` と 同 pattern)。
"""

from __future__ import annotations

import pytest

from agent_hub_bridges.slack.config import Config


@pytest.fixture
def _full_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.setenv("SLACK_BOT_TOKEN", "xoxb-test")
    monkeypatch.setenv("SLACK_APP_TOKEN", "xapp-test")
    monkeypatch.delenv("SLACK_DEFAULT_CHANNEL", raising=False)
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_WORKDIR", raising=False)


def test_config_happy_path_minimal(_full_env: None) -> None:
    cfg = Config.from_env_and_args(user="slack-bot", display_name=None, tenant=None)
    assert cfg.user == "slack-bot"
    assert cfg.slack_bot_token == "xoxb-test"
    assert cfg.slack_app_token == "xapp-test"
    assert cfg.slack_default_channel is None
    assert cfg.workdir is None  # slack bridge は使わない
    assert cfg.display_name is None
    assert cfg.tenant is None


def test_config_with_default_channel(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    monkeypatch.setenv("SLACK_DEFAULT_CHANNEL", "C0123ABCDEF")
    cfg = Config.from_env_and_args(user="slack-bot", display_name=None, tenant=None)
    assert cfg.slack_default_channel == "C0123ABCDEF"


def test_config_missing_slack_bot_token(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    monkeypatch.delenv("SLACK_BOT_TOKEN", raising=False)
    with pytest.raises(ValueError, match="SLACK_BOT_TOKEN"):
        Config.from_env_and_args(user="slack-bot", display_name=None, tenant=None)


def test_config_missing_slack_app_token(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    monkeypatch.delenv("SLACK_APP_TOKEN", raising=False)
    with pytest.raises(ValueError, match="SLACK_APP_TOKEN"):
        Config.from_env_and_args(user="slack-bot", display_name=None, tenant=None)


def test_config_missing_agent_hub_url(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    monkeypatch.delenv("AGENT_HUB_URL", raising=False)
    with pytest.raises(ValueError, match="AGENT_HUB_URL"):
        Config.from_env_and_args(user="slack-bot", display_name=None, tenant=None)


def test_config_missing_github_pat(
    monkeypatch: pytest.MonkeyPatch, _full_env: None
) -> None:
    monkeypatch.delenv("GITHUB_PAT", raising=False)
    with pytest.raises(ValueError, match="GITHUB_PAT"):
        Config.from_env_and_args(user="slack-bot", display_name=None, tenant=None)


# NOTE: `AGENT_HUB_WORKDIR` env を 設定した状態で from_env_and_args を呼ぶと、
# base loader 経由で workdir が解決される。 slack bridge コード自体は workdir
# field を 一切参照しないので、 これは 「carry-along」 状態であり 機能的に
# 問題は無い。 「slack は env workdir を 拒否する」 までは契約していない。
# cli.py 側 で `--workdir` を 受理しつつ Config には渡さないので、 通常運用
# (= cli 経由) の経路では workdir=None 固定。


def test_config_is_frozen(_full_env: None) -> None:
    cfg = Config.from_env_and_args(user="slack-bot", display_name=None, tenant=None)
    with pytest.raises((AttributeError, Exception)):
        cfg.user = "evil"  # type: ignore[misc]
