"""Tests for rate-limit observability improvements (issues #19 and #22).

issue #19: [RATE_LIMIT_RETRY] grep-able log marker in engine.run() retry loop.
issue #22: fallback DM from gemini worker when rate-limit max_retries exhausted.
"""

from __future__ import annotations

import logging
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from agent_hub_bridges.gemini.config import Config
from agent_hub_bridges.gemini.engine import (
    EngineResult,
    GeminiCLIEngine,
    is_rate_limit_error,
)

# ---------- helpers ----------


def _make_engine(
    *,
    tmp_workdir: Path,
    max_retries: int = 3,
    backoff_base_s: float = 0.0,
    backoff_cap_s: float = 60.0,
) -> GeminiCLIEngine:
    cfg = Config(
        user="bridge-gemini-test",
        display_name=None,
        tenant=None,
        agent_hub_url="http://example.invalid/mcp",
        github_pat="ghp_test",
        gemini_api_key="key",
        gemini_model="gemini-2.5-flash",
        gemini_cli_path="gemini",
        workdir=tmp_workdir,
    )
    return GeminiCLIEngine(
        config=cfg,
        home_dir=tmp_workdir,
        cli_path="/bin/true",
        timeout_s=10.0,
        max_retries=max_retries,
        backoff_base_s=backoff_base_s,
        backoff_cap_s=backoff_cap_s,
    )


def _rate_limit_result(attempt: int = 1) -> EngineResult:
    return EngineResult(
        returncode=1,
        stdout="",
        stderr="Quota exceeded",
        duration_s=0.1,
        attempts=attempt,
    )


def _ok_result(attempt: int = 1) -> EngineResult:
    return EngineResult(
        returncode=0,
        stdout="done",
        stderr="",
        duration_s=0.1,
        attempts=attempt,
    )


@pytest.fixture
def no_sleep(monkeypatch):
    async def _noop(delay: float) -> None:
        return None

    import agent_hub_bridges.gemini.engine as engine_mod

    monkeypatch.setattr(engine_mod.asyncio, "sleep", _noop)


# ---------- issue #19: [RATE_LIMIT_RETRY] log marker ----------


class TestRateLimitRetryLogMarker:
    """[RATE_LIMIT_RETRY] grep marker が retry WARNING に含まれることを確認。"""

    @pytest.mark.asyncio
    async def test_marker_emitted_on_rate_limit_retry(
        self, tmp_path, no_sleep, monkeypatch, caplog
    ) -> None:
        """rate-limit → retry のとき [RATE_LIMIT_RETRY] が WARNING に出る。"""
        engine = _make_engine(tmp_workdir=tmp_path, max_retries=1)
        results = iter([_rate_limit_result(attempt=1), _ok_result(attempt=2)])

        async def _invoke_once(**_kwargs):
            return next(results)

        monkeypatch.setattr(engine, "_invoke_once", _invoke_once)

        with caplog.at_level(logging.WARNING, logger="agent_hub_bridges.gemini.engine"):
            await engine.run(peer="@alice", prompt="hello")

        assert "[RATE_LIMIT_RETRY]" in caplog.text

    @pytest.mark.asyncio
    async def test_marker_includes_attempt_and_peer(
        self, tmp_path, no_sleep, monkeypatch, caplog
    ) -> None:
        """[RATE_LIMIT_RETRY] ログに attempt 情報と peer が含まれる。"""
        engine = _make_engine(tmp_workdir=tmp_path, max_retries=2)
        results = iter([
            _rate_limit_result(attempt=1),
            _rate_limit_result(attempt=2),
            _ok_result(attempt=3),
        ])

        async def _invoke_once(**_kwargs):
            return next(results)

        monkeypatch.setattr(engine, "_invoke_once", _invoke_once)

        with caplog.at_level(logging.WARNING, logger="agent_hub_bridges.gemini.engine"):
            await engine.run(peer="@bob", prompt="hi")

        assert "[RATE_LIMIT_RETRY]" in caplog.text
        assert "@bob" in caplog.text
        # attempt と max_attempts の両方が含まれる (format: "attempt=1/3")
        assert "1/3" in caplog.text

    @pytest.mark.asyncio
    async def test_no_marker_on_success_first_try(
        self, tmp_path, no_sleep, monkeypatch, caplog
    ) -> None:
        """初回成功時は [RATE_LIMIT_RETRY] を出さない。"""
        engine = _make_engine(tmp_workdir=tmp_path, max_retries=3)

        async def _invoke_once(**_kwargs):
            return _ok_result(attempt=1)

        monkeypatch.setattr(engine, "_invoke_once", _invoke_once)

        with caplog.at_level(logging.WARNING, logger="agent_hub_bridges.gemini.engine"):
            await engine.run(peer="@carol", prompt="hello")

        assert "[RATE_LIMIT_RETRY]" not in caplog.text

    @pytest.mark.asyncio
    async def test_no_marker_on_non_rate_limit_failure(
        self, tmp_path, no_sleep, monkeypatch, caplog
    ) -> None:
        """非 rate-limit 失敗では [RATE_LIMIT_RETRY] を出さない。"""
        engine = _make_engine(tmp_workdir=tmp_path, max_retries=3)
        non_rl_result = EngineResult(
            returncode=1, stdout="", stderr="invalid API key", duration_s=0.1, attempts=1
        )

        async def _invoke_once(**_kwargs):
            return non_rl_result

        monkeypatch.setattr(engine, "_invoke_once", _invoke_once)

        with caplog.at_level(logging.WARNING, logger="agent_hub_bridges.gemini.engine"):
            await engine.run(peer="@dave", prompt="hello")

        assert "[RATE_LIMIT_RETRY]" not in caplog.text

    @pytest.mark.asyncio
    async def test_marker_count_matches_retry_count(
        self, tmp_path, no_sleep, monkeypatch, caplog
    ) -> None:
        """retry 回数だけ [RATE_LIMIT_RETRY] が出る (max_retries=2 → 2 回)。"""
        engine = _make_engine(tmp_workdir=tmp_path, max_retries=2)
        results = iter([
            _rate_limit_result(attempt=1),
            _rate_limit_result(attempt=2),
            _ok_result(attempt=3),
        ])

        async def _invoke_once(**_kwargs):
            return next(results)

        monkeypatch.setattr(engine, "_invoke_once", _invoke_once)

        with caplog.at_level(logging.WARNING, logger="agent_hub_bridges.gemini.engine"):
            await engine.run(peer="@eve", prompt="hello")

        count = caplog.text.count("[RATE_LIMIT_RETRY]")
        assert count == 2


# ---------- issue #22: fallback DM on rate-limit exhaustion ----------


class TestRateLimitFallbackDM:
    """rate-limit max_retries 全敗時に worker が sender に fallback DM を送る。"""

    def _make_msg(self, msg_id: str = "msg-001", sender: str = "@alice") -> MagicMock:
        msg = MagicMock()
        msg.id = msg_id
        msg.sender = sender
        msg.body = "test message"
        msg.timestamp = "2026-05-22T00:00:00.000Z"
        return msg

    def _make_hub(self) -> AsyncMock:
        hub = AsyncMock()
        hub.send = AsyncMock()
        return hub

    def _make_engine(self, result: EngineResult) -> MagicMock:
        engine = MagicMock()
        engine.run = AsyncMock(return_value=result)
        return engine

    @pytest.mark.asyncio
    async def test_fallback_dm_sent_on_rate_limit_exhaustion(self) -> None:
        """rate-limit 全敗 (returncode=1, rate-limit stderr) → fallback DM 送信。"""
        from agent_hub_bridges.gemini.config import Config
        from agent_hub_bridges.gemini.worker import _handle_one

        config = MagicMock(spec=Config)
        config.user = "bridge-gemini"
        hub = self._make_hub()
        msg = self._make_msg()
        rate_limit_result = _rate_limit_result(attempt=4)
        engine = self._make_engine(rate_limit_result)

        _fmt_patch = "agent_hub_bridges.gemini.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            await _handle_one(hub, engine, msg, config)

        hub.send.assert_called_once()
        call_kwargs = hub.send.call_args
        assert call_kwargs.kwargs["to"] == "@alice"
        assert call_kwargs.kwargs["caused_by"] == "msg-001"  # issue #84
        body = call_kwargs.kwargs["message"].lower()
        assert "rate-limit" in body or "rate-limited" in body

    @pytest.mark.asyncio
    async def test_fallback_dm_not_sent_on_success(self) -> None:
        """成功時 (returncode=0) は fallback DM を送らない。"""
        from agent_hub_bridges.gemini.config import Config
        from agent_hub_bridges.gemini.worker import _handle_one

        config = MagicMock(spec=Config)
        config.user = "bridge-gemini"
        hub = self._make_hub()
        msg = self._make_msg()
        engine = self._make_engine(_ok_result(attempt=1))

        _fmt_patch = "agent_hub_bridges.gemini.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            await _handle_one(hub, engine, msg, config)

        hub.send.assert_not_called()

    @pytest.mark.asyncio
    async def test_fallback_dm_not_sent_on_non_rate_limit_failure(self) -> None:
        """非 rate-limit 失敗 (invalid API key 等) は fallback DM を送らない。"""
        from agent_hub_bridges.gemini.config import Config
        from agent_hub_bridges.gemini.worker import _handle_one

        config = MagicMock(spec=Config)
        config.user = "bridge-gemini"
        hub = self._make_hub()
        msg = self._make_msg()
        non_rl_result = EngineResult(
            returncode=1,
            stdout="",
            stderr="invalid API key",
            duration_s=0.1,
            attempts=1,
        )
        engine = self._make_engine(non_rl_result)

        _fmt_patch = "agent_hub_bridges.gemini.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            await _handle_one(hub, engine, msg, config)

        hub.send.assert_not_called()

    @pytest.mark.asyncio
    async def test_fallback_dm_mentions_attempt_count(self) -> None:
        """fallback DM のメッセージに retry 回数が含まれる。"""
        from agent_hub_bridges.gemini.config import Config
        from agent_hub_bridges.gemini.worker import _handle_one

        config = MagicMock(spec=Config)
        config.user = "bridge-gemini"
        hub = self._make_hub()
        msg = self._make_msg()
        result = _rate_limit_result(attempt=4)
        engine = self._make_engine(result)

        _fmt_patch = "agent_hub_bridges.gemini.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            await _handle_one(hub, engine, msg, config)

        sent_message = hub.send.call_args.kwargs["message"]
        assert "4" in sent_message  # attempt count

    @pytest.mark.asyncio
    async def test_fallback_dm_failure_does_not_propagate(self) -> None:
        """fallback DM 送信がコケても _handle_one は例外を投げない。"""
        from agent_hub_bridges.gemini.config import Config
        from agent_hub_bridges.gemini.worker import _handle_one

        config = MagicMock(spec=Config)
        config.user = "bridge-gemini"
        hub = self._make_hub()
        hub.send = AsyncMock(side_effect=RuntimeError("network error"))
        msg = self._make_msg()
        engine = self._make_engine(_rate_limit_result(attempt=4))

        _fmt_patch = "agent_hub_bridges.gemini.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            # should not raise
            await _handle_one(hub, engine, msg, config)

    @pytest.mark.asyncio
    async def test_self_sent_message_skipped(self) -> None:
        """自己送信 echo は skip → fallback DM も送らない。"""
        from agent_hub_bridges.gemini.config import Config
        from agent_hub_bridges.gemini.worker import _handle_one

        config = MagicMock(spec=Config)
        config.user = "bridge-gemini"
        hub = self._make_hub()
        msg = self._make_msg(sender="@bridge-gemini")
        engine = self._make_engine(_rate_limit_result(attempt=4))

        await _handle_one(hub, engine, msg, config)

        engine.run.assert_not_called()
        hub.send.assert_not_called()


# ---------- issue #84: caused_by on engine exception fallback ----------


class TestEngineExceptionFallback:
    """engine.run が例外を上げたとき fallback DM に caused_by が設定される (issue #84)。"""

    def _make_msg(self, msg_id: str = "msg-exc-001", sender: str = "@alice") -> MagicMock:
        msg = MagicMock()
        msg.id = msg_id
        msg.sender = sender
        msg.body = "test"
        msg.timestamp = "2026-05-22T00:00:00.000Z"
        return msg

    def _make_hub(self) -> AsyncMock:
        hub = AsyncMock()
        hub.send = AsyncMock()
        return hub

    @pytest.mark.asyncio
    async def test_engine_exception_fallback_dm_has_caused_by(self) -> None:
        """engine.run が例外 → fallback DM に caused_by=msg.id が設定される。"""
        from agent_hub_bridges.gemini.config import Config
        from agent_hub_bridges.gemini.worker import _handle_one

        config = MagicMock(spec=Config)
        config.user = "bridge-gemini"
        hub = self._make_hub()
        msg = self._make_msg()
        engine = MagicMock()
        engine.run = AsyncMock(side_effect=RuntimeError("engine crash"))

        _fmt_patch = "agent_hub_bridges.gemini.worker.format_peer_message_prompt"
        with patch(_fmt_patch, return_value="prompt"):
            await _handle_one(hub, engine, msg, config)

        hub.send.assert_called_once()
        call_kwargs = hub.send.call_args.kwargs
        assert call_kwargs["to"] == "@alice"
        assert call_kwargs["caused_by"] == "msg-exc-001"  # issue #84


# ---------- is_rate_limit_error public API ----------


class TestIsRateLimitErrorPublicAPI:
    """is_rate_limit_error が public 関数として呼べることを確認 (issue #22 public化)。"""

    def test_detects_quota_exceeded(self) -> None:
        assert is_rate_limit_error("Quota exceeded") is True

    def test_detects_429(self) -> None:
        assert is_rate_limit_error("HTTP 429 Too Many Requests") is True

    def test_ignores_empty(self) -> None:
        assert is_rate_limit_error("") is False

    def test_ignores_unrelated(self) -> None:
        assert is_rate_limit_error("invalid API key") is False
