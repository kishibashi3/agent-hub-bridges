"""Tests for _handle_one in bridge-claude-p worker.

workdir missing check (issue #51 pattern) and happy path.
"""

from __future__ import annotations

import logging
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from agent_hub_bridges.claude_p.worker import _handle_one

# ---------- helpers ----------


def _make_config(workdir: Path) -> MagicMock:
    cfg = MagicMock()
    cfg.workdir = workdir
    cfg.user = "bridge-claude-p"
    return cfg


def _make_msg(sender: str = "@alice") -> MagicMock:
    msg = MagicMock()
    msg.id = "msg-001"
    msg.sender = sender
    msg.body = "hello"
    return msg


def _make_hub() -> AsyncMock:
    hub = AsyncMock()
    hub.send = AsyncMock()
    return hub


def _make_engine() -> MagicMock:
    engine = MagicMock()
    engine.run = AsyncMock(return_value=MagicMock(returncode=0, duration_s=0.1))
    return engine


# ---------- workdir missing ----------


class TestWorkdirMissing:
    @pytest.mark.asyncio
    async def test_engine_run_not_called_when_workdir_missing(
        self, tmp_path: Path
    ) -> None:
        missing = tmp_path / "does_not_exist"
        hub = _make_hub()
        engine = _make_engine()
        msg = _make_msg()
        config = _make_config(missing)

        await _handle_one(hub, engine, msg, config)

        engine.run.assert_not_called()

    @pytest.mark.asyncio
    async def test_fallback_dm_sent_when_workdir_missing(
        self, tmp_path: Path
    ) -> None:
        missing = tmp_path / "gone"
        hub = _make_hub()
        engine = _make_engine()
        msg = _make_msg()
        config = _make_config(missing)

        await _handle_one(hub, engine, msg, config)

        hub.send.assert_called_once()
        call_kwargs = hub.send.call_args.kwargs
        assert call_kwargs["to"] == "@alice"
        assert call_kwargs["caused_by"] == "msg-001"  # issue #84

    @pytest.mark.asyncio
    async def test_error_logged_when_workdir_missing(
        self, tmp_path: Path, caplog: pytest.LogCaptureFixture
    ) -> None:
        missing = tmp_path / "vanished"
        hub = _make_hub()
        engine = _make_engine()
        msg = _make_msg()
        config = _make_config(missing)

        with caplog.at_level(logging.ERROR, logger="agent_hub_bridges.claude_p.worker"):
            await _handle_one(hub, engine, msg, config)

        assert "workdir" in caplog.text.lower()

    @pytest.mark.asyncio
    async def test_fallback_dm_failure_does_not_propagate(
        self, tmp_path: Path
    ) -> None:
        missing = tmp_path / "gone"
        hub = _make_hub()
        hub.send = AsyncMock(side_effect=RuntimeError("network error"))
        engine = _make_engine()
        msg = _make_msg()
        config = _make_config(missing)

        await _handle_one(hub, engine, msg, config)  # should not raise

    @pytest.mark.asyncio
    async def test_file_instead_of_dir_treated_as_missing(
        self, tmp_path: Path
    ) -> None:
        file_path = tmp_path / "file.txt"
        file_path.write_text("content")

        hub = _make_hub()
        engine = _make_engine()
        msg = _make_msg()
        config = _make_config(file_path)

        await _handle_one(hub, engine, msg, config)

        engine.run.assert_not_called()


# ---------- engine error fallback ----------


class TestEngineErrorFallback:
    @pytest.mark.asyncio
    async def test_fallback_dm_sent_when_engine_raises(
        self, tmp_path: Path
    ) -> None:
        """engine.run が例外を上げたとき fallback DM が送られる (caused_by 付き)。
        issue #84: caused_by=msg.id が設定されることを確認する。"""
        hub = _make_hub()
        engine = _make_engine()
        engine.run = AsyncMock(side_effect=RuntimeError("engine crashed"))
        msg = _make_msg()
        config = _make_config(tmp_path)

        _fmt_patch = "agent_hub_bridges.claude_p.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            await _handle_one(hub, engine, msg, config)

        hub.send.assert_called_once()
        call_kwargs = hub.send.call_args.kwargs
        assert call_kwargs["to"] == "@alice"
        assert call_kwargs["caused_by"] == "msg-001"  # issue #84


# ---------- workdir present ----------


class TestWorkdirPresent:
    @pytest.mark.asyncio
    async def test_engine_run_called_when_workdir_exists(
        self, tmp_path: Path
    ) -> None:
        hub = _make_hub()
        engine = _make_engine()
        msg = _make_msg()
        config = _make_config(tmp_path)

        _fmt_patch = "agent_hub_bridges.claude_p.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            await _handle_one(hub, engine, msg, config)

        engine.run.assert_called_once()

    @pytest.mark.asyncio
    async def test_no_fallback_dm_when_workdir_exists(
        self, tmp_path: Path
    ) -> None:
        hub = _make_hub()
        engine = _make_engine()
        msg = _make_msg()
        config = _make_config(tmp_path)

        _fmt_patch = "agent_hub_bridges.claude_p.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            await _handle_one(hub, engine, msg, config)

        hub.send.assert_not_called()

    @pytest.mark.asyncio
    async def test_self_echo_skipped(self, tmp_path: Path) -> None:
        hub = _make_hub()
        engine = _make_engine()
        msg = _make_msg(sender="@bridge-claude-p")
        config = _make_config(tmp_path)

        await _handle_one(hub, engine, msg, config)

        engine.run.assert_not_called()
