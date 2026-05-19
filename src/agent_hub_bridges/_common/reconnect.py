"""hub session の outer reconnect loop を共通化した helper.

旧 bridge-claude / bridge-gemini が 同じ pattern (= outer `while True` で
`_run_hub_session` を呼び、 例外を catch して backoff sleep してから 再試行)
を private に書いていたので 共通化。 bridge-slack は 3-task TaskGroup
全体の lifetime を 1 hub session に縛る方式なので 同じ pattern が使える。

NOTE: 「reconnect は SDK 内部ではなく caller の責務」 という 設計判断は
agent-hub-sdk 0.3.0 (M2.1) でも 同じ (M2 PR #11 で意図的に deferred)。
SDK 側 reconnect は 将来の milestone 案件。
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable

import anyio

from agent_hub_bridges._common.exc import summarize_exc

logger = logging.getLogger(__name__)


# 旧 bridge-claude / bridge-gemini で 同じ値を使っていた (= 5.0 秒)。
# 短すぎると 連続失敗時に 過負荷、 長すぎると 復旧が遅れる。 中庸の値。
DEFAULT_RECONNECT_BACKOFF_S = 5.0


async def run_with_reconnect(
    session_fn: Callable[[], Awaitable[None]],
    *,
    backoff_s: float = DEFAULT_RECONNECT_BACKOFF_S,
    name: str = "hub session",
) -> None:
    """`session_fn` を outer `while True` で 走らせる reconnect loop.

    `session_fn` は 「1 回ぶんの hub session の lifetime」を表す coroutine。
    register → subscribe → inbox loop → (session 死亡で例外) という一連の流れ
    を内側で 実装する。 例外が出たら ここで catch して backoff sleep してから
    再び `session_fn()` を呼ぶ。

    `KeyboardInterrupt` と `anyio.get_cancelled_exc_class()` は loop を抜けて
    呼出側に伝播する (= 通常終了 / cancel scope による正規 cancel)。 それ
    以外の例外は 一律 retry 対象。

    Args:
        session_fn: 1 回分の hub session を走らせる no-arg coroutine factory。
        backoff_s: 失敗後の sleep 秒数 (default 5.0)。
        name: ログに出す session の名前 (例 `"hub session"`)。

    Raises:
        KeyboardInterrupt: Ctrl-C などで終了したい場合は そのまま伝播。
        BaseException (cancelled): anyio が cancel を伝播してきた場合も
            そのまま伝播。
    """
    while True:
        try:
            await session_fn()
        except (KeyboardInterrupt, anyio.get_cancelled_exc_class()):
            raise
        except BaseException as exc:  # TaskGroup 経由の例外も拾うため意図的に広く取る
            logger.warning(
                "%s ended (%s: %s); reconnecting in %.0fs",
                name,
                type(exc).__name__,
                summarize_exc(exc),
                backoff_s,
            )
            await anyio.sleep(backoff_s)
