"""Tests for workdir existence check and sent_msg_id capture in _handle_one.

issue #51: workdir check
  `_handle_one` の冒頭で `config.workdir.is_dir()` を確認する。
  workdir が存在しない場合:
    - `claude.query` が呼ばれない
    - sender に fallback DM が送られる
    - early return (= caller が hub.ack を実行できる)
  workdir が存在する場合は従来通り `claude.query` が呼ばれる。

issue #94 (follow-up #92): sent_msg_id 捕捉ロジック
  `AssistantMessage` の `ToolUseBlock` で ``mcp__agent-hub__send_message``
  呼び出しを検知し、対応する `UserMessage` の `ToolResultBlock` から
  ``{"id": "<uuid>"}`` を取得して `emit_span` に `sent_msg_id` として渡す。
"""

from __future__ import annotations

import json
import logging
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from claude_agent_sdk import (
    AssistantMessage,
    ResultMessage,
    TextBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)

from agent_hub_bridges._common.journal import Journal
from agent_hub_bridges.claude.worker import _ActivityTracker, _handle_one

# ---------- helpers ----------


def _make_config(workdir: Path) -> MagicMock:
    cfg = MagicMock()
    cfg.workdir = workdir
    cfg.user = "bridge-claude"
    return cfg


def _make_journal(tmp_path: Path) -> Journal:
    """テスト用 Journal (tmp_path に保存)。"""
    return Journal("test-bridge", base_dir=tmp_path / "journals")


def _make_msg(sender: str = "@alice") -> MagicMock:
    msg = MagicMock()
    msg.id = "msg-001"
    msg.sender = sender
    msg.body = "hello"
    msg.timestamp = "2026-05-22T00:00:00.000Z"
    return msg


def _make_hub() -> AsyncMock:
    hub = AsyncMock()
    hub.send = AsyncMock()
    return hub


def _make_claude() -> MagicMock:
    """ClaudeSDKClient stub."""
    claude = MagicMock()
    claude.query = AsyncMock()

    async def _empty_response():
        return
        yield  # make it an async generator

    claude.receive_response = MagicMock(return_value=_empty_response())
    return claude


def _make_result_message(is_error: bool = False) -> ResultMessage:
    """issue #94: テスト用 ResultMessage を作る。"""
    return ResultMessage(
        subtype="success",
        duration_ms=100,
        duration_api_ms=90,
        is_error=is_error,
        num_turns=1,
        session_id="sess-001",
        usage={"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 0},
    )


def _make_claude_with_response(*messages: object) -> MagicMock:
    """issue #94: 指定したメッセージ列を yield する receive_response を持つ stub。"""
    claude = MagicMock()
    claude.query = AsyncMock()

    async def _gen():
        for m in messages:
            yield m

    claude.receive_response = MagicMock(return_value=_gen())
    return claude


# ---------- workdir missing ----------


class TestWorkdirMissing:
    """workdir が存在しない場合の挙動。"""

    @pytest.mark.asyncio
    async def test_claude_query_not_called_when_workdir_missing(
        self, tmp_path: Path
    ) -> None:
        """workdir 不在 → claude.query は呼ばれない。"""
        missing = tmp_path / "does_not_exist"
        assert not missing.exists()

        hub = _make_hub()
        claude = _make_claude()
        msg = _make_msg()
        config = _make_config(missing)
        tracker = _ActivityTracker()

        await _handle_one(hub, claude, msg, config, tracker, _make_journal(tmp_path))

        claude.query.assert_not_called()

    @pytest.mark.asyncio
    async def test_fallback_dm_sent_when_workdir_missing(
        self, tmp_path: Path
    ) -> None:
        """workdir 不在 → sender に fallback DM が送られる。"""
        missing = tmp_path / "gone"
        hub = _make_hub()
        claude = _make_claude()
        msg = _make_msg()
        config = _make_config(missing)
        tracker = _ActivityTracker()

        await _handle_one(hub, claude, msg, config, tracker, _make_journal(tmp_path))

        hub.send.assert_called_once()
        call_kwargs = hub.send.call_args.kwargs
        assert call_kwargs["to"] == "@alice"
        # メッセージに workdir パスが含まれる
        assert str(missing) in call_kwargs["message"]

    @pytest.mark.asyncio
    async def test_error_logged_when_workdir_missing(
        self, tmp_path: Path, caplog: pytest.LogCaptureFixture
    ) -> None:
        """workdir 不在 → ERROR ログが出る。"""
        missing = tmp_path / "vanished"
        hub = _make_hub()
        claude = _make_claude()
        msg = _make_msg()
        config = _make_config(missing)
        tracker = _ActivityTracker()

        with caplog.at_level(logging.ERROR, logger="agent_hub_bridges.claude.worker"):
            await _handle_one(hub, claude, msg, config, tracker, _make_journal(tmp_path))

        assert "workdir does not exist" in caplog.text.lower() or "workdir" in caplog.text

    @pytest.mark.asyncio
    async def test_early_return_when_workdir_missing(
        self, tmp_path: Path
    ) -> None:
        """workdir 不在 → early return (= receive_response も呼ばれない)。"""
        missing = tmp_path / "gone"
        hub = _make_hub()
        claude = _make_claude()
        msg = _make_msg()
        config = _make_config(missing)
        tracker = _ActivityTracker()

        await _handle_one(hub, claude, msg, config, tracker, _make_journal(tmp_path))

        # receive_response が呼ばれていない (= early return 確認)
        claude.receive_response.assert_not_called()

    @pytest.mark.asyncio
    async def test_fallback_dm_failure_does_not_propagate(
        self, tmp_path: Path
    ) -> None:
        """fallback DM 送信がコケても _handle_one は例外を投げない。"""
        missing = tmp_path / "gone"
        hub = _make_hub()
        hub.send = AsyncMock(side_effect=RuntimeError("network error"))
        claude = _make_claude()
        msg = _make_msg()
        config = _make_config(missing)
        tracker = _ActivityTracker()

        # should not raise
        await _handle_one(hub, claude, msg, config, tracker, _make_journal(tmp_path))

    @pytest.mark.asyncio
    async def test_file_instead_of_dir_treated_as_missing(
        self, tmp_path: Path
    ) -> None:
        """workdir にファイル (ディレクトリではない) を渡した場合も early return。"""
        file_path = tmp_path / "file.txt"
        file_path.write_text("content")
        assert file_path.is_file()
        assert not file_path.is_dir()

        hub = _make_hub()
        claude = _make_claude()
        msg = _make_msg()
        config = _make_config(file_path)
        tracker = _ActivityTracker()

        await _handle_one(hub, claude, msg, config, tracker, _make_journal(tmp_path))

        claude.query.assert_not_called()


# ---------- workdir present ----------


class TestWorkdirPresent:
    """workdir が存在する場合は従来通り動作。"""

    @pytest.mark.asyncio
    async def test_claude_query_called_when_workdir_exists(
        self, tmp_path: Path
    ) -> None:
        """workdir が存在する → claude.query が呼ばれる。"""
        hub = _make_hub()
        claude = _make_claude()
        msg = _make_msg()
        config = _make_config(tmp_path)
        tracker = _ActivityTracker()

        _fmt_patch = "agent_hub_bridges.claude.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            await _handle_one(hub, claude, msg, config, tracker, _make_journal(tmp_path))

        claude.query.assert_called_once()

    @pytest.mark.asyncio
    async def test_no_fallback_dm_when_workdir_exists(
        self, tmp_path: Path
    ) -> None:
        """workdir が存在する → hub.send は呼ばれない (正常系では LLM が返信)。"""
        hub = _make_hub()
        claude = _make_claude()
        msg = _make_msg()
        config = _make_config(tmp_path)
        tracker = _ActivityTracker()

        _fmt_patch = "agent_hub_bridges.claude.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            await _handle_one(hub, claude, msg, config, tracker, _make_journal(tmp_path))

        hub.send.assert_not_called()


# ---------- sent_msg_id 捕捉ロジック (issue #92 / #94) ----------


class TestSentMsgIdCapture:
    """issue #94: _handle_one の sent_msg_id 捕捉ロジックを確認する (worker.py)。

    PR #93 (issue #92) で追加した捕捉ロジックの単体テスト。
    ``emit_span`` を mock して引数を確認することで、worker.py 内の
    ToolUseBlock / ToolResultBlock 走査ロジックを直接テストする。
    """

    _FMT_PATCH = "agent_hub_bridges.claude.worker.format_peer_message_prompt"
    _EMIT_PATCH = "agent_hub_bridges.claude.worker.emit_span"

    @pytest.mark.asyncio
    async def test_sent_msg_id_captured_from_tool_result(
        self, tmp_path: Path
    ) -> None:
        """正常系: send_message ToolResultBlock から sent_msg_id が捕捉される。

        流れ:
          AssistantMessage[ToolUseBlock(name="mcp__agent-hub__send_message")]
          → UserMessage[ToolResultBlock(content='{"id": "<uuid>"}')]
          → ResultMessage
          → emit_span(caused_by_id=msg.id, sent_msg_id="<uuid>", ...)
        """
        sent_uuid = "cafebabe-dead-beef-1234-567890abcdef"
        tool_id = "tu-001"

        tool_block = ToolUseBlock(
            id=tool_id,
            name="mcp__agent-hub__send_message",
            input={"to": "@alice", "message": "hi"},
        )
        result_content = json.dumps({
            "id": sent_uuid,
            "from": "@bridge",
            "to": "@alice",
            "message": "hi",
            "caused_by": None,
            "timestamp": "2026-06-01T00:00:00.000Z",
        })
        result_block = ToolResultBlock(
            tool_use_id=tool_id,
            content=result_content,
            is_error=False,
        )
        result_msg = _make_result_message()

        hub = _make_hub()
        msg = _make_msg()
        config = _make_config(tmp_path)
        tracker = _ActivityTracker()
        claude = _make_claude_with_response(
            AssistantMessage(content=[tool_block], model="claude-sonnet-4-6"),
            UserMessage(content=[result_block]),
            result_msg,
        )

        with patch(self._FMT_PATCH, return_value="prompt"):
            with patch(self._EMIT_PATCH) as mock_emit:
                await _handle_one(
                    hub, claude, msg, config, tracker, _make_journal(tmp_path)
                )

        mock_emit.assert_called_once()
        kwargs = mock_emit.call_args.kwargs
        assert kwargs["sent_msg_id"] == sent_uuid
        assert kwargs["caused_by_id"] == msg.id

    @pytest.mark.asyncio
    async def test_sent_msg_id_none_when_no_send_message_call(
        self, tmp_path: Path
    ) -> None:
        """send_message ツール未呼び出し → emit_span に sent_msg_id=None が渡る。

        Claude が TextBlock のみを返し send_message ツールを使わなかった場合、
        捕捉できる msg_id がないため sent_msg_id=None で emit_span を呼ぶ。
        """
        text_block = TextBlock(text="here is my response")
        result_msg = _make_result_message()

        hub = _make_hub()
        msg = _make_msg()
        config = _make_config(tmp_path)
        tracker = _ActivityTracker()
        claude = _make_claude_with_response(
            AssistantMessage(content=[text_block], model="claude-sonnet-4-6"),
            result_msg,
        )

        with patch(self._FMT_PATCH, return_value="prompt"):
            with patch(self._EMIT_PATCH) as mock_emit:
                await _handle_one(
                    hub, claude, msg, config, tracker, _make_journal(tmp_path)
                )

        mock_emit.assert_called_once()
        kwargs = mock_emit.call_args.kwargs
        assert kwargs["sent_msg_id"] is None

    @pytest.mark.asyncio
    async def test_sent_msg_id_none_when_tool_result_is_error(
        self, tmp_path: Path
    ) -> None:
        """ToolResultBlock(is_error=True) → sent_msg_id は捕捉されない (None フォールバック)。

        send_message がサーバ側でエラーを返した場合、
        is_error=True の ToolResultBlock は無視して sent_msg_id=None とする。
        """
        tool_id = "tu-002"

        tool_block = ToolUseBlock(
            id=tool_id,
            name="mcp__agent-hub__send_message",
            input={"to": "@alice", "message": "hi"},
        )
        error_block = ToolResultBlock(
            tool_use_id=tool_id,
            content='{"error": "send_message failed", "message": "recipient not found"}',
            is_error=True,
        )
        result_msg = _make_result_message(is_error=True)

        hub = _make_hub()
        msg = _make_msg()
        config = _make_config(tmp_path)
        tracker = _ActivityTracker()
        claude = _make_claude_with_response(
            AssistantMessage(content=[tool_block], model="claude-sonnet-4-6"),
            UserMessage(content=[error_block]),
            result_msg,
        )

        with patch(self._FMT_PATCH, return_value="prompt"):
            with patch(self._EMIT_PATCH) as mock_emit:
                await _handle_one(
                    hub, claude, msg, config, tracker, _make_journal(tmp_path)
                )

        mock_emit.assert_called_once()
        kwargs = mock_emit.call_args.kwargs
        assert kwargs["sent_msg_id"] is None

    @pytest.mark.asyncio
    async def test_sent_msg_id_none_for_non_send_message_tool(
        self, tmp_path: Path
    ) -> None:
        """send_message 以外のツール名は捕捉しない (完全一致検証)。

        ``block.name == "mcp__agent-hub__send_message"`` の完全一致により、
        ``mcp__agent-hub__get_messages`` のような名前は対象外となる。
        reviewer Minor (PR #93) で指摘された誤検知リスクへの対応確認。
        """
        tool_id = "tu-003"

        # "get_messages" は "send_message" を含まないが念のため完全一致を確認
        other_tool_block = ToolUseBlock(
            id=tool_id,
            name="mcp__agent-hub__get_messages",
            input={},
        )
        result_block = ToolResultBlock(
            tool_use_id=tool_id,
            content=json.dumps({"messages": []}),
            is_error=False,
        )
        result_msg = _make_result_message()

        hub = _make_hub()
        msg = _make_msg()
        config = _make_config(tmp_path)
        tracker = _ActivityTracker()
        claude = _make_claude_with_response(
            AssistantMessage(content=[other_tool_block], model="claude-sonnet-4-6"),
            UserMessage(content=[result_block]),
            result_msg,
        )

        with patch(self._FMT_PATCH, return_value="prompt"):
            with patch(self._EMIT_PATCH) as mock_emit:
                await _handle_one(
                    hub, claude, msg, config, tracker, _make_journal(tmp_path)
                )

        mock_emit.assert_called_once()
        kwargs = mock_emit.call_args.kwargs
        assert kwargs["sent_msg_id"] is None
