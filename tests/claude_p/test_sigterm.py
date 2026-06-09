"""Tests for SIGTERM graceful shutdown (issue #58).

SIGTERM 受信時に engine.close() が呼ばれ MCP config 一時ファイルが
削除されることを確認する。

カバーするケース:
  - SIGTERM ハンドラが loop に登録されること
  - task cancel 時 (SIGTERM 相当) に engine.close() が finally で実行されること
  - SIGTERM コールバックを直接呼ぶと main_task が cancel されること
  - 正常終了時にも engine.close() が呼ばれること (finally 動作確認)
"""

from __future__ import annotations

import asyncio
import contextlib
import signal
from collections.abc import Callable
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from agent_hub_bridges.claude_p.worker import run_worker

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


def _make_config(tmp_path: Path) -> MagicMock:
    cfg = MagicMock()
    cfg.user = "test-user"
    cfg.tenant = None
    cfg.workdir = tmp_path
    cfg.display_name = None
    cfg.agent_hub_url = "http://localhost:3000/mcp"
    cfg.github_pat = "ghp_test"
    return cfg


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


class TestSigtermHandler:
    @pytest.mark.asyncio
    async def test_sigterm_handler_registered(self, tmp_path: Path) -> None:
        """run_worker() 起動時に add_signal_handler(SIGTERM, ...) が呼ばれる。"""
        config = _make_config(tmp_path)
        loop = asyncio.get_running_loop()

        async def _immediate(*_args: object, **_kwargs: object) -> None:
            return

        with (
            patch("agent_hub_bridges.claude_p.worker.ClaudePCLIEngine") as mock_cls,
            patch("agent_hub_bridges.claude_p.worker.run_with_reconnect", _immediate),
            patch.object(
                loop, "add_signal_handler", wraps=loop.add_signal_handler
            ) as mock_add,
        ):
            mock_cls.create.return_value = MagicMock()
            await run_worker(config)

        registered_sigs = [c.args[0] for c in mock_add.call_args_list]
        assert signal.SIGTERM in registered_sigs

    @pytest.mark.asyncio
    async def test_sigterm_handler_removed_after_normal_exit(
        self, tmp_path: Path
    ) -> None:
        """正常終了時に finally で remove_signal_handler(SIGTERM) が呼ばれる。"""
        config = _make_config(tmp_path)
        loop = asyncio.get_running_loop()

        async def _immediate(*_args: object, **_kwargs: object) -> None:
            return

        with (
            patch("agent_hub_bridges.claude_p.worker.ClaudePCLIEngine") as mock_cls,
            patch("agent_hub_bridges.claude_p.worker.run_with_reconnect", _immediate),
            patch.object(
                loop, "remove_signal_handler", wraps=loop.remove_signal_handler
            ) as mock_remove,
        ):
            mock_cls.create.return_value = MagicMock()
            await run_worker(config)

        removed_sigs = [c.args[0] for c in mock_remove.call_args_list]
        assert signal.SIGTERM in removed_sigs


class TestEngineCloseOnShutdown:
    @pytest.mark.asyncio
    async def test_engine_close_on_task_cancel(self, tmp_path: Path) -> None:
        """task cancel (SIGTERM 相当) 時に finally: engine.close() が実行される。"""
        config = _make_config(tmp_path)
        mock_engine = MagicMock()

        async def _stall(*_args: object, **_kwargs: object) -> None:
            await asyncio.sleep(100)

        with (
            patch("agent_hub_bridges.claude_p.worker.ClaudePCLIEngine") as mock_cls,
            patch("agent_hub_bridges.claude_p.worker.run_with_reconnect", _stall),
        ):
            mock_cls.create.return_value = mock_engine
            task = asyncio.create_task(run_worker(config))
            await asyncio.sleep(0)  # タスクを起動させる
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task

        mock_engine.close.assert_called_once()

    @pytest.mark.asyncio
    async def test_engine_close_on_normal_exit(self, tmp_path: Path) -> None:
        """正常終了時にも engine.close() が finally で呼ばれる (回帰確認)。"""
        config = _make_config(tmp_path)
        mock_engine = MagicMock()

        async def _immediate(*_args: object, **_kwargs: object) -> None:
            return

        with (
            patch("agent_hub_bridges.claude_p.worker.ClaudePCLIEngine") as mock_cls,
            patch("agent_hub_bridges.claude_p.worker.run_with_reconnect", _immediate),
        ):
            mock_cls.create.return_value = mock_engine
            await run_worker(config)

        mock_engine.close.assert_called_once()

    @pytest.mark.asyncio
    async def test_sigterm_callback_cancels_main_task(self, tmp_path: Path) -> None:
        """登録された SIGTERM コールバックを直接呼ぶと main_task が cancel される。

        SIGTERM を受信した場合のエンドツーエンド相当の確認:
          add_signal_handler が登録したコールバック → main_task.cancel()
          → CancelledError 伝播 → finally: engine.close() 実行。
        """
        config = _make_config(tmp_path)
        loop = asyncio.get_running_loop()
        mock_engine = MagicMock()

        sigterm_callback: list = []
        original_add = loop.add_signal_handler

        def _capture_add(sig: int, handler: Callable[[], None]) -> None:
            if sig == signal.SIGTERM:
                sigterm_callback.append(handler)
            original_add(sig, handler)

        async def _fire_then_stall(*_args: object, **_kwargs: object) -> None:
            # ハンドラ登録後 (run_worker が add_signal_handler を呼んだ後) に
            # 直接コールバックを発火させてキャンセルを起動する。
            await asyncio.sleep(0)
            if sigterm_callback:
                sigterm_callback[0]()  # fire SIGTERM callback
            await asyncio.sleep(100)  # cancelled here

        with (
            patch("agent_hub_bridges.claude_p.worker.ClaudePCLIEngine") as mock_cls,
            patch(
                "agent_hub_bridges.claude_p.worker.run_with_reconnect", _fire_then_stall
            ),
            patch.object(loop, "add_signal_handler", side_effect=_capture_add),
        ):
            mock_cls.create.return_value = mock_engine
            with contextlib.suppress(asyncio.CancelledError):
                await run_worker(config)

        mock_engine.close.assert_called_once()


class TestCliCancelledError:
    def test_sigterm_returns_143(self) -> None:
        """asyncio.CancelledError (SIGTERM 経路) で終了コード 143 が返る。"""
        from agent_hub_bridges.claude_p.cli import main

        def _raise_cancelled(coro: object) -> None:
            # coroutine を close してから raise する (unawaited coroutine 警告防止)。
            if hasattr(coro, "close"):
                coro.close()
            raise asyncio.CancelledError

        # Config.from_env_and_args を mock して AGENT_HUB_URL 等の env 依存を排除。
        # このクラスのテストは asyncio.run の例外処理経路のみを検証する。
        with (
            patch(
                "agent_hub_bridges.claude_p.cli.Config.from_env_and_args",
                return_value=MagicMock(),
            ),
            patch(
                "agent_hub_bridges.claude_p.cli.asyncio.run",
                side_effect=_raise_cancelled,
            ),
        ):
            rc = main(
                [
                    "--participant", "test-user",
                ]
            )

        assert rc == 143

    def test_keyboard_interrupt_returns_130(self) -> None:
        """KeyboardInterrupt (SIGINT 経路) で終了コード 130 が返る (回帰確認)。"""
        from agent_hub_bridges.claude_p.cli import main

        def _raise_interrupt(coro: object) -> None:
            if hasattr(coro, "close"):
                coro.close()
            raise KeyboardInterrupt

        with (
            patch(
                "agent_hub_bridges.claude_p.cli.Config.from_env_and_args",
                return_value=MagicMock(),
            ),
            patch(
                "agent_hub_bridges.claude_p.cli.asyncio.run",
                side_effect=_raise_interrupt,
            ),
        ):
            rc = main(
                [
                    "--participant", "test-user",
                ]
            )

        assert rc == 130
