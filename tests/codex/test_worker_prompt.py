"""Unit tests for bridge-codex worker prompt formatting."""

from __future__ import annotations

from unittest.mock import MagicMock

from agent_hub_bridges.codex.worker import _format_prompt


def _make_msg(
    sender: str = "@alice",
    body: str = "hello",
    to: str = "@bridge-codex",
    msg_id: str = "test-id",
) -> MagicMock:
    msg = MagicMock()
    msg.sender = sender
    msg.body = body
    msg.to = to
    msg.id = msg_id
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
