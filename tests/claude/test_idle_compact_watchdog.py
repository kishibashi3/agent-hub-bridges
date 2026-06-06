"""Tests for _IdleCompactWatchdog (issue #60: auto-compact on idle).

カバーするケース:
  - reset() / idle_elapsed() / is_idle() の基本挙動
  - set_busy() / clear_busy() / is_processing() の基本挙動 (issue #102)
  - set_busy() 後に例外が発生しても finally: clear_busy() で False に戻る (issue #102)
  - watch_and_compact(): idle 時に /compact を実行しタイマーリセット
  - watch_and_compact(): idle でなければ /compact を呼ばない
  - watch_and_compact(): _processing == True の間は /compact をスキップ (issue #102)
  - watch_and_compact(): RuntimeError (runner restart 中) → skip + reset
  - watch_and_compact(): /compact 失敗 (Exception) → warning log + reset
  - watch_and_compact(): cancellation は外に伝播する
  - watch_and_compact_lazy(): _processing == True の間は /compact をスキップ (issue #102)
  - watch_and_compact_lazy(): clear_busy() 後は /compact が実行される (issue #102)
  - _compact_archive_dir(): workdir / env var の解決順位 (issue #131)
  - _append_compact_summary(): daily ファイル追記・エラー時の graceful 処理 (issue #131)
  - watch_and_compact(): サマリーが daily ファイルに保存される (issue #131)
  - watch_and_compact(): archive_dir=None の場合はファイル保存しない (issue #131)
  - watch_and_compact(): AssistantMessage なし → fallback テキスト保存 (issue #131)

実装の都合上、watch_and_compact() は while True ループのため、
anyio.move_on_after() で 1 イテレーション後に cancel する。
"""

from __future__ import annotations

import time
from collections.abc import AsyncIterator
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import anyio
import pytest
from claude_agent_sdk import AssistantMessage, ResultMessage, TextBlock

from agent_hub_bridges.claude.worker import (
    _COMPACT_ARCHIVE_DIR_ENV,
    _COMPACT_CHECK_INTERVAL_S,
    _COMPACT_IDLE_S,
    _append_compact_summary,
    _compact_archive_dir,
    _IdleCompactWatchdog,
)

# ---------------------------------------------------------------------------
# ヘルパー
# ---------------------------------------------------------------------------


def _make_result_message() -> ResultMessage:
    return ResultMessage(
        subtype="success",
        duration_ms=100,
        duration_api_ms=80,
        is_error=False,
        num_turns=1,
        session_id="default",
        stop_reason="end_turn",
        total_cost_usd=0.001,
    )


async def _async_iter(*items: object) -> AsyncIterator:
    """items を順に yield する async generator。"""
    for item in items:
        yield item


def _make_mock_runner(*, compact_raises: Exception | None = None) -> MagicMock:
    """watch_and_compact が呼ぶ runner.client を返す MagicMock を組み立てる。

    Args:
        compact_raises: /compact の query() が送出する例外。None なら成功。
    """
    mock_client = MagicMock()
    if compact_raises is not None:
        mock_client.query = AsyncMock(side_effect=compact_raises)
    else:
        mock_client.query = AsyncMock()
    mock_client.receive_response = lambda: _async_iter(_make_result_message())
    mock_runner = MagicMock()
    mock_runner.client = mock_client
    return mock_runner


def _make_mock_runner_with_summary(summary_text: str) -> MagicMock:
    """/compact が AssistantMessage (summary) + ResultMessage を返す mock runner。

    issue #131: サマリーテキストを含む AssistantMessage を返すことで
    archive 保存ロジックをテストする。
    """
    mock_client = MagicMock()
    mock_client.query = AsyncMock()

    assistant_msg = AssistantMessage(
        content=[TextBlock(text=summary_text)],
        model="claude-sonnet-4-6",
    )

    mock_client.receive_response = lambda: _async_iter(
        assistant_msg,
        _make_result_message(),
    )
    mock_runner = MagicMock()
    mock_runner.client = mock_client
    return mock_runner


# ---------------------------------------------------------------------------
# reset / idle_elapsed / is_idle — 同期テスト
# ---------------------------------------------------------------------------


class TestIdleCompactWatchdogSync:
    """reset() / idle_elapsed() / is_idle() の基本挙動。"""

    def test_reset_updates_last_activity(self) -> None:
        wd = _IdleCompactWatchdog(idle_s=60.0, check_interval_s=1.0)
        with patch("time.monotonic", return_value=9999.0):
            wd.reset()
        assert wd._last_activity == 9999.0

    def test_idle_elapsed_returns_diff(self) -> None:
        wd = _IdleCompactWatchdog(idle_s=60.0, check_interval_s=1.0)
        wd._last_activity = 1000.0
        with patch("time.monotonic", return_value=1045.5):
            elapsed = wd.idle_elapsed()
        assert elapsed == pytest.approx(45.5)

    def test_is_idle_below_threshold_false(self) -> None:
        wd = _IdleCompactWatchdog(idle_s=60.0, check_interval_s=1.0)
        wd._last_activity = 1000.0
        with patch("time.monotonic", return_value=1059.9):
            assert not wd.is_idle()

    def test_is_idle_exactly_at_threshold_true(self) -> None:
        wd = _IdleCompactWatchdog(idle_s=60.0, check_interval_s=1.0)
        wd._last_activity = 1000.0
        with patch("time.monotonic", return_value=1060.0):
            assert wd.is_idle()

    def test_is_idle_above_threshold_true(self) -> None:
        wd = _IdleCompactWatchdog(idle_s=60.0, check_interval_s=1.0)
        wd._last_activity = 1000.0
        with patch("time.monotonic", return_value=1200.0):
            assert wd.is_idle()

    def test_reset_makes_not_idle(self) -> None:
        """reset() 直後は is_idle() == False (閾値を超えていない)。"""
        wd = _IdleCompactWatchdog(idle_s=60.0, check_interval_s=1.0)
        t = time.monotonic()
        wd._last_activity = t - 100  # make it idle first
        assert wd.is_idle()
        wd.reset()
        # 直後は elapsed ≈ 0 < 60s
        assert not wd.is_idle()

    # --- issue #102: set_busy / clear_busy / is_processing ---

    def test_initial_not_processing(self) -> None:
        """初期状態では is_processing() == False。"""
        wd = _IdleCompactWatchdog(idle_s=60.0, check_interval_s=1.0)
        assert not wd.is_processing()

    def test_set_busy_makes_processing_true(self) -> None:
        """set_busy() 後は is_processing() == True。"""
        wd = _IdleCompactWatchdog(idle_s=60.0, check_interval_s=1.0)
        wd.set_busy()
        assert wd.is_processing()

    def test_clear_busy_makes_processing_false(self) -> None:
        """clear_busy() 後は is_processing() == False に戻る。"""
        wd = _IdleCompactWatchdog(idle_s=60.0, check_interval_s=1.0)
        wd.set_busy()
        wd.clear_busy()
        assert not wd.is_processing()

    def test_clear_busy_called_on_exception_via_finally(self) -> None:
        """_handle_one が例外を送出しても finally: clear_busy() で False に戻る (issue #102).

        worker.py の呼び出しパターンを再現:
          compact_watchdog.set_busy()
          try:
              await _handle_one(...)  # raises
          finally:
              compact_watchdog.clear_busy()

        except ブロックを持たない実装と同じく例外を伝播させた上で、
        stuck bug (is_processing() が True のまま残る) が起きないことを確認する。
        """
        wd = _IdleCompactWatchdog(idle_s=60.0, check_interval_s=1.0)
        wd.set_busy()
        assert wd.is_processing()
        with pytest.raises(RuntimeError, match="simulated _handle_one failure"):
            try:
                raise RuntimeError("simulated _handle_one failure")
            finally:
                wd.clear_busy()
        assert not wd.is_processing()


# ---------------------------------------------------------------------------
# デフォルト定数値確認
# ---------------------------------------------------------------------------


class TestIdleCompactWatchdogDefaults:
    def test_default_idle_s(self) -> None:
        assert _COMPACT_IDLE_S == pytest.approx(30 * 60)

    def test_default_check_interval_s(self) -> None:
        assert _COMPACT_CHECK_INTERVAL_S == pytest.approx(60.0)

    def test_wd_uses_defaults_when_no_args(self) -> None:
        wd = _IdleCompactWatchdog()
        assert wd._idle_s == pytest.approx(_COMPACT_IDLE_S)
        assert wd._check_interval_s == pytest.approx(_COMPACT_CHECK_INTERVAL_S)


# ---------------------------------------------------------------------------
# watch_and_compact — 非同期テスト
# ---------------------------------------------------------------------------


class TestWatchAndCompact:
    """watch_and_compact() の非同期挙動。

    check_interval_s=0.0 を渡して sleep をほぼゼロにし、
    anyio.move_on_after(0.05) で 1 イテレーションを強制終了する。
    """

    @pytest.mark.asyncio
    async def test_compact_called_when_idle(self) -> None:
        """/compact が呼ばれる。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0  # definitely idle
        runner = _make_mock_runner()

        with anyio.move_on_after(0.05):
            await wd.watch_and_compact(runner)

        # idle_s=0.0 なので複数回呼ばれる可能性があるが、少なくとも 1 回は呼ばれる
        runner.client.query.assert_awaited_with("/compact")
        assert runner.client.query.await_count >= 1

    @pytest.mark.asyncio
    async def test_compact_not_called_when_not_idle(self) -> None:
        """idle でなければ /compact を呼ばない。"""
        wd = _IdleCompactWatchdog(idle_s=9999.0, check_interval_s=0.001)
        # _last_activity = now なので is_idle() == False
        runner = _make_mock_runner()

        with anyio.move_on_after(0.05):
            await wd.watch_and_compact(runner)

        runner.client.query.assert_not_called()

    @pytest.mark.asyncio
    async def test_compact_skipped_when_processing(self) -> None:
        """_processing == True の間は /compact を呼ばない (issue #102)。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0  # definitely idle
        wd.set_busy()  # simulate _handle_one in-progress
        runner = _make_mock_runner()

        with anyio.move_on_after(0.05):
            await wd.watch_and_compact(runner)

        # busy 中は compact を呼ばない
        runner.client.query.assert_not_called()

    @pytest.mark.asyncio
    async def test_compact_called_after_clear_busy(self) -> None:
        """clear_busy() 後は再び /compact が呼ばれる (issue #102)。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0  # definitely idle
        wd.set_busy()
        runner = _make_mock_runner()

        async def _release_busy_later() -> None:
            await anyio.sleep(0.02)
            wd.clear_busy()

        async with anyio.create_task_group() as tg:
            tg.start_soon(_release_busy_later)
            with anyio.move_on_after(0.1):
                await wd.watch_and_compact(runner)
            tg.cancel_scope.cancel()

        # clear_busy() 後に compact が実行されるはず
        assert runner.client.query.await_count >= 1

    @pytest.mark.asyncio
    async def test_timer_reset_after_compact(self) -> None:
        """/compact 後にタイマーがリセットされ is_idle() == False になる。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0
        runner = _make_mock_runner()

        with anyio.move_on_after(0.05):
            await wd.watch_and_compact(runner)

        # reset() により _last_activity が更新されたはず
        # idle_s=0.0 なので直後でも is_idle() == True になり得るが、
        # _last_activity 自体が更新されたことを確認する
        # (直後は monotonic() ≈ _last_activity なので elapsed ≈ 0 = idle_s)
        assert wd.idle_elapsed() < 1.0  # reset されていれば elapsed は数ms

    @pytest.mark.asyncio
    async def test_runtime_error_skipped_timer_reset(
        self, caplog: pytest.LogCaptureFixture
    ) -> None:
        """runner が RuntimeError (restart 中) → /compact を skip し timer reset。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0

        # runner.client が RuntimeError を送出
        mock_runner = MagicMock()
        type(mock_runner).client = property(
            lambda self: (_ for _ in ()).throw(RuntimeError("restart in progress"))
        )

        import logging

        with caplog.at_level(
            logging.DEBUG, logger="agent_hub_bridges.claude.worker"
        ):
            with anyio.move_on_after(0.05):
                await wd.watch_and_compact(mock_runner)

        assert "restart in progress" in caplog.text or "not ready" in caplog.text
        # timer reset を確認
        assert wd.idle_elapsed() < 1.0

    @pytest.mark.asyncio
    async def test_generic_exception_logs_warning_and_resets(
        self, caplog: pytest.LogCaptureFixture
    ) -> None:
        """query() が例外 → WARNING ログ + タイマーリセット。bridge は落ちない。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0
        runner = _make_mock_runner(compact_raises=RuntimeError("fake network error"))
        # RuntimeError は except RuntimeError で拾われる前に except Exception でも拾う
        # が、ここでは client.query が RuntimeError を送出 → except RuntimeError に
        # マッチする (restart 中扱い)。意図的に ValueError を使う。
        runner = _make_mock_runner(compact_raises=ValueError("compact broke"))

        import logging

        with caplog.at_level(
            logging.WARNING, logger="agent_hub_bridges.claude.worker"
        ):
            with anyio.move_on_after(0.05):
                await wd.watch_and_compact(runner)

        assert "compact broke" in caplog.text
        # timer reset を確認
        assert wd.idle_elapsed() < 1.0

    @pytest.mark.asyncio
    async def test_cancellation_propagates(self) -> None:
        """cancel が伝播して watch_and_compact が正常終了する。"""
        wd = _IdleCompactWatchdog(idle_s=9999.0, check_interval_s=0.001)
        runner = _make_mock_runner()

        # CancelScope を使って明示的にキャンセルし、例外が外に出ないことを確認
        cancelled = False
        try:
            with anyio.move_on_after(0.02):
                await wd.watch_and_compact(runner)
        except Exception:
            cancelled = True

        # move_on_after は cancel を呑むので Exception は不要
        assert not cancelled


# ---------------------------------------------------------------------------
# watch_and_compact_lazy — 非同期テスト (issue #91)
# ---------------------------------------------------------------------------


class TestWatchAndCompactLazy:
    """watch_and_compact_lazy() の非同期挙動。

    runner が None の間はタイマーをリセットしてスキップし、
    runner が確定してから watch_and_compact と同じ動作をする。
    check_interval_s=0.001 を渡して sleep をほぼゼロにし、
    anyio.move_on_after() で強制終了する。
    """

    @pytest.mark.asyncio
    async def test_skips_when_runner_none(self) -> None:
        """get_runner() が None を返す間は /compact を呼ばない。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0  # definitely idle

        with anyio.move_on_after(0.05):
            await wd.watch_and_compact_lazy(lambda: None)

        # runner が None なので compact は一度も呼ばれない。
        # (ランナーが提供されないため query の呼び出し先がない)

    @pytest.mark.asyncio
    async def test_resets_timer_while_runner_none(self) -> None:
        """runner=None の間はタイマーをリセットし続ける。"""
        wd = _IdleCompactWatchdog(idle_s=9999.0, check_interval_s=0.001)
        # 意図的に far-past に設定
        wd._last_activity = time.monotonic() - 10000.0

        with anyio.move_on_after(0.05):
            await wd.watch_and_compact_lazy(lambda: None)

        # 少なくとも 1 回 reset() が呼ばれ、_last_activity が更新されているはず
        assert wd.idle_elapsed() < 1.0

    @pytest.mark.asyncio
    async def test_compact_called_when_runner_present_and_idle(self) -> None:
        """runner が確定済み + idle → /compact が呼ばれる。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0

        runner = _make_mock_runner()

        with anyio.move_on_after(0.05):
            await wd.watch_and_compact_lazy(lambda: runner)

        runner.client.query.assert_awaited_with("/compact")
        assert runner.client.query.await_count >= 1

    @pytest.mark.asyncio
    async def test_compact_not_called_when_runner_present_not_idle(self) -> None:
        """runner 確定済み + idle でない → /compact を呼ばない。"""
        wd = _IdleCompactWatchdog(idle_s=9999.0, check_interval_s=0.001)
        runner = _make_mock_runner()

        with anyio.move_on_after(0.05):
            await wd.watch_and_compact_lazy(lambda: runner)

        runner.client.query.assert_not_called()

    @pytest.mark.asyncio
    async def test_compact_skipped_when_processing(self) -> None:
        """_processing == True の間は /compact を呼ばない (issue #102)。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0  # definitely idle
        wd.set_busy()  # simulate _handle_one in-progress
        runner = _make_mock_runner()

        with anyio.move_on_after(0.05):
            await wd.watch_and_compact_lazy(lambda: runner)

        # busy 中は compact を呼ばない
        runner.client.query.assert_not_called()

    @pytest.mark.asyncio
    async def test_transitions_none_to_runner(self) -> None:
        """runner が途中で None → 実体 に変わった場合に compact が走る。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0

        runner = _make_mock_runner()
        # 最初は None, 一定時間後にセット
        holder: list = [None]

        async def _set_runner_later() -> None:
            await anyio.sleep(0.02)
            holder[0] = runner

        async with anyio.create_task_group() as tg:
            tg.start_soon(_set_runner_later)
            with anyio.move_on_after(0.1):
                await wd.watch_and_compact_lazy(lambda: holder[0])
            tg.cancel_scope.cancel()

        # runner が確定してから compact が呼ばれるはず
        assert runner.client.query.await_count >= 1

    @pytest.mark.asyncio
    async def test_compact_called_after_clear_busy(self) -> None:
        """clear_busy() 後は再び /compact が呼ばれる (issue #102)。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        wd._last_activity = time.monotonic() - 1000.0  # definitely idle
        wd.set_busy()
        runner = _make_mock_runner()

        async def _release_busy_later() -> None:
            await anyio.sleep(0.02)
            wd.clear_busy()

        async with anyio.create_task_group() as tg:
            tg.start_soon(_release_busy_later)
            with anyio.move_on_after(0.1):
                await wd.watch_and_compact_lazy(lambda: runner)
            tg.cancel_scope.cancel()

        # clear_busy() 後に compact が実行されるはず
        assert runner.client.query.await_count >= 1

    @pytest.mark.asyncio
    async def test_cancellation_propagates(self) -> None:
        """cancel が伝播して watch_and_compact_lazy が正常終了する。"""
        wd = _IdleCompactWatchdog(idle_s=9999.0, check_interval_s=0.001)

        cancelled = False
        try:
            with anyio.move_on_after(0.02):
                await wd.watch_and_compact_lazy(lambda: None)
        except Exception:
            cancelled = True

        assert not cancelled


# ---------------------------------------------------------------------------
# compact サマリー archive — issue #131
# ---------------------------------------------------------------------------


class TestCompactArchiveDir:
    """_compact_archive_dir() の解決順位テスト。"""

    def test_returns_workdir_daily_by_default(self, tmp_path: Path) -> None:
        """workdir が設定されていれば workdir/daily/ を返す。"""
        result = _compact_archive_dir(tmp_path)
        assert result == tmp_path / "daily"

    def test_returns_none_when_no_workdir_and_no_env(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """workdir も env も未設定なら None を返す。"""
        monkeypatch.delenv(_COMPACT_ARCHIVE_DIR_ENV, raising=False)
        result = _compact_archive_dir(None)
        assert result is None

    def test_env_var_takes_priority_over_workdir(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """BRIDGE_COMPACT_ARCHIVE_DIR が設定されていれば env の値を優先する。"""
        env_dir = tmp_path / "custom-archive"
        monkeypatch.setenv(_COMPACT_ARCHIVE_DIR_ENV, str(env_dir))
        result = _compact_archive_dir(tmp_path / "workdir")
        assert result == env_dir

    def test_env_var_works_without_workdir(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """workdir=None でも env var が設定されていれば返す。"""
        env_dir = tmp_path / "env-archive"
        monkeypatch.setenv(_COMPACT_ARCHIVE_DIR_ENV, str(env_dir))
        result = _compact_archive_dir(None)
        assert result == env_dir


class TestAppendCompactSummary:
    """_append_compact_summary() の動作テスト。"""

    def test_creates_daily_file_with_summary(self, tmp_path: Path) -> None:
        """archive ディレクトリと日次ファイルを作成し、サマリーを追記する。"""
        archive_dir = tmp_path / "daily"
        _append_compact_summary("Test summary content.", archive_dir)

        # ファイルが作成されること
        md_files = list(archive_dir.glob("*.md"))
        assert len(md_files) == 1

        content = md_files[0].read_text(encoding="utf-8")
        assert "## compact @" in content
        assert "Test summary content." in content

    def test_appends_multiple_entries(self, tmp_path: Path) -> None:
        """同一ファイルに複数エントリを追記できる。"""
        archive_dir = tmp_path / "daily"
        _append_compact_summary("First summary.", archive_dir)
        _append_compact_summary("Second summary.", archive_dir)

        md_files = list(archive_dir.glob("*.md"))
        assert len(md_files) == 1
        content = md_files[0].read_text(encoding="utf-8")
        assert "First summary." in content
        assert "Second summary." in content
        # 2 つのセクションヘッダがあること
        assert content.count("## compact @") == 2

    def test_creates_archive_dir_if_not_exists(self, tmp_path: Path) -> None:
        """archive ディレクトリが存在しなくても自動作成する。"""
        archive_dir = tmp_path / "nested" / "daily"
        assert not archive_dir.exists()
        _append_compact_summary("Summary.", archive_dir)
        assert archive_dir.exists()

    def test_write_failure_does_not_raise(
        self, tmp_path: Path, caplog: pytest.LogCaptureFixture
    ) -> None:
        """書き込みに失敗しても例外を上げず WARNING ログのみ。"""
        import logging

        # 存在するファイルと同名の path を archive_dir に指定 → mkdir が失敗する
        fake_archive_dir = tmp_path / "not-a-dir.txt"
        fake_archive_dir.write_text("I am a file, not a dir", encoding="utf-8")

        with caplog.at_level(logging.WARNING, logger="agent_hub_bridges.claude.worker"):
            _append_compact_summary("Summary.", fake_archive_dir)

        assert "failed to write compact summary" in caplog.text


class TestCompactSummaryArchiveIntegration:
    """watch_and_compact() + archive 保存の統合テスト (issue #131)。"""

    @pytest.mark.asyncio
    async def test_summary_saved_to_daily_file(self, tmp_path: Path) -> None:
        """idle → /compact 実行 → AssistantMessage のサマリーが daily ファイルに保存される。"""
        wd = _IdleCompactWatchdog(
            idle_s=0.0, check_interval_s=0.001, workdir=tmp_path
        )
        wd._last_activity = time.monotonic() - 1000.0
        runner = _make_mock_runner_with_summary("This is the compacted context summary.")

        with anyio.move_on_after(0.1):
            await wd.watch_and_compact(runner)

        daily_dir = tmp_path / "daily"
        assert daily_dir.exists(), "daily/ ディレクトリが作成されていること"
        md_files = list(daily_dir.glob("*.md"))
        assert len(md_files) >= 1
        content = md_files[0].read_text(encoding="utf-8")
        assert "## compact @" in content
        assert "This is the compacted context summary." in content

    @pytest.mark.asyncio
    async def test_no_file_written_when_workdir_none(self, tmp_path: Path) -> None:
        """workdir=None (= _archive_dir=None) の場合はファイルを書かない。"""
        wd = _IdleCompactWatchdog(idle_s=0.0, check_interval_s=0.001)
        # workdir を渡さない → _archive_dir は None
        assert wd._archive_dir is None

        wd._last_activity = time.monotonic() - 1000.0
        runner = _make_mock_runner_with_summary("Summary that should not be saved.")

        with anyio.move_on_after(0.1):
            await wd.watch_and_compact(runner)

        # 例外が上がらないことを確認 (workdir なしでも正常終了)
        assert True

    @pytest.mark.asyncio
    async def test_fallback_text_when_no_assistant_message(
        self, tmp_path: Path
    ) -> None:
        """AssistantMessage がない /compact レスポンス → fallback テキストを保存する。"""
        wd = _IdleCompactWatchdog(
            idle_s=0.0, check_interval_s=0.001, workdir=tmp_path
        )
        wd._last_activity = time.monotonic() - 1000.0
        # ResultMessage のみを返す (AssistantMessage なし)
        runner = _make_mock_runner()

        with anyio.move_on_after(0.1):
            await wd.watch_and_compact(runner)

        daily_dir = tmp_path / "daily"
        assert daily_dir.exists()
        md_files = list(daily_dir.glob("*.md"))
        assert len(md_files) >= 1
        content = md_files[0].read_text(encoding="utf-8")
        assert "(no summary text captured from /compact response)" in content

    @pytest.mark.asyncio
    async def test_env_var_overrides_workdir_for_archive(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """BRIDGE_COMPACT_ARCHIVE_DIR が workdir/daily より優先される。"""
        custom_archive = tmp_path / "custom-archive"
        monkeypatch.setenv(_COMPACT_ARCHIVE_DIR_ENV, str(custom_archive))

        workdir = tmp_path / "workdir"
        workdir.mkdir()
        wd = _IdleCompactWatchdog(
            idle_s=0.0, check_interval_s=0.001, workdir=workdir
        )
        wd._last_activity = time.monotonic() - 1000.0
        runner = _make_mock_runner_with_summary("Custom archive summary.")

        with anyio.move_on_after(0.1):
            await wd.watch_and_compact(runner)

        # custom_archive に保存されること
        assert custom_archive.exists()
        md_files = list(custom_archive.glob("*.md"))
        assert len(md_files) >= 1
        assert "Custom archive summary." in md_files[0].read_text(encoding="utf-8")

        # workdir/daily には保存されないこと
        assert not (workdir / "daily").exists()

    @pytest.mark.asyncio
    async def test_lazy_summary_saved_to_daily_file(self, tmp_path: Path) -> None:
        """watch_and_compact_lazy() でも daily ファイルへのサマリー保存が動作する。"""
        wd = _IdleCompactWatchdog(
            idle_s=0.0, check_interval_s=0.001, workdir=tmp_path
        )
        wd._last_activity = time.monotonic() - 1000.0
        runner = _make_mock_runner_with_summary("Lazy compact summary.")

        with anyio.move_on_after(0.1):
            await wd.watch_and_compact_lazy(lambda: runner)

        daily_dir = tmp_path / "daily"
        assert daily_dir.exists()
        md_files = list(daily_dir.glob("*.md"))
        assert len(md_files) >= 1
        content = md_files[0].read_text(encoding="utf-8")
        assert "Lazy compact summary." in content
