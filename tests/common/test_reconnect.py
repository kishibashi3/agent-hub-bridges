"""Unit tests for `_common.reconnect.run_with_reconnect` (= Suggestion 3 強化).

`run_with_reconnect` の loop 構造:
  while True:
    try: await session_fn()
    except KeyboardInterrupt or CancelledError: raise
    except BaseException: log + sleep(backoff_s)

検証ポイント:
- 正常完了 (= session_fn が return) なら次の iteration に進む (= 設計上は
  「session 1 回分を回す coroutine が return することは想定しない」 が、
  return しても loop は continue する: 引数で 終了 trigger を入れて確認)
- `KeyboardInterrupt` / `CancelledError` は伝播 (= retry しない)
- 通常例外 (`RuntimeError` 等) は catch されて backoff sleep してから retry
- `BaseExceptionGroup` (TaskGroup 由来) も catch して retry
"""

from __future__ import annotations

import anyio
import pytest

from agent_hub_bridges._common.reconnect import run_with_reconnect


@pytest.mark.anyio
async def test_run_with_reconnect_retries_on_exception(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """通常例外は catch されて 一定回数 retry される."""
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)

    calls = {"n": 0}

    async def session_fn() -> None:
        calls["n"] += 1
        if calls["n"] < 3:
            raise RuntimeError(f"boom #{calls['n']}")
        # 3 回目で KeyboardInterrupt を投げて loop を抜ける
        raise KeyboardInterrupt

    with pytest.raises(KeyboardInterrupt):
        await run_with_reconnect(session_fn, backoff_s=0.0, name="test")

    assert calls["n"] == 3, "3 回呼ばれて 最後の KeyboardInterrupt で抜けるはず"


@pytest.mark.anyio
async def test_run_with_reconnect_propagates_keyboard_interrupt(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)

    async def session_fn() -> None:
        raise KeyboardInterrupt

    with pytest.raises(KeyboardInterrupt):
        await run_with_reconnect(session_fn, backoff_s=0.0)


@pytest.mark.anyio
async def test_run_with_reconnect_propagates_cancelled(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """anyio が cancel を伝播してきた場合は そのまま raise (= retry しない)."""
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)

    cancelled_cls = anyio.get_cancelled_exc_class()

    async def session_fn() -> None:
        raise cancelled_cls

    with pytest.raises(cancelled_cls):
        await run_with_reconnect(session_fn, backoff_s=0.0)


@pytest.mark.anyio
async def test_run_with_reconnect_retries_on_exception_group(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """TaskGroup 経由の `BaseExceptionGroup` も 通常例外と同じく retry."""
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)

    calls = {"n": 0}

    async def session_fn() -> None:
        calls["n"] += 1
        if calls["n"] < 2:
            raise BaseExceptionGroup(
                "group", [RuntimeError("inner1"), ValueError("inner2")]
            )
        raise KeyboardInterrupt

    with pytest.raises(KeyboardInterrupt):
        await run_with_reconnect(session_fn, backoff_s=0.0)

    assert calls["n"] == 2


async def _noop_sleep(_seconds: float) -> None:
    """`anyio.sleep` を 即座に return する no-op に置換 (test 時間短縮用)."""
    return None
