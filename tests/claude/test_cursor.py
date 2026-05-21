"""Tests for agent_hub_bridges.claude.cursor (issue #37)."""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from agent_hub_bridges.claude.cursor import (
    _CURSOR_FILE_ENV,
    _DEFAULT_CURSOR_TEMPLATE,
    cursor_path,
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
    """有効な cursor file から timestamp を読み込む."""
    ts = "2026-05-21T12:00:00.000Z"
    cursor_file = tmp_path / "cursor.json"
    cursor_file.write_text(json.dumps({"last_processed_at": ts}))
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    result = load_cursor("user1")
    assert result == ts


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
    save_cursor("user1", ts)

    assert cursor_file.exists()
    data = json.loads(cursor_file.read_text())
    assert data["last_processed_at"] == ts


def test_save_cursor_overwrites_existing(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """save_cursor が既存 cursor を上書きする."""
    cursor_file = tmp_path / "cursor.json"
    cursor_file.write_text(json.dumps({"last_processed_at": "2026-05-21T10:00:00.000Z"}))
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    new_ts = "2026-05-21T20:00:00.000Z"
    save_cursor("user1", new_ts)

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
    save_cursor("user1", "2026-05-21T12:00:00.000Z")


# ---------------------------------------------------------------------------
# round-trip: save then load
# ---------------------------------------------------------------------------


def test_save_then_load_roundtrip(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """save_cursor → load_cursor の round-trip が正しく動く."""
    cursor_file = tmp_path / "cursor.json"
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(cursor_file))

    ts = "2026-05-22T08:00:00.000Z"
    save_cursor("userA", ts)
    result = load_cursor("userA")
    assert result == ts
