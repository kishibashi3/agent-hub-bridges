"""M0 smoke tests for the shared `_common` helpers.

これらは「 import が壊れていない」「 関数が 期待形 を 返す」 を 確認するだけの
最低限の test。 各 bridge の実装が 入る M1 以降で 必要な追加 test (Config の
env 解決順、 reconnect の retry 動作、 prompt の文面安定性 等) は それぞれ
追加していく。
"""

from __future__ import annotations

import os

import pytest
from agent_hub_sdk import IncomingMessage

import agent_hub_bridges
from agent_hub_bridges._common import format_peer_message_prompt, summarize_exc
from agent_hub_bridges._common.base_config import load_base_config


def test_version_string() -> None:
    assert isinstance(agent_hub_bridges.__version__, str)
    assert agent_hub_bridges.__version__  # non-empty


def test_summarize_exc_plain() -> None:
    assert summarize_exc(ValueError("boom")) == "boom"


def test_summarize_exc_group() -> None:
    group = BaseExceptionGroup(
        "two failures",
        [ValueError("a"), RuntimeError("b")],
    )
    summary = summarize_exc(group)
    assert summary.startswith("[")
    assert summary.endswith("]")
    assert "ValueError: a" in summary
    assert "RuntimeError: b" in summary


def _make_msg(*, sender: str, to: str, body: str) -> IncomingMessage:
    """`agent_hub_sdk.IncomingMessage` を test 用に組み立てる helper.

    M3 で `_IncomingMessageLike` Protocol を削除した結果、 prompt formatter
    は SDK の dataclass を直接受ける。 test も `IncomingMessage` を直接
    インスタンス化する。
    """
    return IncomingMessage(
        id="test-id", sender=sender, to=to, body=body, timestamp="2026-05-19T22:00:00Z"
    )


def test_format_peer_message_prompt_minimal() -> None:
    msg = _make_msg(sender="@alice", to="@bob", body="hello")
    out = format_peer_message_prompt(msg)
    assert "@alice" in out
    assert "@bob" in out
    assert "hello" in out
    assert "mcp__agent-hub__send_message" in out


def test_format_peer_message_prompt_with_self_handle() -> None:
    msg = _make_msg(sender="@alice", to="@team", body="hi all")
    out = format_peer_message_prompt(msg, self_handle="@me")
    assert out.startswith("あなたは agent-hub の peer worker `@me`")


def test_format_peer_message_prompt_reply_target_override() -> None:
    msg = _make_msg(sender="@alice", to="@team", body="x")
    out = format_peer_message_prompt(msg, reply_target="@team")
    # reply target が 明示上書きされて使われていること
    assert "send_message` で @team へ" in out


def test_load_base_config_missing_required(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("AGENT_HUB_URL", raising=False)
    monkeypatch.delenv("GITHUB_PAT", raising=False)
    with pytest.raises(ValueError, match="AGENT_HUB_URL"):
        load_base_config(user="alice")


def test_load_base_config_happy_path(
    monkeypatch: pytest.MonkeyPatch, tmp_path: os.PathLike[str]
) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_WORKDIR", raising=False)

    cfg = load_base_config(user="alice", workdir=str(tmp_path))
    assert cfg.user == "alice"
    assert cfg.agent_hub_url == "http://localhost:3000/mcp"
    assert cfg.github_pat == "ghp_test"
    assert cfg.workdir is not None
    assert cfg.workdir.is_dir()
    assert cfg.display_name is None
    assert cfg.tenant is None
