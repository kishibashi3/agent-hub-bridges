"""Outgoing message journal for crash-safe bridge sends (issue #183 / agent-hub#168).

ブリッジが ``hub.send()`` を呼ぶ前に journal entry を書き出し、
ack 受信後に削除する。再起動時は pending entry を replay することで
「bridge クラッシュによる outgoing メッセージ消失」を防止する。

設計ポイント
------------
- **at-least-once**: journal に書いてから送信するため、クラッシュ後に
  再送されると重複する可能性がある。replay 時の重複防止 (idempotency) は
  TODO (Issue #183) — operator の Go 待ち。

- **フォーマット**: JSONL (1 行 1 エントリ)。末尾 append + ``os.fsync`` で
  crash-safe に書き出し、削除時は tmpfile + ``os.replace`` (atomic rename) で
  安全に更新する。

- **保存先**: デフォルト ``~/.agent-hub/journals/<name>.journal``。
  環境変数 ``AGENT_HUB_JOURNAL_DIR`` でディレクトリを上書き可能。

NOTE: 冪等性 (idempotency_key) 対応は TODO (issue #183)。
  - Option A: ``send_message`` MCP ツールに ``idempotency_key`` パラメータを追加し
    server 側で重複チェック (agent-hub L1 変更が必要)。
  - Option B: journal entry に ``sent_message_id`` を記録し bridge 側で照合。
  operator の Go 取得後に別 PR で実装する。
"""

from __future__ import annotations

import contextlib
import json
import logging
import os
import uuid
from dataclasses import asdict, dataclass, field
from datetime import UTC, datetime
from pathlib import Path

logger = logging.getLogger(__name__)

_JOURNAL_DIR_ENV = "AGENT_HUB_JOURNAL_DIR"
_DEFAULT_JOURNAL_DIR = Path.home() / ".agent-hub" / "journals"


def journal_dir() -> Path:
    """journal ディレクトリの Path を返す。

    ``AGENT_HUB_JOURNAL_DIR`` 環境変数があればそれを優先。
    なければ ``~/.agent-hub/journals/``。
    """
    env_val = os.environ.get(_JOURNAL_DIR_ENV)
    return Path(env_val) if env_val else _DEFAULT_JOURNAL_DIR


@dataclass
class JournalEntry:
    """journal の 1 エントリ。``hub.send()`` の引数と同じ形式。

    Attributes:
        id: エントリの UUID。将来 idempotency_key として server に渡す予定
            (TODO: issue #183)。
        to: 送信先 handle (例: ``"@planner"``)。
        message: 送信するメッセージ本文。
        caused_by: 因果チェーン用の元 message ID (issue #162 / agent-hub)。省略可。
        created_at: 書き込み時刻 (ISO-8601 UTC)。
    """

    id: str
    to: str
    message: str
    caused_by: str | None = None
    created_at: str = field(default_factory=lambda: datetime.now(UTC).isoformat())


class Journal:
    """JSONL 形式の outgoing message journal。

    スレッドセーフ・crash-safe:

    - ``write()``: 末尾 append + ``os.fsync`` で即時永続化。
    - ``delete()``: tmpfile + ``os.replace`` (atomic rename) で安全に更新。
    - ``load_all()``: 破損行はスキップして warning ログのみ (クラッシュしない)。

    使い方::

        journal = Journal("claude-impl")
        entry = journal.make_entry(to="@alice", message="hello", caused_by=msg.id)
        journal.write(entry)
        try:
            await hub.send(to=entry.to, message=entry.message, caused_by=entry.caused_by)
            journal.delete(entry.id)
        except Exception:
            # 失敗時は entry を残す → 次回起動時に replay
            raise
    """

    def __init__(self, name: str, base_dir: Path | None = None) -> None:
        """
        Args:
            name: ジャーナルファイル名のベース (拡張子 ``.journal`` が付く)。
                  通常はブリッジの user handle (例: ``"claude-impl"``)。
            base_dir: ジャーナルディレクトリ。``None`` なら :func:`journal_dir` を使う。
        """
        base = base_dir if base_dir is not None else journal_dir()
        self._path = base / f"{name}.journal"

    @property
    def path(self) -> Path:
        """journal file の Path。"""
        return self._path

    def make_entry(
        self,
        *,
        to: str,
        message: str,
        caused_by: str | None = None,
    ) -> JournalEntry:
        """新規 :class:`JournalEntry` を生成する (書き込みは行わない)。"""
        return JournalEntry(
            id=str(uuid.uuid4()),
            to=to,
            message=message,
            caused_by=caused_by,
        )

    def write(self, entry: JournalEntry) -> bool:
        """journal に entry を末尾 append する。

        ``os.fsync`` で即時ディスク反映。

        Returns:
            True  — 書き込み成功。
            False — 書き込み失敗 (warning ログのみ、例外は上げない)。
                    呼び出し側は False を受け取ったら送信を中止すること。
        """
        try:
            self._path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            with open(self._path, "a", encoding="utf-8") as f:
                f.write(json.dumps(asdict(entry), ensure_ascii=False) + "\n")
                f.flush()
                os.fsync(f.fileno())
            logger.debug("Journal write: entry %s → %s", entry.id, self._path)
            return True
        except Exception:
            logger.warning(
                "Failed to write journal entry %s to %s — send will be aborted",
                entry.id,
                self._path,
                exc_info=True,
            )
            return False

    def delete(self, entry_id: str) -> None:
        """entry_id に一致するエントリを journal から削除する。

        tmpfile + ``os.replace`` (atomic rename) で crash-safe に更新する。
        失敗しても例外を上げない (warning ログのみ)。
        """
        if not self._path.exists():
            return
        entries = self.load_all()
        remaining = [e for e in entries if e.id != entry_id]
        if len(remaining) == len(entries):
            logger.debug("Journal delete: entry %s not found (already deleted?)", entry_id)
            return
        self._write_all(remaining)
        logger.debug("Journal delete: entry %s removed from %s", entry_id, self._path)

    def load_all(self) -> list[JournalEntry]:
        """journal の全エントリを読み込む。

        破損行はスキップして warning ログのみ (クラッシュしない)。
        ファイルが存在しない場合は空リストを返す。
        """
        if not self._path.exists():
            return []
        entries: list[JournalEntry] = []
        try:
            with open(self._path, encoding="utf-8") as f:
                for lineno, raw in enumerate(f, 1):
                    raw = raw.strip()
                    if not raw:
                        continue
                    try:
                        data = json.loads(raw)
                        # 前方互換: 未知フィールド (将来バージョンで追加) を無視する。
                        # JournalEntry.__dataclass_fields__ で既知フィールドをフィルタし
                        # TypeError を防ぐ (issue #183 reviewer Minor 2)。
                        known = JournalEntry.__dataclass_fields__.keys()
                        entries.append(JournalEntry(**{k: v for k, v in data.items() if k in known}))
                    except (json.JSONDecodeError, TypeError) as exc:
                        logger.warning(
                            "Skipping corrupt journal line %d in %s: %s",
                            lineno,
                            self._path,
                            exc,
                        )
        except Exception:
            logger.warning(
                "Failed to read journal from %s — treating as empty",
                self._path,
                exc_info=True,
            )
        return entries

    def _write_all(self, entries: list[JournalEntry]) -> None:
        """journal を entries で上書きする (atomic rename)。

        entries が空の場合はファイルを削除する。
        失敗しても例外を上げない (warning ログのみ)。
        """
        if not entries:
            with contextlib.suppress(FileNotFoundError):
                self._path.unlink()
            logger.debug("Journal cleared (empty), file removed: %s", self._path)
            return

        # PID を含めることでマルチプロセス環境での tmp ファイル衝突を防ぐ
        # (reviewer Minor 1: issue #183)
        tmp_path = self._path.with_name(f"{self._path.stem}.{os.getpid()}.journal.tmp")
        try:
            with open(tmp_path, "w", encoding="utf-8") as f:
                for e in entries:
                    f.write(json.dumps(asdict(e), ensure_ascii=False) + "\n")
                f.flush()
                os.fsync(f.fileno())
            os.replace(tmp_path, self._path)
        except Exception:
            logger.warning(
                "Failed to rewrite journal at %s",
                self._path,
                exc_info=True,
            )
            with contextlib.suppress(FileNotFoundError):
                tmp_path.unlink()
