"""ExceptionGroup を log-friendly な 1 行 string に整形するヘルパ.

anyio.TaskGroup 配下で複数 task が同時に例外を投げた場合 BaseExceptionGroup
で受け取ることになるが、 そのまま `%s` で format すると 長大な repr が
出てログが汚れる。 旧 bridge-claude / bridge-slack / bridge-gemini で
それぞれ private 実装していた `_summarize_exc` を共通化したもの。
"""

from __future__ import annotations


def summarize_exc(exc: BaseException) -> str:
    """例外を ログ向けに 1 行で要約する.

    - 単発の例外なら `str(exc)` をそのまま返す。
    - `BaseExceptionGroup` なら 中の各 exception を `Type: msg` で並べて
      `[ ... ]` で囲む。 ネストした group は再帰しない (= 浅い 1 段のみ;
      深い nest が出てくる前提なら呼出側で再考)。

    Args:
        exc: 任意の例外 (BaseException 派生)。

    Returns:
        ログに 1 行で書ける string。
    """
    if isinstance(exc, BaseExceptionGroup):
        inner = ", ".join(f"{type(e).__name__}: {e}" for e in exc.exceptions)
        return f"[{inner}]"
    return str(exc)
