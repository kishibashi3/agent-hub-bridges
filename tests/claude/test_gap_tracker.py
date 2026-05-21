"""Tests for _MessageGapTracker (issue #26: safety-net firing log)."""

from __future__ import annotations

import logging
from unittest.mock import patch

import pytest

from agent_hub_bridges.claude.worker import (
    _PUSH_SILENT_THRESHOLD_S,
    _MessageGapTracker,
)


class TestMessageGapTrackerFirstMessage:
    """first message: gap 計測対象なし → WARNING なし。"""

    def test_first_message_no_warning(self, caplog: pytest.LogCaptureFixture) -> None:
        tracker = _MessageGapTracker()
        with caplog.at_level(logging.WARNING, logger="agent_hub_bridges.claude.worker"):
            tracker.on_message_received("msg-001")
        assert "[safety-net]" not in caplog.text

    def test_first_message_sets_last_received(self) -> None:
        tracker = _MessageGapTracker()
        assert tracker._last_received_at is None
        with patch("time.monotonic", return_value=1000.0):
            tracker.on_message_received("msg-001")
        assert tracker._last_received_at == 1000.0


class TestMessageGapTrackerBelowThreshold:
    """gap < threshold: WARNING なし。"""

    def test_short_gap_no_warning(self, caplog: pytest.LogCaptureFixture) -> None:
        tracker = _MessageGapTracker()
        times = [1000.0, 1000.0 + _PUSH_SILENT_THRESHOLD_S - 1.0]
        with patch("time.monotonic", side_effect=times):
            tracker.on_message_received("msg-001")
            with caplog.at_level(
                logging.WARNING, logger="agent_hub_bridges.claude.worker"
            ):
                tracker.on_message_received("msg-002")
        assert "[safety-net]" not in caplog.text

    def test_exactly_one_second_gap_no_warning(
        self, caplog: pytest.LogCaptureFixture
    ) -> None:
        tracker = _MessageGapTracker()
        with patch("time.monotonic", side_effect=[1000.0, 1001.0]):
            tracker.on_message_received("msg-001")
            with caplog.at_level(
                logging.WARNING, logger="agent_hub_bridges.claude.worker"
            ):
                tracker.on_message_received("msg-002")
        assert "[safety-net]" not in caplog.text


class TestMessageGapTrackerAtThreshold:
    """gap == threshold: WARNING 発火。"""

    def test_exact_threshold_warns(self, caplog: pytest.LogCaptureFixture) -> None:
        tracker = _MessageGapTracker()
        times = [1000.0, 1000.0 + _PUSH_SILENT_THRESHOLD_S]
        with patch("time.monotonic", side_effect=times):
            tracker.on_message_received("msg-001")
            with caplog.at_level(
                logging.WARNING, logger="agent_hub_bridges.claude.worker"
            ):
                tracker.on_message_received("msg-002")
        assert "[safety-net]" in caplog.text
        assert "msg-002" in caplog.text


class TestMessageGapTrackerAboveThreshold:
    """gap > threshold: WARNING 発火 + msg_id / gap 値 / threshold 値が含まれる。"""

    def test_large_gap_warns_with_msg_id(
        self, caplog: pytest.LogCaptureFixture
    ) -> None:
        tracker = _MessageGapTracker()
        gap = _PUSH_SILENT_THRESHOLD_S + 60.0  # well above threshold
        times = [2000.0, 2000.0 + gap]
        with patch("time.monotonic", side_effect=times):
            tracker.on_message_received("msg-A")
            with caplog.at_level(
                logging.WARNING, logger="agent_hub_bridges.claude.worker"
            ):
                tracker.on_message_received("msg-B")
        assert "[safety-net]" in caplog.text
        assert "msg-B" in caplog.text
        # gap value (rounded to int) should appear in log
        assert str(int(gap)) in caplog.text

    def test_warning_mentions_poll_fallback(
        self, caplog: pytest.LogCaptureFixture
    ) -> None:
        tracker = _MessageGapTracker()
        gap = _PUSH_SILENT_THRESHOLD_S + 5.0
        with patch("time.monotonic", side_effect=[0.0, gap]):
            tracker.on_message_received("msg-1")
            with caplog.at_level(
                logging.WARNING, logger="agent_hub_bridges.claude.worker"
            ):
                tracker.on_message_received("msg-2")
        assert "poll" in caplog.text.lower()


class TestMessageGapTrackerMultipleMessages:
    """複数メッセージの連続受信で last_received_at が更新される。"""

    def test_updates_last_received_at(self) -> None:
        tracker = _MessageGapTracker()
        times = [100.0, 110.0, 120.0]
        with patch("time.monotonic", side_effect=times):
            tracker.on_message_received("msg-1")
            tracker.on_message_received("msg-2")
            tracker.on_message_received("msg-3")
        assert tracker._last_received_at == 120.0

    def test_short_gaps_between_bursts_no_warning(
        self, caplog: pytest.LogCaptureFixture
    ) -> None:
        """burst メッセージ群は gap < threshold なので WARNING なし。"""
        tracker = _MessageGapTracker()
        # 3 messages, 5s apart each — well below 25s threshold
        times = [1000.0, 1005.0, 1010.0]
        with patch("time.monotonic", side_effect=times):
            tracker.on_message_received("msg-1")
            with caplog.at_level(
                logging.WARNING, logger="agent_hub_bridges.claude.worker"
            ):
                tracker.on_message_received("msg-2")
                tracker.on_message_received("msg-3")
        assert "[safety-net]" not in caplog.text

    def test_first_short_then_long_gap_warns_only_on_long(
        self, caplog: pytest.LogCaptureFixture
    ) -> None:
        """short gap → long gap: long gap の message だけ WARNING。"""
        tracker = _MessageGapTracker()
        threshold = _PUSH_SILENT_THRESHOLD_S
        times = [0.0, 5.0, 5.0 + threshold + 10.0]
        with patch("time.monotonic", side_effect=times):
            tracker.on_message_received("short-before")
            with caplog.at_level(
                logging.WARNING, logger="agent_hub_bridges.claude.worker"
            ):
                tracker.on_message_received("short-after")  # gap=5s, no warn
                tracker.on_message_received("long-after")  # gap>threshold, warn
        assert "[safety-net]" in caplog.text
        assert "long-after" in caplog.text
        assert "short-after" not in caplog.text


class TestMessageGapTrackerDefaultThreshold:
    """デフォルト閾値 (_PUSH_SILENT_THRESHOLD_S) が 25.0 秒であることを確認。"""

    def test_default_threshold_value(self) -> None:
        assert _PUSH_SILENT_THRESHOLD_S == 25.0
