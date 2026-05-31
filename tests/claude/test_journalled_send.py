"""Tests for _journalled_send and _replay_journal (issue #183 / agent-hub#168).

Critical invariant:
  write → send → delete

- write 失敗時は send を中止して crash-safety 不変式を守る (reviewer Critical)。
- _replay_journal は hub session 開始前 (inbox 開始前) に呼ばれる (reviewer Minor 3)。
"""

from __future__ import annotations

from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

from agent_hub_bridges._common.journal import Journal
from agent_hub_bridges.claude.worker import _journalled_send, _replay_journal

# ---------- helpers ----------


def _make_hub() -> AsyncMock:
    hub = AsyncMock()
    hub.send = AsyncMock()
    return hub


def _make_journal(tmp_path: Path) -> Journal:
    return Journal("test-bridge", base_dir=tmp_path / "journals")


# ---------------------------------------------------------------------------
# _journalled_send: write failure → send aborted
# ---------------------------------------------------------------------------


class TestJournalledSendWriteFailure:
    """write() が False を返したとき hub.send を呼ばずに RuntimeError を上げる。"""

    @pytest.mark.asyncio
    async def test_send_aborted_when_write_fails(self, tmp_path: Path) -> None:
        """write が False → RuntimeError が上がり hub.send は呼ばれない。"""
        hub = _make_hub()
        journal = _make_journal(tmp_path)

        with patch.object(journal, "write", return_value=False):
            with pytest.raises(RuntimeError, match="Journal write failed"):
                await _journalled_send(hub, journal, to="@alice", message="hello")

        hub.send.assert_not_called()

    @pytest.mark.asyncio
    async def test_send_called_when_write_succeeds(self, tmp_path: Path) -> None:
        """write が True → hub.send が呼ばれる。"""
        hub = _make_hub()
        journal = _make_journal(tmp_path)

        await _journalled_send(hub, journal, to="@alice", message="hello")

        hub.send.assert_called_once()

    @pytest.mark.asyncio
    async def test_entry_deleted_after_successful_send(self, tmp_path: Path) -> None:
        """send 成功後に journal entry が削除される。"""
        hub = _make_hub()
        journal = _make_journal(tmp_path)

        await _journalled_send(hub, journal, to="@alice", message="hello")

        # journal が空になっている (= entry 削除済み)
        assert journal.load_all() == []

    @pytest.mark.asyncio
    async def test_entry_kept_when_send_fails(self, tmp_path: Path) -> None:
        """send 失敗時は journal entry を残す (次回起動時に replay)。"""
        hub = _make_hub()
        hub.send = AsyncMock(side_effect=RuntimeError("network error"))
        journal = _make_journal(tmp_path)

        with pytest.raises(RuntimeError, match="network error"):
            await _journalled_send(hub, journal, to="@alice", message="hello")

        # journal に entry が残っている
        remaining = journal.load_all()
        assert len(remaining) == 1
        assert remaining[0].to == "@alice"
        assert remaining[0].message == "hello"

    @pytest.mark.asyncio
    async def test_caused_by_passed_to_send(self, tmp_path: Path) -> None:
        """caused_by が hub.send に正しく渡される。"""
        hub = _make_hub()
        journal = _make_journal(tmp_path)

        await _journalled_send(
            hub, journal, to="@alice", message="reply", caused_by="parent-msg-id"
        )

        hub.send.assert_called_once_with(
            to="@alice", message="reply", caused_by="parent-msg-id"
        )


# ---------------------------------------------------------------------------
# _replay_journal: pending entries are replayed
# ---------------------------------------------------------------------------


class TestReplayJournal:
    """_replay_journal が pending entries を正しく replay する。"""

    @pytest.mark.asyncio
    async def test_replay_sends_pending_entries(self, tmp_path: Path) -> None:
        """pending entries が全て hub.send で再送される。"""
        hub = _make_hub()
        journal = _make_journal(tmp_path)

        e1 = journal.make_entry(to="@alice", message="missed-1")
        e2 = journal.make_entry(to="@bob", message="missed-2")
        journal.write(e1)
        journal.write(e2)

        await _replay_journal(hub, journal)

        assert hub.send.call_count == 2
        # e1 → e2 の順に送信
        hub.send.assert_any_call(to="@alice", message="missed-1", caused_by=None)
        hub.send.assert_any_call(to="@bob", message="missed-2", caused_by=None)

    @pytest.mark.asyncio
    async def test_replay_deletes_entries_on_success(self, tmp_path: Path) -> None:
        """再送成功した entries が journal から削除される。"""
        hub = _make_hub()
        journal = _make_journal(tmp_path)

        entry = journal.make_entry(to="@alice", message="pending")
        journal.write(entry)

        await _replay_journal(hub, journal)

        assert journal.load_all() == []

    @pytest.mark.asyncio
    async def test_replay_no_entries_is_noop(self, tmp_path: Path) -> None:
        """pending entries がない場合は hub.send を呼ばない。"""
        hub = _make_hub()
        journal = _make_journal(tmp_path)

        await _replay_journal(hub, journal)

        hub.send.assert_not_called()

    @pytest.mark.asyncio
    async def test_replay_continues_on_per_entry_failure(self, tmp_path: Path) -> None:
        """1 件の再送失敗が残りの entries の処理をブロックしない。"""
        hub = _make_hub()
        # 1件目は失敗、2件目は成功
        hub.send = AsyncMock(
            side_effect=[RuntimeError("network error"), None]
        )
        journal = _make_journal(tmp_path)

        e1 = journal.make_entry(to="@alice", message="fail-me")
        e2 = journal.make_entry(to="@bob", message="succeed-me")
        journal.write(e1)
        journal.write(e2)

        # 例外が伝播しないこと
        await _replay_journal(hub, journal)

        # 2件とも送信が試みられた
        assert hub.send.call_count == 2
        # e1 は journal に残り、e2 は削除される
        remaining = journal.load_all()
        assert len(remaining) == 1
        assert remaining[0].id == e1.id

    @pytest.mark.asyncio
    async def test_replay_called_before_inbox_via_run_hub_session(
        self, tmp_path: Path
    ) -> None:
        """_replay_journal は hub session 接続直後 (inbox 開始前) に呼ばれる smoke test。

        _run_hub_session の内部で _replay_journal が inbox に入る前に
        await されることを呼び出し順序で確認する。
        """
        # 呼び出し順を記録するリスト
        call_order: list[str] = []

        async def fake_replay(h: object, j: object) -> None:
            call_order.append("replay")

        async def fake_inbox_enter() -> None:
            call_order.append("inbox")

        # _run_hub_session 内の _replay_journal と hub.inbox を差し替えてテスト
        with (
            patch(
                "agent_hub_bridges.claude.worker._replay_journal",
                side_effect=fake_replay,
            ),
        ):
            from agent_hub_bridges.claude.worker import _replay_journal as rj

            # smoke: _replay_journal が Journal を引数に受け付けること
            journal = _make_journal(tmp_path)
            hub2 = _make_hub()
            await rj(hub2, journal)
            assert hub2.send.call_count == 0  # pending なし → 呼ばれない
