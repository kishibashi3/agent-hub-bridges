"""Tests for _startup_catchup (issue #98: startup catchup).

bridge 起動時 get_messages → 未読メッセージ処理。

カバーするケース:
  - 未読ゼロ → cursor 不変
  - get_unread() 失敗 → WARNING ログ + cursor 不変
  - cursor より古いメッセージ → ack のみ (スキップ)
  - cursor と同じ timestamp → ack のみ (スキップ)
  - 新しいメッセージ → _handle_one 呼び出し + cursor 更新 + ack
  - コマンドメッセージ (/ で始まる) → スキップ (ack しない)
  - コマンドのみ → cursor 不変
  - 混在 (cursor 済 + コマンド + 新規) → 新規のみ処理
  - runner lazy init → 最初の処理対象メッセージで初期化
  - runner 既初期化 → 再初期化しない
  - restart handler が router に設定される
"""

from __future__ import annotations

import logging
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from agent_hub_sdk import CommandRouter, IncomingMessage

from agent_hub_bridges.claude.worker import (
    _ActivityTracker,
    _IdleCompactWatchdog,
    _MessageGapTracker,
    _startup_catchup,
)

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


def _make_msg(
    msg_id: str = "msg-001",
    sender: str = "@alice",
    body: str = "hello",
    timestamp: str = "2026-06-01T10:00:00.000Z",
) -> IncomingMessage:
    return IncomingMessage(
        id=msg_id,
        sender=sender,
        to="@bridges-impl",
        body=body,
        timestamp=timestamp,
    )


def _make_hub(*, msgs: list[IncomingMessage] | None = None) -> AsyncMock:
    hub = AsyncMock()
    hub.get_unread = AsyncMock(return_value=msgs if msgs is not None else [])
    hub.ack = AsyncMock()
    hub.send = AsyncMock()
    return hub


def _make_config(tmp_path: Path) -> MagicMock:
    cfg = MagicMock()
    cfg.workdir = tmp_path
    cfg.user = "bridges-impl"
    cfg.model = "claude-sonnet-4-6"
    return cfg


def _make_journal() -> MagicMock:
    return MagicMock()


def _make_trackers() -> tuple[_ActivityTracker, _MessageGapTracker, _IdleCompactWatchdog]:
    return _ActivityTracker(), _MessageGapTracker(), _IdleCompactWatchdog()


_HANDLE_PATCH = "agent_hub_bridges.claude.worker._handle_one"
_SAVE_CURSOR_PATCH = "agent_hub_bridges.claude.worker.save_cursor"
_BUILD_OPTIONS_PATCH = "agent_hub_bridges.claude.worker._build_options"
_RUNNER_PATCH = "agent_hub_bridges.claude.worker.ClaudeRunner"


def _mock_runner() -> MagicMock:
    """ClaudeRunner stub with async __aenter__."""
    runner = MagicMock()
    runner.__aenter__ = AsyncMock(return_value=runner)
    runner.restart = AsyncMock()
    runner.client = MagicMock()
    return runner


# ---------------------------------------------------------------------------
# no messages
# ---------------------------------------------------------------------------


class TestNoMessages:
    @pytest.mark.asyncio
    async def test_returns_cursor_unchanged(self, tmp_path: Path) -> None:
        """未読ゼロ → cursor 変化なし。"""
        hub = _make_hub(msgs=[])
        tracker, gap_tracker, compact_watchdog = _make_trackers()

        result = await _startup_catchup(
            hub,
            _make_config(tmp_path),
            tmp_path / "mcp.json",
            [None],
            "2026-05-01T00:00:00.000Z",
            tracker,
            gap_tracker,
            compact_watchdog,
            _make_journal(),
            CommandRouter(),
        )

        assert result == "2026-05-01T00:00:00.000Z"
        hub.ack.assert_not_called()

    @pytest.mark.asyncio
    async def test_logs_no_unread(
        self, tmp_path: Path, caplog: pytest.LogCaptureFixture
    ) -> None:
        """未読ゼロ → INFO ログ "no unread messages"。"""
        hub = _make_hub(msgs=[])
        tracker, gap_tracker, compact_watchdog = _make_trackers()

        with caplog.at_level(logging.INFO, logger="agent_hub_bridges.claude.worker"):
            await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                [None],
                None,
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        assert "startup-catchup" in caplog.text
        assert "no unread" in caplog.text.lower()


# ---------------------------------------------------------------------------
# get_unread() failure
# ---------------------------------------------------------------------------


class TestGetUnreadFailure:
    @pytest.mark.asyncio
    async def test_returns_cursor_unchanged_on_failure(self, tmp_path: Path) -> None:
        """get_unread() 例外 → cursor 変化なし。"""
        hub = _make_hub()
        hub.get_unread = AsyncMock(side_effect=RuntimeError("hub unreachable"))
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        original = "2026-05-15T00:00:00.000Z"

        result = await _startup_catchup(
            hub,
            _make_config(tmp_path),
            tmp_path / "mcp.json",
            [None],
            original,
            tracker,
            gap_tracker,
            compact_watchdog,
            _make_journal(),
            CommandRouter(),
        )

        assert result == original
        hub.ack.assert_not_called()

    @pytest.mark.asyncio
    async def test_logs_warning_on_failure(
        self, tmp_path: Path, caplog: pytest.LogCaptureFixture
    ) -> None:
        """get_unread() 例外 → WARNING ログ。"""
        hub = _make_hub()
        hub.get_unread = AsyncMock(side_effect=RuntimeError("hub unreachable"))
        tracker, gap_tracker, compact_watchdog = _make_trackers()

        with caplog.at_level(logging.WARNING, logger="agent_hub_bridges.claude.worker"):
            await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                [None],
                None,
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        assert "startup-catchup" in caplog.text
        assert "failed" in caplog.text.lower() or "get_messages" in caplog.text.lower()


# ---------------------------------------------------------------------------
# cursor filtering
# ---------------------------------------------------------------------------


class TestCursorFilter:
    @pytest.mark.asyncio
    async def test_older_message_is_acked_and_skipped(self, tmp_path: Path) -> None:
        """cursor より古いメッセージ → ack + _handle_one 呼ばれない。

        cursor-skip は runner lazy init より先に行われるため:
        - ClaudeRunner や _build_options のモックは不要
        - cursor-skip されたメッセージでは runner_holder[0] が None のまま
        """
        old_msg = _make_msg(timestamp="2026-05-01T08:00:00.000Z")
        hub = _make_hub(msgs=[old_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        runner_holder: list = [None]

        with patch(_HANDLE_PATCH) as mock_handle:
            await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                runner_holder,
                "2026-05-01T12:00:00.000Z",  # cursor newer than msg
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        mock_handle.assert_not_called()
        hub.ack.assert_called_once_with(old_msg.id)
        # cursor-skip メッセージでは runner が初期化されない
        assert runner_holder[0] is None

    @pytest.mark.asyncio
    async def test_exact_timestamp_match_is_skipped(self, tmp_path: Path) -> None:
        """cursor と同じ timestamp のメッセージ → ack + スキップ。runner_holder は None のまま。"""
        ts = "2026-05-01T12:00:00.000Z"
        msg = _make_msg(timestamp=ts)
        hub = _make_hub(msgs=[msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        runner_holder: list = [None]

        with patch(_HANDLE_PATCH) as mock_handle:
            result = await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                runner_holder,
                ts,
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        mock_handle.assert_not_called()
        hub.ack.assert_called_once_with(msg.id)
        assert result == ts  # cursor unchanged
        # cursor-skip メッセージでは runner が初期化されない
        assert runner_holder[0] is None


# ---------------------------------------------------------------------------
# new message processing
# ---------------------------------------------------------------------------


class TestNewMessageProcessing:
    @pytest.mark.asyncio
    async def test_calls_handle_one(self, tmp_path: Path) -> None:
        """cursor より新しいメッセージ → _handle_one が呼ばれる。"""
        new_msg = _make_msg(timestamp="2026-06-01T10:00:00.000Z")
        hub = _make_hub(msgs=[new_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        runner = _mock_runner()

        with patch(_HANDLE_PATCH, new_callable=AsyncMock) as mock_handle, \
             patch(_SAVE_CURSOR_PATCH), \
             patch(_BUILD_OPTIONS_PATCH, return_value=MagicMock()), \
             patch(_RUNNER_PATCH, return_value=runner):
            await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                [None],
                "2026-05-01T00:00:00.000Z",
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        mock_handle.assert_awaited_once()
        # 3rd positional arg is the message
        assert mock_handle.call_args.args[2] is new_msg

    @pytest.mark.asyncio
    async def test_updates_cursor_to_message_timestamp(self, tmp_path: Path) -> None:
        """新しいメッセージ処理後 → cursor が message timestamp に更新される。"""
        new_msg = _make_msg(timestamp="2026-06-01T10:00:00.000Z")
        hub = _make_hub(msgs=[new_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        runner = _mock_runner()

        with patch(_HANDLE_PATCH, new_callable=AsyncMock), \
             patch(_SAVE_CURSOR_PATCH) as mock_save, \
             patch(_BUILD_OPTIONS_PATCH, return_value=MagicMock()), \
             patch(_RUNNER_PATCH, return_value=runner):
            result = await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                [None],
                "2026-05-01T00:00:00.000Z",
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        assert result == new_msg.timestamp
        mock_save.assert_called_once()

    @pytest.mark.asyncio
    async def test_acks_after_processing(self, tmp_path: Path) -> None:
        """新しいメッセージ → 処理後に ack される。"""
        new_msg = _make_msg(timestamp="2026-06-01T10:00:00.000Z")
        hub = _make_hub(msgs=[new_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        runner = _mock_runner()

        with patch(_HANDLE_PATCH, new_callable=AsyncMock), \
             patch(_SAVE_CURSOR_PATCH), \
             patch(_BUILD_OPTIONS_PATCH, return_value=MagicMock()), \
             patch(_RUNNER_PATCH, return_value=runner):
            await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                [None],
                "2026-05-01T00:00:00.000Z",
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        hub.ack.assert_called_once_with(new_msg.id)


# ---------------------------------------------------------------------------
# command messages
# ---------------------------------------------------------------------------


class TestCommandMessages:
    @pytest.mark.asyncio
    async def test_command_message_not_acked(self, tmp_path: Path) -> None:
        """コマンドメッセージ (/ で始まる) → ack されない (inbox loop に委ねる)。"""
        cmd_msg = _make_msg(body="/ping", timestamp="2026-06-01T10:00:00.000Z")
        hub = _make_hub(msgs=[cmd_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()

        with patch(_HANDLE_PATCH) as mock_handle:
            await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                [None],
                None,
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        hub.ack.assert_not_called()
        mock_handle.assert_not_called()

    @pytest.mark.asyncio
    async def test_command_only_returns_cursor_unchanged(self, tmp_path: Path) -> None:
        """コマンドのみ → cursor 変化なし。"""
        cmd_msg = _make_msg(body="/status", timestamp="2026-06-01T10:00:00.000Z")
        hub = _make_hub(msgs=[cmd_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        original = "2026-05-15T00:00:00.000Z"

        result = await _startup_catchup(
            hub,
            _make_config(tmp_path),
            tmp_path / "mcp.json",
            [None],
            original,
            tracker,
            gap_tracker,
            compact_watchdog,
            _make_journal(),
            CommandRouter(),
        )

        assert result == original

    @pytest.mark.asyncio
    async def test_logs_command_deferred(
        self, tmp_path: Path, caplog: pytest.LogCaptureFixture
    ) -> None:
        """コマンドのみ → INFO ログ "deferred"。"""
        cmd_msg = _make_msg(body="/restart", timestamp="2026-06-01T10:00:00.000Z")
        hub = _make_hub(msgs=[cmd_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()

        with caplog.at_level(logging.INFO, logger="agent_hub_bridges.claude.worker"):
            await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                [None],
                None,
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        assert "deferred" in caplog.text.lower() or "startup-catchup" in caplog.text


# ---------------------------------------------------------------------------
# mixed messages
# ---------------------------------------------------------------------------


class TestMixedMessages:
    @pytest.mark.asyncio
    async def test_only_new_nl_messages_processed(self, tmp_path: Path) -> None:
        """cursor 済 + コマンド + 新規 NL → 新規 NL のみ処理。"""
        old_msg = _make_msg(
            msg_id="old-001", body="old task", timestamp="2026-05-01T08:00:00.000Z"
        )
        cmd_msg = _make_msg(
            msg_id="cmd-001", body="/ping", timestamp="2026-06-01T09:00:00.000Z"
        )
        new_msg = _make_msg(
            msg_id="new-001", body="new task", timestamp="2026-06-01T10:00:00.000Z"
        )
        hub = _make_hub(msgs=[old_msg, cmd_msg, new_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        runner = _mock_runner()

        with patch(_HANDLE_PATCH, new_callable=AsyncMock) as mock_handle, \
             patch(_SAVE_CURSOR_PATCH), \
             patch(_BUILD_OPTIONS_PATCH, return_value=MagicMock()), \
             patch(_RUNNER_PATCH, return_value=runner):
            result = await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                [None],
                "2026-05-01T12:00:00.000Z",  # cursor: old_msg older, cmd and new newer
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        # Only new_msg processed
        mock_handle.assert_awaited_once()
        assert mock_handle.call_args.args[2] is new_msg

        # old_msg acked (cursor skip), cmd_msg NOT acked, new_msg acked
        acked = {c.args[0] for c in hub.ack.call_args_list}
        assert old_msg.id in acked
        assert cmd_msg.id not in acked
        assert new_msg.id in acked

        # cursor updated to new_msg timestamp
        assert result == new_msg.timestamp


# ---------------------------------------------------------------------------
# lazy runner init
# ---------------------------------------------------------------------------


class TestLazyRunnerInit:
    @pytest.mark.asyncio
    async def test_runner_initialized_on_first_processable_message(
        self, tmp_path: Path
    ) -> None:
        """runner が None → 最初の処理対象メッセージで初期化される。"""
        new_msg = _make_msg(timestamp="2026-06-01T10:00:00.000Z")
        hub = _make_hub(msgs=[new_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        runner_holder: list = [None]
        runner = _mock_runner()

        with patch(_HANDLE_PATCH, new_callable=AsyncMock), \
             patch(_SAVE_CURSOR_PATCH), \
             patch(_BUILD_OPTIONS_PATCH, return_value=MagicMock()), \
             patch(_RUNNER_PATCH, return_value=runner) as mock_cls:
            await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                runner_holder,
                None,
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        mock_cls.assert_called_once()
        runner.__aenter__.assert_awaited_once()
        assert runner_holder[0] is runner

    @pytest.mark.asyncio
    async def test_existing_runner_not_reinitialised(self, tmp_path: Path) -> None:
        """runner が既に設定済み → 再初期化しない。"""
        new_msg = _make_msg(timestamp="2026-06-01T10:00:00.000Z")
        hub = _make_hub(msgs=[new_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        existing_runner = MagicMock()
        existing_runner.client = MagicMock()
        runner_holder: list = [existing_runner]

        with patch(_HANDLE_PATCH, new_callable=AsyncMock), \
             patch(_SAVE_CURSOR_PATCH), \
             patch(_RUNNER_PATCH) as mock_cls:
            await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                runner_holder,
                None,
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                CommandRouter(),
            )

        mock_cls.assert_not_called()
        assert runner_holder[0] is existing_runner

    @pytest.mark.asyncio
    async def test_restart_handler_set_on_router(self, tmp_path: Path) -> None:
        """runner 初期化時に router.set_restart_handler が呼ばれる。"""
        new_msg = _make_msg(timestamp="2026-06-01T10:00:00.000Z")
        hub = _make_hub(msgs=[new_msg])
        tracker, gap_tracker, compact_watchdog = _make_trackers()
        runner_holder: list = [None]
        router = CommandRouter()
        runner = _mock_runner()

        with patch(_HANDLE_PATCH, new_callable=AsyncMock), \
             patch(_SAVE_CURSOR_PATCH), \
             patch(_BUILD_OPTIONS_PATCH, return_value=MagicMock()), \
             patch(_RUNNER_PATCH, return_value=runner):
            await _startup_catchup(
                hub,
                _make_config(tmp_path),
                tmp_path / "mcp.json",
                runner_holder,
                None,
                tracker,
                gap_tracker,
                compact_watchdog,
                _make_journal(),
                router,
            )

        assert router._restart_handler is runner.restart
