"""M4 unit test: 周期的 re-subscribe loop.

`worker._resubscribe_once` の挙動 (例外を握り潰す) と、`_periodic_resubscribe`
が sleep → resubscribe を繰り返すループ構造を test する。`sleep_fn` を inject
して loop を強制終了させる pattern を使う。
"""

from __future__ import annotations

import pytest

from agent_hub_bridges.slack.worker import _periodic_resubscribe, _resubscribe_once


class _FakeHub:
    def __init__(self, fail_with: Exception | None = None) -> None:
        self.subscribe_calls = 0
        self.fail_with = fail_with

    async def subscribe_inbox(self) -> None:
        self.subscribe_calls += 1
        if self.fail_with is not None:
            raise self.fail_with


# ----- _resubscribe_once -----------------------------------------------


class TestResubscribeOnce:
    @pytest.mark.asyncio
    async def test_calls_subscribe_inbox(self) -> None:
        hub = _FakeHub()
        await _resubscribe_once(hub)  # type: ignore[arg-type]
        assert hub.subscribe_calls == 1

    @pytest.mark.asyncio
    async def test_exception_is_swallowed(self) -> None:
        # subscribe_inbox が raise してもループは止めない
        hub = _FakeHub(fail_with=RuntimeError("MCP 502"))
        await _resubscribe_once(hub)  # type: ignore[arg-type]  # should not raise
        assert hub.subscribe_calls == 1  # 呼出は試みた


# ----- _periodic_resubscribe -------------------------------------------


class _LoopExit(Exception):
    """テスト用に loop を意図的に抜けるための sentinel."""


class TestPeriodicResubscribe:
    @pytest.mark.asyncio
    async def test_loop_sleeps_then_resubscribes(self) -> None:
        # 2 周回したら sleep 側で _LoopExit を投げて loop を抜ける
        hub = _FakeHub()
        sleep_calls: list[float] = []

        async def fake_sleep(s: float) -> None:
            sleep_calls.append(s)
            if len(sleep_calls) >= 3:
                raise _LoopExit()

        with pytest.raises(_LoopExit):
            await _periodic_resubscribe(
                hub,  # type: ignore[arg-type]
                interval_s=42.0,
                sleep_fn=fake_sleep,
            )

        # interval が正しく渡っている
        assert sleep_calls == [42.0, 42.0, 42.0]
        # 3 周目の sleep で抜けたので resubscribe は 2 回成功
        assert hub.subscribe_calls == 2

    @pytest.mark.asyncio
    async def test_resubscribe_error_does_not_break_loop(self) -> None:
        # subscribe 失敗しても loop は継続する
        hub = _FakeHub(fail_with=RuntimeError("flap"))
        sleep_calls: list[float] = []

        async def fake_sleep(s: float) -> None:
            sleep_calls.append(s)
            if len(sleep_calls) >= 3:
                raise _LoopExit()

        with pytest.raises(_LoopExit):
            await _periodic_resubscribe(
                hub,  # type: ignore[arg-type]
                interval_s=1.0,
                sleep_fn=fake_sleep,
            )
        # 2 回 resubscribe を試みた (= 全部失敗してるが loop は継続)
        assert hub.subscribe_calls == 2


if __name__ == "__main__":
    pytest.main([__file__, "-v"])
