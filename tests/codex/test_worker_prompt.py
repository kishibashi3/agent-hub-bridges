"""Unit tests for bridge-codex worker prompt formatting."""

from __future__ import annotations

from unittest.mock import MagicMock

from agent_hub_bridges.codex.worker import _format_prompt


def _make_msg(sender: str = "@alice", body: str = "hello", to: str = "@bridge-codex") -> MagicMock:
    msg = MagicMock()
    msg.sender = sender
    msg.body = body
    msg.to = to
    return msg


def test_format_prompt_contains_get_user_history() -> None:
    """プロンプトに get_user_history の呼び出し指示が含まれること。"""
    msg = _make_msg()
    prompt = _format_prompt("@bridge-codex", msg)
    assert "get_user_history" in prompt


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
    msg = _make_msg(body="今日の天気は？")
    prompt = _format_prompt("@bridge-codex", msg)
    assert "今日の天気は？" in prompt


def test_format_prompt_contains_self_handle() -> None:
    """プロンプトに自分の handle が含まれること。"""
    msg = _make_msg()
    prompt = _format_prompt("@bridge-codex", msg)
    assert "@bridge-codex" in prompt


def test_format_prompt_history_before_send() -> None:
    """get_user_history の指示が send_message の指示より先に現れること。"""
    msg = _make_msg()
    prompt = _format_prompt("@bridge-codex", msg)
    idx_history = prompt.index("get_user_history")
    idx_send = prompt.index("send_message")
    assert idx_history < idx_send, "history instruction should precede send_message instruction"
