"""Persistent timestamp cursor for restart-safe inbox processing.

agent-hub-bridges issue #37: bridge 再起動で in-memory ``seen_ids`` が
リセットされ、未処理メッセージが重複 dispatch されるバグへの対処。

解決策: 最後に処理したメッセージの timestamp と、その timestamp で処理済みの
message ID を JSON file に永続化し、再起動後は cursor より前のメッセージと、
cursor と同じ timestamp で処理済みの ID を skip する。

issue #323 (#322 と同じパターン): timestamp は ms 単位なので、同じ ms の
メッセージが複数あり得る。timestamp だけで ``<=`` 判定すると、同じ ms の
2 通目以降を処理せずに捨ててしまう。

保存順は **process → save_cursor → ack** (crash-safe):

- process 済みで ack 前にクラッシュ → 再起動後に再処理されるが、
  外部への副作用を 1 度だけにしたければ冪等に実装することが前提。
  同一 message を 2 回 dispatch するよりも 0 回 dispatch の方が悪い
  ので、 "at-least-once" 方向に倒す。
- save_cursor 済みで ack 前にクラッシュ → 再起動後に skip される
  (= ack は飛ぶが、 すでに処理済みなので問題なし)。

cursor file のデフォルト:
  ``/tmp/agent-hub-bridge-<user>-cursor.json``

環境変数 ``AGENT_HUB_CURSOR_FILE`` で上書き可能。
"""

from __future__ import annotations

import json
import logging
import os
from dataclasses import dataclass
from pathlib import Path

logger = logging.getLogger(__name__)

_CURSOR_FILE_ENV = "AGENT_HUB_CURSOR_FILE"
_DEFAULT_CURSOR_TEMPLATE = "/tmp/agent-hub-bridge-{user}-cursor.json"


@dataclass(frozen=True)
class Cursor:
    """処理済み位置。

    Attributes:
        ts: 最後に処理した message の timestamp (ISO-8601 UTC)。
        ids: ``ts`` と同じ timestamp で処理済みの message ID (issue #323)。
    """

    ts: str
    ids: tuple[str, ...] = ()


def is_seen(cursor: Cursor | None, msg_id: str, timestamp: str) -> bool:
    """message が処理済み位置以前にあるかを返す (issue #37 の skip 判定)。

    ``cursor.ts`` と同じ timestamp の message は、処理済み ID に入っている
    ものだけを seen とする。旧形式の cursor file (ID なし) から読んだ直後は、
    同じ timestamp の message を seen としない (捨てるより 1 度通す)。

    NOTE: ISO-8601 UTC 文字列 (例: "2026-05-21T12:00:00.000Z") は辞書順比較が
    時系列順と一致する。これは server が一貫した形式を返す前提。
    """
    if cursor is None or timestamp > cursor.ts:
        return False
    if timestamp < cursor.ts:
        return True
    return msg_id in cursor.ids


def advance(cursor: Cursor | None, msg_id: str, timestamp: str) -> Cursor:
    """message を処理済みにした cursor を返す。

    message が今の位置より前 (通常は起きない) なら位置を戻さない。
    """
    if cursor is None or timestamp > cursor.ts:
        return Cursor(ts=timestamp, ids=(msg_id,))
    if timestamp == cursor.ts and msg_id not in cursor.ids:
        return Cursor(ts=cursor.ts, ids=(*cursor.ids, msg_id))
    return cursor


def cursor_path(user: str) -> Path:
    """cursor file の Path を返す。

    ``AGENT_HUB_CURSOR_FILE`` 環境変数があればそれを優先。
    なければ ``/tmp/agent-hub-bridge-<user>-cursor.json``。
    """
    env_val = os.environ.get(_CURSOR_FILE_ENV)
    if env_val:
        return Path(env_val)
    return Path(_DEFAULT_CURSOR_TEMPLATE.format(user=user))


def load_cursor(user: str) -> Cursor | None:
    """永続化された cursor を読む。

    ``ids_at_last_processed_at`` の無い旧形式の file は、ID を空として読む。

    Returns:
        :class:`Cursor`、またはファイルが存在しない / 読み込み失敗時は ``None``。
    """
    path = cursor_path(user)
    try:
        data = json.loads(path.read_text())
        ts = data.get("last_processed_at")
        if isinstance(ts, str) and ts:
            raw_ids = data.get("ids_at_last_processed_at") or []
            ids = tuple(i for i in raw_ids if isinstance(i, str))
            logger.info(
                "Loaded cursor: last_processed_at=%s ids_at_last_processed_at=%d (from %s)",
                ts,
                len(ids),
                path,
            )
            return Cursor(ts=ts, ids=ids)
    except FileNotFoundError:
        logger.debug("No cursor file at %s, starting fresh", path)
    except Exception:
        logger.warning(
            "Failed to load cursor from %s, starting fresh",
            path,
            exc_info=True,
        )
    return None


def save_cursor(user: str, cursor: Cursor) -> None:
    """cursor を永続化する。

    失敗しても例外を上げず warning ログだけ吐く (= cursor 書き込み失敗で
    bridge がダウンするのは避ける)。
    """
    path = cursor_path(user)
    try:
        path.write_text(
            json.dumps(
                {
                    "last_processed_at": cursor.ts,
                    "ids_at_last_processed_at": list(cursor.ids),
                },
                indent=2,
            )
        )
        logger.debug(
            "Cursor saved: last_processed_at=%s ids_at_last_processed_at=%d → %s",
            cursor.ts,
            len(cursor.ids),
            path,
        )
    except Exception:
        logger.warning(
            "Failed to save cursor to %s",
            path,
            exc_info=True,
        )
