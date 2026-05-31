"""Unit tests for `_common.inventory` (issue #82: circuit breaker)."""

from __future__ import annotations

from pathlib import Path

import pytest

from agent_hub_bridges._common.inventory import (
    _resolve_inventory_path,
    dead_marker_path,
    write_dead_marker,
    write_lost_hub_to_inventory,
)

# ---------------------------------------------------------------------------
# dead_marker_path
# ---------------------------------------------------------------------------


def test_dead_marker_path_format() -> None:
    """dead marker のパス形式を確認する."""
    path = dead_marker_path("bridges-impl")
    assert str(path) == "/tmp/agent-hub-bridge-bridges-impl.dead"


# ---------------------------------------------------------------------------
# write_dead_marker
# ---------------------------------------------------------------------------


def test_write_dead_marker_creates_file(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """write_dead_marker が dead marker ファイルを作成する."""
    import agent_hub_bridges._common.inventory as inv_module

    marker_path = tmp_path / "agent-hub-bridge-test-user.dead"
    monkeypatch.setattr(inv_module, "_DEAD_MARKER_DIR", tmp_path)

    write_dead_marker("test-user")

    assert marker_path.exists(), "dead marker ファイルが作成されるはず"
    content = marker_path.read_text(encoding="utf-8")
    assert content.startswith("lost-hub\n"), "1 行目は 'lost-hub' のはず"


def test_write_dead_marker_does_not_raise_on_error(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """書き込み失敗しても例外が伝播しない (= circuit breaker shutdown を妨げない)."""
    import agent_hub_bridges._common.inventory as inv_module

    # 存在しないディレクトリに書こうとする → PermissionError 相当
    monkeypatch.setattr(inv_module, "_DEAD_MARKER_DIR", Path("/nonexistent/path"))

    # 例外が出ないことを確認
    write_dead_marker("some-user")  # should not raise


# ---------------------------------------------------------------------------
# _resolve_inventory_path
# ---------------------------------------------------------------------------


def test_resolve_inventory_path_returns_none_when_unset(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv("BRIDGE_INVENTORY", raising=False)
    assert _resolve_inventory_path() is None


def test_resolve_inventory_path_returns_path_from_env(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    expected = tmp_path / "bridge-inventory.md"
    monkeypatch.setenv("BRIDGE_INVENTORY", str(expected))
    result = _resolve_inventory_path()
    assert result == expected


# ---------------------------------------------------------------------------
# write_lost_hub_to_inventory
# ---------------------------------------------------------------------------


def test_write_lost_hub_to_inventory_skips_when_env_unset(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """BRIDGE_INVENTORY が未設定なら何もしない (例外なし)."""
    monkeypatch.delenv("BRIDGE_INVENTORY", raising=False)
    write_lost_hub_to_inventory("test-user", pid=12345)  # should not raise


def test_write_lost_hub_to_inventory_skips_missing_file(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    """inventory ファイルが存在しなければ何もしない (例外なし)."""
    monkeypatch.setenv("BRIDGE_INVENTORY", str(tmp_path / "nonexistent.md"))
    write_lost_hub_to_inventory("test-user", pid=12345)  # should not raise


def test_write_lost_hub_to_inventory_inserts_after_marker(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    """マーカー行の直後に lost-hub エントリが挿入される."""
    inventory = tmp_path / "bridge-inventory.md"
    inventory.write_text(
        "## Activity log\n"
        "\n"
        "新しいエントリを上に追加\n"
        "- 2026-05-30 12:00 — **start** `@old-bridge` — pid=111\n",
        encoding="utf-8",
    )
    monkeypatch.setenv("BRIDGE_INVENTORY", str(inventory))

    write_lost_hub_to_inventory("bridges-impl", pid=42)

    content = inventory.read_text(encoding="utf-8")
    lines = content.splitlines()

    # マーカー行の次の行が lost-hub エントリになっているはず
    marker_idx = next(i for i, line in enumerate(lines) if "新しいエントリを上に追加" in line)
    inserted = lines[marker_idx + 1]
    assert "**lost-hub**" in inserted, f"inserted line: {inserted!r}"
    assert "`@bridges-impl`" in inserted
    assert "pid=42" in inserted


def test_write_lost_hub_to_inventory_fallback_without_marker(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    """マーカーが無い inventory でも末尾にエントリが追加される (fallback)."""
    inventory = tmp_path / "bridge-inventory.md"
    inventory.write_text(
        "## Activity log\n\n- old entry\n",
        encoding="utf-8",
    )
    monkeypatch.setenv("BRIDGE_INVENTORY", str(inventory))

    write_lost_hub_to_inventory("some-bridge", pid=99)

    content = inventory.read_text(encoding="utf-8")
    assert "**lost-hub**" in content
    assert "`@some-bridge`" in content
    assert "pid=99" in content


def test_write_lost_hub_to_inventory_does_not_raise_on_write_error(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    """書き込み失敗しても例外が伝播しない."""
    inventory = tmp_path / "readonly.md"
    inventory.write_text("新しいエントリを上に追加\n", encoding="utf-8")
    inventory.chmod(0o444)  # read-only
    monkeypatch.setenv("BRIDGE_INVENTORY", str(inventory))

    write_lost_hub_to_inventory("some-bridge", pid=1)  # should not raise

    inventory.chmod(0o644)  # cleanup
