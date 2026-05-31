"""Tests for workdir existence check in _handle_one (issue #51).

`_handle_one` の冒頭で `config.workdir.is_dir()` を確認する。
workdir が存在しない場合:
  - `claude.query` が呼ばれない
  - sender に fallback DM が送られる
  - early return (= caller が hub.ack を実行できる)

workdir が存在する場合は従来通り `claude.query` が呼ばれる。
"""

from __future__ import annotations

import logging
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

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
