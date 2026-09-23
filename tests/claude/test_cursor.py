"""Tests for agent_hub_bridges.claude.cursor (issue #37)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from agent_hub_bridges.claude.cursor import (
    _CURSOR_FILE_ENV,
    _DEFAULT_CURSOR_TEMPLATE,
    Cursor,
    advance,
    cursor_path,
    is_seen,
    load_cursor,
    save_cursor,
)

# ---------------------------------------------------------------------------
# cursor_path
# ---------------------------------------------------------------------------


def test_cursor_path_default(monkeypatch: pytest.MonkeyPatch) -> None:
    """環境変数なしのデフォルト path を確認."""
    monkeypatch.delenv(_CURSOR_FILE_ENV, raising=False)
    expected = Path(_DEFAULT_CURSOR_TEMPLATE.format(user="testuser"))
    assert cursor_path("testuser") == expected


def test_cursor_path_env_override(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """AGENT_HUB_CURSOR_FILE 環境変数で path が上書きされることを確認."""
    custom = tmp_path / "my-cursor.json"
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(custom))
    assert cursor_path("anyuser") == custom


# ---------------------------------------------------------------------------
# load_cursor
# ---------------------------------------------------------------------------


def test_load_cursor_no_file(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """cursor file が存在しない場合は None を返す (クラッシュしない)."""
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(tmp_path / "nonexistent.json"))
    assert load_cursor("user1") is None


def test_load_cursor_valid_file(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """有効な cursor file から timestamp と処理済み ID を読み込む."""
    ts = "2026-05-21T12:00:00.000Z"
    cursor_file = tmp_path / "cursor.json"
    cursor_file.write_text(
        json.dumps({"last_processed_at": ts, "ids_at_last_processed_at": ["m1", "m2"]})
    )
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    result = load_cursor("user1")
    assert result == Cursor(ts=ts, ids=("m1", "m2"))


def test_load_cursor_legacy_file(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """ID の無い旧形式の file は ID 空で読み、同じ timestamp の message を捨てない (issue #323)."""
    ts = "2026-05-21T12:00:00.000Z"
    cursor_file = tmp_path / "cursor.json"
    cursor_file.write_text(json.dumps({"last_processed_at": ts}))
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    result = load_cursor("user1")
    assert result == Cursor(ts=ts)
    assert is_seen(result, "old", "2026-05-21T11:59:59.999Z")
    assert not is_seen(result, "m2", ts)


def test_load_cursor_malformed_json(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """壊れた JSON ファイルでも None を返す (クラッシュしない)."""
    cursor_file = tmp_path / "cursor.json"
    cursor_file.write_text("NOT VALID JSON {{{")
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    assert load_cursor("user1") is None


def test_load_cursor_missing_key(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """``last_processed_at`` キーが欠けている場合は None を返す."""
    cursor_file = tmp_path / "cursor.json"
    cursor_file.write_text(json.dumps({"other_key": "value"}))
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    assert load_cursor("user1") is None


def test_load_cursor_empty_string_value(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """``last_processed_at`` が空文字の場合は None を返す."""
    cursor_file = tmp_path / "cursor.json"
    cursor_file.write_text(json.dumps({"last_processed_at": ""}))
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    assert load_cursor("user1") is None


# ---------------------------------------------------------------------------
# save_cursor
# ---------------------------------------------------------------------------


def test_save_cursor_creates_file(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """save_cursor が正しい JSON ファイルを作成する."""
    cursor_file = tmp_path / "cursor.json"
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    ts = "2026-05-21T15:30:00.000Z"
    save_cursor("user1", Cursor(ts=ts, ids=("m1",)))

    assert cursor_file.exists()
    data = json.loads(cursor_file.read_text())
    assert data["last_processed_at"] == ts
    assert data["ids_at_last_processed_at"] == ["m1"]


def test_save_cursor_overwrites_existing(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """save_cursor が既存 cursor を上書きする."""
    cursor_file = tmp_path / "cursor.json"
    cursor_file.write_text(json.dumps({"last_processed_at": "2026-05-21T10:00:00.000Z"}))
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    new_ts = "2026-05-21T20:00:00.000Z"
    save_cursor("user1", Cursor(ts=new_ts))

    data = json.loads(cursor_file.read_text())
    assert data["last_processed_at"] == new_ts


def test_save_cursor_unwritable_dir_does_not_raise(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    """書き込み不可のパスでも例外を上げない (warning ログのみ)."""
    # 存在しないサブディレクトリ内を指定 → write_text が失敗する
    cursor_file = tmp_path / "nonexistent_subdir" / "cursor.json"
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    # 例外が上がらないことを確認
    save_cursor("user1", Cursor(ts="2026-05-21T12:00:00.000Z"))


# ---------------------------------------------------------------------------
# round-trip: save then load
# ---------------------------------------------------------------------------


def test_save_then_load_roundtrip(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """save_cursor → load_cursor の round-trip が正しく動く."""
    cursor_file = tmp_path / "cursor.json"
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    cursor = Cursor(ts="2026-05-22T08:00:00.000Z", ids=("m1", "m2"))
    save_cursor("userA", cursor)
    result = load_cursor("userA")
    assert result == cursor


# ---------------------------------------------------------------------------
# is_seen / advance (issue #323)
# ---------------------------------------------------------------------------


def test_is_seen_same_millisecond_only_processed_ids() -> None:
    """同じ ms の message は処理済みの ID だけ seen になる."""
    ts = "2026-09-16T09:00:01.000Z"
    cursor = advance(None, "m1", ts)

    assert is_seen(cursor, "m1", ts)
    assert not is_seen(cursor, "m2", ts)
    assert is_seen(cursor, "m0", "2026-09-16T09:00:00.999Z")
    assert not is_seen(cursor, "m3", "2026-09-16T09:00:01.001Z")


def test_is_seen_none_sees_nothing() -> None:
    assert not is_seen(None, "m1", "2026-09-16T09:00:01.000Z")


def test_advance() -> None:
    """同じ ts は ID を積み、新しい ts で 1 件に戻り、古い ts では位置を戻さない."""
    ts = "2026-09-16T09:00:01.000Z"
    c1 = advance(None, "m1", ts)
    c2 = advance(c1, "m2", ts)
    assert c2 == Cursor(ts=ts, ids=("m1", "m2"))
    assert c1 == Cursor(ts=ts, ids=("m1",))
    assert advance(c2, "m1", ts) == c2

    c3 = advance(c2, "m3", "2026-09-16T09:00:01.001Z")
    assert c3 == Cursor(ts="2026-09-16T09:00:01.001Z", ids=("m3",))
    assert advance(c3, "old", ts) == c3
