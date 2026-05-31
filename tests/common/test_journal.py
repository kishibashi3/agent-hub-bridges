"""Tests for agent_hub_bridges._common.journal (issue #183 / agent-hub#168)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from agent_hub_bridges._common.journal import (
    _JOURNAL_DIR_ENV,
    Journal,
    JournalEntry,
    journal_dir,
)


# ---------------------------------------------------------------------------
# journal_dir
# ---------------------------------------------------------------------------


def test_journal_dir_default(monkeypatch: pytest.MonkeyPatch) -> None:
    """環境変数なしのデフォルトは ~/.agent-hub/journals/ を返す."""
    monkeypatch.delenv(_JOURNAL_DIR_ENV, raising=False)
    result = journal_dir()
    assert result == Path.home() / ".agent-hub" / "journals"


def test_journal_dir_env_override(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """AGENT_HUB_JOURNAL_DIR 環境変数でディレクトリを上書きできる."""
    monkeypatch.setenv(_JOURNAL_DIR_ENV, str(tmp_path / "my-journals"))
    result = journal_dir()
    assert result == tmp_path / "my-journals"


# ---------------------------------------------------------------------------
# Journal.__init__ と path
# ---------------------------------------------------------------------------


def test_journal_path(tmp_path: Path) -> None:
    """journal ファイルのパスが正しい."""
    journal = Journal("claude-impl", base_dir=tmp_path)
    assert journal.path == tmp_path / "claude-impl.journal"


# ---------------------------------------------------------------------------
# Journal.make_entry
# ---------------------------------------------------------------------------


def test_make_entry_fields(tmp_path: Path) -> None:
    """make_entry が正しいフィールドを持つ JournalEntry を返す."""
    journal = Journal("test-bridge", base_dir=tmp_path)
    entry = journal.make_entry(to="@alice", message="hello", caused_by="msg-001")
    assert entry.to == "@alice"
    assert entry.message == "hello"
    assert entry.caused_by == "msg-001"
    assert len(entry.id) == 36  # UUID4
    assert entry.created_at  # non-empty


def test_make_entry_no_caused_by(tmp_path: Path) -> None:
    """caused_by 省略時は None になる."""
    journal = Journal("test-bridge", base_dir=tmp_path)
    entry = journal.make_entry(to="@bob", message="ping")
    assert entry.caused_by is None


# ---------------------------------------------------------------------------
# Journal.write
# ---------------------------------------------------------------------------


def test_write_creates_file(tmp_path: Path) -> None:
    """write が journal ファイルを作成する."""
    journal = Journal("bridge-x", base_dir=tmp_path)
    entry = journal.make_entry(to="@alice", message="hello")
    journal.write(entry)
    assert journal.path.exists()


def test_write_appends_jsonl(tmp_path: Path) -> None:
    """write が JSONL 形式で末尾 append される."""
    journal = Journal("bridge-x", base_dir=tmp_path)
    e1 = journal.make_entry(to="@alice", message="msg1")
    e2 = journal.make_entry(to="@bob", message="msg2")
    journal.write(e1)
    journal.write(e2)

    lines = journal.path.read_text().strip().split("\n")
    assert len(lines) == 2
    assert json.loads(lines[0])["message"] == "msg1"
    assert json.loads(lines[1])["message"] == "msg2"


def test_write_creates_parent_dir(tmp_path: Path) -> None:
    """write が存在しない親ディレクトリを自動作成する."""
    nested = tmp_path / "deep" / "nested"
    journal = Journal("bridge-y", base_dir=nested)
    entry = journal.make_entry(to="@alice", message="hello")
    journal.write(entry)  # 例外なし
    assert journal.path.exists()


def test_write_returns_true_on_success(tmp_path: Path) -> None:
    """write が成功したとき True を返す."""
    journal = Journal("success-check", base_dir=tmp_path)
    entry = journal.make_entry(to="@alice", message="hello")
    result = journal.write(entry)
    assert result is True


def test_write_returns_false_on_unwritable_path(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """書き込み不可のパスでは False を返し、例外を上げない (warning ログのみ)."""
    from pathlib import Path as _Path
    from unittest.mock import patch

    journal = Journal("test", base_dir=tmp_path / "nested")
    entry = journal.make_entry(to="@alice", message="hello")
    # Path.mkdir を mock して PermissionError を起こす
    with patch.object(_Path, "mkdir", side_effect=PermissionError("mocked")):
        result = journal.write(entry)  # 例外が上がらないことを確認
    assert result is False


# ---------------------------------------------------------------------------
# Journal.load_all
# ---------------------------------------------------------------------------


def test_load_all_no_file(tmp_path: Path) -> None:
    """ファイルが存在しない場合は空リストを返す."""
    journal = Journal("nonexistent", base_dir=tmp_path)
    assert journal.load_all() == []


def test_load_all_empty_file(tmp_path: Path) -> None:
    """空ファイルは空リストを返す."""
    journal = Journal("empty", base_dir=tmp_path)
    journal.path.write_text("")
    assert journal.load_all() == []


def test_load_all_valid_entries(tmp_path: Path) -> None:
    """有効なエントリを正しく読み込む."""
    journal = Journal("bridge-z", base_dir=tmp_path)
    e1 = journal.make_entry(to="@alice", message="hello")
    e2 = journal.make_entry(to="@bob", message="world", caused_by="msg-123")
    journal.write(e1)
    journal.write(e2)

    loaded = journal.load_all()
    assert len(loaded) == 2
    assert loaded[0].to == "@alice"
    assert loaded[0].message == "hello"
    assert loaded[0].caused_by is None
    assert loaded[1].to == "@bob"
    assert loaded[1].caused_by == "msg-123"


def test_load_all_skips_corrupt_lines(tmp_path: Path) -> None:
    """破損行をスキップして残りのエントリを返す."""
    journal = Journal("bridge-corrupt", base_dir=tmp_path)
    e1 = journal.make_entry(to="@alice", message="valid")
    journal.path.write_text(
        json.dumps({"id": e1.id, "to": e1.to, "message": e1.message, "created_at": e1.created_at})
        + "\n"
        + "NOT VALID JSON {{{{\n"
        + json.dumps({"id": "other-id", "to": "@bob", "message": "also valid", "created_at": e1.created_at})
        + "\n"
    )

    loaded = journal.load_all()
    assert len(loaded) == 2  # corrupt line はスキップ
    assert loaded[0].message == "valid"
    assert loaded[1].message == "also valid"


def test_load_all_ignores_unknown_fields(tmp_path: Path) -> None:
    """未来バージョンで追加された未知フィールドを含む行でも TypeError にならず読み込める (前方互換)."""
    journal = Journal("compat-check", base_dir=tmp_path)
    # known フィールド + 未知フィールド "unknown_field_v2"
    journal.path.write_text(
        json.dumps({
            "id": "abc-123",
            "to": "@alice",
            "message": "hello",
            "created_at": "2026-01-01T00:00:00+00:00",
            "unknown_field_v2": "extra-data",  # 未来バージョンで追加された仮想フィールド
        })
        + "\n"
    )

    loaded = journal.load_all()
    assert len(loaded) == 1
    assert loaded[0].id == "abc-123"
    assert loaded[0].to == "@alice"
    assert loaded[0].message == "hello"


# ---------------------------------------------------------------------------
# Journal.delete
# ---------------------------------------------------------------------------


def test_delete_removes_entry(tmp_path: Path) -> None:
    """delete が指定した entry を削除する."""
    journal = Journal("bridge-del", base_dir=tmp_path)
    e1 = journal.make_entry(to="@alice", message="keep")
    e2 = journal.make_entry(to="@bob", message="delete-me")
    journal.write(e1)
    journal.write(e2)

    journal.delete(e2.id)

    remaining = journal.load_all()
    assert len(remaining) == 1
    assert remaining[0].message == "keep"


def test_delete_last_entry_removes_file(tmp_path: Path) -> None:
    """最後のエントリを削除するとファイルが削除される."""
    journal = Journal("bridge-last", base_dir=tmp_path)
    entry = journal.make_entry(to="@alice", message="solo")
    journal.write(entry)

    journal.delete(entry.id)

    assert not journal.path.exists()


def test_delete_nonexistent_id_is_noop(tmp_path: Path) -> None:
    """存在しない ID を削除しても例外なし・既存エントリは保持される."""
    journal = Journal("bridge-noop", base_dir=tmp_path)
    entry = journal.make_entry(to="@alice", message="hello")
    journal.write(entry)

    journal.delete("nonexistent-uuid-xxxx")  # 例外なし

    assert len(journal.load_all()) == 1


def test_delete_no_file_is_noop(tmp_path: Path) -> None:
    """ファイルが存在しない場合の delete は例外なし."""
    journal = Journal("ghost", base_dir=tmp_path)
    journal.delete("any-id")  # 例外なし


# ---------------------------------------------------------------------------
# round-trip: write → load → delete
# ---------------------------------------------------------------------------


def test_write_load_delete_roundtrip(tmp_path: Path) -> None:
    """write → load_all → delete の round-trip が正しく動く."""
    journal = Journal("roundtrip", base_dir=tmp_path)

    entries_in = [
        journal.make_entry(to=f"@peer{i}", message=f"msg-{i}", caused_by=f"root-{i}")
        for i in range(5)
    ]
    for e in entries_in:
        journal.write(e)

    loaded = journal.load_all()
    assert len(loaded) == 5

    # 中間の 2 エントリを削除
    journal.delete(entries_in[1].id)
    journal.delete(entries_in[3].id)

    remaining = journal.load_all()
    assert len(remaining) == 3
    remaining_ids = {e.id for e in remaining}
    assert entries_in[0].id in remaining_ids
    assert entries_in[2].id in remaining_ids
    assert entries_in[4].id in remaining_ids
    assert entries_in[1].id not in remaining_ids
    assert entries_in[3].id not in remaining_ids


# ---------------------------------------------------------------------------
# JournalEntry: caused_by が None の場合 JSON に null で保存され読み込める
# ---------------------------------------------------------------------------


def test_caused_by_none_roundtrip(tmp_path: Path) -> None:
    """caused_by=None が JSON に null で保存され、 load 後も None になる."""
    journal = Journal("nullcheck", base_dir=tmp_path)
    entry = journal.make_entry(to="@alice", message="no-cause")
    assert entry.caused_by is None
    journal.write(entry)

    loaded = journal.load_all()
    assert loaded[0].caused_by is None


# ---------------------------------------------------------------------------
# atomic rename: _write_all が tmp ファイルを経由する
# ---------------------------------------------------------------------------


def test_delete_is_atomic_via_rename(tmp_path: Path) -> None:
    """delete 後に tmp ファイルが残らないことを確認 (atomic rename の副作用チェック)."""
    import os

    journal = Journal("atomic", base_dir=tmp_path)
    e1 = journal.make_entry(to="@a", message="stay")
    e2 = journal.make_entry(to="@b", message="go")
    journal.write(e1)
    journal.write(e2)

    journal.delete(e2.id)

    # PID-based .journal.tmp が残っていないこと (reviewer Minor 1: PID suffix)
    pid_tmp = journal.path.with_name(f"{journal.path.stem}.{os.getpid()}.journal.tmp")
    assert not pid_tmp.exists()
    # ディレクトリ全体に .tmp ファイルが残っていないこと
    assert not list(tmp_path.glob("*.tmp"))
