"""Tests for cursor same-ms handling through the _run_hub_session inbox loop (issue #329).

``test_cursor.py`` は ``is_seen`` / ``advance`` / load / save の単体だけを見ている。
ここでは ``_run_hub_session`` の inbox loop を実際に回し、同じ ms の message が
全件 dispatch されること、保存された cursor にその ID が全部入っていることを確かめる。

hub (``AgentHub.connect``) / ``ClaudeRunner`` / ``_handle_one`` は mock に差し替える。
"""

from __future__ import annotations

import contextlib
from collections.abc import AsyncIterator
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from agent_hub_sdk import IncomingMessage

from agent_hub_bridges.claude.cursor import _CURSOR_FILE_ENV, Cursor, load_cursor
from agent_hub_bridges.claude.worker import (
    _ActivityTracker,
    _IdleCompactWatchdog,
    _MessageGapTracker,
    _run_hub_session,
)

_TS = "2026-09-16T09:00:01.000Z"


def _msg(msg_id: str, ts: str = _TS) -> IncomingMessage:
    return IncomingMessage(
        id=msg_id, sender="@alice", to="@bridges-impl", body=f"body {msg_id}", timestamp=ts
    )


def _make_hub(inbox: list[IncomingMessage]) -> MagicMock:
    """startup catchup は空、inbox loop は ``inbox`` を順に流す hub。"""
    hub = MagicMock()
    hub.register = AsyncMock(return_value="ok")
    hub.get_unread = AsyncMock(return_value=[])
    hub.ack = AsyncMock()

    @contextlib.asynccontextmanager
    async def _inbox(**_kwargs: object) -> AsyncIterator[AsyncIterator[IncomingMessage]]:
        async def _gen() -> AsyncIterator[IncomingMessage]:
            for m in inbox:
                yield m

        yield _gen()

    hub.inbox = _inbox
    return hub


def _make_runner() -> MagicMock:
    runner = MagicMock()
    runner.__aenter__ = AsyncMock(return_value=runner)
    runner.__aexit__ = AsyncMock(return_value=None)
    return runner


async def _run_session(
    tmp_path: Path, inbox: list[IncomingMessage], cursor: Cursor | None
) -> tuple[Cursor | None, list[str]]:
    """``_run_hub_session`` を 1 回回し、返った cursor と dispatch した message ID を返す。"""
    hub = _make_hub(inbox)

    @contextlib.asynccontextmanager
    async def _connect(**_kwargs: object) -> AsyncIterator[MagicMock]:
        yield hub

    cfg = MagicMock()
    cfg.workdir = tmp_path
    cfg.user = "bridges-impl"
    cfg.model = "claude-sonnet-4-6"

    handle_one = AsyncMock()
    with (
        patch("agent_hub_bridges.claude.worker.AgentHub.connect", _connect),
        patch("agent_hub_bridges.claude.worker.ClaudeRunner", return_value=_make_runner()),
        patch("agent_hub_bridges.claude.worker._build_options", return_value=MagicMock()),
        patch("agent_hub_bridges.claude.worker._replay_journal", AsyncMock()),
        patch("agent_hub_bridges.claude.worker._handle_one", handle_one),
    ):
        result = await _run_hub_session(
            cfg,
            tmp_path / "mcp.json",
            [None],
            cursor,
            _ActivityTracker(),
            _MessageGapTracker(),
            _IdleCompactWatchdog(),
            MagicMock(),
        )
    dispatched = [c.args[2].id for c in handle_one.await_args_list]
    return result, dispatched


async def test_inbox_loop_same_millisecond_all_dispatched(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """同じ ms の 3 通が inbox loop で全件 dispatch され、cursor に 3 件の ID が入る。

    再起動相当 (保存された cursor を読み直す) の後は、同じ 3 通を再 dispatch せず、
    同じ ms の新着だけを dispatch する。
    """
    monkeypatch.setenv(_CURSOR_FILE_ENV, str(tmp_path / "cursor.json"))
    msgs = [_msg("m1"), _msg("m2"), _msg("m3")]

    cursor, dispatched = await _run_session(tmp_path, msgs, None)

    assert dispatched == ["m1", "m2", "m3"], "same-ms messages must not be skipped"
    want = Cursor(ts=_TS, ids=("m1", "m2", "m3"))
    assert cursor == want
    assert load_cursor("bridges-impl") == want

    # 再起動相当: 保存された cursor を読み直し、同じ 3 件 + 同じ ms の新着 1 件を流す
    reloaded = load_cursor("bridges-impl")
    cursor, dispatched = await _run_session(tmp_path, [*msgs, _msg("m4")], reloaded)

    assert dispatched == ["m4"]
    assert load_cursor("bridges-impl") == Cursor(ts=_TS, ids=("m1", "m2", "m3", "m4"))
