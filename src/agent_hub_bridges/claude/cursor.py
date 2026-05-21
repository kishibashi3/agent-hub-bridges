"""Persistent timestamp cursor for restart-safe inbox processing.

agent-hub-bridges issue #37: bridge 再起動で in-memory ``seen_ids`` が
リセットされ、未処理メッセージが重複 dispatch されるバグへの対処。

解決策: 最後に処理したメッセージの timestamp を JSON file に永続化し、
再起動後は ``msg.timestamp <= cursor`` のメッセージを skip する。

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
from pathlib import Path

logger = logging.getLogger(__name__)

_CURSOR_FILE_ENV = "AGENT_HUB_CURSOR_FILE"
_DEFAULT_CURSOR_TEMPLATE = "/tmp/agent-hub-bridge-{user}-cursor.json"


def cursor_path(user: str) -> Path:
    """cursor file の Path を返す。

    ``AGENT_HUB_CURSOR_FILE`` 環境変数があればそれを優先。
    なければ ``/tmp/agent-hub-bridge-<user>-cursor.json``。
    """
    env_val = os.environ.get(_CURSOR_FILE_ENV)
    if env_val:
        return Path(env_val)
    return Path(_DEFAULT_CURSOR_TEMPLATE.format(user=user))


def load_cursor(user: str) -> str | None:
    """永続化された cursor timestamp を読む。

    Returns:
        ISO-8601 UTC timestamp 文字列、またはファイルが存在しない / 読み込み
        失敗時は ``None``。
    """
    path = cursor_path(user)
    try:
        data = json.loads(path.read_text())
        ts = data.get("last_processed_at")
        if isinstance(ts, str) and ts:
            logger.info(
                "Loaded cursor: last_processed_at=%s (from %s)",
                ts,
                path,
            )
            return ts
    except FileNotFoundError:
        logger.debug("No cursor file at %s, starting fresh", path)
    except Exception:
        logger.warning(
            "Failed to load cursor from %s, starting fresh",
            path,
            exc_info=True,
        )
    return None


def save_cursor(user: str, timestamp: str) -> None:
    """cursor timestamp を永続化する。

    失敗しても例外を上げず warning ログだけ吐く (= cursor 書き込み失敗で
    bridge がダウンするのは避ける)。
    """
    path = cursor_path(user)
    try:
        path.write_text(json.dumps({"last_processed_at": timestamp}, indent=2))
        logger.debug("Cursor saved: last_processed_at=%s → %s", timestamp, path)
    except Exception:
        logger.warning(
            "Failed to save cursor to %s",
            path,
            exc_info=True,
        )
