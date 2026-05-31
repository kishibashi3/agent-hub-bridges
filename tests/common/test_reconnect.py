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

circuit breaker (issue #82):
- 連続失敗 N 回で CircuitBreakerOpenError が raise される
- 成功すると連続失敗カウントがリセットされる
- max_retries=0 で circuit breaker が無効になる
- on_circuit_open callback が circuit 発火時に呼ばれる
- env AGENT_HUB_BRIDGE_MAX_RETRIES で上限を制御できる
"""

from __future__ import annotations

import anyio
import pytest

from agent_hub_bridges._common.reconnect import (
    CircuitBreakerOpenError,
    _resolve_max_retries,
    run_with_reconnect,
)

# ---------------------------------------------------------------------------
# 既存テスト (= circuit breaker 追加後も動作保証)
# ---------------------------------------------------------------------------
# NOTE: これらのテストでは明示的に max_retries を渡さない (= env から読む)。
# 環境変数 AGENT_HUB_BRIDGE_MAX_RETRIES が未設定なので default 10 が使われる。
# 各テストの失敗回数は 10 未満 → circuit は開かない。


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


# ---------------------------------------------------------------------------
# circuit breaker テスト (issue #82)
# ---------------------------------------------------------------------------


@pytest.mark.anyio
async def test_circuit_breaker_fires_after_n_failures(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """連続 N 回失敗すると CircuitBreakerOpenError が raise される."""
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)

    calls = {"n": 0}

    async def session_fn() -> None:
        calls["n"] += 1
        raise RuntimeError("hub down")

    with pytest.raises(CircuitBreakerOpenError):
        await run_with_reconnect(session_fn, backoff_s=0.0, max_retries=3)

    assert calls["n"] == 3, "3 回失敗で circuit が開くはず"


@pytest.mark.anyio
async def test_circuit_breaker_resets_on_success(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """成功すると連続失敗カウンタがリセットされ、再び N 回失敗で circuit が開く."""
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)

    # フェーズ: fail×2 → success → fail×3 → CircuitBreakerOpenError
    # max_retries=3 で設定すると:
    #   - 最初の fail×2 では circuit は開かない (consecutive_failures=2 < 3)
    #   - success で consecutive_failures が 0 にリセット
    #   - 次の fail×3 で circuit が開く (consecutive_failures=3 >= 3)
    events: list[str] = []

    async def session_fn() -> None:
        total = len(events)
        events.append(f"call_{total}")
        if total < 2:
            raise RuntimeError(f"phase1 failure #{total}")
        if total == 2:
            return  # success — counter resets
        raise RuntimeError(f"phase2 failure #{total}")

    with pytest.raises(CircuitBreakerOpenError):
        await run_with_reconnect(session_fn, backoff_s=0.0, max_retries=3)

    # 呼び出し回数: 2 (fail) + 1 (success) + 3 (fail) = 6
    assert len(events) == 6, f"expected 6 calls, got {len(events)}: {events}"


@pytest.mark.anyio
async def test_circuit_breaker_disabled_when_max_retries_zero(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """max_retries=0 (= 無効化) では何度失敗しても CircuitBreakerOpenError は raise されない."""
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)

    calls = {"n": 0}

    async def session_fn() -> None:
        calls["n"] += 1
        if calls["n"] < 20:
            raise RuntimeError("still failing")
        raise KeyboardInterrupt  # 無限 retry を抜ける

    with pytest.raises(KeyboardInterrupt):
        await run_with_reconnect(session_fn, backoff_s=0.0, max_retries=0)

    assert calls["n"] == 20


@pytest.mark.anyio
async def test_circuit_breaker_calls_on_circuit_open_callback(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """circuit 発火時に on_circuit_open callback が呼ばれる."""
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)

    callback_called = {"n": 0}

    async def on_open() -> None:
        callback_called["n"] += 1

    async def session_fn() -> None:
        raise RuntimeError("hub down")

    with pytest.raises(CircuitBreakerOpenError):
        await run_with_reconnect(
            session_fn,
            backoff_s=0.0,
            max_retries=2,
            on_circuit_open=on_open,
        )

    assert callback_called["n"] == 1, "callback は 1 回だけ呼ばれるはず"


@pytest.mark.anyio
async def test_circuit_breaker_raises_even_if_callback_fails(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """on_circuit_open が例外を投げても CircuitBreakerOpenError は伝播する."""
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)

    async def failing_callback() -> None:
        raise RuntimeError("callback exploded")

    async def session_fn() -> None:
        raise RuntimeError("hub down")

    with pytest.raises(CircuitBreakerOpenError):
        await run_with_reconnect(
            session_fn,
            backoff_s=0.0,
            max_retries=2,
            on_circuit_open=failing_callback,
        )


@pytest.mark.anyio
async def test_circuit_breaker_env_var_controls_max_retries(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """env AGENT_HUB_BRIDGE_MAX_RETRIES が max_retries の解決に使われる."""
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)
    monkeypatch.setenv("AGENT_HUB_BRIDGE_MAX_RETRIES", "2")

    calls = {"n": 0}

    async def session_fn() -> None:
        calls["n"] += 1
        raise RuntimeError("hub down")

    with pytest.raises(CircuitBreakerOpenError):
        # max_retries=None → env から読む → 2
        await run_with_reconnect(session_fn, backoff_s=0.0, max_retries=None)

    assert calls["n"] == 2


@pytest.mark.anyio
async def test_circuit_breaker_env_zero_disables(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """env AGENT_HUB_BRIDGE_MAX_RETRIES=0 で circuit breaker が無効化される."""
    monkeypatch.setattr(anyio, "sleep", _noop_sleep)
    monkeypatch.setenv("AGENT_HUB_BRIDGE_MAX_RETRIES", "0")

    calls = {"n": 0}

    async def session_fn() -> None:
        calls["n"] += 1
        if calls["n"] >= 15:
            raise KeyboardInterrupt
        raise RuntimeError("hub down")

    with pytest.raises(KeyboardInterrupt):
        await run_with_reconnect(session_fn, backoff_s=0.0, max_retries=None)

    assert calls["n"] == 15


# ---------------------------------------------------------------------------
# _resolve_max_retries ユニットテスト
# ---------------------------------------------------------------------------


def test_resolve_max_retries_explicit_override(monkeypatch: pytest.MonkeyPatch) -> None:
    """明示的に override を渡した場合は env を無視する."""
    monkeypatch.setenv("AGENT_HUB_BRIDGE_MAX_RETRIES", "99")
    assert _resolve_max_retries(5) == 5


def test_resolve_max_retries_explicit_zero_returns_none(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """override=0 は無効化 (= None) として扱う."""
    monkeypatch.delenv("AGENT_HUB_BRIDGE_MAX_RETRIES", raising=False)
    assert _resolve_max_retries(0) is None


def test_resolve_max_retries_reads_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """override=None のとき env から読む."""
    monkeypatch.setenv("AGENT_HUB_BRIDGE_MAX_RETRIES", "7")
    assert _resolve_max_retries(None) == 7


def test_resolve_max_retries_default_when_env_unset(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """env 未設定のとき default 10 を返す."""
    monkeypatch.delenv("AGENT_HUB_BRIDGE_MAX_RETRIES", raising=False)
    assert _resolve_max_retries(None) == 10


def test_resolve_max_retries_env_zero_returns_none(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """env=0 は無効化 (= None)."""
    monkeypatch.setenv("AGENT_HUB_BRIDGE_MAX_RETRIES", "0")
    assert _resolve_max_retries(None) is None


def test_resolve_max_retries_env_invalid_falls_back_to_default(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """env が不正値のとき default 10 にフォールバック."""
    monkeypatch.setenv("AGENT_HUB_BRIDGE_MAX_RETRIES", "not-a-number")
    assert _resolve_max_retries(None) == 10


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


async def _noop_sleep(_seconds: float) -> None:
    """`anyio.sleep` を 即座に return する no-op に置換 (test 時間短縮用)."""
    return None
