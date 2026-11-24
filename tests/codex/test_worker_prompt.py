"""Unit tests for bridge-codex worker prompt formatting."""

from __future__ import annotations

from datetime import UTC, datetime
from unittest.mock import MagicMock

from agent_hub_bridges.codex.worker import _format_prompt


def _make_msg(
    sender: str = "@alice",
    body: str = "hello",
    to: str = "@bridge-codex",
    msg_id: str = "test-id",
    timestamp: str = "",
) -> MagicMock:
    msg = MagicMock()
    msg.sender = sender
    msg.body = body
    msg.to = to
    msg.id = msg_id
    msg.timestamp = timestamp
    return msg


def test_format_prompt_no_get_user_history() -> None:
    """issue #79: セッション永続化により get_user_history への依存を廃止."""
    msg = _make_msg()
    prompt = _format_prompt("@bridge-codex", msg)
    assert "get_user_history" not in prompt


def test_format_prompt_contains_send_message() -> None:
    """プロンプトに send_message の呼び出し指示が含まれること。"""
    msg = _make_msg()
    prompt = _format_prompt("@bridge-codex", msg)
    assert "send_message" in prompt


def test_format_prompt_contains_sender() -> None:
    """プロンプトに送信者 handle が含まれること。"""
    msg = _make_msg(sender="@bob")
    prompt = _format_prompt("@bridge-codex", msg)
    assert "@bob" in prompt


def test_format_prompt_contains_message_body() -> None:
    """プロンプトにメッセージ本文が含まれること。"""
    msg = _make_msg(body="今日の天気は?")
    prompt = _format_prompt("@bridge-codex", msg)
    assert "今日の天気は?" in prompt


def test_format_prompt_contains_self_handle() -> None:
    """プロンプトに自分の handle が含まれること。"""
    msg = _make_msg()
    prompt = _format_prompt("@bridge-codex", msg)
    assert "@bridge-codex" in prompt


def test_format_prompt_contains_send_message_instruction() -> None:
    """プロンプトに send_message の呼び出し指示が含まれること。"""
    msg = _make_msg()
    prompt = _format_prompt("@bridge-codex", msg)
    assert "send_message" in prompt


def test_format_prompt_includes_caused_by_instruction() -> None:
    """プロンプトに caused_by 設定指示と受信メッセージ ID が含まれること (issue #80 / #162)."""
    msg = _make_msg(msg_id="bbbbbbbb-0000-0000-0000-000000000001")
    prompt = _format_prompt("@bridge-codex", msg)
    assert "caused_by" in prompt
    assert "bbbbbbbb-0000-0000-0000-000000000001" in prompt


_NOW = datetime(2026, 11, 24, 12, 0, 0, tzinfo=UTC)


def test_format_prompt_sent_at_line_between_to_and_body() -> None:
    """issue #417: 宛先の行と本文の間に送信時刻の行が入ること (#405 と同じ規則)."""
    msg = _make_msg(to="@bridge-codex", body="hello", timestamp="2026-11-22T10:30:00.000Z")
    prompt = _format_prompt("@bridge-codex", msg, now=_NOW)
    assert (
        "宛先: @bridge-codex\n"
        "送信時刻: 2026-11-22T10:30:00.000Z (受信処理の 2 日前)\n"
        "本文:\nhello\n"
    ) in prompt
    assert prompt.count("送信時刻:") == 1


def test_format_prompt_sent_at_line_minutes() -> None:
    """issue #417: 経過時間が分単位のとき「N 分前」になること."""
    msg = _make_msg(timestamp="2026-11-24T11:55:30Z")
    prompt = _format_prompt("@bridge-codex", msg, now=_NOW)
    assert (
        "宛先: @bridge-codex\n送信時刻: 2026-11-24T11:55:30Z (受信処理の 4 分前)\n本文:\n" in prompt
    )


def test_format_prompt_omits_sent_at_line_when_timestamp_empty() -> None:
    """issue #417: timestamp が空なら行ごと省き、宛先の直後が本文になること."""
    msg = _make_msg(to="@bridge-codex", body="hello", timestamp="")
    prompt = _format_prompt("@bridge-codex", msg, now=_NOW)
    assert "送信時刻" not in prompt
    assert "宛先: @bridge-codex\n本文:\nhello\n" in prompt
