"""Bridge inventory write-back helper (issue #82: circuit breaker).

circuit breaker が発火したとき、以下の 2 つを記録する:

1. **dead marker file** (``/tmp/agent-hub-bridge-<user>.dead``)
   - ``stop-bridge.sh --dead`` がこのファイルを検索して一括 kill する目印。
   - 内容: "lost-hub\\n<ISO8601 timestamp>\\n"

2. **inventory activity log** (``BRIDGE_INVENTORY`` env が指す markdown file)
   - operator の bridge-inventory.md に ``lost-hub`` エントリを追記する。
   - ``BRIDGE_INVENTORY`` が未設定の場合は skip (= operator スクリプト非導入
     環境では何もしない)。

NOTE: inventory ファイルの Currently running テーブルの `is_online` 列は
operator による reconcile (pgrep + get_participants) で更新する設計なので、
bridge 側からは activity log のみを更新する (テーブル行の直接書き換えは
行わない)。
"""

from __future__ import annotations

import logging
import os
from datetime import datetime
from pathlib import Path

logger = logging.getLogger(__name__)

# dead marker: /tmp/agent-hub-bridge-<user>.dead
_DEAD_MARKER_DIR = Path("/tmp")
_DEAD_MARKER_PREFIX = "agent-hub-bridge-"
_DEAD_MARKER_SUFFIX = ".dead"

# inventory 更新マーカー (stop-bridge.sh と同じ文字列)
_INVENTORY_INSERT_MARKER = "新しいエントリを上に追加"


def dead_marker_path(user: str) -> Path:
    """ユーザ handle に対応する dead marker file のパスを返す.

    例: ``user="bridges-impl"`` → ``/tmp/agent-hub-bridge-bridges-impl.dead``
    """
    return _DEAD_MARKER_DIR / f"{_DEAD_MARKER_PREFIX}{user}{_DEAD_MARKER_SUFFIX}"


def write_dead_marker(user: str) -> None:
    """dead marker file を書き込む.

    ``stop-bridge.sh --dead`` が ``/tmp/agent-hub-bridge-*.dead`` を glob して
    一括 kill するための目印ファイル。

    内容: ``"lost-hub\\n<ISO8601>\\n"``
    書き込みに失敗しても WARNING を出すだけで例外は伝播しない
    (= circuit breaker の shutdown を妨げない)。
    """
    path = dead_marker_path(user)
    try:
        path.write_text(f"lost-hub\n{datetime.now().isoformat()}\n", encoding="utf-8")
        logger.info("[circuit-breaker] dead marker written: %s", path)
    except Exception as exc:
        logger.warning(
            "[circuit-breaker] failed to write dead marker %s: %s", path, exc
        )


def _resolve_inventory_path() -> Path | None:
    """``BRIDGE_INVENTORY`` env を読んで Path を返す; 未設定なら ``None``."""
    val = os.environ.get("BRIDGE_INVENTORY")
    if not val:
        return None
    return Path(val)


def write_lost_hub_to_inventory(user: str, pid: int | None = None) -> None:
    """inventory の activity log に ``lost-hub`` エントリを追記する.

    ``BRIDGE_INVENTORY`` 環境変数が未設定、またはファイルが存在しない場合は
    何もしない (= WARNING ログのみ)。

    フォーマット (stop-bridge.sh の stop エントリと同じスタイル)::

        - YYYY-MM-DD HH:MM — **lost-hub** `@<user>` — circuit-breaker triggered (pid=<N>)

    マーカー行 ``新しいエントリを上に追加`` の直後に挿入する。
    マーカーが見つからない場合はファイル末尾に追記する (fallback)。

    書き込みに失敗しても WARNING を出すだけで例外は伝播しない。
    """
    path = _resolve_inventory_path()
    if path is None:
        logger.debug(
            "[circuit-breaker] BRIDGE_INVENTORY not set, skipping inventory update"
        )
        return
    if not path.is_file():
        logger.warning(
            "[circuit-breaker] inventory file not found: %s — skipping update", path
        )
        return

    now = datetime.now().strftime("%Y-%m-%d %H:%M")
    pid_str = str(pid) if pid is not None else "unknown"
    entry = (
        f"- {now} — **lost-hub** `@{user}` — "
        f"circuit-breaker triggered (pid={pid_str})"
    )

    try:
        content = path.read_text(encoding="utf-8")
        if _INVENTORY_INSERT_MARKER in content:
            # マーカー直後に挿入 (stop-bridge.sh の sed ロジックと同等)
            content = content.replace(
                _INVENTORY_INSERT_MARKER,
                f"{_INVENTORY_INSERT_MARKER}\n{entry}",
                1,
            )
        else:
            # fallback: ファイル末尾に追記
            logger.debug(
                "[circuit-breaker] inventory insert marker not found, appending to end"
            )
            content = content.rstrip("\n") + f"\n{entry}\n"
        path.write_text(content, encoding="utf-8")
        logger.info(
            "[circuit-breaker] inventory updated (lost-hub @%s): %s", user, path
        )
    except Exception as exc:
        logger.warning(
            "[circuit-breaker] failed to update inventory %s: %s", path, exc
        )
